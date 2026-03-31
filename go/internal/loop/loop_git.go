package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// handleRebase syncs the worktree to the latest default branch via
// EnsureUpToDate, which handles all conflict resolution internally.
func (l *Loop) handleRebase(ctx context.Context) error {
	return l.git.EnsureUpToDate(ctx)
}

// prepareBranch consolidates all branch setup for a task: find stack head,
// reset to correct base if no stack, rebase onto latest, and checkout or
// rename the branch for this task. Called once per task change by Run().
// Also called by initRun for the first iteration on resume.
//
// isFirstIteration controls whether PrepareForNextTask is called (skipped
// on first iteration since initRun handles the resume-vs-new-task logic).
func (l *Loop) prepareBranch(ctx context.Context, taskID, nextTask string) error {
	l.git.PrepareForNextTask()

	// Rebase onto latest base when running in a worktree.
	if l.git.GetWorktreeBranch() != "" && l.git.GetWorkDir() != l.git.GetProjectDir() {
		l.setStackHead()
		if l.git.GetPrevBranch() == "" {
			l.git.ResetToDefaultBranch()
		}
		if err := l.handleRebase(ctx); err != nil {
			return err
		}
	} else {
		l.setStackHead()
	}

	l.checkoutExistingBranch(taskID, nextTask)
	writeRunBranch(l.cfg.Dirs.RalphDir, l.git.GetWorktreeBranch())
	return nil
}

// mergeWithRetry delegates to git.Manager.MergeWithRetry, passing a CI fix
// callback that spawns a fix agent. Test overrides via mergeFunc bypass the
// git module entirely for loop-level tests that only care about the outcome.
func (l *Loop) mergeWithRetry(ctx context.Context, taskID, nextTask, workDir, rawLogPath string) (bool, error) {
	if l.mergeFunc != nil {
		return l.mergeFunc(ctx)
	}
	return l.git.MergeWithRetry(ctx, git.MergeRetryOpts{
		OnCIFailure: func(ciErr *git.CIFailureError) git.CIFixResult {
			return l.tryFixCI(ctx, ciErr, taskID, nextTask, workDir, rawLogPath)
		},
		OnConflict: func(conflictErr *git.UnresolvedConflictError) bool {
			return l.tryFixConflict(ctx, conflictErr, taskID, nextTask, workDir, rawLogPath)
		},
	})
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
		_ = l.cfg.TaskBackend.SetExternalRef(taskID, prURL(repoURL, prNumber))
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
			_ = l.cfg.TaskBackend.SetExternalRef(taskID, prURL(l.git.RemoteURL(), prNum))
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

	prState, err := gh.GetPRState(l.git.GetWorkDir(), prNumber)
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
			workDir:    l.git.GetWorkDir(),
			rawLogPath: filepath.Join(l.cfg.Dirs.RalphDir, "raw.log"),
		})
		if l.cfg.Notify {
			notify.TaskCompleted(taskID, nextTask, "")
		}
		notify.TaskMerged(taskID, nextTask)
		return true

	case "OPEN":
		if ok, reason := prChainIsHealthy(gh, l.git.GetWorkDir(), l.git, prNumber); !ok {
			l.logger.Warn("git", "%s chain unhealthy: %s — re-running agent", pr, reason)
			return false
		}
		l.finalizePR(finalizePRParams{
			ctx:        ctx,
			taskID:     taskID,
			nextTask:   nextTask,
			prNumber:   prNumber,
			prState:    "OPEN",
			workDir:    l.git.GetWorkDir(),
			rawLogPath: filepath.Join(l.cfg.Dirs.RalphDir, "raw.log"),
		})
		if l.cfg.Notify {
			notify.TaskCompleted(taskID, nextTask, "")
		}
		return true

	default:
		l.logger.Warn("git", "%s is %s (not merged) — re-running agent", pr, prState)
		// Clear stale refs so the closed PR isn't re-discovered on the
		// next iteration and the agent pushes to a fresh branch.
		if taskID != "" {
			_ = l.cfg.TaskBackend.SetExternalRef(taskID, "")
			_ = l.cfg.TaskBackend.SetMetadata(taskID, "branch", "")
		}
		l.git.PrepareForNextTask()
		l.checkoutExistingBranch(taskID, nextTask)
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

// setStackHead finds the most recent branch that's cleanly ahead of main
// and sets it as the stack base for the next task. When gh is available,
// fetches only branches with open PRs (one API call). Otherwise falls
// back to checking all completed task branches.
func (l *Loop) setStackHead() {
	l.git.SetPrevBranch("")

	tasks, err := l.state.GetCompletedTasks()
	if err != nil || len(tasks) == 0 {
		return
	}

	// Only consider branches with open PRs. One gh API call replaces
	// fetching every completed task's branch individually.
	gh := l.git.GH()
	repoURL := l.git.RemoteURL()
	if repoURL == "" || gh == nil || !gh.Available() {
		return
	}
	openBranches, err := gh.ListOpenPRBranches(repoURL)
	if err != nil || len(openBranches) == 0 {
		return
	}
	openSet := make(map[string]bool, len(openBranches))
	for _, b := range openBranches {
		openSet[b] = true
	}

	for i := len(tasks) - 1; i >= 0; i-- {
		id := tasks[i]
		if id == "" {
			continue
		}
		branch, _ := l.cfg.TaskBackend.GetMetadata(id, "branch")
		if branch == "" || !openSet[branch] {
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

// prURL builds the canonical PR URL from the remote URL and PR number.
// Always returns a full URL; never returns a "gh-" prefixed string.
func prURL(remoteURL, prNumber string) string {
	nwo := git.NWOFromRemote(remoteURL)
	if nwo == "" || prNumber == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%s", nwo, prNumber)
}

// parsePRNumber extracts a PR number from a URL
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
