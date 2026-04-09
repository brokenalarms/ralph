package git

import (
	"context"
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
func (m *Repo) SyncWorktreeBase(ctx context.Context, completedBranches []string) error {
	setStackHead(m, completedBranches)
	if m.PrevBranch == "" {
		m.ResetToDefaultBranch()
	}
	return m.EnsureUpToDate(ctx)
}

// BranchForTask prepares a branch for the given task: detects the stack head,
// resets/rebases if in a worktree, and checks out or renames to the task branch.
// Returns the resulting branch name.
func (m *Repo) BranchForTask(ctx context.Context, taskID, title string, meta BranchTaskMeta) (string, error) {
	m.PrepareForNextTask(taskID)

	if m.WorktreeBranch != "" && m.WorkDir != m.ProjectDir {
		setStackHead(m, meta.CompletedBranches)
		if m.PrevBranch == "" {
			m.ResetToDefaultBranch()
		}
		if err := m.EnsureUpToDate(ctx); err != nil {
			return "", err
		}
	} else {
		setStackHead(m, meta.CompletedBranches)
	}

	if _, err := checkoutExistingBranch(m, meta, taskID, title); err != nil {
		return "", err
	}
	return m.WorktreeBranch, nil
}

// setStackHead finds the most recent completed branch that is cleanly ahead of
// main and sets it as the stack base for the next task.
func setStackHead(m *Repo, completedBranches []string) {
	m.PrevBranch = ""
	if len(completedBranches) == 0 {
		return
	}

	openBranches, err := m.ListOpenPRBranches()
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
		if err := m.FetchBranch(branch); err != nil {
			continue
		}
		if !m.RemoteBranchHasCommits(branch) {
			continue
		}
		if !m.BranchIsAheadOfMain(branch) {
			m.Logger.Emit(logging.Opts{Domain: logging.Git}, "Branch %s not ahead of main — skipping", branch)
			continue
		}
		m.PrevBranch = branch
		m.Logger.Emit(logging.Opts{Domain: logging.Git}, "Stack head: %s", branch)
		return
	}
	m.Logger.Emit(logging.Opts{Domain: logging.Git}, "No stacked parents — starting from %s", m.DetectDefaultBranch())
}

// checkoutExistingBranch checks meta for a branch from a previous iteration.
// If the remote has that branch with clean work, it checks it out.
// Otherwise it renames the current branch for the task.
// Returns true if an existing remote branch was checked out.
func checkoutExistingBranch(m *Repo, meta BranchTaskMeta, taskID, nextTask string) (bool, error) {
	storedBranch := meta.Branch
	if storedBranch != "" {
		_ = m.FetchBranch(storedBranch)
		if m.RemoteBranchHasCommits(storedBranch) {
			if m.RemoteBranchIsOnMain(storedBranch) {
				m.CheckoutRemoteBranch(storedBranch)
				return true, nil
			}
			m.Logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Remote branch %s diverged from main — cleaning up", storedBranch)
			if parsePRNumber(meta.ExternalRef) == 0 {
				if err := m.DeleteRemoteBranchByName(storedBranch); err != nil {
					m.Logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to delete stale remote branch: %v", err)
				}
			}
		}
		m.RenameBranchTo(storedBranch)
		return false, nil
	}
	if err := m.RenameBranchForTask(nextTask, taskID); err != nil {
		return false, fmt.Errorf("branch rename failed: %w", err)
	}
	return false, nil
}
