package loop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
)

// binaryHashChanged returns true when the on-disk binary at os.Executable()
// has a different SHA-256 than the hash captured at loop startup. Returns
// false when the startup hash is unset (hash guard disabled) or on error.
func (l *Loop) binaryHashChanged() bool {
	if len(l.startupBinaryHash) == 0 {
		return false
	}
	current, err := l.binaryHasher.Hash()
	if err != nil {
		l.logger.Emit(logging.Opts{Domain: "loop", Level: logging.Warn}, "Evolve: binary hash check failed: %v — skipping restart", err)
		return false
	}
	return !bytes.Equal(l.startupBinaryHash, current)
}

// maybeEvolve checks whether the on-disk binary has changed since loop
// startup. If it has, it records the evolve_restart status and returns
// signalEvolve so the caller can restart. When the binary is unchanged,
// it logs the decision and returns fallback so the iteration continues
// normally. Must be called only after execRunPostTask has returned.
func (l *Loop) maybeEvolve(fallback postSignalAction) postSignalAction {
	if l.binaryHashChanged() {
		l.logger.Phase("Evolve: binary changed — restarting")
		l.state.Write("status", "evolve_restart") //nolint:errcheck
		return signalEvolve
	}
	l.logger.Emit(logging.Opts{Domain: "loop"}, "Evolve: binary unchanged — continuing")
	return fallback
}

// initialize performs all one-time setup: skipped task loading,
// state config write, and worktree sync. The caller is responsible for
// calling limiter.Init() before initialize.
func (l *Loop) initialize(ctx context.Context) error {
	l.state.WriteConfig(l.cfg.MaxIterations)

	if skipped, err := l.state.GetSkippedTasks(); err == nil && len(skipped) > 0 {
		l.taskBackend.SetSkippedIDs(skipped)
		l.logger.Emit(logging.Opts{Domain: logging.Beads}, "Loaded %d skipped tasks from state", len(skipped))
	}

	l.state.ClearCompletedTasksFile()

	return l.initWorktree(ctx)
}

// initWorktree restores worktree state on resume and syncs to the correct base.
// Derives the stack head from completed beads, then rebases onto it
// (or the default branch if no stack exists).
func (l *Loop) initWorktree(ctx context.Context) error {
	if l.git.GetWorktreeBranch() == "" || l.git.GetWorkDir() == l.git.GetProjectDir() {
		return nil
	}

	// One-time sync: rebase onto latest base so the first iteration
	// starts from a clean state. BranchForTask handles this on subsequent
	// iterations, but we need it here before the loop starts.
	completedBranches := l.completedBranches()
	if err := l.git.SyncWorktreeBase(ctx, completedBranches); err != nil {
		if ctx.Err() != nil {
			l.state.Write("status", "stopped")
			return nil
		}
		// A local-rebase abort on an in-flight branch is recoverable: the
		// branch state is intact, the agent or the next task boundary will
		// handle divergence. Warn and continue rather than stranding the loop.
		var localConflict *git.LocalRebaseConflictError
		if errors.As(err, &localConflict) {
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "%v — continuing with stale base", localConflict)
		} else {
			l.state.Write("status", "error")
			return fmt.Errorf("initial rebase failed: %w", err)
		}
	}

	// If resuming the same task, mark the branch as already renamed so
	// BranchForTask doesn't re-rename it on the first iteration.
	if lastID, _ := l.state.Read("last_task_id"); lastID != "" {
		l.taskBackend.SetResumeTaskID(lastID)
	}
	nextInfo, _ := l.taskBackend.GetNextTaskInfo()
	lastID, _ := l.state.Read("last_task_id")
	lastTask, _ := l.state.Read("last_task")
	if !isNewTask(lastID, lastTask, nextInfo.ID, nextInfo.Title) {
		l.git.SetBranchRenamed(true)
	}

	l.logger.Emit(logging.Opts{Domain: logging.Git}, "Branch: %s", l.git.GetWorktreeBranch())
	l.state.WriteRunBranch(l.git.GetWorktreeBranch())
	return nil
}

type analyzeIterationParams struct {
	hasDiff      bool
	changedFiles []string
	signals      claude.SignalPaths
}

// analyzeIteration assembles an IterationState from pre-computed git data and
// log content. The caller is responsible for calling analyzer.Analyze on the
// returned state.
func analyzeIteration(p analyzeIterationParams, rawLogPath string, logStart int, headBefore, headAfter, taskKey string) analyzer.IterationState {
	iterLog := readLogFrom(rawLogPath, logStart)
	newCommits := headBefore != "" && headAfter != "" && headBefore != headAfter

	hasSignal := false
	if _, err := os.Stat(p.signals.Complete); err == nil {
		hasSignal = true
	}
	if _, err := os.Stat(p.signals.AllComplete); err == nil {
		hasSignal = true
	}

	return analyzer.IterationState{
		HasDiff:      p.hasDiff,
		NewCommits:   newCommits,
		HasSignal:    hasSignal,
		ChangedFiles: p.changedFiles,
		IterationLog: iterLog,
		TaskKey:      taskKey,
	}
}

// processRunOutcome logs the Claude result and records attempts. Returns the
// diffStat (needed by signal handling) and halt=true if the analyzer says to
// stop (caller should return nil).
func (l *Loop) processRunOutcome(result claude.Result, elapsed time.Duration, runIteration int, prep iterationPrompt, taskID, nextTask string, analysisResult analyzer.Result, headAfter string) (string, bool, analyzer.Action) {
	if result.Summary != "" {
		l.logger.Emit(logging.Opts{Domain: logging.LLM, Model: l.cfg.Model}, "Summary: %s", result.Summary)
	}

	completed, _ := l.taskBackend.CountCompleted()
	total, _ := l.taskBackend.CountTotal()
	l.logger.Emit(logging.Opts{}, "Run iteration %d complete (%dm%ds). %d/%d tasks done.",
		runIteration, int(elapsed.Minutes()), int(elapsed.Seconds())%60, completed, total)

	diffStat := l.git.DiffStatRange(prep.headBefore, headAfter)

	summary := result.Summary
	if summary == "" {
		summary = "no completion summary"
	}
	analysisDesc := analysisResult.Reason
	if analysisDesc == "" {
		analysisDesc = "continue"
	}

	switch analysisResult.Action {
	case analyzer.Halt:
		l.logger.Emit(logging.Opts{Domain: logging.Analyzer, Level: logging.Error}, "Halting: %s", analysisResult.Reason)
		if analysisResult.Detail != "" {
			l.logger.Emit(logging.Opts{Domain: logging.Analyzer, Level: logging.Error}, "  %s", analysisResult.Detail)
		}
		l.attempts.Record(taskID, nextTask, "Halted: "+analysisResult.Reason, diffStat, analysisResult.Detail)
		l.state.Write("status", "halted_"+analysisResult.Reason)
		l.git.TagTaskEnd(taskID)
		return diffStat, true, analysisResult.Action
	case analyzer.Warn:
		l.logger.Emit(logging.Opts{Domain: logging.Analyzer, Level: logging.Warn}, "Analysis: %s", analysisResult.Reason)
		l.attempts.Record(taskID, nextTask, summary, diffStat, "warn: "+analysisDesc)
	default:
		if !result.SignalDetected {
			l.attempts.Record(taskID, nextTask, summary, diffStat, analysisDesc)
		}
	}

	return diffStat, false, analysisResult.Action
}
