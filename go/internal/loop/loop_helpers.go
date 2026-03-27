package loop

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/prompt"
)

// handleRebase syncs the worktree to the latest default branch via
// EnsureUpToDate, which handles all conflict resolution internally.
func (l *Loop) handleRebase(ctx context.Context) error {
	return l.git.EnsureUpToDate(ctx)
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

func (l *Loop) pushAndCreatePR(ctx context.Context, taskID, taskDesc, body string) (string, error) {
	if l.pushPRFunc != nil {
		return l.pushPRFunc(ctx, taskID, taskDesc, body)
	}
	return l.git.PushAndCreatePR(ctx, taskID, taskDesc, body)
}

// buildPRBody assembles a PR description from bead context and agent summary.
// Uses whatever context is available — bead description, acceptance criteria,
// agent summary — and composes them into a coherent body.
func (l *Loop) buildPRBody(taskID, summary string) string {
	var sections []string

	if taskID != "" {
		if desc, err := l.cfg.TaskBackend.GetDescription(taskID); err == nil && desc != "" {
			sections = append(sections, "## Description\n"+desc)
		}
		if ac, err := l.cfg.TaskBackend.GetAcceptance(taskID); err == nil && ac != "" {
			sections = append(sections, "## Acceptance Criteria\n"+ac)
		}
	}

	if summary != "" {
		sections = append(sections, "## Summary\n"+summary)
	}

	if len(sections) == 0 {
		return ""
	}
	return strings.Join(sections, "\n\n")
}

// resumeViaPR checks the bead's metadata and external-ref for existing work
// and resolves accordingly. Returns true if the task was fully handled (merged
// or skipped) and the loop should continue to the next task; false if the
// agent should run.
func (l *Loop) resumeViaPR(ctx context.Context, taskID, nextTask string) bool {
	if taskID == "" {
		return false
	}

	// Check bead's external-ref for an existing PR.
	ref, _ := l.cfg.TaskBackend.GetExternalRef(taskID)
	if prNumber := parsePRNumber(ref); prNumber != "" {
		return l.resolveByPRState(ctx, taskID, nextTask, prNumber)
	}

	// No PR — check metadata for a branch that was pushed but never got a PR.
	gh := l.git.GH()
	if gh == nil || !gh.Available() {
		return false
	}
	repoURL := l.git.RemoteURL()
	if repoURL == "" {
		return false
	}

	// Check metadata for a stored branch name first, fall back to remote search.
	branch, _ := l.cfg.TaskBackend.GetMetadata(taskID, "branch")
	if branch == "" {
		branch = l.git.FindRemoteBranchForTask(taskID)
	}
	if branch == "" {
		return false
	}

	l.logger.Log("git", "Found remote branch %s for task %s", branch, taskID)

	// Check if a PR exists for this branch.
	prNumber, _ := gh.FindOpenPR(branch, repoURL)
	if prNumber != "" {
		l.logger.Log("git", "Found PR #%s for %s — resolving", prNumber, branch)
		_ = l.cfg.TaskBackend.SetExternalRef(taskID, "gh-"+prNumber)
		return l.resolveByPRState(ctx, taskID, nextTask, prNumber)
	}

	// No PR — create one from the existing remote branch.
	l.logger.Log("git", "Remote branch %s has work but no PR — creating PR", branch)
	l.git.CheckoutRemoteBranch(branch)
	prNum, err := l.pushAndCreatePR(ctx, taskID, nextTask, "")
	if err == nil && prNum != "" {
		_ = l.cfg.TaskBackend.SetExternalRef(taskID, "gh-"+prNum)
		return l.resolveByPRState(ctx, taskID, nextTask, prNum)
	}

	// PR creation failed — the remote branch exists but has no diff.
	// This means the branch was reset to main during a previous recovery.
	// Delete the stale remote branch and let the agent re-run.
	l.logger.Log("git", "Remote branch %s has no usable work — deleting stale branch", branch)
	_ = l.git.DeleteRemoteBranchByName(branch)
	return false
}

// resolveByPRState inspects the PR's state and takes the appropriate action.
func (l *Loop) resolveByPRState(ctx context.Context, taskID, nextTask, prNumber string) bool {
	gh := l.git.GH()
	if gh == nil || !gh.Available() {
		l.logger.Warn("git", "gh CLI not available — cannot check PR state")
		return false
	}

	prState, err := gh.GetPRState(l.git.WorkDir, prNumber)
	if err != nil {
		l.logger.Warn("git", "Failed to get PR #%s state: %v", prNumber, err)
		return false
	}

	switch strings.ToUpper(prState) {
	case "MERGED":
		l.logger.Success("git", "PR #%s already merged — closing bead and moving on", prNumber)
		l.attempts.Clear(taskID, nextTask)
		l.attempts.ClearMergeFailures(taskID)
		l.recordCompletedTask(taskID, nextTask)
		if taskID != "" {
			_ = l.cfg.TaskBackend.SetState(taskID, "phase", "verified", "ralph: PR merged")
			if err := l.cfg.TaskBackend.CloseTask(taskID, fmt.Sprintf("Fixed in PR #%s", prNumber)); err != nil {
				l.logger.Warn("beads", "CloseTask failed: %v", err)
			} else {
				l.logger.Log("beads", "Closed task %s (PR #%s merged)", taskID, prNumber)
			}
		}
		l.git.PostMergeUpdateMain()
		return true

	case "OPEN":
		l.logger.Log("git", "PR #%s still open — proceeding to merge", prNumber)
		merged := false
		if l.cfg.AutoMerge {
			var mergeErr error
			merged, mergeErr = l.mergeWithRetry(ctx, taskID, nextTask, l.git.WorkDir, filepath.Join(l.cfg.Dirs.RalphDir, "raw.log"))
			if mergeErr != nil {
				l.logger.Warn("git", "Auto-merge: %v", mergeErr)
			}
		}
		if taskID != "" {
			if merged || !l.cfg.AutoMerge {
				l.attempts.ClearMergeFailures(taskID)
				if err := l.cfg.TaskBackend.CloseTask(taskID, fmt.Sprintf("Fixed in PR #%s", prNumber)); err != nil {
					l.logger.Warn("beads", "CloseTask failed: %v", err)
				} else {
					l.logger.Log("beads", "Closed task %s (PR #%s merged)", taskID, prNumber)
				}
			} else {
				l.logger.Warn("git", "Merge failed for PR #%s — skipping task", prNumber)
				l.skipTask(taskID, "merge_failed_open_pr")
			}
		}
		if merged {
			l.git.PostMergeUpdateMain()
		}
		return true

	default:
		l.logger.Warn("git", "PR #%s is %s (not merged) — re-running agent", prNumber, prState)
		return false
	}
}

func (l *Loop) flushUnpushedWork(ctx context.Context) {
	taskID, _ := l.state.Read("last_task_id")
	taskDesc, _ := l.state.Read("last_task")
	if l.pushPRFunc != nil || l.mergeFunc != nil {
		if _, err := l.pushAndCreatePR(ctx, taskID, taskDesc, ""); err != nil {
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
					l.git.PostMergeUpdateMain()
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

const refactorCheckInterval = 5
const refactorLookbackCommits = 10

func (l *Loop) maybeRefactor() error {
	if !l.cfg.Refactor {
		return nil
	}

	completedCount := len(l.sessionTasks)
	if completedCount == 0 || completedCount%refactorCheckInterval != 0 {
		return nil
	}

	recentFiles := l.git.RecentChangedFiles(refactorLookbackCommits)
	if recentFiles == "" {
		return nil
	}

	archSpec := l.readArchSpec()

	shouldRefactor, err := l.llmShouldRefactor(context.Background(), archSpec, recentFiles)
	if err != nil {
		return fmt.Errorf("refactor check: %w", err)
	}

	if !shouldRefactor {
		l.logger.Log("refactor", "LLM says no refactoring needed — skipping")
		return nil
	}

	l.logger.Phase("--- Adaptive refactor (LLM recommended) ---")

	refactorPrompt, err := prompt.BuildRefactorPrompt(prompt.Vars{
		PromptsDir:       l.cfg.Dirs.PromptsDir,
		WorkDir:          l.git.WorkDir,
		SignalToken:      l.signals.Complete,
		CurrentTaskToken: l.signals.CurrentTask,
		AllCompleteToken: l.signals.AllComplete,
	}, recentFiles)
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
		Verbose:      l.cfg.Verbose,
		Signals:      l.signals,
		PollInterval: 2 * time.Second,
	})
	l.limiter.Increment()

	l.logger.Success("", "Refactor iteration complete")

	return err
}

func (l *Loop) readArchSpec() string {
	path := filepath.Join(l.git.WorkDir, "docs", "specs", "architecture.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := string(data)
	if len(content) > 4000 {
		content = content[:4000]
	}
	return content
}

func (l *Loop) llmShouldRefactor(ctx context.Context, archSpec, recentFiles string) (bool, error) {
	queryFn := l.refactorQueryFunc
	if queryFn == nil && l.agentRunner != nil {
		queryFn = l.agentRunner.Query
	}
	if queryFn == nil {
		return false, fmt.Errorf("no query function available")
	}

	prompt := "You are deciding whether a codebase needs refactoring.\n\n"
	if archSpec != "" {
		prompt += "## Architecture spec\n" + archSpec + "\n\n"
	}
	prompt += "## Recently changed files\n" + recentFiles + "\n\n"
	prompt += "Based on the recently changed files and the architecture spec, does this codebase need refactoring right now?\n"
	prompt += "Consider: code duplication, unclear naming, files growing too large, architectural drift from the spec, dead code.\n"
	prompt += "Reply with exactly YES or NO on the first line, followed by a brief explanation."

	response, err := queryFn(ctx, l.git.WorkDir, prompt, "")
	if err != nil {
		return false, err
	}

	firstLine := strings.SplitN(strings.TrimSpace(response), "\n", 2)[0]
	return strings.EqualFold(strings.TrimSpace(firstLine), "YES"), nil
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

// skipTask adds a task to the skip list in state.json and updates the
// backend's filter so the task is excluded from future selection.
func (l *Loop) skipTask(id, reason string) {
	if id == "" {
		return
	}
	l.logger.Warn("beads", "Skipping task %s: %s", id, reason)
	if err := l.state.AddSkippedTask(id); err != nil {
		l.logger.Warn("beads", "Failed to persist skip for %s: %v", id, err)
	}
	skipped, _ := l.state.GetSkippedTasks()
	l.cfg.TaskBackend.SetSkippedIDs(skipped)
}

// isOnline checks internet connectivity with a quick DNS lookup.
func isOnline() bool {
	conn, err := net.DialTimeout("tcp", "api.anthropic.com:443", 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// waitForInternet blocks until internet connectivity is restored.
// Shows a single updating line in the terminal log, writes one summary
// line to the log file when restored. Returns false if context is cancelled.
func (l *Loop) waitForInternet(ctx context.Context) bool {
	if isOnline() {
		return true
	}

	start := time.Now()
	l.logger.Warn("", "Internet unreachable — waiting for connectivity...")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if isOnline() {
				elapsed := time.Since(start).Truncate(time.Second)
				l.logger.Success("", "Internet restored after %s", elapsed)
				return true
			}
			elapsed := time.Since(start).Truncate(time.Second)
			l.logger.Log("", "Internet still unreachable (%s elapsed)", elapsed)
		}
	}
}

// parsePRNumber extracts a PR number from either a URL
// (https://github.com/owner/repo/pull/123) or a legacy gh-123 ref.
func parsePRNumber(ref string) string {
	if strings.HasPrefix(ref, "gh-") {
		return strings.TrimPrefix(ref, "gh-")
	}
	if strings.Contains(ref, "/pull/") {
		parts := strings.Split(ref, "/pull/")
		if len(parts) == 2 && parts[1] != "" {
			return parts[1]
		}
	}
	return ""
}
