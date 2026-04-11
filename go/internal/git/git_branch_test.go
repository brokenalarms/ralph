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
	gh := &StubGitHub{IsAvailable: true, OpenPRBranches: nil}
	m := &Repo{
		logger: log,
		github: gh,
	}

	setStackHead(m, []string{"ralph/some-task"})

	if m.PrevBranch != "" {
		t.Errorf("PrevBranch should be empty when no open PR branches, got %q", m.PrevBranch)
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
	m := &Repo{logger: log}

	setStackHead(m, nil)

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

	m := &Repo{
		ProjectDir:     "/project",
		WorkDir:        "/project/worktrees/wt1",
		WorktreeBranch: "ralph/wip-branch",
		Runner:         runner,
		logger:         logging.New(nil),
	}

	checkedOut, err := checkoutExistingBranch(m, BranchTaskMeta{}, "ralph-xyz", "Fix login")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if checkedOut {
		t.Error("expected checkedOut=false when no stored branch, got true")
	}
	if m.WorktreeBranch == "ralph/wip-branch" {
		t.Error("expected branch to be renamed, got original name")
	}
}

// checkoutExistingBranch returns an error when rename fails, preventing the
// iteration from proceeding on a placeholder branch.
func TestCheckoutExistingBranch_RenameFailure_ReturnsError(t *testing.T) {
	renameErr := fmt.Errorf("git branch -m: fatal: branch already exists")
	runner := newStubRunner()
	runner.On("branch", "", renameErr)

	m := &Repo{
		ProjectDir:     "/project",
		WorkDir:        "/project/worktrees/wt1",
		WorktreeBranch: "ralph/next",
		Runner:         runner,
		logger:         logging.New(nil),
	}

	_, err := checkoutExistingBranch(m, BranchTaskMeta{}, "ralph-xyz", "Fix login")
	if err == nil {
		t.Fatal("expected error when rename fails, got nil")
	}
	if m.BranchRenamed {
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

	m := &Repo{
		ProjectDir:     "/project",
		WorkDir:        "/project/worktrees/wt1",
		WorktreeBranch: "ralph/wip-branch",
		Runner:         runner,
		logger:         logging.New(nil),
		github:         &StubGitHub{},
	}

	branch, err := m.BranchForTask(context.Background(), "ralph-abc", "My task", BranchTaskMeta{
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
