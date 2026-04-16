package git

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/logging"
)

// setStackHead leaves prevBranch empty when the candidate branch has no
// remote commits (RemoteBranchHasCommits returns false). The open-PR check
// was removed so that pr_creation_failed branches (pushed but no PR) are
// also eligible; the remote-commits guard still prevents selecting stale
// candidates.
func TestSetStackHead_SkipsWhenRemoteBranchHasNoCommits(t *testing.T) {
	log := &testLog{}
	r := newRepoForTest(Config{Logger: log}, nil)
	// Default stub runner returns "" for rev-list → RemoteBranchHasCommits false.

	setStackHead(r, []string{"ralph/some-task"})

	if r.prevBranch != "" {
		t.Errorf("prevBranch should be empty when remote branch has no commits, got %q", r.prevBranch)
	}
	for _, msg := range log.messages {
		if strings.Contains(msg, "Stack head") {
			t.Errorf("should not log 'Stack head' when remote branch has no commits, got: %s", msg)
		}
	}
}

// setStackHead selects a branch that has commits ahead of main even when
// that branch has no open PR — this is the pr_creation_failed scenario where
// the push succeeded but CreatePR errored. The fix removes the open-PR gate
// and relies solely on RemoteBranchHasCommits + BranchIsAheadOfMain.
func TestSetStackHead_SelectsPushedNoPRBranch(t *testing.T) {
	log := &testLog{}
	runner := newStubRunner()
	// FetchBranch succeeds (default no-op).
	// RemoteBranchHasCommits: rev-list returns "1" → true.
	runner.On("rev-list", "1", nil)
	// BranchIsAheadOfMain: merge-base succeeds (exit 0) → true.
	runner.On("merge-base", "", nil)

	r := newRepoForTest(Config{Logger: log}, nil, withRunner(runner))

	setStackHead(r, []string{"ralph/task-a"})

	if r.prevBranch != "ralph/task-a" {
		t.Errorf("expected prevBranch=ralph/task-a (pushed with no PR), got %q", r.prevBranch)
	}
	found := false
	for _, msg := range log.messages {
		if strings.Contains(msg, "Stack head: ralph/task-a") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'Stack head: ralph/task-a' log, got: %v", log.messages)
	}
}

// setStackHead selects a branch in a diverged state — branch has commits not
// on main AND main has commits not on branch. This simulates a pre-push
// rebase failure where the branch was pushed without resolving the diverged
// state. BranchIsAheadOfMain returns false for diverged branches (the
// pre-fix predicate skipped them), but BranchHasUnmergedWork returns true
// because the branch still has real unique commits. Such branches are valid
// stack parents — the next task chains onto them, and the merge pipeline
// re-aligns the chain via --update-refs.
//
// Regression bead: ralph-op9h. Pre-fix, this test failed with prevBranch==""
// and a "No stacked parents" log entry.
func TestSetStackHead_SelectsDivergedBranchWithUnmergedWork(t *testing.T) {
	log := &testLog{}
	runner := newStubRunner()
	// FetchBranch succeeds (default no-op).
	// RemoteBranchHasCommits: rev-list returns "1" → true.
	// BranchHasUnmergedWork: rev-list --count also returns "1" → true.
	runner.On("rev-list", "1", nil)
	// BranchIsAheadOfMain: merge-base --is-ancestor returns error → false
	// (diverged: main is NOT ancestor of branch). Pre-fix this caused the
	// branch to be skipped; post-fix this predicate is no longer consulted.
	runner.On("merge-base", "", fmt.Errorf("not an ancestor"))

	r := newRepoForTest(Config{Logger: log}, nil, withRunner(runner))

	setStackHead(r, []string{"ralph/diverged-task"})

	if r.prevBranch != "ralph/diverged-task" {
		t.Errorf("expected prevBranch=ralph/diverged-task (diverged with unmerged work), got %q", r.prevBranch)
	}
	foundStackHead := false
	for _, msg := range log.messages {
		if strings.Contains(msg, "Stack head: ralph/diverged-task") {
			foundStackHead = true
		}
		if strings.Contains(msg, "No stacked parents") {
			t.Errorf("should not log 'No stacked parents' when branch has unmerged work, got: %s", msg)
		}
	}
	if !foundStackHead {
		t.Errorf("expected 'Stack head: ralph/diverged-task' log, got: %v", log.messages)
	}
}

// setStackHead does NOT log "No stacked parents" when completedBranches is
// empty — the early-return path is silent.
func TestSetStackHead_SilentWhenNoCompletedBranches(t *testing.T) {
	log := &testLog{}
	r := newRepoForTest(Config{Logger: log}, nil)

	setStackHead(r, nil)

	for _, msg := range log.messages {
		if strings.Contains(msg, "No stacked parents") {
			t.Errorf("should not log 'No stacked parents' when completedBranches is empty, got: %s", msg)
		}
	}
}

// checkoutExistingBranch renames the branch to a task-based name when no
// stored branch exists in meta.
func TestCheckoutExistingBranch_NoStoredBranch_RenamesBranch(t *testing.T) {
	runner := newStubRunner()
	runner.On("branch", "", nil)

	r := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/worktrees/wt1", Logger: logging.New(nil)},
		nil,
		withRunner(runner),
		withWorktreeBranch("ralph/wip-branch"),
	)

	checkedOut, err := checkoutExistingBranch(r, BranchTaskMeta{}, "ralph-xyz", "Fix login")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if checkedOut {
		t.Error("expected checkedOut=false when no stored branch, got true")
	}
	if r.worktreeBranch == "ralph/wip-branch" {
		t.Error("expected branch to be renamed, got original name")
	}
}

// checkoutExistingBranch returns an error when rename fails, preventing the
// iteration from proceeding on a placeholder branch.
func TestCheckoutExistingBranch_RenameFailure_ReturnsError(t *testing.T) {
	renameErr := fmt.Errorf("git branch -m: fatal: branch already exists")
	runner := newStubRunner()
	runner.On("branch", "", renameErr)

	r := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/worktrees/wt1", Logger: logging.New(nil)},
		nil,
		withRunner(runner),
		withWorktreeBranch("ralph/next"),
	)

	_, err := checkoutExistingBranch(r, BranchTaskMeta{}, "ralph-xyz", "Fix login")
	if err == nil {
		t.Fatal("expected error when rename fails, got nil")
	}
	if r.branchRenamed {
		t.Error("BranchRenamed should remain false after rename failure")
	}
}

// BranchForTask uses the stored branch from meta when one exists, renaming
// the worktree branch to match without fetching from remote.
func TestBranchForTask_UsesStoredBranchWhenRemoteEmpty(t *testing.T) {
	runner := newStubRunner()
	runner.On("fetch", "", nil)
	runner.On("branch", "", nil)
	// rev-list returns "" (no commits ahead) — RemoteBranchHasCommits returns false
	runner.On("rev-list", "", nil)

	r := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/worktrees/wt1", Logger: logging.New(nil)},
		newStubGitHub(StubGitHubConfig{}),
		withRunner(runner),
		withWorktreeBranch("ralph/wip-branch"),
	)

	branch, err := r.BranchForTask(context.Background(), "ralph-abc", "My task", BranchTaskMeta{
		Branch: "ralph/ralph-abc-my-task",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// When remote has no commits, RenameBranchTo is called with the stored name.
	if branch != "ralph/ralph-abc-my-task" {
		t.Errorf("expected stored branch %q, got %q", "ralph/ralph-abc-my-task", branch)
	}
}
