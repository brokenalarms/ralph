package git

import (
	"context"
	"errors"
	"fmt"

	"github.com/brokenalarms/ralph/internal/logging"
)

// BranchTaskMeta is the data the loop extracts from the backend before calling BranchForTask.
type BranchTaskMeta struct {
	Branch            string   // stored branch name for this task from a prior iteration
	ExternalRef       string   // PR URL from a prior iteration
	CompletedBranches []string // branches of completed tasks, oldest first; used for stack head detection
}

// SyncWorktreeBase detects the current stack head and rebases the worktree onto
// it (or the default branch if no stack exists). Called once on startup before
// the first iteration, before any task-specific branch setup.
func (r *repo) SyncWorktreeBase(ctx context.Context, completedBranches []string) error {
	setStackHead(r, completedBranches)
	if r.prevBranch == "" {
		r.ResetToDefaultBranch()
	}
	return r.EnsureUpToDate(ctx)
}

// BranchForTask prepares a branch for the given task: detects the stack head,
// resets/rebases if in a worktree, and checks out or renames to the task branch.
// Returns the resulting branch name.
func (r *repo) BranchForTask(ctx context.Context, taskID, title string, meta BranchTaskMeta) (string, error) {
	r.PrepareForNextTask(taskID)

	if r.worktreeBranch != "" && r.workDir != r.projectDir {
		setStackHead(r, meta.CompletedBranches)
		if r.prevBranch == "" {
			r.ResetToDefaultBranch()
		}
		if err := r.EnsureUpToDate(ctx); err != nil {
			// A local-rebase abort is recoverable — the branch still has its
			// in-flight commits. Warn and proceed with the stale base; the
			// agent will either resolve or a later merge pipeline (which uses
			// UnresolvedConflictError semantics) will handle it.
			var localConflict *LocalRebaseConflictError
			if errors.As(err, &localConflict) {
				r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "%v — continuing with stale base", localConflict)
			} else {
				return "", err
			}
		}
	} else {
		setStackHead(r, meta.CompletedBranches)
	}

	if _, err := checkoutExistingBranch(r, meta, taskID, title); err != nil {
		return "", err
	}
	return r.worktreeBranch, nil
}

// setStackHead finds the most recent completed branch that is cleanly ahead of
// main and sets it as the stack base for the next task.
func setStackHead(r *repo, completedBranches []string) {
	r.prevBranch = ""
	if len(completedBranches) == 0 {
		return
	}

	openBranches, err := r.ListOpenPRBranches()
	if err != nil || len(openBranches) == 0 {
		return
	}
	openSet := make(map[string]bool, len(openBranches))
	for _, b := range openBranches {
		openSet[b] = true
	}

	for i := len(completedBranches) - 1; i >= 0; i-- {
		branch := completedBranches[i]
		if branch == "" || !openSet[branch] {
			continue
		}
		if err := r.FetchBranch(branch); err != nil {
			continue
		}
		if !r.RemoteBranchHasCommits(branch) {
			continue
		}
		if !r.BranchIsAheadOfMain(branch) {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Branch %s not ahead of main — skipping", branch)
			continue
		}
		r.prevBranch = branch
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Stack head: %s", branch)
		return
	}
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "No stacked parents — starting from %s", r.DetectDefaultBranch())
}

// checkoutExistingBranch checks meta for a branch from a previous iteration.
// If the remote has that branch with clean work, it checks it out.
// Otherwise it renames the current branch for the task.
// Returns true if an existing remote branch was checked out.
func checkoutExistingBranch(r *repo, meta BranchTaskMeta, taskID, nextTask string) (bool, error) {
	storedBranch := meta.Branch
	if storedBranch != "" {
		_ = r.FetchBranch(storedBranch)
		if r.RemoteBranchHasCommits(storedBranch) {
			if r.RemoteBranchIsOnMain(storedBranch) {
				r.CheckoutRemoteBranch(storedBranch)
				return true, nil
			}
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Remote branch %s diverged from main — cleaning up", storedBranch)
			if parsePRNumber(meta.ExternalRef) == 0 {
				if err := r.DeleteRemoteBranchByName(storedBranch); err != nil {
					r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to delete stale remote branch: %v", err)
				}
			}
		}
		r.RenameBranchTo(storedBranch)
		return false, nil
	}
	if err := r.RenameBranchForTask(nextTask, taskID); err != nil {
		return false, fmt.Errorf("branch rename failed: %w", err)
	}
	return false, nil
}
