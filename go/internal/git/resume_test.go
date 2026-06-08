package git

import (
	"context"
	"fmt"
	"testing"
)

// ResumeTask returns an empty result for an empty task ID.
func TestResumeTask_EmptyTaskID(t *testing.T) {
	repo := newRepoForTest(Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}}, nil)
	result, err := repo.ResumeTask(context.Background(), ResumeTaskMeta{}, ResumeTaskOpts{})
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
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs:       []StubPR{{Number: 42, State: PRStateMerged}},
	})
	repo := newRepoForTest(Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}}, gh)

	result, err := repo.ResumeTask(context.Background(), ResumeTaskMeta{
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
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs:       []StubPR{{Number: 99, State: PRStateClosed}},
	})
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
		withWorktreeBranch("ralph/next"),
		withBranchRenamed(true), // stale from previous run
	)

	result, err := repo.ResumeTask(context.Background(), ResumeTaskMeta{
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
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs:       []StubPR{{Number: 439, State: PRStateClosed}},
	})

	dir := t.TempDir()
	worktreeDir := t.TempDir()
	runner := newStubRunner()
	// branch -m succeeds (rename), branch -D succeeds
	runner.On("branch", "", nil)

	repo := newRepoForTest(
		Config{ProjectDir: dir, WorkDir: worktreeDir, BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/next"),
		withBranchRenamed(true), // stale from previous run
	)

	result, err := repo.ResumeTask(context.Background(), ResumeTaskMeta{
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
	if repo.worktreeBranch == "ralph/next" {
		t.Error("branch should be renamed from ralph/next")
	}
	if result.NewBranch == "" {
		t.Error("NewBranch should be set for closed PR case")
	}
}

// ResumeTask returns no result when neither external-ref nor branch metadata
// points to an existing PR, and the remote branch is absent.
func TestResumeTask_NoPriorWork(t *testing.T) {
	// Empty world → FindPR returns zero values → no PR found.
	gh := newStubGitHub(StubGitHubConfig{Available: true})
	runner := newStubRunner()
	// ls-remote returns nothing — remote branch absent.
	runner.On("ls-remote", "", nil)
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
	)

	result, err := repo.ResumeTask(context.Background(), ResumeTaskMeta{
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

// ResumeTask sets ShipFailedAfterPush when it detects a remote branch with pushed
// commits but no existing PR, calls Ship to create one, and Ship returns an error.
// The loop must halt rather than re-invoking the agent on already-pushed work.
func TestResumeTask_ShipFailsAfterPushedCommits(t *testing.T) {
	createErr := fmt.Errorf("pr creation failed: authentication required")
	gh := newStubGitHub(StubGitHubConfig{
		Available:         true,
		CreatePRErr:       createErr,
		CreatePRViaAPIErr: fmt.Errorf("api fallback failed"),
	})

	runner := newStubRunner()
	// rev-parse --verify: refExists → ref exists (returns non-error)
	runner.On("rev-parse", "", nil)
	// rev-list --count: RemoteBranchHasCommits and BranchHasUnmergedWork → "1" (has commits)
	runner.On("rev-list", "1", nil)
	// merge-base --is-ancestor: RemoteBranchIsOnMain and BranchIsAheadOfMain → success
	runner.On("merge-base", "", nil)
	// remote get-url origin: RemoteURL → GitHub URL so CreatePR is attempted
	runner.On("remote", "https://github.com/owner/repo.git", nil)

	// WorkDir == ProjectDir so r.Push returns nil early (no real push operations).
	dir := t.TempDir()
	repo := newRepoForTest(
		Config{ProjectDir: dir, WorkDir: dir, BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/ralph-abc-fix-it"),
	)

	result, err := repo.ResumeTask(context.Background(), ResumeTaskMeta{
		TaskID:    "ralph-abc",
		TaskTitle: "fix it",
		Branch:    "ralph/ralph-abc-fix-it",
	}, ResumeTaskOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.ShipFailedAfterPush {
		t.Error("expected ShipFailedAfterPush=true when Ship fails after detecting pushed commits")
	}
	if result.Handled {
		t.Error("expected Handled=false when ShipFailedAfterPush is true")
	}
	if result.ShipErr == nil {
		t.Error("expected ShipErr to be set when Ship returns an error")
	}
}

// ResumeTask stores the external-ref URL when PR was found via branch (not external-ref).
func TestResumeTask_FoundViaBranchStoresPRURL(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 55,
			Branch: "ralph/ralph-mm1-add-feature",
			State:  PRStateMerged,
		}},
	})

	runner := newStubRunner()
	// remote get-url origin returns a valid GitHub URL so buildPRURL can construct the link.
	runner.On("remote", "https://github.com/owner/repo.git", nil)

	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
	)

	result, err := repo.ResumeTask(context.Background(), ResumeTaskMeta{
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

// ResumeTask is discovery only: an open PR is reported via ShipExisting (not
// Handled), so the loop routes it through shipAndFinalize → doShip, where CI is
// awaited, the fix agent runs on failure, and the merge happens in one retry
// loop. resolveByState itself no longer inspects CI or merges — that decision
// lives in exactly one place. Crucially it must NOT mark an open PR Handled
// (which would close the bead as "merge pending" and advance dependent tasks on
// top of unmerged work).
func TestResumeTask_OpenPR_RoutesToShipFinalize(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 970,
			Branch: "ralph/ralph-cif-fix",
			Title:  "fix it",
			State:  PRStateOpen,
		}},
	})

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/owner/repo.git", nil)
	runner.On("fetch", "", nil)
	// Not an ancestor of main (exit 1) — i.e. not already merged, so the chain
	// is healthy and the open PR is shippable.
	runner.On("merge-base --is-ancestor", "", fmt.Errorf("exit status 1"))
	runner.On("rev-list --count", "1", nil)

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/ralph-cif-fix"),
	)

	result, err := repo.ResumeTask(context.Background(), ResumeTaskMeta{
		TaskID:      "ralph-cif",
		TaskTitle:   "fix it",
		Branch:      "ralph/ralph-cif-fix",
		ExternalRef: "https://github.com/owner/repo/pull/970",
	}, ResumeTaskOpts{AutoMerge: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Handled {
		t.Error("expected Handled=false for an open PR — resolveByState must not close/merge; the loop ships it via shipAndFinalize")
	}
	if !result.ShipExisting {
		t.Error("expected ShipExisting=true for an open PR so the loop routes it through shipAndFinalize")
	}
	if result.Merged {
		t.Error("expected Merged=false — resolveByState is discovery only and does not merge")
	}
	if result.PRNumber != 970 {
		t.Errorf("expected PRNumber=970, got %d", result.PRNumber)
	}
}
