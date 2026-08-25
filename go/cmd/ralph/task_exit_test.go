package main

import (
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
)

const exitTestBranch = "ralph/task/20260701-01"
const exitTestWorkDir = "/tmp/ralph/.ralph/worktrees/ralph-task-20260701-01"
const exitTestSessionID = "12345678-1234-4567-89ab-123456789abc"

// Proves: a `ralph task` session that ends without leaving anything behind —
// no uncommitted edits, no commits ahead of origin/main — has its worktree
// removed automatically, with no question asked. Every such session used to
// leak its worktree because the old prompt defaulted to Keep on an empty or
// EOF stdin, which is what a closed pane, a Ctrl-C or a killed tmux produces.
func TestCleanupTaskWorktree_RemovesWorktreeWithNothingToLose(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{WorktreeBranch: exitTestBranch})
	var out strings.Builder

	cleanupTaskWorktree(&out, gm, exitTestWorkDir, exitTestBranch, exitTestSessionID)

	inspector := gm.(git.StubInspector)
	if got := inspector.GetRemoveWorktreeForBranchCalls(); got != 1 {
		t.Errorf("RemoveWorktreeForBranch calls = %d, want 1", got)
	}
	if got := inspector.GetRemovedWorktreeForBranch(); got != exitTestBranch {
		t.Errorf("removed branch = %q, want %q", got, exitTestBranch)
	}
}

// Proves: a worktree holding uncommitted edits survives the session and the
// user is told where it is, so hands-on work in progress is never destroyed
// by the automatic cleanup.
func TestCleanupTaskWorktree_KeepsDirtyWorktreeAndPrintsPath(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{WorktreeBranch: exitTestBranch, HasUncommitted: true})
	var out strings.Builder

	cleanupTaskWorktree(&out, gm, exitTestWorkDir, exitTestBranch, exitTestSessionID)

	if got := gm.(git.StubInspector).GetRemoveWorktreeForBranchCalls(); got != 0 {
		t.Errorf("RemoveWorktreeForBranch calls = %d, want 0 for a dirty worktree", got)
	}
	if !strings.Contains(out.String(), exitTestWorkDir) {
		t.Errorf("kept-worktree notice should name the worktree path %q, got:\n%s", exitTestWorkDir, out.String())
	}
	if !strings.Contains(out.String(), exitTestBranch) {
		t.Errorf("kept-worktree notice should name the branch %q, got:\n%s", exitTestBranch, out.String())
	}
}

// Proves: a worktree whose branch carries commits that are not on
// origin/main survives, so unpushed local commits are never discarded even
// when the working tree itself is clean.
func TestCleanupTaskWorktree_KeepsWorktreeWithCommitsAhead(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		WorktreeBranch:   exitTestBranch,
		LogOnelineResult: "abc1234 hands-on fix",
	})
	var out strings.Builder

	cleanupTaskWorktree(&out, gm, exitTestWorkDir, exitTestBranch, exitTestSessionID)

	if got := gm.(git.StubInspector).GetRemoveWorktreeForBranchCalls(); got != 0 {
		t.Errorf("RemoveWorktreeForBranch calls = %d, want 0 with commits ahead of origin/main", got)
	}
	if !strings.Contains(out.String(), exitTestWorkDir) {
		t.Errorf("kept-worktree notice should name the worktree path %q, got:\n%s", exitTestWorkDir, out.String())
	}
}

// Proves: the resume command is printed on every exit, whether the worktree
// was removed or kept — resuming no longer depends on the old worktree
// surviving, so the hint is always actionable.
func TestCleanupTaskWorktree_PrintsResumeHintInBothOutcomes(t *testing.T) {
	cases := []struct {
		name string
		cfg  git.StubRepoConfig
	}{
		{name: "removed", cfg: git.StubRepoConfig{WorktreeBranch: exitTestBranch}},
		{name: "kept", cfg: git.StubRepoConfig{WorktreeBranch: exitTestBranch, HasUncommitted: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder

			cleanupTaskWorktree(&out, git.NewStub(tc.cfg), exitTestWorkDir, exitTestBranch, exitTestSessionID)

			if !strings.Contains(out.String(), "ralph task --resume "+exitTestSessionID) {
				t.Errorf("exit output should carry the resume command, got:\n%s", out.String())
			}
		})
	}
}
