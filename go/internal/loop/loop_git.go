package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// branchParams bundles the dependencies needed by prepareBranch and its helpers.
type branchParams struct {
	git      git.GitOps
	backend  tasks.Backend
	state    *state.Store
	logger   *logging.Logger
	ralphDir string
}

// prepareBranch is the package-level implementation of branch setup for a task.
// Called by the Loop method wrapper which passes its fields as a branchParams.
func prepareBranch(ctx context.Context, p branchParams, taskID, title string) error {
	p.git.PrepareForNextTask()

	if p.git.GetWorktreeBranch() != "" && p.git.GetWorkDir() != p.git.GetProjectDir() {
		setStackHead(p.git, p.backend, p.state, p.logger)
		if p.git.GetPrevBranch() == "" {
			p.git.ResetToDefaultBranch()
		}
		if err := p.git.EnsureUpToDate(ctx); err != nil {
			return err
		}
	} else {
		setStackHead(p.git, p.backend, p.state, p.logger)
	}

	checkoutExistingBranch(p.git, p.backend, p.logger, taskID, title)
	writeRunBranch(p.ralphDir, p.git.GetWorktreeBranch())
	return nil
}

// setStackHead finds the most recent branch that's cleanly ahead of main
// and sets it as the stack base for the next task. When gh is available,
// fetches only branches with open PRs (one API call). Otherwise falls
// back to checking all completed task branches.
func setStackHead(g git.GitOps, backend tasks.Backend, st *state.Store, logger *logging.Logger) {
	g.SetPrevBranch("")

	completedTasks, err := st.GetCompletedTasks()
	if err != nil || len(completedTasks) == 0 {
		return
	}

	openBranches, err := g.ListOpenPRBranches()
	if err != nil || len(openBranches) == 0 {
		return
	}
	openSet := make(map[string]bool, len(openBranches))
	for _, b := range openBranches {
		openSet[b] = true
	}

	for i := len(completedTasks) - 1; i >= 0; i-- {
		id := completedTasks[i].ID
		if id == "" {
			continue
		}
		branch, _ := backend.GetMetadata(id, "branch")
		if branch == "" || !openSet[branch] {
			continue
		}
		if err := g.FetchBranch(branch); err != nil {
			continue
		}
		if !g.RemoteBranchHasCommits(branch) {
			continue
		}
		if !g.BranchIsAheadOfMain(branch) {
			logger.Log("git", "Branch %s not ahead of main — skipping", branch)
			continue
		}
		g.SetPrevBranch(branch)
		logger.Log("git", "Stack head: %s (from %s)", branch, id)
		return
	}
	logger.Log("git", "No stacked parents — starting from %s", g.DetectDefaultBranch())
}

// checkoutExistingBranch checks metadata for a branch from a previous
// iteration. If the remote has that branch with work, it checks it out.
// Otherwise, it renames the current branch for the task and stores the
// new name in metadata. Returns true if an existing remote branch was
// checked out.
func checkoutExistingBranch(g git.GitOps, backend tasks.Backend, logger *logging.Logger, taskID, nextTask string) bool {
	storedBranch := ""
	if taskID != "" {
		storedBranch, _ = backend.GetMetadata(taskID, "branch")
	}
	if storedBranch != "" {
		_ = g.FetchBranch(storedBranch)
		if g.RemoteBranchHasCommits(storedBranch) {
			if g.RemoteBranchIsOnMain(storedBranch) {
				g.CheckoutRemoteBranch(storedBranch)
				return true
			}
			logger.Warn("git", "Remote branch %s diverged from main — cleaning up", storedBranch)
			ref, _ := backend.GetExternalRef(taskID)
			if parsePRNumber(ref) == "" {
				if err := g.DeleteRemoteBranchByName(storedBranch); err != nil {
					logger.Warn("git", "Failed to delete stale remote branch: %v", err)
				}
			}
		}
		g.RenameBranchTo(storedBranch)
		return false
	}
	g.RenameBranchForTask(nextTask, taskID)
	if taskID != "" && g.GetWorktreeBranch() != "" && strings.Contains(g.GetWorktreeBranch(), taskID) {
		_ = backend.SetMetadata(taskID, "branch", g.GetWorktreeBranch())
	}
	return false
}

// prLink builds a logging.Link for a PR number using the remote URL.
func (l *Loop) prLink(prNumber string) *logging.Link {
	url := prURL(l.git.RemoteURL(), prNumber)
	if url == "" {
		return nil
	}
	return &logging.Link{Text: "PR #" + prNumber, URL: url}
}

// handleRebase syncs the worktree to the latest default branch via
// EnsureUpToDate, which handles all conflict resolution internally.
func (l *Loop) handleRebase(ctx context.Context) error {
	return l.git.EnsureUpToDate(ctx)
}

func (l *Loop) prepareBranch(ctx context.Context, taskID, nextTask string) error {
	return prepareBranch(ctx, branchParams{
		git:      l.git,
		backend:  l.cfg.TaskBackend,
		state:    l.state,
		logger:   l.logger,
		ralphDir: l.cfg.Dirs.RalphDir,
	}, taskID, nextTask)
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

func (l *Loop) shipWork(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
	if l.pushPRFunc != nil {
		// Test path: delegate to legacy pushPRFunc stub.
		prNumber, err := l.pushPRFunc(ctx, opts.TaskID, opts.TaskTitle, opts.Body)
		return git.ShipResult{PRNumber: prNumber}, err
	}
	return l.git.Ship(ctx, opts)
}

func (l *Loop) pushAndCreatePR(ctx context.Context, taskID, taskDesc, body string) (string, error) {
	result, err := l.shipWork(ctx, git.ShipOpts{TaskID: taskID, TaskTitle: taskDesc, Body: body})
	return result.PRNumber, err
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

	// Check if a PR exists for this exact branch.
	prNumber, _ := l.git.FindOpenPRForBranch(branch)
	if prNumber != "" {
		l.logger.Emit(logging.Opts{Domain: "git", Link: l.prLink(prNumber)}, "Found for %s (task %s) — resolving", branch, taskID)
		_ = l.cfg.TaskBackend.SetExternalRef(taskID, prURL(l.git.RemoteURL(), prNumber))
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
			l.logger.Emit(logging.Opts{Domain: "git", Link: l.prLink(prNum)}, "Created for %s (task %s)", branch, taskID)
			_ = l.cfg.TaskBackend.SetExternalRef(taskID, prURL(l.git.RemoteURL(), prNum))
			return l.resolveByPRState(ctx, taskID, nextTask, prNum)
		}
	}

	return false
}

// resolveByPRState inspects the PR's state and takes the appropriate action.
// Delegates merge+close to finalizePR so resume and post-signal share one path.
func (l *Loop) resolveByPRState(ctx context.Context, taskID, nextTask, prNumber string) bool {
	prState, err := l.git.GetPRState(prNumber)
	if err != nil {
		l.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: l.prLink(prNumber)}, "Failed to get state: %v", err)
		return false
	}

	switch strings.ToUpper(prState) {
	case "MERGED":
		l.logger.Emit(logging.Opts{Domain: "git", Level: logging.Success, Link: l.prLink(prNumber)}, "already merged — closing bead and moving on")
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
		if ok, reason := l.git.PRChainIsHealthy(prNumber); !ok {
			l.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: l.prLink(prNumber)}, "chain unhealthy: %s — re-running agent", reason)
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
		l.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: l.prLink(prNumber)}, "is %s (not merged) — re-running agent", prState)
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

func (l *Loop) setStackHead() {
	setStackHead(l.git, l.cfg.TaskBackend, l.state, l.logger)
}

func (l *Loop) checkoutExistingBranch(taskID, nextTask string) bool {
	return checkoutExistingBranch(l.git, l.cfg.TaskBackend, l.logger, taskID, nextTask)
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
