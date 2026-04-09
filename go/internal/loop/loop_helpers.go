package loop

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/attempts"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/workctx"
)

type initParams struct {
	maxIter int
	state   *state.Store
	backend tasks.Backend
	logger  *logging.Logger
	git     git.GitOps
}

type initWorktreeParams struct {
	git     git.GitOps
	dirs    workctx.WorkContext
	backend tasks.Backend
	state   *state.Store
	logger  *logging.Logger
}

// initialize performs all one-time setup: skipped task loading,
// state config write, and worktree sync. The caller is responsible for
// calling limiter.Init() before initialize.
func initialize(ctx context.Context, p initParams) error {
	p.state.WriteConfig(p.maxIter)

	if skipped, err := p.state.GetSkippedTasks(); err == nil && len(skipped) > 0 {
		p.backend.SetSkippedIDs(skipped)
		p.logger.Emit(logging.Opts{Domain: logging.Beads}, "Loaded %d skipped tasks from state", len(skipped))
	}

	p.state.ClearCompletedTasksFile()

	return initWorktree(ctx, initWorktreeParams{
		git:     p.git,
		backend: p.backend,
		state:   p.state,
		logger:  p.logger,
	})
}

// initWorktree restores worktree state on resume and syncs to the correct base.
// Derives the stack head from completed beads, then rebases onto it
// (or the default branch if no stack exists).
func initWorktree(ctx context.Context, p initWorktreeParams) error {
	if p.git.GetWorktreeBranch() == "" || p.git.GetWorkDir() == p.git.GetProjectDir() {
		return nil
	}

	// One-time sync: rebase onto latest base so the first iteration
	// starts from a clean state. prepareBranch handles this on subsequent
	// iterations, but we need it here before the loop starts.
	setStackHead(p.git, p.backend, p.state, p.logger)
	if p.git.GetPrevBranch() == "" {
		p.git.ResetToDefaultBranch()
	}
	if err := p.git.EnsureUpToDate(ctx); err != nil {
		if ctx.Err() != nil {
			p.state.Write("status", "stopped")
			return nil
		}
		p.state.Write("status", "error")
		return fmt.Errorf("initial rebase failed: %w", err)
	}

	// If resuming the same task, mark the branch as already renamed so
	// prepareBranch doesn't re-rename it on the first iteration.
	if lastID, _ := p.state.Read("last_task_id"); lastID != "" {
		p.backend.SetResumeTaskID(lastID)
	}
	nextInfo, _ := p.backend.GetNextTaskInfo()
	if !isNewTask(p.state, nextInfo.ID, nextInfo.Title) {
		p.git.SetBranchRenamed(true)
	}

	p.logger.Emit(logging.Opts{Domain: logging.Git}, "Branch: %s", p.git.GetWorktreeBranch())
	p.state.WriteRunBranch(p.git.GetWorktreeBranch())
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

type processRunOutcomeParams struct {
	backend        tasks.Backend
	git            git.GitOps
	logger         *logging.Logger
	state          *state.Store
	attempts       *attempts.Tracker
	analysisResult analyzer.Result
	headAfter      string
	model          string
}

// processRunOutcome logs the Claude result and records attempts. Returns the
// diffStat (needed by signal handling) and halt=true if the analyzer says to
// stop (caller should return nil).
func processRunOutcome(p processRunOutcomeParams, result claude.Result, elapsed time.Duration, runIteration int, prep iterationPrompt, taskID, nextTask string) (string, bool, analyzer.Action) {
	if result.Summary != "" {
		p.logger.Emit(logging.Opts{Domain: logging.LLM, Model: p.model}, "Summary: %s", result.Summary)
	}

	completed, _ := p.backend.CountCompleted()
	total, _ := p.backend.CountTotal()
	p.logger.Emit(logging.Opts{}, "Run iteration %d complete (%dm%ds). %d/%d tasks done.",
		runIteration, int(elapsed.Minutes()), int(elapsed.Seconds())%60, completed, total)

	diffStat := p.git.DiffStatRange(prep.headBefore, p.headAfter)
	analysisResult := p.analysisResult

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
		p.logger.Emit(logging.Opts{Domain: logging.Analyzer, Level: logging.Error}, "Halting: %s", analysisResult.Reason)
		if analysisResult.Detail != "" {
			p.logger.Emit(logging.Opts{Domain: logging.Analyzer, Level: logging.Error}, "  %s", analysisResult.Detail)
		}
		p.attempts.Record(taskID, nextTask, "Halted: "+analysisResult.Reason, diffStat, analysisResult.Detail)
		p.state.Write("status", "halted_"+analysisResult.Reason)
		p.git.TagTaskEnd(taskID)
		return diffStat, true, analysisResult.Action
	case analyzer.Warn:
		p.logger.Emit(logging.Opts{Domain: logging.Analyzer, Level: logging.Warn}, "Analysis: %s", analysisResult.Reason)
		p.attempts.Record(taskID, nextTask, summary, diffStat, "warn: "+analysisDesc)
	default:
		if !result.SignalDetected {
			p.attempts.Record(taskID, nextTask, summary, diffStat, analysisDesc)
		}
	}

	return diffStat, false, analysisResult.Action
}
