package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/quality"
	"github.com/brokenalarms/ralph/internal/prompt"
)

// handleRebase attempts to rebase onto the default branch, and if a conflict
// is detected, consults the OnRebaseConflict handler for recovery.
func (l *Loop) handleRebase(ctx context.Context) error {
	err := l.git.RebaseOntoDefaultBranch(ctx)
	if err == nil {
		return nil
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	var conflictErr *git.RebaseConflictError
	if !errors.As(err, &conflictErr) {
		return err
	}

	if l.cfg.OnRebaseConflict == nil {
		return err
	}

	switch l.cfg.OnRebaseConflict(err) {
	case git.RebaseFreshWorktree:
		l.logger.Log("git", "Recreating worktree from main...")
		if recreateErr := l.git.RecreateFromMain(ctx); recreateErr != nil {
			return fmt.Errorf("worktree recreation failed: %w", recreateErr)
		}
		return nil
	case git.RebaseManualResolve:
		l.logger.Warn("git", "Pausing for manual conflict resolution. Re-run ralph to resume.")
		return fmt.Errorf("paused for manual resolution: %w", err)
	default:
		return err
	}
}

// mergeWithRetry delegates to git.Manager.MergeWithRetry, passing a CI fix
// callback that spawns a fix agent. Test overrides via mergeFunc bypass the
// git module entirely for loop-level tests that only care about the outcome.
func (l *Loop) mergeWithRetry(ctx context.Context, taskID, nextTask, workDir, rawLogPath string) (bool, error) {
	if l.mergeFunc != nil {
		return l.mergeFunc(ctx)
	}
	return l.git.MergeWithRetry(ctx, git.MergeRetryOpts{
		OnCIFailure: func(ciErr *git.CIFailureError) bool {
			return l.tryFixCI(ctx, ciErr, taskID, nextTask, workDir, rawLogPath)
		},
	})
}

// isNewTask returns true when the next task differs from the last one stored
// in state. Prefers task ID comparison (stable across description edits);
// falls back to description when no ID is available.
func (l *Loop) isNewTask(taskID, taskDesc string) bool {
	if taskID != "" {
		lastID, _ := l.state.Read("last_task_id")
		return lastID != taskID
	}
	lastTask, _ := l.state.Read("last_task")
	return lastTask != taskDesc
}

func (l *Loop) pushAndCreatePR(ctx context.Context, taskID, taskDesc string) error {
	if l.pushPRFunc != nil {
		return l.pushPRFunc(ctx, taskID, taskDesc)
	}
	return l.git.PushAndCreatePR(ctx, taskID, taskDesc)
}

func (l *Loop) flushUnpushedWork(ctx context.Context) {
	taskID, _ := l.state.Read("last_task_id")
	taskDesc, _ := l.state.Read("last_task")
	if l.pushPRFunc != nil || l.mergeFunc != nil {
		// Test override path: use the existing test funcs.
		if err := l.pushAndCreatePR(ctx, taskID, taskDesc); err != nil {
			l.logger.Warn("git", "Flush push/PR: %v", err)
			return
		}
		if l.cfg.AutoMerge && !l.lastTaskMerged {
			if l.mergeFunc != nil {
				merged, err := l.mergeFunc(ctx)
				if err != nil {
					l.logger.Warn("git", "Flush merge: %v", err)
				}
				if merged {
					notify.TaskMerged(taskID, taskDesc)
					if err := l.git.PostMergeReset(); err != nil {
						l.logger.Warn("git", "Flush post-merge reset: %v", err)
					}
				}
			}
		}
		return
	}
	merged, err := l.git.FlushUnpushedWork(ctx, taskID, taskDesc, l.cfg.AutoMerge && !l.lastTaskMerged)
	if err != nil {
		l.logger.Warn("git", "Flush: %v", err)
	}
	if merged {
		notify.TaskMerged(taskID, taskDesc)
	}
}

func (l *Loop) waitForTasks(ctx context.Context) bool {
	l.logger.Log("beads", "Waiting for new tasks (polling every %s)...", l.cfg.WaitInterval)
	l.state.Write("status", "waiting")
	l.updateStreamTask("", "Waiting for tasks...", nil)
	touchFile(filepath.Join(l.cfg.Dirs.RalphDir, ".plan-refresh"))

	ticker := time.NewTicker(l.cfg.WaitInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			l.state.Write("status", "stopped")
			return false
		case <-ticker.C:
			if l.checkStopFile() {
				l.logger.Warn("", "Stop file detected - halting")
				l.state.Write("status", "stopped")
				return false
			}
			hasRemaining, err := l.cfg.TaskBackend.HasRemaining()
			if err != nil {
				l.logger.Warn("beads", "Task check error during wait: %v", err)
				continue
			}
			if hasRemaining {
				l.logger.Success("beads", "New tasks detected!")
				touchFile(filepath.Join(l.cfg.Dirs.RalphDir, ".plan-refresh"))
				return true
			}
		}
	}
}

func (l *Loop) checkStopFile() bool {
	stopFile := filepath.Join(l.cfg.Dirs.RalphDir, "stop")
	if _, err := os.Stat(stopFile); err == nil {
		os.Remove(stopFile)
		return true
	}
	return false
}

const defaultLookbackCommits = 5

func (l *Loop) maybeRefactor() error {
	if l.cfg.NoRefactor {
		return nil
	}

	threshold := l.cfg.RefactorThreshold
	if threshold <= 0 {
		return nil
	}

	lookback := defaultLookbackCommits
	recentFiles := l.git.RecentChangedFiles(lookback)
	if recentFiles == "" {
		return nil
	}

	files := strings.Split(strings.TrimSpace(recentFiles), "\n")
	findingsFile := filepath.Join(l.cfg.Dirs.RalphDir, ".quality-findings")

	opts := &quality.Options{DisabledChecks: make(map[string]bool)}
	for _, name := range l.cfg.DisabledChecks {
		opts.DisabledChecks[name] = true
	}

	score, err := quality.Assess(l.git.WorkDir, findingsFile, opts, files...)
	if err != nil {
		return fmt.Errorf("quality assessment: %w", err)
	}

	l.state.Write("quality_score", strconv.Itoa(score))

	if score < threshold {
		l.logger.Log("quality", "Score %d/%d — below threshold, skipping refactor", score, threshold)
		return nil
	}

	l.logger.Phase("--- Adaptive refactor (quality score %d >= threshold %d) ---", score, threshold)

	qualityFindings := quality.FormatFindings(findingsFile)

	refactorPrompt, err := prompt.BuildRefactorPrompt(prompt.Vars{
		PromptsDir:       l.cfg.Dirs.PromptsDir,
		WorkDir:          l.git.WorkDir,
		SignalToken:      l.signals.Complete,
		CurrentTaskToken: l.signals.CurrentTask,
		AllCompleteToken: l.signals.AllComplete,
	}, recentFiles, qualityFindings)
	if err != nil {
		return fmt.Errorf("building refactor prompt: %w", err)
	}

	if !l.limiter.Allowed() {
		l.logger.Warn("llm", "Rate limit hit before refactor — waiting for reset")
		if err := l.limiter.WaitForReset(context.Background(), func(secs int) {
			l.logger.Log("llm", "Rate limit: %ds until reset", secs)
		}); err != nil {
			return err
		}
	}

	rawLogPath := filepath.Join(l.cfg.Dirs.RalphDir, "raw.log")
	_, err = l.runner.Run(claude.RunConfig{
		WorkDir:      l.git.WorkDir,
		RalphDir:     l.cfg.Dirs.RalphDir,
		Prompt:       refactorPrompt,
		RawLog:       rawLogPath,
		LogFile:      filepath.Join(l.cfg.Dirs.RalphDir, "loop.log"),
		Quiet:        l.cfg.Quiet,
		Signals:      l.signals,
		PollInterval: 2 * time.Second,
	})
	l.limiter.Increment()

	l.logger.Success("", "Refactor iteration complete")

	return err
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

func (l *Loop) writeRunBranch() {
	branch := l.git.WorktreeBranch
	if branch == "" {
		branch = "ralph"
	}
	os.WriteFile(filepath.Join(l.cfg.Dirs.RalphDir, ".run-branch"), []byte(branch), 0o644)
}

func (l *Loop) updateStreamTask(taskID, nextTask string, priority *int) {
	streamTaskFile := filepath.Join(l.cfg.Dirs.RalphDir, ".stream-task")
	content := nextTask
	if taskID != "" {
		tag := logging.PriorityTag(priority)
		if tag != "" {
			content = taskID + ": " + tag + " " + nextTask
		} else {
			content = taskID + ": " + nextTask
		}
	}
	os.WriteFile(streamTaskFile, []byte(content), 0o644)
}

// recordCompletedTask appends a completed task label to .completed-tasks
// so the plan pane can show which tasks were finished in this run.
func (l *Loop) recordCompletedTask(taskID, taskTitle string) {
	label := taskID
	if label == "" {
		label = taskTitle
	}
	if label == "" {
		return
	}
	path := filepath.Join(l.cfg.Dirs.RalphDir, ".completed-tasks")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(label + "\n")
}

func touchFile(path string) {
	f, err := os.Create(path)
	if err == nil {
		f.Close()
	}
}

func fileLineCount(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	count := 0
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}
	return count
}

func readLogFrom(path string, startLine int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := 0
	offset := 0
	for i, b := range data {
		if b == '\n' {
			lines++
			if lines >= startLine {
				offset = i + 1
				break
			}
		}
	}
	if offset >= len(data) {
		return ""
	}
	return string(data[offset:])
}
