package loop

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/health"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/prompt"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// initRun restores worktree state on resume and syncs to the correct base.
// Derives the stack head from completed beads, then rebases onto it
// (or the default branch if no stack exists).
func (l *Loop) initRun(ctx context.Context) error {
	if l.git.WorktreeBranch == "" || l.git.WorkDir == l.git.ProjectDir {
		return nil
	}
	l.setStackHead()
	// No stack head = all previous work merged. Reset to default branch
	// so we don't carry stale commits that would conflict on rebase.
	if l.git.PrevBranch == "" {
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
	nextInfo, _ := l.cfg.TaskBackend.GetNextTaskInfo()
	if !isNewTask(l.state, nextInfo.ID, nextInfo.Title) {
		l.git.BranchRenamed = true
	} else {
		l.git.PrepareForNextTask()
	}
	l.logger.Log("git", "Branch: %s", l.git.WorktreeBranch)
	writeRunBranch(l.cfg.Dirs.RalphDir, l.git.WorktreeBranch)
	return nil
}

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
		OnConflict: func(conflictErr *git.UnresolvedConflictError) bool {
			return l.tryFixConflict(ctx, conflictErr, taskID, nextTask, workDir, rawLogPath)
		},
	})
}

// isNewTask returns true when the next task differs from the last one stored
// in state. Prefers task ID comparison (stable across description edits);
// falls back to description when no ID is available.
func isNewTask(st *state.Store, taskID, taskDesc string) bool {
	if taskID != "" {
		lastID, _ := st.Read("last_task_id")
		return lastID != taskID
	}
	lastTask, _ := st.Read("last_task")
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
func buildPRBody(backend tasks.Backend, taskID, summary string) string {
	var sections []string

	if taskID != "" {
		if desc, err := backend.GetDescription(taskID); err == nil && desc != "" {
			sections = append(sections, "## Description\n"+desc)
		}
		if ac, err := backend.GetAcceptance(taskID); err == nil && ac != "" {
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

	// Check metadata for the exact branch name stored when work started.
	branch, _ := l.cfg.TaskBackend.GetMetadata(taskID, "branch")
	if branch == "" || !strings.Contains(branch, taskID) {
		return false
	}

	gh := l.git.GH()
	if gh == nil || !gh.Available() {
		return false
	}
	repoURL := l.git.RemoteURL()

	// Check if a PR exists for this exact branch.
	nwo := git.NWOFromRemote(repoURL)
	prNumber, _ := gh.FindOpenPR(branch, repoURL)
	if prNumber != "" {
		l.logger.Log("git", "Found %s for %s (task %s) — resolving", logging.PRLink(nwo, prNumber), branch, taskID)
		_ = l.cfg.TaskBackend.SetExternalRef(taskID, "gh-"+prNumber)
		return l.resolveByPRState(ctx, taskID, nextTask, prNumber)
	}

	// No PR — check if the remote branch exists with clean work on top of main.
	_ = l.git.FetchBranch(branch)
	if l.git.RemoteBranchHasCommits(branch) {
		if !l.git.RemoteBranchIsOnMain(branch) {
			l.logger.Warn("git", "Remote branch %s diverged from main — abandoning stale work", branch)
			_ = l.git.DeleteRemoteBranchByName(branch)
			return false
		}
		l.logger.Log("git", "Remote branch %s has clean work but no PR — creating PR", branch)
		l.git.CheckoutRemoteBranch(branch)
		prNum, err := l.pushAndCreatePR(ctx, taskID, nextTask, "")
		if err == nil && prNum != "" {
			nwo := git.NWOFromRemote(l.git.RemoteURL())
			l.logger.Log("git", "Created %s for %s (task %s)", logging.PRLink(nwo, prNum), branch, taskID)
			_ = l.cfg.TaskBackend.SetExternalRef(taskID, "gh-"+prNum)
			return l.resolveByPRState(ctx, taskID, nextTask, prNum)
		}
	}

	return false
}

// resolveByPRState inspects the PR's state and takes the appropriate action.
// Delegates merge+close to finalizePR so resume and post-signal share one path.
func (l *Loop) resolveByPRState(ctx context.Context, taskID, nextTask, prNumber string) bool {
	gh := l.git.GH()
	if gh == nil || !gh.Available() {
		l.logger.Warn("git", "gh CLI not available — cannot check PR state")
		return false
	}

	nwo := git.NWOFromRemote(l.git.RemoteURL())
	pr := logging.PRLink(nwo, prNumber)

	prState, err := gh.GetPRState(l.git.WorkDir, prNumber)
	if err != nil {
		l.logger.Warn("git", "Failed to get %s state: %v", pr, err)
		return false
	}

	switch strings.ToUpper(prState) {
	case "MERGED":
		l.logger.Success("git", "%s already merged — closing bead and moving on", pr)
		l.attempts.Clear(taskID, nextTask)
		recordCompletedTask(l.cfg.Dirs.RalphDir, taskID, nextTask)
		l.finalizePR(finalizePRParams{
			ctx:        ctx,
			taskID:     taskID,
			nextTask:   nextTask,
			prNumber:   prNumber,
			prState:    "MERGED",
			workDir:    l.git.WorkDir,
			rawLogPath: filepath.Join(l.cfg.Dirs.RalphDir, "raw.log"),
		})
		if l.cfg.Notify {
			notify.TaskCompleted(taskID, nextTask, "")
		}
		notify.TaskMerged(taskID, nextTask)
		return true

	case "OPEN":
		if ok, reason := prChainIsHealthy(gh, l.git.WorkDir, l.git, prNumber); !ok {
			l.logger.Warn("git", "%s chain unhealthy: %s — re-running agent", pr, reason)
			return false
		}
		l.finalizePR(finalizePRParams{
			ctx:        ctx,
			taskID:     taskID,
			nextTask:   nextTask,
			prNumber:   prNumber,
			prState:    "OPEN",
			workDir:    l.git.WorkDir,
			rawLogPath: filepath.Join(l.cfg.Dirs.RalphDir, "raw.log"),
		})
		if l.cfg.Notify {
			notify.TaskCompleted(taskID, nextTask, "")
		}
		return true

	default:
		l.logger.Warn("git", "%s is %s (not merged) — re-running agent", pr, prState)
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
	const waitPollInterval = 5 * time.Second
	l.logger.Log("beads", "Waiting for new tasks (polling every %s)...", waitPollInterval)
	l.state.Write("status", "waiting")
	updateStreamTask(l.cfg.Dirs.RalphDir, "", "Waiting for tasks...", nil)
	touchFile(filepath.Join(l.cfg.Dirs.RalphDir, ".plan-refresh"))

	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			l.state.Write("status", "stopped")
			return false
		case <-ticker.C:
			if checkStopFile(l.cfg.Dirs.RalphDir) {
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

func checkStopFile(ralphDir string) bool {
	stopFile := filepath.Join(ralphDir, "stop")
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

	archSpec := readArchSpec(l.git.WorkDir)

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

func readArchSpec(workDir string) string {
	path := filepath.Join(workDir, "docs", "specs", "architecture.md")
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

// logIterationBanner prints the health dashboard, separator, task banner,
// and iteration phase line between iterations.
func (l *Loop) logIterationBanner(runIteration, maxIter, iteration int, taskID, nextTask string, taskChanged bool, taskInfo tasks.TaskInfo) {
	completed, _ := l.cfg.TaskBackend.CountCompleted()
	total, _ := l.cfg.TaskBackend.CountTotal()

	if runIteration > 1 {
		health.Log(l.logger, health.Collect(l.cfg.Dirs.RalphDir, l.git.WorkDir))
		l.logger.DashedSeparator(logging.Yellow)
	}

	if taskID != "" && taskChanged {
		l.logger.TaskBanner(taskID, nextTask, taskInfo.Priority)
	}

	phaseColor := logging.Green
	if l.lastAction == analyzer.Warn {
		phaseColor = logging.Yellow
	}
	versionTag := ""
	if l.cfg.Version != "" {
		versionTag = fmt.Sprintf(" | Ralph v%s", l.cfg.Version)
	}
	l.logger.PhaseColor(phaseColor, "--- Run iteration %d/%d | %d lifetime [%d/%d done]%s ---",
		runIteration, maxIter, iteration, completed, total, versionTag)
	if desc := getBeadDescription(l.cfg.TaskBackend, taskID); desc != "" {
		l.logger.Log("beads", "  %s", desc)
	}
}

func writeRunBranch(ralphDir, branch string) {
	if branch == "" {
		branch = "ralph"
	}
	os.WriteFile(filepath.Join(ralphDir, ".run-branch"), []byte(branch), 0o644)
}

func updateStreamTask(ralphDir, taskID, nextTask string, priority *int) {
	streamTaskFile := filepath.Join(ralphDir, ".stream-task")
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

// setStackHead walks completed beads backwards to find the most recent
// branch with unmerged work. Uses git ancestry checks (no GitHub API):
// if a branch is an ancestor of origin/main, its work has landed → skip.
// Otherwise it has unmerged work → use it as stack head.
func (l *Loop) setStackHead() {
	l.git.SetPrevBranch("")

	tasks, err := l.state.GetCompletedTasks()
	if err != nil || len(tasks) == 0 {
		return
	}

	for i := len(tasks) - 1; i >= 0; i-- {
		id := tasks[i]
		if id == "" {
			continue
		}
		branch, _ := l.cfg.TaskBackend.GetMetadata(id, "branch")
		if branch == "" {
			continue
		}
		if err := l.git.FetchBranch(branch); err != nil {
			continue
		}
		if !l.git.RemoteBranchHasCommits(branch) {
			continue
		}
		if !l.git.BranchIsAheadOfMain(branch) {
			l.logger.Log("git", "Branch %s not ahead of main — skipping", branch)
			continue
		}
		l.git.SetPrevBranch(branch)
		l.logger.Log("git", "Stack head: %s (from %s)", branch, id)
		return
	}
	l.logger.Log("git", "No stacked parents — starting from %s", l.git.DetectDefaultBranch())
}

// persistCompletedTask writes a completed task ID to state.json so
// ralph-task can verify tasks weren't falsely closed.
func persistCompletedTask(st *state.Store, logger *logging.Logger, taskID string) {
	if taskID == "" {
		return
	}
	if err := st.AddCompletedTask(taskID); err != nil {
		logger.Warn("state", "AddCompletedTask: %v", err)
	}
}

// recordCompletedTask appends a completed task label to .completed-tasks
// so the plan pane can show which tasks were finished in this run.
func recordCompletedTask(ralphDir, taskID, taskTitle string) {
	label := taskID
	if label == "" {
		label = taskTitle
	}
	if label == "" {
		return
	}
	path := filepath.Join(ralphDir, ".completed-tasks")
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

// skipTask defers the task in the bd backend so it is excluded from
// future selection by bd ready.
func skipTask(backend tasks.Backend, logger *logging.Logger, id, reason string) {
	if id == "" {
		return
	}
	logger.Warn("beads", "Skipping task %s: %s", id, reason)
	if err := backend.SkipTask(id, reason); err != nil {
		logger.Warn("beads", "Failed to defer task %s in backend: %v", id, err)
	}
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
func waitForInternet(ctx context.Context, logger *logging.Logger) bool {
	if isOnline() {
		return true
	}

	start := time.Now()
	logger.Warn("", "Internet unreachable — waiting for connectivity...")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if isOnline() {
				elapsed := time.Since(start).Truncate(time.Second)
				logger.Success("", "Internet restored after %s", elapsed)
				return true
			}
			elapsed := time.Since(start).Truncate(time.Second)
			logger.Log("", "Internet still unreachable (%s elapsed)", elapsed)
		}
	}
}

// branchChecker provides the narrow git operations needed by prChainIsHealthy.
type branchChecker interface {
	FetchBranch(branch string) error
	BranchIsAncestorOfMain(branch string) bool
	RemoteBranchHasCommits(branch string) bool
}

func prChainIsHealthy(gh git.GitHub, workDir string, branches branchChecker, prNumber string) (bool, string) {
	if gh == nil || !gh.Available() {
		return false, "gh CLI not available"
	}

	headBranch, _ := gh.GetPRHead(workDir, prNumber)
	if headBranch == "" {
		return false, fmt.Sprintf("PR #%s has no head branch", prNumber)
	}
	_ = branches.FetchBranch(headBranch)
	if !branches.RemoteBranchHasCommits(headBranch) {
		return false, fmt.Sprintf("branch %s missing from remote", headBranch)
	}
	if branches.BranchIsAncestorOfMain(headBranch) {
		return false, fmt.Sprintf("branch %s already merged into main", headBranch)
	}
	return true, ""
}

func getPRBase(gh git.GitHub, workDir, prNumber string) string {
	if gh == nil || !gh.Available() {
		return ""
	}
	base, _ := gh.GetPRBase(workDir, prNumber)
	return base
}

// runPostTask executes the --post-task script if configured. Runs in the
// project directory with RALPH_TASK_ID, RALPH_PR_NUMBER, and RALPH_MERGED
// env vars. Non-zero exit warns and continues.
func (l *Loop) runPostTask(ctx context.Context, taskID, prNumber string, merged bool) {
	if l.cfg.PostTask == "" {
		return
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", l.cfg.PostTask)
	cmd.Dir = l.cfg.Dirs.ProjectDir
	cmd.Env = append(os.Environ(),
		"RALPH_TASK_ID="+taskID,
		"RALPH_PR_NUMBER="+prNumber,
		"RALPH_MERGED="+fmt.Sprintf("%t", merged),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	l.logger.Log("post-task", "Running %s (task=%s pr=%s merged=%t)", l.cfg.PostTask, taskID, prNumber, merged)
	if err := cmd.Run(); err != nil {
		l.logger.Warn("post-task", "Script exited with error: %v", err)
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
