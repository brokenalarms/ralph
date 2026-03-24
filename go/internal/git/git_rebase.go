package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/brokenalarms/ralph/internal/logging"
)

// RebaseRecovery represents the user's chosen recovery action when rebase
// fails due to squash-merged branches.
type RebaseRecovery int

const (
	RebaseAbort         RebaseRecovery = iota
	RebaseFreshWorktree
	RebaseManualResolve
)

// RebaseConflictError signals that a rebase failed and can potentially be
// recovered by recreating the worktree from main. Cause describes what
// went wrong (squash-merge conflicts vs real conflicts).
type RebaseConflictError struct {
	Cause string
}

func (e *RebaseConflictError) Error() string {
	return e.Cause
}

// autoResolveAndContinue attempts to resolve rebase conflicts mechanically
// and continue. For each conflicted file: if only one side changed from base,
// take that side. If both changed but ours is a subset of theirs, take theirs.
// Returns true if the rebase completed successfully.
func (m *Manager) autoResolveAndContinue(ctx context.Context, defaultBranch string) bool {
	for i := 0; i < 50; i++ { // max steps to prevent infinite loop
		conflicted := m.gitOutput(m.WorkDir, "diff", "--name-only", "--diff-filter=U")
		if conflicted == "" {
			// No conflicts — try to continue
			if err := m.gitCmdErrCtx(ctx, m.WorkDir, "rebase", "--continue"); err == nil {
				return true
			}
			// Might be done
			if !m.mRebaseInProgress() {
				return true
			}
			continue
		}

		resolvedAny := false
		for _, f := range strings.Split(conflicted, "\n") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}

			ours := m.gitOutput(m.WorkDir, "show", ":2:"+f)
			theirs := m.gitOutput(m.WorkDir, "show", ":3:"+f)
			base := m.gitOutput(m.WorkDir, "show", ":1:"+f)

			if ours == theirs {
				m.Logger.Log("git", "Auto-resolved (identical): %s", f)
				m.gitCmd(m.WorkDir, "checkout", "--theirs", f)
				m.gitCmd(m.WorkDir, "add", f)
				resolvedAny = true
			} else if ours == base {
				m.Logger.Log("git", "Auto-resolved (only theirs changed): %s", f)
				m.gitCmd(m.WorkDir, "checkout", "--theirs", f)
				m.gitCmd(m.WorkDir, "add", f)
				resolvedAny = true
			} else if theirs == base {
				m.Logger.Log("git", "Auto-resolved (only ours changed): %s", f)
				m.gitCmd(m.WorkDir, "checkout", "--ours", f)
				m.gitCmd(m.WorkDir, "add", f)
				resolvedAny = true
			} else {
				// Both changed — check if ours is subset of theirs
				if strings.Contains(theirs, ours) || isSubsetByLines(base, ours, theirs) {
					m.Logger.Log("git", "Auto-resolved (ours is subset of theirs): %s", f)
					m.gitCmd(m.WorkDir, "checkout", "--theirs", f)
					m.gitCmd(m.WorkDir, "add", f)
					resolvedAny = true
				} else {
					m.Logger.Warn("git", "Real conflict in %s — cannot auto-resolve", f)
					return false
				}
			}
		}

		if !resolvedAny {
			return false
		}

		// Continue the rebase
		if err := m.gitCmdErrCtx(ctx, m.WorkDir, "rebase", "--continue"); err != nil {
			if !m.mRebaseInProgress() {
				return true
			}
			// Another step with conflicts — loop again
			continue
		}
		return true
	}
	return false
}

// isSubsetByLines checks if every line ours added (relative to base) is
// present in theirs. If so, theirs is a superset and wins.
func isSubsetByLines(base, ours, theirs string) bool {
	baseLines := make(map[string]bool)
	for _, line := range strings.Split(base, "\n") {
		baseLines[line] = true
	}

	theirsLines := make(map[string]bool)
	for _, line := range strings.Split(theirs, "\n") {
		theirsLines[line] = true
	}

	for _, line := range strings.Split(ours, "\n") {
		if !baseLines[line] && !theirsLines[line] {
			return false
		}
	}
	return true
}

// resumedBranchIsStale returns true when the stored branch no longer exists
// or its changes have already been squash-merged into origin/defaultBranch.
func (m *Manager) resumedBranchIsStale(defaultBranch string) bool {
	if m.WorktreeBranch == "" {
		return false
	}

	if !m.refExists(m.WorkDir, m.WorktreeBranch) {
		return true
	}

	if !m.refExists(m.WorkDir, "origin/"+defaultBranch) {
		return false
	}

	// If HEAD has no unique commits beyond origin/main, it's already up to date or empty.
	revCount := m.gitOutput(m.WorkDir, "rev-list", "--count", "origin/"+defaultBranch+"..HEAD")
	if revCount == "" || revCount == "0" {
		return false
	}

	return IsBranchSquashMerged(m.WorkDir, m.WorktreeBranch, m.BaseBranch)
}

// resetResumedWorktree force-resets the worktree to origin/defaultBranch,
// equivalent to PostMergeReset but from a resume context. The old branch's
// work is already on main (squash-merged), so starting fresh is safe.
func (m *Manager) resetResumedWorktree(defaultBranch string) error {
	oldBranch := m.WorktreeBranch
	newBranch := m.TempBranch()

	if err := m.gitCmdErr(m.WorkDir, "checkout", "--force", "-B", newBranch, "origin/"+defaultBranch); err != nil {
		return fmt.Errorf("resume reset: checkout failed: %w", err)
	}

	if err := m.gitCmdErr(m.WorkDir, "clean", "-fd"); err != nil {
		m.Logger.Warn("git", "git clean failed (non-fatal): %v", err)
	}

	if oldBranch != newBranch {
		m.gitCmd(m.WorkDir, "branch", "-D", oldBranch)
	}

	m.WorktreeBranch = newBranch
	m.BranchRenamed = false
	if m.State != nil {
		_ = m.State.Write("worktree_branch", m.WorktreeBranch)
	}
	return nil
}

// RebaseOntoDefaultBranch rebases the worktree onto origin's default branch,
// detecting and skipping squash-merged branches when a naive rebase conflicts.
func (m *Manager) RebaseOntoDefaultBranch(ctx context.Context) error {
	// Stash dirty state so rebase can proceed cleanly.
	dirty := m.gitCmdErr(m.WorkDir, "diff", "--quiet") != nil ||
		m.gitCmdErr(m.WorkDir, "diff", "--cached", "--quiet") != nil
	if dirty {
		m.Logger.Log("git", "Stashing uncommitted changes before rebase...")
		m.gitCmd(m.WorkDir, "stash", "push", "-m", "ralph-rebase-autostash")
	}
	defer func() {
		if dirty {
			if err := m.gitCmdErr(m.WorkDir, "stash", "pop"); err != nil {
				m.Logger.Warn("git", "Stash pop conflict — committing stash as WIP")
				m.gitCmd(m.WorkDir, "checkout", "--theirs", ".")
				m.gitCmd(m.WorkDir, "add", "-A")
				m.gitCmd(m.WorkDir, "commit", "-m", "WIP: reapply stashed changes after rebase (may need review)")
			} else {
				m.Logger.Log("git", "Re-applied stashed changes")
			}
		}
	}()

	defaultBranch := m.detectDefaultBranch()
	if err := m.gitCmdErrCtx(ctx, m.WorkDir, "fetch", "origin", defaultBranch); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m.Logger.Warn("git", "Failed to fetch origin/%s: %v", defaultBranch, err)
	}

	// Skip if remote branch doesn't exist (e.g. repo never pushed)
	if !m.refExists(m.WorkDir, "origin/"+defaultBranch) {
		m.Logger.Log("git", "No remote branch origin/%s — skipping rebase", defaultBranch)
		return nil
	}

	// Already up to date: origin/main is ancestor of HEAD means HEAD
	// includes everything from main. The reverse (HEAD ancestor of
	// origin/main) would incorrectly skip rebase when HEAD is behind.
	if m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", "origin/"+defaultBranch, "HEAD") == nil {
		m.Logger.Log("git", "%s Already up to date with origin/%s", logging.BranchTag(defaultBranch), defaultBranch)
		return nil
	}

	// Try simple rebase
	if m.gitCmdErrCtx(ctx, m.WorkDir, "rebase", "--update-refs", "origin/"+defaultBranch) == nil {
		m.Logger.Log("git", "%s Rebased onto origin/%s", logging.BranchTag(defaultBranch), defaultBranch)
		return nil
	}

	// Try auto-resolving mechanical conflicts (e.g. squash-merge overlaps)
	if m.autoResolveAndContinue(ctx, defaultBranch) {
		m.Logger.Log("git", "%s Rebased onto origin/%s (auto-resolved conflicts)", logging.BranchTag(defaultBranch), defaultBranch)
		return nil
	}
	m.gitCmd(m.WorkDir, "rebase", "--abort")

	if ctx.Err() != nil {
		return ctx.Err()
	}

	m.Logger.Warn("git", "Rebase failed, checking for squash-merged branches...")

	lastMerged := m.findLastSquashMergedBranch(defaultBranch)

	if lastMerged == "" {
		m.Logger.Error("git", "%s Rebase onto %s failed with real conflicts", logging.BranchTag(defaultBranch), defaultBranch)
		return &RebaseConflictError{Cause: fmt.Sprintf("rebase onto %s failed with real conflicts", defaultBranch)}
	}

	m.Logger.Log("git", "Detected squash-merged branch: %s", lastMerged)

	if err := m.gitCmdErrCtx(ctx, m.WorkDir, "rebase", "--update-refs", "--onto", "origin/"+defaultBranch, lastMerged, "HEAD"); err != nil {
		m.gitCmd(m.WorkDir, "rebase", "--abort")
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m.Logger.Error("git", "%s Rebase onto %s past squash-merged branches failed", logging.BranchTag(defaultBranch), defaultBranch)
		return &RebaseConflictError{Cause: fmt.Sprintf("rebase onto %s past squash-merged branches failed", defaultBranch)}
	}

	m.Logger.Log("git", "%s Rebased onto origin/%s (skipped squash-merged branches)", logging.BranchTag(defaultBranch), defaultBranch)

	m.TaskSeq = ParseTaskSeqFromBranches(m.ProjectDir, m.ProjectName)
	m.gitCmd(m.ProjectDir, "branch", "-D", lastMerged)

	return nil
}

// findLastSquashMergedBranch iterates ralph/<project>/* branches (sorted by
// refname, skipping */next) and returns the last one whose changes are already
// absorbed into origin/defaultBranch. Detection uses a temporary git index:
// read-tree origin/default, then reverse-apply the branch's diff. If that
// succeeds, main already contains the branch's changes (squash-merged).
func (m *Manager) findLastSquashMergedBranch(defaultBranch string) string {
	branches := ListProjectBranches(m.ProjectDir, m.ProjectName)
	lastMerged := ""

	for _, branch := range branches {
		if strings.HasSuffix(branch, "/next") {
			continue
		}

		mergeBase := m.gitOutput(m.WorkDir, "merge-base", "origin/"+defaultBranch, branch)
		if mergeBase == "" {
			continue
		}

		// Skip branches with no changes
		branchFiles := m.gitOutput(m.WorkDir, "diff", "--name-only", mergeBase, branch)
		if branchFiles == "" {
			continue
		}

		if m.isSquashMerged(defaultBranch, mergeBase, branch) {
			lastMerged = branch
		}
	}

	return lastMerged
}

// isSquashMerged checks whether branch's changes (relative to mergeBase) are
// already present in origin/defaultBranch by reverse-applying the diff against
// a temporary index loaded with origin/default's tree.
func (m *Manager) isSquashMerged(defaultBranch, mergeBase, branch string) bool {
	return checkSquashMerged(m.WorkDir, defaultBranch, mergeBase, branch)
}

// IsBranchSquashMerged checks whether a branch's changes have been
// squash-merged into origin's default branch.
func IsBranchSquashMerged(dir, branch, baseBranch string) bool {
	defaultBranch := detectDefaultBranch(dir, baseBranch)
	if !refExists(dir, "origin/"+defaultBranch) {
		return false
	}

	mergeBase := gitOutput(dir, "merge-base", "origin/"+defaultBranch, branch)
	if mergeBase == "" {
		return false
	}

	branchFiles := gitOutput(dir, "diff", "--name-only", mergeBase, branch)
	if branchFiles == "" {
		return false
	}

	return checkSquashMerged(dir, defaultBranch, mergeBase, branch)
}

// checkSquashMerged tests whether a branch's changes (from mergeBase) are
// already present in origin/defaultBranch. Loads origin's tree into a
// temporary index and reverse-applies the branch diff — success means
// origin already contains the changes (squash-merged).
func checkSquashMerged(dir, defaultBranch, mergeBase, branch string) bool {
	tmpIndex, err := os.CreateTemp(dir, ".ralph_squash_check.*")
	if err != nil {
		return false
	}
	tmpPath := tmpIndex.Name()
	tmpIndex.Close()
	defer os.Remove(tmpPath)

	readTreeCmd := exec.Command("git", "-C", dir, "read-tree", "origin/"+defaultBranch)
	readTreeCmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+tmpPath)
	if readTreeCmd.Run() != nil {
		return false
	}

	diffCmd := exec.Command("git", "-C", dir, "diff", mergeBase, branch)
	diffOut, err := diffCmd.Output()
	if err != nil || len(diffOut) == 0 {
		return false
	}

	applyCmd := exec.Command("git", "-C", dir, "apply", "--cached", "--reverse", "--check", "-C0")
	applyCmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+tmpPath)
	applyCmd.Stdin = strings.NewReader(string(diffOut))
	return applyCmd.Run() == nil
}

// RecreateFromMain removes the current worktree and creates a fresh one from
// origin's default branch. State is preserved except for git-specific fields
// (worktree_dir, worktree_branch). This is the recovery path when rebase fails
// because base branches were squash-merged — the completed work is already on
// main, so starting fresh is safe.
func (m *Manager) RecreateFromMain(ctx context.Context) error {
	if m.WorkDir == "" || m.WorkDir == m.ProjectDir {
		return fmt.Errorf("cannot recreate: no worktree active")
	}

	m.Logger.Log("git", "Removing old worktree: %s", m.WorkDir)
	m.gitCmd(m.ProjectDir, "worktree", "remove", "--force", m.WorkDir)

	// Prune stale worktree references before listing branches — a worktree
	// whose directory was deleted still marks its branch as checked-out.
	m.gitCmd(m.ProjectDir, "worktree", "prune")

	// Delete all ralph project branches (squash-merged work is on main).
	// A branch may be checked out in an external worktree (e.g. a Claude
	// sub-agent in .claude/worktrees/). Force-remove such worktrees first.
	branches := ListProjectBranches(m.ProjectDir, m.ProjectName)
	for _, b := range branches {
		if wt := m.findWorktreeForBranch(m.ProjectDir, b); wt != "" {
			m.Logger.Log("git", "Removing worktree holding branch %s: %s", b, wt)
			m.gitCmd(m.ProjectDir, "worktree", "remove", "--force", wt)
		}
		m.gitCmd(m.ProjectDir, "branch", "-D", b)
	}

	// Reset git-tracking fields
	m.WorktreeBranch = ""
	m.BranchRenamed = false
	m.TaskSeq = 0

	// Re-run SetupWorktree to create a fresh worktree from main
	m.Resume = false
	if err := m.SetupWorktree(ctx); err != nil {
		return fmt.Errorf("recreating worktree: %w", err)
	}

	m.Logger.Log("git", "Fresh worktree created from main: %s", m.WorkDir)
	return nil
}
