package git

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/logging"
)

// setStackHead leaves prevBranch empty when the top completed branch has no
// open PR. The default stubGitHub has no PRs configured, so ListOpenPRBranches
// returns an empty slice and the branch is rejected.
func TestSetStackHead_SkipsWhenNoOpenPR(t *testing.T) {
	log := &testLog{}
	// Default stub GitHub has no PRs → ListOpenPRBranches returns [].
	r := newRepoForTest(Config{Logger: log}, nil)

	setStackHead(context.Background(), r, []string{"ralph/some-task"})

	if r.prevBranch != "" {
		t.Errorf("prevBranch should be empty when top branch has no open PR, got %q", r.prevBranch)
	}
	for _, msg := range log.messages {
		if strings.Contains(msg, "Stack head") {
			t.Errorf("should not log 'Stack head' when branch has no open PR, got: %s", msg)
		}
	}
}

// setStackHead does NOT log "No stacked parents" when completedBranches is
// empty — the early-return path is silent.
func TestSetStackHead_SilentWhenNoCompletedBranches(t *testing.T) {
	log := &testLog{}
	r := newRepoForTest(Config{Logger: log}, nil)

	setStackHead(context.Background(), r, nil)

	for _, msg := range log.messages {
		if strings.Contains(msg, "No stacked parents") {
			t.Errorf("should not log 'No stacked parents' when completedBranches is empty, got: %s", msg)
		}
	}
}

// setStackHead returns prevBranch='' when the completed stack's newest branch
// has no open PR — the squash-merged scenario where every PR landed and the
// open PR list is empty or disjoint from the top branch.
func TestSetStackHead_AllMergedStack_PrevBranchEmpty(t *testing.T) {
	log := &testLog{}
	// No open PRs: the top branch's PR was squash-merged to main.
	gh := newStubGitHub(StubGitHubConfig{Available: true})
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	r := newRepoForTest(Config{Logger: log}, gh, withRunner(runner))

	setStackHead(context.Background(), r, []string{"ralph/task-a", "ralph/task-b"})

	if r.prevBranch != "" {
		t.Errorf("prevBranch should be empty when all PRs are merged, got %q", r.prevBranch)
	}
}

// setStackHead returns prevBranch = top when the top completed branch has an
// open PR and is cleanly ahead of main. This covers the ralph merge
// --update-refs path: a mid-stack PR merges, the higher branches are rebased
// onto the new main, and the top PR is still open and ahead of main.
func TestSetStackHead_TopOpenAndAheadOfMain_Selected(t *testing.T) {
	log := &testLog{}
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{
			{Number: 10, Branch: "ralph/task-b"}, // open PR for top branch
		},
	})
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	// BranchIsAheadOfMain: merge-base --is-ancestor origin/main origin/task-b returns nil → true.
	runner.On("merge-base", "", nil)

	r := newRepoForTest(Config{Logger: log}, gh, withRunner(runner))

	setStackHead(context.Background(), r, []string{"ralph/task-a", "ralph/task-b"})

	if r.prevBranch != "ralph/task-b" {
		t.Errorf("expected prevBranch=ralph/task-b, got %q", r.prevBranch)
	}
	found := false
	for _, msg := range log.messages {
		if strings.Contains(msg, "Stack head: ralph/task-b") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'Stack head: ralph/task-b' log, got: %v", log.messages)
	}
}

// setStackHead returns prevBranch='' when the top PR was closed (merged
// out-of-order via GitHub UI or abandoned) even if older PRs are still open.
// Only the top branch is examined — orphaned lower PRs are not revived.
func TestSetStackHead_TopPRClosed_PrevBranchEmpty(t *testing.T) {
	log := &testLog{}
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{
			{Number: 10, Branch: "ralph/task-a"}, // lower branch still open
			// ralph/task-b (top) has no open PR
		},
	})
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	r := newRepoForTest(Config{Logger: log}, gh, withRunner(runner))

	setStackHead(context.Background(), r, []string{"ralph/task-a", "ralph/task-b"})

	if r.prevBranch != "" {
		t.Errorf("prevBranch should be empty when top PR is closed, got %q", r.prevBranch)
	}
}

// setStackHead returns prevBranch='' when the top branch has an open PR but
// BranchIsAheadOfMain returns false. This catches the squash-merged + locally
// stale case: the branch diverged from main after the squash, so it is not a
// clean ancestor.
func TestSetStackHead_OpenPRButNotAheadOfMain_PrevBranchEmpty(t *testing.T) {
	log := &testLog{}
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{
			{Number: 10, Branch: "ralph/task-a"}, // open PR
		},
	})
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	// BranchIsAheadOfMain: merge-base --is-ancestor returns error → false.
	runner.On("merge-base", "", fmt.Errorf("not an ancestor"))

	r := newRepoForTest(Config{Logger: log}, gh, withRunner(runner))

	setStackHead(context.Background(), r, []string{"ralph/task-a"})

	if r.prevBranch != "" {
		t.Errorf("prevBranch should be empty when branch is not ahead of main, got %q", r.prevBranch)
	}
	for _, msg := range log.messages {
		if strings.Contains(msg, "Stack head") {
			t.Errorf("should not log 'Stack head' when branch is not ahead of main, got: %s", msg)
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

// countingGitHub wraps a gitHub stub and counts ListOpenPRBranches calls.
type countingGitHub struct {
	gitHub
	listOpenPRBranchesCalls int
}

func (c *countingGitHub) ListOpenPRBranches(ctx context.Context, repoURL string) ([]string, error) {
	c.listOpenPRBranchesCalls++
	return c.gitHub.ListOpenPRBranches(ctx, repoURL)
}

// SyncWorktreeBase followed by BranchForTask on the first iteration must not
// call setStackHead twice. On first task, BranchForTask skips setStackHead
// because SyncWorktreeBase already ran it; on second task, it runs normally.
func TestBranchForTask_SkipsSetStackHeadAfterSyncWorktreeBase(t *testing.T) {
	log := &testLog{}
	gh := &countingGitHub{gitHub: newStubGitHub(StubGitHubConfig{Available: true})}
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("branch", "", nil)
	runner.On("rev-list", "", nil)

	r := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/worktrees/wt1", Logger: log},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/wip"),
	)

	// SyncWorktreeBase calls setStackHead once (ListOpenPRBranches count → 1).
	_ = r.SyncWorktreeBase(context.Background(), []string{"ralph/task-a"})
	afterSync := gh.listOpenPRBranchesCalls

	// First BranchForTask must skip setStackHead (count stays at 1).
	_, _ = r.BranchForTask(context.Background(), "ralph-abc", "Task A", BranchTaskMeta{
		Branch:            "ralph/ralph-abc-task-a",
		CompletedBranches: []string{"ralph/task-a"},
	})
	afterFirstTask := gh.listOpenPRBranchesCalls

	// Second BranchForTask must call setStackHead again (count increments).
	r.worktreeBranch = "ralph/wip"
	r.branchRenamed = false
	_, _ = r.BranchForTask(context.Background(), "ralph-def", "Task B", BranchTaskMeta{
		Branch:            "ralph/ralph-def-task-b",
		CompletedBranches: []string{"ralph/task-a"},
	})
	afterSecondTask := gh.listOpenPRBranchesCalls

	if afterSync != 1 {
		t.Errorf("SyncWorktreeBase should call ListOpenPRBranches once, got %d", afterSync)
	}
	if afterFirstTask != afterSync {
		t.Errorf("first BranchForTask should skip setStackHead (count unchanged at %d), got %d", afterSync, afterFirstTask)
	}
	if afterSecondTask != afterFirstTask+1 {
		t.Errorf("second BranchForTask should call setStackHead (count %d+1), got %d", afterFirstTask, afterSecondTask)
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
