package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// Manager handles git worktree creation, branch naming, and sync.
type Manager struct {
	ProjectDir     string
	RalphDir       string
	ProjectName    string
	WorkDir        string
	WorktreeBranch string
	PrevBranch     string
	Resume         bool
	BranchRenamed bool
	BaseBranch    string
	GitHub         GitHub
	Runner         Runner
	State          StateStore
	Logger         Log
	PrePush        func(ctx context.Context) error
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

	// Use a placeholder branch name; RenameBranchForTask will rename it
	// to a proper task branch before the first commit.
	m.WorktreeBranch = WipBranchName()
	m.WorkDir = filepath.Join(m.RalphDir, "worktrees", fmt.Sprintf("ralph-%s-%02d", today, runSeq))

	if err := os.MkdirAll(filepath.Join(m.RalphDir, "worktrees"), 0o755); err != nil {
		return fmt.Errorf("creating worktrees dir: %w", err)
	}

	m.gitCmd(m.ProjectDir, "worktree", "prune")

	if _, err := os.Stat(m.WorkDir); err == nil {
		os.RemoveAll(m.WorkDir)
	}

	// Clean up leftover wip branch from a previous run.
	if m.refExists(m.ProjectDir, m.WorktreeBranch) {
		if wt := m.findWorktreeForBranch(m.ProjectDir, m.WorktreeBranch); wt != "" && strings.Contains(wt, "/.ralph/worktrees/") {
			m.gitCmd(m.ProjectDir, "worktree", "remove", "--force", wt)
		}
		_ = m.gitCmdErr(m.ProjectDir, "branch", "-D", m.WorktreeBranch)
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
// Only restores the worktree path and state — no rebase or reset. The actual
// branch setup happens later in checkoutExistingBranch once the task is known
// and the correct base branch can be determined.
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
	}

	m.Logger.Log("git", "Resuming worktree: %s", m.WorkDir)
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
// changes, rebases onto origin, and reapplies the stash. If rebase fails
// after auto-resolve, it aborts and returns an error — the caller decides
// what to do (e.g. push anyway and let GitHub handle merge conflicts).
func (m *Manager) EnsureUpToDate(ctx context.Context) error {
	if m.WorkDir == "" || m.WorkDir == m.ProjectDir {
		return nil
	}

	// Rebase onto stack head if set, otherwise the default branch.
	baseBranch := m.detectDefaultBranch()
	if m.PrevBranch != "" {
		baseBranch = m.PrevBranch
	}

	if err := m.gitCmdErrCtx(ctx, m.WorkDir, "fetch", "origin", baseBranch); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if m.PrevBranch != "" {
			// Stack head branch missing from remote — likely merged and deleted.
			// Fall back to the default branch silently.
			m.SetPrevBranch("")
			baseBranch = m.detectDefaultBranch()
			if err2 := m.gitCmdErrCtx(ctx, m.WorkDir, "fetch", "origin", baseBranch); err2 != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				m.Logger.Warn("git", "Failed to fetch origin/%s: %v", baseBranch, err2)
				return nil
			}
		} else {
			m.Logger.Warn("git", "Failed to fetch origin/%s: %v", baseBranch, err)
			return nil
		}
	}

	if !m.refExists(m.WorkDir, "origin/"+baseBranch) {
		return nil
	}

	if m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", "origin/"+baseBranch, "HEAD") == nil {
		m.Logger.Log("git", "%s Already up to date with origin/%s", logging.BranchTag(baseBranch), baseBranch)
		return nil
	}

	// No local commits ahead of base → safe to force-reset (fresh start).
	localCommits := m.gitOutput(m.WorkDir, "rev-list", "--count", "origin/"+baseBranch+"..HEAD")
	if localCommits == "" || localCommits == "0" {
		m.Logger.Log("git", "%s Resetting to origin/%s (no local work)", logging.BranchTag(baseBranch), baseBranch)
		m.gitCmd(m.WorkDir, "reset", "--hard", "origin/"+baseBranch)
		return nil
	}

	// Local commits exist — try to rebase them onto latest base.
	var result error
	m.withStash("ralph-autostash", func() {
		if ctx.Err() != nil {
			result = ctx.Err()
			return
		}

		// 1. Fast-forward rebase
		if m.tryRebase(ctx, baseBranch) {
			return
		}

		// 2. Auto-resolve mechanical conflicts
		if m.tryAutoResolve(ctx, baseBranch) {
			return
		}

		// Stack diverges — abort rebase, keep local commits, let merge handle it.
		m.Logger.Warn("git", "Rebase conflict with local work — stack diverged, continuing")
		m.gitCmd(m.WorkDir, "rebase", "--abort")
		result = nil // not an error — diverged stack is expected
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


// RenameBranchForTask renames the current branch to include a task slug.
// Records the previous branch name for stacked PR targeting.
// Only renames once per task (tracked by BranchRenamed).
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

	newBranch := BranchName(taskID, slug)
	if err := m.gitCmdErr(m.WorkDir, "branch", "-m", m.WorktreeBranch, newBranch); err == nil {
		m.WorktreeBranch = newBranch
		m.BranchRenamed = true
		if m.State != nil {
			_ = m.State.Write("worktree_branch", m.WorktreeBranch)
			_ = m.State.Write("branch_renamed", "true")
		}
	}
}

// RenameBranchTo renames the current worktree branch to a specific name.
// Used when the bead already has a stored branch name from a previous run.
func (m *Manager) RenameBranchTo(name string) {
	if m.BranchRenamed || m.WorktreeBranch == "" || name == "" {
		return
	}
	if m.WorktreeBranch == name {
		m.BranchRenamed = true
		if m.State != nil {
			_ = m.State.Write("branch_renamed", "true")
		}
		return
	}
	if err := m.gitCmdErr(m.WorkDir, "branch", "-m", m.WorktreeBranch, name); err == nil {
		m.WorktreeBranch = name
		m.BranchRenamed = true
		if m.State != nil {
			_ = m.State.Write("worktree_branch", m.WorktreeBranch)
			_ = m.State.Write("branch_renamed", "true")
		}
	}
}

// ResetToDefaultBranch resets the worktree to origin's default branch.
// Used on resume when no stack exists — stale local commits are discarded.
// No-ops silently when the worktree is already at the target ref.
func (m *Manager) ResetToDefaultBranch() {
	defaultBranch := m.detectDefaultBranch()
	_ = m.gitCmdErr(m.WorkDir, "fetch", "origin", defaultBranch)
	target := "origin/" + defaultBranch
	if m.refExists(m.WorkDir, target) && m.gitOutput(m.WorkDir, "rev-parse", "HEAD") == m.gitOutput(m.WorkDir, "rev-parse", target) {
		return
	}
	m.gitCmd(m.WorkDir, "reset", "--hard", target)
	m.BranchRenamed = false
	if m.State != nil {
		_ = m.State.Write("branch_renamed", "false")
	}
	m.Logger.Log("git", "Reset worktree to %s", target)
}

// SetPrevBranch sets the previous branch for stacked PR targeting and
// persists it to state so it survives process restarts.
func (m *Manager) SetPrevBranch(branch string) {
	m.PrevBranch = branch
	if m.State != nil {
		_ = m.State.Write("prev_branch", branch)
	}
}

// PrepareForNextTask creates a fresh wip branch from HEAD so the next task
// gets its own branch. RenameBranchForTask will rename it to a task-specific
// name before the first commit.
func (m *Manager) PrepareForNextTask() {
	m.BranchRenamed = false
	if m.State != nil {
		_ = m.State.Write("branch_renamed", "false")
	}

	if m.WorkDir == m.ProjectDir || m.WorktreeBranch == "" {
		return
	}

	newBranch := WipBranchName()
	if m.WorktreeBranch == newBranch {
		return
	}
	if err := m.gitCmdErr(m.WorkDir, "checkout", "-B", newBranch); err == nil {
		m.WorktreeBranch = newBranch
		if m.State != nil {
			_ = m.State.Write("worktree_branch", newBranch)
		}
	}
}

// SquashToOneCommit squashes all commits since baseSHA into a single commit
// with the given message. No-op if there is already exactly one commit
// ahead of base. Returns an error if there are no commits to squash.
func (m *Manager) SquashToOneCommit(baseSHA, message string) error {
	countStr := m.gitOutput(m.WorkDir, "rev-list", "--count", baseSHA+"..HEAD")
	count := 0
	if countStr != "" {
		fmt.Sscanf(countStr, "%d", &count)
	}
	if count == 0 {
		return fmt.Errorf("no commits ahead of %s", baseSHA)
	}
	if count == 1 {
		return nil
	}
	m.Logger.Log("git", "Squashing %d commits into one", count)
	m.gitCmd(m.WorkDir, "reset", "--soft", baseSHA)
	return m.gitCmdErr(m.WorkDir, "commit", "-m", message)
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

// extractSeqSlug pulls the slug portion from a branch like "ralph/my-task"
// or legacy "ralph/project/01-my-task".
func extractSeqSlug(branch string) string {
	stripped := strings.TrimPrefix(branch, branchPrefix)
	if stripped == branch || stripped == "" {
		return ""
	}
	// Handle legacy format with extra path segment.
	if idx := strings.LastIndex(stripped, "/"); idx >= 0 {
		stripped = stripped[idx+1:]
	}
	if stripped == "next" || stripped == "wip" || stripped == "" {
		return ""
	}
	return stripped
}
