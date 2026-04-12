package git

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// ResumeTask returns an empty result for an empty task ID.
func TestResumeTask_EmptyTaskID(t *testing.T) {
	mgr := stubManager(t.TempDir(), nil, nil)
	result, err := mgr.ResumeTask(context.Background(), ResumeTaskMeta{}, ResumeTaskOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Handled {
		t.Error("expected Handled=false for empty task ID")
	}
}

// ResumeTask finds an existing PR via external-ref and delegates to Ship.
// When Ship reports AlreadyMerged, returns Handled=true with AlreadyMerged=true.
func TestResumeTask_AlreadyMergedViaPRURL(t *testing.T) {
	gh := NewStubGitHub()
	gh.PRState = PRStateMerged
	gh.PRNumber = 42
	mgr := stubManager(t.TempDir(), nil, gh)

	result, err := mgr.ResumeTask(context.Background(), ResumeTaskMeta{
		TaskID:      "ralph-abc",
		TaskTitle:   "fix something",
		ExternalRef: "https://github.com/owner/repo/pull/42",
	}, ResumeTaskOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Handled {
		t.Error("expected Handled=true for already-merged PR")
	}
	if !result.AlreadyMerged {
		t.Error("expected AlreadyMerged=true")
	}
	if result.PRNumber != 42 {
		t.Errorf("expected PRNumber=42, got %d", result.PRNumber)
	}
}

// ResumeTask handles a closed PR by clearing metadata and returning Handled=false.
func TestResumeTask_ClosedPRClearsMetadata(t *testing.T) {
	gh := NewStubGitHub()
	gh.PRState = PRStateClosed
	gh.PRNumber = 99
	mgr := stubManager(t.TempDir(), nil, gh)
	mgr.worktreeBranch = "ralph/next"
	mgr.branchRenamed = true // stale from previous run

	result, err := mgr.ResumeTask(context.Background(), ResumeTaskMeta{
		TaskID:      "ralph-cdr3",
		TaskTitle:   "Fix auth bug",
		ExternalRef: "https://github.com/owner/repo/pull/99",
	}, ResumeTaskOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Handled {
		t.Error("expected Handled=false for closed PR")
	}
	if !result.ClearMetadata {
		t.Error("expected ClearMetadata=true for closed PR")
	}
	if result.PRNumber != 99 {
		t.Errorf("expected PRNumber=99, got %d", result.PRNumber)
	}
}

// ResumeTask handles a closed PR: the branch is renamed to a task-specific name.
func TestResumeTask_ClosedPRRenamesBranch(t *testing.T) {
	gh := NewStubGitHub()
	gh.PRState = PRStateClosed
	gh.PRNumber = 439

	dir := t.TempDir()
	worktreeDir := t.TempDir()
	runner := newStubRunner()
	// branch -m succeeds (rename), branch -D succeeds
	runner.On("branch", "", nil)

	mgr := &Repo{
		projectDir:     dir,
		workDir:        worktreeDir, // distinct from ProjectDir so rename is allowed
		worktreeBranch: "ralph/next",
		branchRenamed:  true, // stale from previous run
		baseBranch:     "main",
		runner:         runner,
		github:         gh,
		state:          newMemState(),
		logger:         discardLog{},
	}

	result, err := mgr.ResumeTask(context.Background(), ResumeTaskMeta{
		TaskID:      "ralph-cdr3",
		TaskTitle:   "Fix auth bug",
		ExternalRef: "https://github.com/owner/repo/pull/439",
	}, ResumeTaskOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Handled {
		t.Fatal("expected Handled=false for closed PR")
	}

	// Branch must be renamed from ralph/next to a task-specific name.
	if mgr.worktreeBranch == "ralph/next" {
		t.Error("branch should be renamed from ralph/next")
	}
	if result.NewBranch == "" {
		t.Error("NewBranch should be set for closed PR case")
	}
}

// ResumeTask returns no result when neither external-ref nor branch metadata
// points to an existing PR, and the remote branch is absent.
func TestResumeTask_NoPriorWork(t *testing.T) {
	gh := NewStubGitHub()
	gh.FindPRErr = errors.New("not found")
	gh.PRNumber = 0
	runner := newStubRunner()
	// ls-remote returns nothing — remote branch absent.
	runner.On("ls-remote", "", nil)
	mgr := stubManager(t.TempDir(), runner, gh)

	result, err := mgr.ResumeTask(context.Background(), ResumeTaskMeta{
		TaskID:    "ralph-xyz",
		TaskTitle: "new work",
	}, ResumeTaskOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Handled {
		t.Error("expected Handled=false when no prior work")
	}
	if result.PRNumber != 0 {
		t.Errorf("expected PRNumber=0, got %d", result.PRNumber)
	}
}

// ResumeTask stores the external-ref URL when PR was found via branch (not external-ref).
func TestResumeTask_FoundViaBranchStoresPRURL(t *testing.T) {
	gh := NewStubGitHub()
	gh.PRState = PRStateMerged
	gh.PRNumber = 55

	runner := newStubRunner()
	// remote get-url origin returns a valid GitHub URL so buildPRURL can construct the link.
	runner.On("remote", "https://github.com/owner/repo.git", nil)

	mgr := stubManager(t.TempDir(), runner, gh)

	result, err := mgr.ResumeTask(context.Background(), ResumeTaskMeta{
		TaskID:      "ralph-mm1",
		TaskTitle:   "add feature",
		Branch:      "ralph/ralph-mm1-add-feature",
		ExternalRef: "", // no prior external-ref — discovered via branch
	}, ResumeTaskOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Handled {
		t.Error("expected Handled=true for already-merged PR")
	}
	if result.PRURLToStore == "" {
		t.Error("expected PRURLToStore to be set when found via branch")
	}
	expected := fmt.Sprintf("https://github.com/owner/repo/pull/%d", result.PRNumber)
	if result.PRURLToStore != expected {
		t.Errorf("PRURLToStore = %q, want %q", result.PRURLToStore, expected)
	}
}
