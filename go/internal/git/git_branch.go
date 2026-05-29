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
	setStackHead(ctx, r, completedBranches)
	r.stackHeadResolved = true
	if r.prevBranch == "" {
		if len(completedBranches) > 0 {
			// Stack drained (top branch merged or PR closed) — local commits on
			// the worktree are ghosts from the prior stack and cannot cleanly
			// rebase onto an advanced origin/main. Force-reset discards them;
			// any dirty WIP is captured via `git stash create` (a dangling
			// commit, not on the shared stash stack) and re-applied after reset.
			r.forceResetToDefaultBranch()
		} else {
			// No prior stack — local commits may be genuine mid-task WIP from
			// a loop restart. Preserve and let EnsureUpToDate rebase or abort.
			r.ResetToDefaultBranch()
		}
	}
	return r.EnsureUpToDate(ctx)
}

// BranchForTask prepares a branch for the given task: detects the stack head,
// anchors a fresh wip branch at the resolved base, resets/rebases if in a
// worktree, and checks out or renames to the task branch.
// Returns the resulting branch name.
func (r *repo) BranchForTask(ctx context.Context, taskID, title string, meta BranchTaskMeta) (string, error) {
	// Resolve stack head before creating the new branch so baseRef is known.
	// Skip on the first task if SyncWorktreeBase already ran setStackHead at
	// startup — the result is still valid and re-running emits duplicate logs.
	if r.stackHeadResolved {
		r.stackHeadResolved = false
	} else {
		setStackHead(ctx, r, meta.CompletedBranches)
	}

	baseRef := "origin/" + r.detectDefaultBranch()
	if r.prevBranch != "" {
		baseRef = "origin/" + r.prevBranch
	}

	r.PrepareForNextTask(taskID, baseRef)

	if r.worktreeBranch != "" && r.workDir != r.projectDir {
		if r.prevBranch == "" {
			// PrepareForNextTask already anchored at origin/main; this is a
			// defensive no-op when HEAD == origin/main, kept for safety.
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
	}

	if _, err := checkoutExistingBranch(r, meta, taskID, title); err != nil {
		return "", err
	}
	return r.worktreeBranch, nil
}

// setStackHead sets the stack parent for the next task.
//
// Only completedBranches[len-1] (the newest completed branch) is considered.
// Both guards must pass for prevBranch to be set:
//   - The branch has an open PR (ListOpenPRBranches membership)
//   - The branch is cleanly ahead of main (BranchIsAheadOfMain)
//
// This prevents squash-merged branches from qualifying: after a PR lands via
// squash-merge, the local branch is diverged from main (it has commits main
// does not, but main has the squashed commit the branch lacks). BranchIsAheadOfMain
// returns false for diverged branches, so the stale branch is rejected and the
// next task starts from main instead.
func setStackHead(ctx context.Context, r *repo, completedBranches []string) {
	r.prevBranch = ""
	if len(completedBranches) == 0 {
		return
	}

	top := completedBranches[len(completedBranches)-1]
	if top == "" {
		return
	}

	openBranches, err := r.ListOpenPRBranches(ctx)
	if err != nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "No stacked parents — ListOpenPRBranches error: %v", err)
		return
	}

	hasOpenPR := false
	for _, b := range openBranches {
		if b == top {
			hasOpenPR = true
			break
		}
	}
	if !hasOpenPR {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "No stacked parents — %s has no open PR", top)
		return
	}

	if err := r.FetchBranch(top); err != nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "No stacked parents — FetchBranch(%s) error: %v", top, err)
		return
	}

	if !r.BranchIsAheadOfMain(top) {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "No stacked parents — %s is not ahead of main", top)
		return
	}

	r.prevBranch = top
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Stack head: %s", top)
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
