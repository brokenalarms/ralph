package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// StateStore abstracts reading and writing ralph JSON state.
type StateStore interface {
	Read(key string) (string, error)
	Write(key, value string) error
}

// Log is the logging interface used by Manager, matching the subset of
// logging.Logger that git operations need.
type Log interface {
	Log(domain string, format string, args ...any)
	Warn(domain string, format string, args ...any)
	Error(domain string, format string, args ...any)
}

// Manager handles git worktree creation, branch rotation, and renaming.
type Manager struct {
	ProjectDir     string
	RalphDir       string
	ProjectName    string
	WorkDir        string
	WorktreeBranch string
	Resume         bool
	TaskSeq        int
	BranchRenamed bool
	BaseBranch    string
	MergeAdmin    bool
	GitHub         GitHub
	Runner         Runner
	State          StateStore
	Logger         Log
}

// GH returns the GitHub interface, using the injected stub if set (tests)
// or a live ghCLI wrapper in production.
func (m *Manager) GH() GitHub {
	if m.GitHub != nil {
		return m.GitHub
	}
	return &ghCLI{}
}

func (m *Manager) gh() GitHub {
	return m.GH()
}

// TempBranch returns the temporary branch name for the current project.
func (m *Manager) TempBranch() string {
	return "ralph/" + m.ProjectName + "/next"
}

// SetupWorktree creates (or resumes) a git worktree for isolated work.
func (m *Manager) SetupWorktree(ctx context.Context) error {
	m.WorkDir = m.ProjectDir

	if !IsGitRepo(m.ProjectDir) {
		return fmt.Errorf("not a git repo — ralph requires git")
	}

	if m.Resume {
		if err := m.tryResumeWorktree(); err == nil {
			return nil
		}
	}

	m.ProjectName = filepath.Base(m.ProjectDir)

	today := time.Now().Format("20060102")
	runSeq := 1
	worktreeRoot := filepath.Join(m.RalphDir, "worktrees")
	if entries, err := os.ReadDir(worktreeRoot); err == nil {
		prefix := "ralph-" + today + "-"
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
				runSeq++
			}
		}
	}

	m.WorktreeBranch = m.TempBranch()
	m.WorkDir = filepath.Join(m.RalphDir, "worktrees", fmt.Sprintf("ralph-%s-%02d", today, runSeq))

	if err := os.MkdirAll(filepath.Join(m.RalphDir, "worktrees"), 0o755); err != nil {
		return fmt.Errorf("creating worktrees dir: %w", err)
	}

	m.gitCmd(m.ProjectDir, "worktree", "prune")

	// Remove leftover worktree directory from a previous run (git prune
	// cleans the bookkeeping but leaves the directory on disk).
	if _, err := os.Stat(m.WorkDir); err == nil {
		os.RemoveAll(m.WorkDir)
	}

	if err := m.cleanTempBranch(); err != nil {
		return err
	}

	defaultBranch := m.detectDefaultBranch()
	m.gitCmdCtx(ctx, m.ProjectDir, "fetch", "origin", defaultBranch)

	if !m.refExists(m.ProjectDir, "origin/"+defaultBranch) &&
		m.refExists(m.ProjectDir, "HEAD") &&
		m.remoteExists() {
		m.Logger.Log("git", "Pushing %s to origin (empty remote — ensures correct default branch)", defaultBranch)
		m.gitCmdCtx(ctx, m.ProjectDir, "push", "-u", "origin", defaultBranch)
	}

	if err := m.gitCmdErr(m.ProjectDir, "worktree", "add", "-b", m.WorktreeBranch, m.WorkDir, "origin/"+defaultBranch); err != nil {
		if err := m.gitCmdErr(m.ProjectDir, "worktree", "add", "-b", m.WorktreeBranch, m.WorkDir, "HEAD"); err != nil {
			return fmt.Errorf("failed to create worktree: %w", err)
		}
	}

	m.gitCmd(m.WorkDir, "config", "rebase.updateRefs", "true")
	m.Logger.Log("git", "Worktree: %s (branch: %s)", m.WorkDir, m.WorktreeBranch)

	if m.State != nil {
		_ = m.State.Write("worktree_dir", m.WorkDir)
		_ = m.State.Write("worktree_branch", m.WorktreeBranch)
	}

	return nil
}

// tryResumeWorktree attempts to reuse a stored worktree from a previous run.
func (m *Manager) tryResumeWorktree() error {
	if m.State == nil {
		return fmt.Errorf("no state store")
	}
	stored, err := m.State.Read("worktree_dir")
	if err != nil || stored == "" || stored == "null" {
		return fmt.Errorf("no stored worktree")
	}
	info, err := os.Stat(stored)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("stored worktree missing")
	}

	m.WorkDir = stored
	branch, _ := m.State.Read("worktree_branch")
	m.WorktreeBranch = branch
	m.ProjectName = filepath.Base(m.ProjectDir)
	if renamed, _ := m.State.Read("branch_renamed"); renamed == "true" {
		m.BranchRenamed = true
	} else {
		m.BranchRenamed = branch != m.TempBranch()
	}

	if seqStr, _ := m.State.Read("task_seq"); seqStr != "" {
		if n, err := strconv.Atoi(seqStr); err == nil {
			m.TaskSeq = n
		}
	}

	m.Logger.Log("git", "Resuming worktree: %s", m.WorkDir)

	defaultBranch := m.detectDefaultBranch()
	if err := m.gitCmdErr(m.WorkDir, "fetch", "origin", defaultBranch); err != nil {
		m.Logger.Warn("git", "Failed to fetch origin/%s on resume: %v", defaultBranch, err)
	}

	if m.resumedBranchIsStale(defaultBranch) {
		m.Logger.Log("git", "Stale branch detected on resume — resetting to origin/%s", defaultBranch)
		return m.resetResumedWorktree(defaultBranch)
	}

	_ = m.EnsureUpToDate(context.Background())
	return nil
}

// withStash stashes any uncommitted changes, runs fn, then reapplies the stash.
// Used by EnsureUpToDate and RebaseOntoDefaultBranch to avoid duplicating
// the stash/pop logic.
func (m *Manager) withStash(stashMsg string, fn func()) {
	dirty := m.gitCmdErr(m.WorkDir, "diff", "--quiet") != nil ||
		m.gitCmdErr(m.WorkDir, "diff", "--cached", "--quiet") != nil
	if dirty {
		m.Logger.Log("git", "Stashing uncommitted changes before rebase...")
		m.gitCmd(m.WorkDir, "stash", "push", "-m", stashMsg)
	}

	fn()

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
}

// EnsureUpToDate fetches the latest base branch, stashes any uncommitted
// changes, rebases onto origin, and reapplies the stash. This is the single
// sync point that ALL git operations go through: push, merge, resume, and
// the loop's handleRebase. Includes squash-merge detection, auto-resolve,
// and force-reset as last resort.
// EnsureUpToDate is the single sync point for all git operations. It
// fetches the latest default branch and applies an escalating series of
// strategies to get the worktree up to date:
//
//  1. Fast-forward rebase — clean replay on top of origin
//  2. Auto-resolve — mechanical conflict resolution (theirs for new files)
//  3. Skip squash-merged — rebase past branches already on main
//  4. Reset and replay — force-reset to origin, cherry-pick local commits
//
// Each strategy preserves the agent's committed work. If cherry-pick
// fails on a real conflict, the stale work is discarded and the remote
// branch is cleaned up so the task re-runs on a clean base.
func (m *Manager) EnsureUpToDate(ctx context.Context) error {
	if m.WorkDir == "" || m.WorkDir == m.ProjectDir {
		return nil
	}

	defaultBranch := m.detectDefaultBranch()

	if err := m.gitCmdErrCtx(ctx, m.WorkDir, "fetch", "origin", defaultBranch); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m.Logger.Warn("git", "Failed to fetch origin/%s: %v", defaultBranch, err)
		return nil
	}

	if !m.refExists(m.WorkDir, "origin/"+defaultBranch) {
		return nil
	}

	if m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", "origin/"+defaultBranch, "HEAD") == nil {
		m.Logger.Log("git", "%s Already up to date with origin/%s", logging.BranchTag(defaultBranch), defaultBranch)
		return nil
	}

	var result error
	m.withStash("ralph-autostash", func() {
		if ctx.Err() != nil {
			result = ctx.Err()
			return
		}

		// 1. Fast-forward rebase
		if m.tryRebase(ctx, defaultBranch) {
			return
		}

		// 2. Auto-resolve mechanical conflicts
		if m.tryAutoResolve(ctx, defaultBranch) {
			return
		}

		// 3. Skip squash-merged branches
		if m.trySkipSquashMerged(ctx, defaultBranch) {
			return
		}

		// 4. Last resort: reset to origin, replay local commits
		result = m.ResetAndReplay(defaultBranch)
	})
	return result
}

func (m *Manager) tryRebase(ctx context.Context, defaultBranch string) bool {
	if m.gitCmdErrCtx(ctx, m.WorkDir, "rebase", "--update-refs", "origin/"+defaultBranch) == nil {
		m.Logger.Log("git", "%s Rebased onto origin/%s", logging.BranchTag(defaultBranch), defaultBranch)
		return true
	}
	return false
}

func (m *Manager) tryAutoResolve(ctx context.Context, defaultBranch string) bool {
	if m.autoResolveAndContinue(ctx, defaultBranch) {
		m.Logger.Log("git", "%s Rebased onto origin/%s (auto-resolved)", logging.BranchTag(defaultBranch), defaultBranch)
		return true
	}
	m.gitCmd(m.WorkDir, "rebase", "--abort")
	return false
}

func (m *Manager) trySkipSquashMerged(ctx context.Context, defaultBranch string) bool {
	m.Logger.Warn("git", "Rebase failed, checking for squash-merged branches...")
	lastMerged := m.findLastSquashMergedBranch(defaultBranch)
	if lastMerged == "" {
		return false
	}
	m.Logger.Log("git", "Detected squash-merged branch: %s", lastMerged)
	if m.gitCmdErrCtx(ctx, m.WorkDir, "rebase", "--update-refs", "--onto", "origin/"+defaultBranch, lastMerged, "HEAD") == nil {
		m.Logger.Log("git", "%s Rebased onto origin/%s (skipped squash-merged)", logging.BranchTag(defaultBranch), defaultBranch)
		m.TaskSeq = m.ParseTaskSeqFromBranches()
		m.gitCmd(m.ProjectDir, "branch", "-D", lastMerged)
		return true
	}
	m.gitCmd(m.WorkDir, "rebase", "--abort")
	return false
}

// ResetAndReplay saves local commits ahead of origin/defaultBranch,
// force-resets to origin/defaultBranch, then cherry-picks the commits
// back on top. This is the last-resort sync strategy when all rebase
// approaches fail — it always gets to the latest base while preserving
// the agent's committed work.
func (m *Manager) ResetAndReplay(defaultBranch string) error {
	raw := m.gitOutput(m.WorkDir, "rev-list", "--reverse", "origin/"+defaultBranch+"..HEAD")
	commits := strings.Split(strings.TrimSpace(raw), "\n")

	count := 0
	for _, c := range commits {
		if c != "" {
			count++
		}
	}

	m.Logger.Warn("git", "%s Rebase failed — resetting to origin/%s and replaying %d commits",
		logging.BranchTag(defaultBranch), defaultBranch, count)
	m.gitCmd(m.WorkDir, "reset", "--hard", "origin/"+defaultBranch)

	for _, sha := range commits {
		if sha == "" {
			continue
		}
		if err := m.gitCmdErr(m.WorkDir, "cherry-pick", sha); err != nil {
			m.Logger.Warn("git", "Cherry-pick %s failed — discarding stale work, task will re-run", sha[:8])
			m.gitCmd(m.WorkDir, "cherry-pick", "--abort")
			// Stay on the clean origin/default — stale work is gone.
			// Delete the remote branch so RemoteBranchHasWork doesn't
			// find it again. Close any open PR for this branch.
			if m.WorktreeBranch != "" {
				_ = m.gitCmdErr(m.WorkDir, "push", "origin", "--delete", m.WorktreeBranch)
				m.Logger.Log("git", "Deleted stale remote branch %s", m.WorktreeBranch)
			}
			return nil // no error — worktree is clean, task re-runs
		}
	}
	return nil
}

// cleanTempBranch removes the temp branch, force-removing a stale worktree
// if necessary. Returns an error only if the branch is checked out in a
// non-ralph worktree.
func (m *Manager) cleanTempBranch() error {
	if !m.refExists(m.ProjectDir, m.WorktreeBranch) {
		return nil
	}
	if err := m.gitCmdErr(m.ProjectDir, "branch", "-D", m.WorktreeBranch); err == nil {
		return nil
	}

	existingWt := m.findWorktreeForBranch(m.ProjectDir, m.WorktreeBranch)
	if existingWt != "" && strings.Contains(existingWt, "/.ralph/worktrees/") {
		m.Logger.Warn("git", "Removing stale ralph worktree: %s", existingWt)
		m.gitCmd(m.ProjectDir, "worktree", "remove", "--force", existingWt)
		m.gitCmd(m.ProjectDir, "branch", "-D", m.WorktreeBranch)
		return nil
	}

	return fmt.Errorf("cannot delete branch '%s' — it is checked out in a non-ralph worktree: %s",
		m.WorktreeBranch, existingWt)
}

// RenameBranchForTask renames the current branch to include a task slug.
// Each call increments TaskSeq. Only renames once per iteration (tracked by
// BranchRenamed). When taskID is provided, it is included in the branch name
// for traceability.
func (m *Manager) RenameBranchForTask(taskDesc, taskID string) {
	if m.BranchRenamed || m.WorktreeBranch == "" || taskDesc == "" {
		return
	}
	if m.WorkDir == m.ProjectDir {
		return
	}

	slug := Slugify(taskDesc)
	if slug == "" {
		return
	}

	m.TaskSeq++
	var newBranch string
	if taskID != "" {
		newBranch = fmt.Sprintf("ralph/%s/%02d-%s-%s", m.ProjectName, m.TaskSeq, taskID, slug)
	} else {
		newBranch = fmt.Sprintf("ralph/%s/%02d-%s", m.ProjectName, m.TaskSeq, slug)
	}
	if err := m.gitCmdErr(m.WorkDir, "branch", "-m", m.WorktreeBranch, newBranch); err == nil {
		m.WorktreeBranch = newBranch
		m.BranchRenamed = true
		if m.State != nil {
			_ = m.State.Write("worktree_branch", m.WorktreeBranch)
			_ = m.State.Write("task_seq", fmt.Sprintf("%d", m.TaskSeq))
			_ = m.State.Write("branch_renamed", "true")
		}
	}
}

// RotateBranch creates a fresh temp branch from the current HEAD, ready for
// the next iteration.
func (m *Manager) RotateBranch() {
	if m.WorktreeBranch == "" || m.WorkDir == m.ProjectDir {
		return
	}

	newBranch := m.TempBranch()

	// Already on the temp branch (e.g. after PostMergeReset)
	if m.WorktreeBranch == newBranch {
		m.BranchRenamed = false
		if m.State != nil {
			_ = m.State.Write("branch_renamed", "false")
		}
		return
	}

	if err := m.gitCmdErr(m.WorkDir, "checkout", "-B", newBranch); err == nil {
		m.WorktreeBranch = newBranch
		m.BranchRenamed = false
		if m.State != nil {
			_ = m.State.Write("worktree_branch", m.WorktreeBranch)
			_ = m.State.Write("branch_renamed", "false")
		}
		m.Logger.Log("git", "Branch: %s (from previous iteration)", m.WorktreeBranch)
	} else {
		m.Logger.Warn("git", "Branch rotation failed, continuing on %s", m.WorktreeBranch)
	}
}

// RemoveWorktree force-removes a worktree and deletes its branch.
func (m *Manager) RemoveWorktree() {
	m.gitCmd(m.ProjectDir, "worktree", "remove", "--force", m.WorkDir)
	m.gitCmd(m.ProjectDir, "branch", "-D", m.WorktreeBranch)
}

// TagTaskStart creates a lightweight git tag marking the start of a task iteration.
// The tag name is task/{taskID}/start when a backend ID is available,
// or task/{seq}-{slug}/start derived from the current branch name.
func (m *Manager) TagTaskStart(taskID string) {
	tag := m.taskTag(taskID, "start")
	if tag == "" {
		return
	}
	if err := m.gitCmdErr(m.WorkDir, "tag", "-f", tag); err == nil {
		m.Logger.Log("git", "Tag: %s", tag)
	}
}

// TagTaskEnd creates a lightweight git tag marking the end of a task iteration.
func (m *Manager) TagTaskEnd(taskID string) {
	tag := m.taskTag(taskID, "end")
	if tag == "" {
		return
	}
	if err := m.gitCmdErr(m.WorkDir, "tag", "-f", tag); err == nil {
		m.Logger.Log("git", "Tag: %s", tag)
	}
}

// taskTag builds a tag name like task/{id}/{suffix}. Returns empty
// string if there's not enough info to build a meaningful tag.
func (m *Manager) taskTag(taskID, suffix string) string {
	if m.WorkDir == "" || m.WorkDir == m.ProjectDir {
		return ""
	}
	if taskID != "" {
		return fmt.Sprintf("task/%s/%s", taskID, suffix)
	}
	seqSlug := extractSeqSlug(m.WorktreeBranch)
	if seqSlug == "" {
		return ""
	}
	return fmt.Sprintf("task/%s/%s", seqSlug, suffix)
}

// extractSeqSlug pulls the "NN-slug" portion from a branch like
// "ralph/project/01-my-task".
func extractSeqSlug(branch string) string {
	parts := strings.SplitN(branch, "/", 3)
	if len(parts) < 3 {
		return ""
	}
	seg := parts[2]
	if seg == "next" || seg == "" {
		return ""
	}
	return seg
}
