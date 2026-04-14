package git

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/logging"
)

// setStackHead falls through silently when GitHub reports no open PR branches —
// PrevBranch stays empty.
func TestSetStackHead_SkipsWhenNoBranchesAvailable(t *testing.T) {
	log := &testLog{}
	gh := newStubGitHub(StubGitHubConfig{Available: true})
	r := newRepoForTest(Config{Logger: log}, gh)

	setStackHead(r, []string{"ralph/some-task"})

	if r.prevBranch != "" {
		t.Errorf("PrevBranch should be empty when no open PR branches, got %q", r.prevBranch)
	}
	for _, msg := range log.messages {
		if strings.Contains(msg, "Stack head") {
			t.Errorf("should not log 'Stack head' when no open PR branches, got: %s", msg)
		}
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
