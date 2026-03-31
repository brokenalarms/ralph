package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/logging"
)

// initRun restores worktree state on resume and syncs to the correct base.
// Derives the stack head from completed beads, then rebases onto it
// (or the default branch if no stack exists).
func (l *Loop) initRun(ctx context.Context) error {
	if l.git.GetWorktreeBranch() == "" || l.git.GetWorkDir() == l.git.GetProjectDir() {
		return nil
	}

	// One-time sync: rebase onto latest base so the first iteration
	// starts from a clean state. prepareBranch handles this on subsequent
	// iterations, but we need it here before the loop starts.
	l.setStackHead()
	if l.git.GetPrevBranch() == "" {
		l.git.ResetToDefaultBranch()
	}
	if err := l.handleRebase(ctx); err != nil {
		if ctx.Err() != nil {
			l.state.Write("status", "stopped")
			return nil
		}
		l.state.Write("status", "error")
		return fmt.Errorf("initial rebase failed: %w", err)
	}

	// If resuming the same task, mark the branch as already renamed so
	// prepareBranch doesn't re-rename it on the first iteration.
	if lastID, _ := l.state.Read("last_task_id"); lastID != "" {
		l.cfg.TaskBackend.SetResumeTaskID(lastID)
	}
	nextInfo, _ := l.cfg.TaskBackend.GetNextTaskInfo()
	if !isNewTask(l.state, nextInfo.ID, nextInfo.Title) {
		l.git.SetBranchRenamed(true)
	}

	l.logger.Log("git", "Branch: %s", l.git.GetWorktreeBranch())
	writeRunBranch(l.cfg.Dirs.RalphDir, l.git.GetWorktreeBranch())
	return nil
}

func (l *Loop) waitForTasks(ctx context.Context) bool {
	const waitPollInterval = 5 * time.Second
	l.logger.Log("beads", "Waiting for new tasks (polling every %s)...", waitPollInterval)
	l.state.Write("status", "waiting")
	updateStreamTask(l.cfg.Dirs.RalphDir, "", "Waiting for tasks...", nil)
	touchFile(filepath.Join(l.cfg.Dirs.RalphDir, ".plan-refresh"))
	if l.onWaitFunc != nil {
		l.onWaitFunc()
	}

	// Check immediately before waiting for the first tick.
	if found, done := l.pollForTasks(); found || done {
		return found
	}

	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			l.state.Write("status", "stopped")
			return false
		case <-ticker.C:
			if found, done := l.pollForTasks(); found || done {
				return found
			}
		}
	}
}

// pollForTasks checks once for new tasks. Returns (found=true, _) if tasks
// are available, (false, done=true) if a stop condition was hit.
func (l *Loop) pollForTasks() (found, done bool) {
	if checkStopFile(l.cfg.Dirs.RalphDir) {
		l.logger.Warn("", "Stop file detected - halting")
		l.state.Write("status", "stopped")
		return false, true
	}
	if skipped, err := l.state.GetSkippedTasks(); err == nil {
		l.cfg.TaskBackend.SetSkippedIDs(skipped)
	}
	hasRemaining, err := l.cfg.TaskBackend.HasRemaining()
	if err != nil {
		l.logger.Warn("beads", "Task check error during wait: %v", err)
		return false, false
	}
	if hasRemaining {
		l.logger.Success("beads", "New tasks detected!")
		touchFile(filepath.Join(l.cfg.Dirs.RalphDir, ".plan-refresh"))
		return true, false
	}
	return false, false
}

// beginIteration records that a task iteration is starting.
func (l *Loop) beginIteration(task taskContext, iteration int) {
	touchFile(filepath.Join(l.cfg.Dirs.RalphDir, ".plan-refresh"))
	l.state.BeginIteration(task.id, task.title, iteration)
	l.git.TagTaskStart(task.id)
	updateStreamTask(l.cfg.Dirs.RalphDir, task.id, task.title, task.info.Priority)
}

func (l *Loop) waitForRate(ctx context.Context) bool {
	if l.limiter.Allowed() {
		return true
	}

	l.logger.Warn("llm", "Rate limit reached (%d/%d calls this hour)", l.limiter.Count(), l.cfg.CallsPerHour)

	err := l.limiter.WaitForReset(ctx, func(secs int) {
		l.logger.Log("llm", "Rate limit: %ds until reset", secs)
	})
	return err == nil
}

func (l *Loop) analyzeIteration(rawLogPath string, logStart int, headBefore, headAfter, taskKey string) analyzer.Result {
	iterLog := readLogFrom(rawLogPath, logStart)
	hasDiff := l.git.HasDiff()
	newCommits := headBefore != "" && headAfter != "" && headBefore != headAfter
	changedFiles := l.git.ChangedFiles(headBefore, headAfter)

	hasSignal := false
	if _, err := os.Stat(l.signals.Complete); err == nil {
		hasSignal = true
	}
	if _, err := os.Stat(l.signals.AllComplete); err == nil {
		hasSignal = true
	}

	return l.analyzer.Analyze(analyzer.IterationState{
		HasDiff:      hasDiff,
		NewCommits:   newCommits,
		HasSignal:    hasSignal,
		ChangedFiles: changedFiles,
		IterationLog: iterLog,
		TaskKey:      taskKey,
	})
}

// processRunOutcome logs the Claude result, analyzes the iteration, and
// records attempts. Returns the diffStat (needed by signal handling) and
// halt=true if the analyzer says to stop (caller should return nil).
func (l *Loop) processRunOutcome(result claude.Result, elapsed time.Duration, runIteration int, prep iterationPrompt, taskID, nextTask string) (string, bool) {
	if result.Summary != "" {
		l.logger.Log("llm", "Summary: %s", result.Summary)
	}

	completed, _ := l.cfg.TaskBackend.CountCompleted()
	total, _ := l.cfg.TaskBackend.CountTotal()
	l.logger.Log("", "Run iteration %d complete (%dm%ds). %d/%d tasks done.",
		runIteration, int(elapsed.Minutes()), int(elapsed.Seconds())%60, completed, total)

	headAfter := l.git.HeadRev()
	diffStat := l.git.DiffStatRange(prep.headBefore, headAfter)
	analysisResult := l.analyzeIteration(prep.rawLogPath, prep.logStart, prep.headBefore, headAfter, taskID)

	summary := result.Summary
	if summary == "" {
		summary = "no completion summary"
	}
	analysisDesc := analysisResult.Reason
	if analysisDesc == "" {
		analysisDesc = "continue"
	}

	l.lastAction = analysisResult.Action

	switch analysisResult.Action {
	case analyzer.Halt:
		l.logger.Error(logging.Analyzer, "Halting: %s", analysisResult.Reason)
		if analysisResult.Detail != "" {
			l.logger.Error(logging.Analyzer, "  %s", analysisResult.Detail)
		}
		l.attempts.Record(taskID, nextTask, "Halted: "+analysisResult.Reason, diffStat, analysisResult.Detail)
		l.state.Write("status", "halted_"+analysisResult.Reason)
		l.git.TagTaskEnd(taskID)
		return diffStat, true
	case analyzer.Warn:
		l.logger.Warn(logging.Analyzer, "Analysis: %s", analysisResult.Reason)
		l.attempts.Record(taskID, nextTask, summary, diffStat, "warn: "+analysisDesc)
	default:
		if !result.SignalDetected {
			l.attempts.Record(taskID, nextTask, summary, diffStat, analysisDesc)
		}
	}

	return diffStat, false
}

// logIterationBanner gathers context and delegates to the logger.
func (l *Loop) logIterationBanner(runIteration, maxIter, iteration int, task taskContext) {
	completed, _ := l.cfg.TaskBackend.CountCompleted()
	total, _ := l.cfg.TaskBackend.CountTotal()

	if runIteration > 1 {
		l.logger.DashedSeparator(logging.Yellow)
	}

	l.logger.IterationBanner(logging.BannerOpts{
		RunIteration: runIteration,
		MaxIteration: maxIter,
		Lifetime:     iteration,
		Completed:    completed,
		Total:        total,
		TaskID:       task.id,
		TaskTitle:    task.title,
		TaskChanged:  task.changed,
		Priority:     task.info.Priority,
		Version:      l.cfg.Version,
		WarnPhase:    l.lastAction == analyzer.Warn,
		Description:  getBeadDescription(l.cfg.TaskBackend, task.id),
	})
}
