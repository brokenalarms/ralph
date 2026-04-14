package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubCISleep replaces ciSleep with a no-op for tests that use stub runners
// (which don't have real CI). Returns a cleanup function to restore the original.
func stubCISleep(t *testing.T) {
	t.Helper()
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	t.Cleanup(func() { ciSleep = origSleep })
}

// Push rebases onto a diverged origin/main before pushing, so the remote
// branch includes the latest base branch changes even when main has moved.
func TestPush_RebasesWhenOriginMainDiverged(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)

	// Create a worktree branch with a commit.
	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test-rebase-push", wtDir)
	run(t, "git", "-C", wtDir, "config", "user.name", "test")
	run(t, "git", "-C", wtDir, "config", "user.email", "test@test")
	writeFile(t, wtDir, "feature.txt", "feature content\n")
	run(t, "git", "-C", wtDir, "commit", "-m", "feature work")

	// Push the branch so it has a tracking ref.
	run(t, "git", "-C", wtDir, "push", "-u", "origin", "ralph/test-rebase-push")

	// Advance origin/main via a separate clone (simulating another merge landing).
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	run(t, "git", "-C", tmpClone, "config", "user.name", "test")
	run(t, "git", "-C", tmpClone, "config", "user.email", "test@test")
	writeFile(t, tmpClone, "main-update.txt", "main moved ahead\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "main advances")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	mainSHA := strings.TrimSpace(cmdOutput(t, "git", "-C", tmpClone, "rev-parse", "HEAD"))

	repo := newRepoForTest(
		Config{ProjectDir: project, WorkDir: wtDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
		withWorktreeBranch("ralph/test-rebase-push"),
	)

	if err := repo.Push(context.Background()); err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	// The pushed commit should be based on the new main (mainSHA is ancestor).
	if gitCmdErr(wtDir, "merge-base", "--is-ancestor", mainSHA, "HEAD") != nil {
		t.Error("after push, HEAD should include the latest main changes (main SHA should be ancestor)")
	}

	// Remote branch should have exactly 1 commit above main.
	countAfter := strings.TrimSpace(cmdOutput(t, "git", "-C", bare, "rev-list", "--count", "main..ralph/test-rebase-push"))
	if countAfter != "1" {
		t.Errorf("expected 1 commit on remote after rebase+squash+push, got %s", countAfter)
	}

	// The feature file should still be present.
	out := strings.TrimSpace(cmdOutput(t, "git", "-C", bare, "show", "ralph/test-rebase-push:feature.txt"))
	if out != "feature content" {
		t.Errorf("feature.txt content = %q, want %q", out, "feature content")
	}
}

// AutoMergeCurrentBranch returns MergeConflictError when main moves while CI
// is running, so MergeWithRetry loops back through rebase+push+CI instead of
// merging stale code onto an updated base.
func TestAutoMerge_MainMovedWhileCIRunning_ReturnsMergeConflictError(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	// Sequence: EnsureUpToDate succeeds (up to date), then branchNeedsUpdate
	// detects main moved (not ancestor) → MergeConflictError.
	// No plain "rev-parse" stub → Push skips merge-base (baseSHA="").
	runner.OnSequence("merge-base --is-ancestor", []stubResponse{
		{"", nil},                        // EnsureUpToDate: already up to date
		{"", errors.New("not ancestor")}, // branchNeedsUpdate: main moved
	})
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("rev-parse HEAD", "abc123", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 42,
			Branch: "ralph/test/01-main-moved",
			Title:  "some PR",
			State:  PRStateOpen,
		}},
		Checks: map[int][]CICheckResult{42: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}}},
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-main-moved"),
	)

	_, err := repo.AutoMergeCurrentBranch(context.Background())
	if err == nil {
		t.Fatal("expected error when main moved while CI was running")
	}

	var conflictErr *MergeConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected MergeConflictError, got %T: %v", err, err)
	}
	if conflictErr.PRNumber != 42 {
		t.Errorf("expected PRNumber=42, got %d", conflictErr.PRNumber)
	}

	// Verify merge did NOT happen — PR remains open in the world.
	pr, _ := gh.GetPR("", 42)
	if pr == nil || pr.State != PRStateOpen {
		t.Errorf("expected PR 42 to remain open after conflict, got state=%v", pr)
	}
}

// A PR marked Blocked causes executeMerge to wait for CI and retry,
// not to treat the response as a content conflict.
func TestExecuteMerge_NotMergeableClassifiedAsBlocked(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("rev-parse HEAD", "abc123", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:  88,
			Branch:  "ralph/test/01-not-mergeable",
			Title:   "some PR",
			State:   PRStateOpen,
			Blocked: true, // world: branch protection blocks merge
		}},
		Checks: map[int][]CICheckResult{88: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}}},
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-not-mergeable"),
	)

	_, err := repo.AutoMergeCurrentBranch(context.Background())
	if err == nil {
		t.Fatal("expected error for non-mergeable PR")
	}

	// With structured MergeResult, Blocked triggers CI-gated path (wait + retry).
	// The retry also returns Blocked, so we get "merge retry failed."
	if !strings.Contains(err.Error(), "merge retry failed") {
		t.Fatalf("expected merge retry failure for blocked PR, got: %v", err)
	}
}

// Ship package function creates a PR via gh interface without accessing Manager.
// Proves AC #1: Ship(ctx, runner, gh, workDir, branch, remoteURL, opts) is a
// callable package function independent of Manager.
func TestShip_PackageFunction_CreatesPR(t *testing.T) {
	// Preload an open PR for the branch so FindOpenPR returns it — shipPR's
	// CreatePR path finds the existing PR and returns its number.
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 77,
			Branch: "ralph/gmxa-ship",
			Title:  "fix: ship as package fn",
			State:  PRStateOpen,
		}},
	})

	pushedCalled := false
	opts := ShipOpts{
		TaskID:     "ralph-gmxa",
		TaskTitle:  "ship as package fn",
		Summary:    "extracted ship body",
		BaseBranch: "main",
	}
	infra := shipInfra{
		push:           func(ctx context.Context) error { pushedCalled = true; return nil },
		hasUncommitted: func() bool { return false },
		commitAll:      func(string) {},
		logger:         discardLog{},
	}

	result, err := shipPR(context.Background(), nil, gh, "/wt", "ralph/gmxa-ship", "https://github.com/test/repo.git", opts, infra)
	if err != nil {
		t.Fatalf("shipPR failed: %v", err)
	}
	if !pushedCalled {
		t.Error("expected push to be called")
	}
	if result.PRNumber != 77 {
		t.Errorf("PRNumber = %d, want %d", result.PRNumber, 77)
	}
}

// shipPR skips CreatePR and returns an empty result when branchAheadOfMain
// reports the branch is not ahead of main after pushing — prevents spurious
// 422 API errors on empty/already-merged branches.
func TestShip_SkipsCreatePRWhenNotAheadOfMain(t *testing.T) {
	// Empty world: if CreatePR were called, a PR would appear. We verify it
	// wasn't by checking ListAllPRs is empty after shipPR returns.
	gh := newStubGitHub(StubGitHubConfig{Available: true})

	opts := ShipOpts{}
	infra := shipInfra{
		push:              func(ctx context.Context) error { return nil },
		hasUncommitted:    func() bool { return false },
		commitAll:         func(string) {},
		branchAheadOfMain: func(string) bool { return false },
		logger:            discardLog{},
	}

	result, err := shipPR(context.Background(), nil, gh, "/wt", "ralph/test-branch", "", opts, infra)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prs, _ := gh.ListAllPRs("")
	if len(prs) != 0 {
		t.Errorf("CreatePR must not be called when branch is not ahead of main; world has PRs: %+v", prs)
	}
	if result.PRNumber != 0 {
		t.Errorf("PRNumber = %d, want 0 (no-op)", result.PRNumber)
	}
}

// AutoMergeCurrentBranch returns CIFailureError when CI fails due to
// infrastructure (zero steps executed). Branch protection is never bypassed —
// the loop closes the bead and leaves the PR open for CI to gate naturally.
func TestAutoMerge_InfraFailure_ReturnsCIFailureError(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("rev-parse HEAD", "abc123", nil)
	runner.On("reset --hard", "", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 99,
			Branch: "ralph/test/01-infra-failure",
			Title:  "infra failure test",
			State:  PRStateOpen,
		}},
		Checks:       map[int][]CICheckResult{99: {{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
		JobStepCount: 0,
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-infra-failure"),
	)

	_, err := repo.AutoMergeCurrentBranch(context.Background())
	if err == nil {
		t.Fatal("expected CIFailureError for infra CI failure, got nil")
	}
	var ciErr *CIFailureError
	if !errors.As(err, &ciErr) {
		t.Fatalf("expected CIFailureError, got %T: %v", err, err)
	}
}

// AutoMergeCurrentBranch returns CIFailureError when CI fails regardless of
// whether local tests passed — branch protection is always respected.
func TestAutoMerge_CIFailure_AlwaysReturnsCIFailureError(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("rev-parse HEAD", "abc123", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 100,
			Branch: "ralph/test/01-ci-failure",
			Title:  "ci failure test",
			State:  PRStateOpen,
		}},
		Checks: map[int][]CICheckResult{100: {{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-ci-failure"),
	)

	_, err := repo.AutoMergeCurrentBranch(context.Background())
	if err == nil {
		t.Fatal("expected CIFailureError when CI fails")
	}

	var ciErr *CIFailureError
	if !errors.As(err, &ciErr) {
		t.Fatalf("expected CIFailureError, got %T: %v", err, err)
	}
}

// Ship sets ShipResult.InfrastructureFailure when CI fails with zero job steps
// executed (billing/runner allocation failure). The git module classifies the
// failure; the loop uses the field to decide whether to close the bead.
func TestShip_InfrastructureFailure_SetOnCIFailure(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("rev-parse HEAD", "abc123", nil)
	runner.On("reset --hard", "", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 99,
			Branch: "ralph/test/01-ship-infra",
			Base:   "main",
			State:  PRStateOpen,
		}},
		Checks:       map[int][]CICheckResult{99: {{Name: "ci", State: "FAILURE", Bucket: "fail", IsRequired: true}}},
		JobStepCount: 0, // zero steps = infrastructure failure
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-ship-infra"),
	)

	result, err := repo.Ship(context.Background(), ShipOpts{AutoMerge: true, PRNumber: 99})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.CIFailure {
		t.Error("expected CIFailure=true")
	}
	if !result.InfrastructureFailure {
		t.Error("expected InfrastructureFailure=true when GetJobStepCount returns 0")
	}
}

// Ship sets ShipResult.InfrastructureFailure=false when CI fails with job steps
// executed — actual test failures, not infrastructure issues.
func TestShip_InfrastructureFailure_FalseWhenJobStepsExecuted(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("rev-parse HEAD", "abc123", nil)
	runner.On("reset --hard", "", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 100,
			Branch: "ralph/test/01-ship-real-failure",
			Base:   "main",
			State:  PRStateOpen,
		}},
		Checks:       map[int][]CICheckResult{100: {{Name: "ci", State: "FAILURE", Bucket: "fail", IsRequired: true}}},
		JobStepCount: 5, // non-zero steps = actual test failure
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-ship-real-failure"),
	)

	result, err := repo.Ship(context.Background(), ShipOpts{AutoMerge: true, PRNumber: 100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.CIFailure {
		t.Error("expected CIFailure=true")
	}
	if result.InfrastructureFailure {
		t.Error("expected InfrastructureFailure=false when job steps were executed")
	}
}

// AutoMergeCurrentBranch returns an error when CI times out (does not complete
// within the poll timeout). The PR is left open unmerged for CI to gate naturally.
func TestAutoMerge_CITimeout_ReturnsErrorWithoutMerging(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("rev-parse HEAD", "abc123", nil)
	runner.On("reset --hard", "", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 102,
			Branch: "ralph/test/01-ci-timeout",
			Title:  "ci timeout test",
			State:  PRStateOpen,
		}},
		// ListChecksErr causes AwaitCI to exhaust its timeout returning an error.
		ListChecksErr: fmt.Errorf("API unavailable"),
	})

	repo := newRepoForTest(
		Config{
			ProjectDir:    "/project",
			WorkDir:       "/project/wt",
			BaseBranch:    "main",
			Logger:        &testLog{},
			CIPollTimeout: 50 * time.Millisecond,
		},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-ci-timeout"),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if merged {
		t.Error("expected merged=false when CI times out")
	}
	if err == nil {
		t.Fatal("expected error when CI times out (PR must be left open)")
	}
	// PR must not have been merged — still open in the world.
	pr, _ := gh.GetPR("", 102)
	if pr == nil || pr.State != PRStateOpen {
		t.Errorf("expected PR 102 to remain open after CI timeout, got state=%v", pr)
	}
}

// CreatePR targets PrevBranch (the parent in a stacked PR) instead of main,
// so GitHub shows the correct diff between the child and parent branch.
// Observed via the new PR's base in the fake's world.
func TestCreatePR_StackedPRTargetsParentBranch(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)

	gh := newStubGitHub(StubGitHubConfig{Available: true})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/02-child-task"),
		withPrevBranch("ralph/parent-task"),
	)

	prNum, err := repo.CreatePR(context.Background(), "ralph-abc", "child task description", "body")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if prNum == 0 {
		t.Fatal("expected non-empty PR number")
	}

	// The newly-created PR should target the parent branch, not main.
	pr, _ := gh.GetPR("", prNum)
	if pr == nil {
		t.Fatal("expected PR to exist in fake world")
	}
	if pr.BaseRef != "ralph/parent-task" {
		t.Errorf("CreatePR base = %q, want %q (should target parent branch, not main)", pr.BaseRef, "ralph/parent-task")
	}
	if pr.HeadRef != "ralph/test/02-child-task" {
		t.Errorf("CreatePR head = %q, want %q", pr.HeadRef, "ralph/test/02-child-task")
	}
}

// CreatePR targets main when PrevBranch is not set (non-stacked case).
func TestCreatePR_NonStackedTargetsMain(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)

	gh := newStubGitHub(StubGitHubConfig{Available: true})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-solo-task"),
	)

	prNum, err := repo.CreatePR(context.Background(), "ralph-xyz", "solo task", "body")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}

	pr, _ := gh.GetPR("", prNum)
	if pr == nil {
		t.Fatal("expected PR to exist in fake world")
	}
	if pr.BaseRef != "main" {
		t.Errorf("CreatePR base = %q, want %q (should target main when no PrevBranch)", pr.BaseRef, "main")
	}
}

// PostMergeUpdateMain rebases the worktree onto the updated main after a merge
// so the worktree picks up the merged changes for the next task.
func TestPostMergeUpdateMain_RebasesWorktree(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")

	repo := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
	)
	if err := repo.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	repo.RenameBranchForTask("post merge task", "")

	// Add a commit in the worktree so it diverges from main.
	writeFile(t, repo.workDir, "worktree-file.txt", "worktree content\n")
	run(t, "git", "-C", repo.workDir, "commit", "-m", "worktree commit")

	worktreeCommitBefore := gitOutput(repo.workDir, "rev-parse", "HEAD")

	// Advance origin/main (simulating a merged PR).
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	run(t, "git", "-C", tmpClone, "config", "user.name", "test")
	run(t, "git", "-C", tmpClone, "config", "user.email", "test@test")
	writeFile(t, tmpClone, "merged-file.txt", "merged content\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	originMainSHA := strings.TrimSpace(cmdOutput(t, "git", "-C", tmpClone, "rev-parse", "HEAD"))

	localMainBefore := gitOutput(project, "rev-parse", "main")

	repo.PostMergeUpdateMain()

	// Local main must NOT be modified by PostMergeUpdateMain.
	localMainAfter := gitOutput(project, "rev-parse", "main")
	if localMainAfter != localMainBefore {
		t.Errorf("PostMergeUpdateMain must not modify local main: SHA changed from %s to %s", localMainBefore, localMainAfter)
	}

	// Worktree HEAD should have changed (rebased).
	worktreeCommitAfter := gitOutput(repo.workDir, "rev-parse", "HEAD")
	if worktreeCommitAfter == worktreeCommitBefore {
		t.Error("worktree HEAD should have changed after rebase onto new main")
	}

	// origin/main should be an ancestor of the worktree HEAD (rebase succeeded).
	if gitCmdErr(repo.workDir, "merge-base", "--is-ancestor", originMainSHA, "HEAD") != nil {
		t.Error("origin/main should be ancestor of worktree HEAD after rebase")
	}

	// The worktree file should still exist (rebase preserved the worktree commit).
	if gitCmdErr(repo.workDir, "show", "HEAD:worktree-file.txt") != nil {
		t.Error("worktree-file.txt should still exist after rebase")
	}
}

// MergeWithRetry returns CIFailureError to the caller when no OnCIFailure
// callback is provided, so the caller can decide what to do with the failure.
func TestMergeWithRetry_CIFailureWithNoCallback_ReturnsError(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("rev-parse HEAD", "abc123", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 55,
			Branch: "ralph/test/01-ci-no-callback",
			State:  PRStateOpen,
		}},
		Checks: map[int][]CICheckResult{55: {{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-ci-no-callback"),
	)

	_, err := repo.MergeWithRetry(context.Background(), MergeRetryOpts{})
	if err == nil {
		t.Fatal("expected CIFailureError when no callback provided")
	}

	var ciErr *CIFailureError
	if !errors.As(err, &ciErr) {
		t.Fatalf("expected CIFailureError, got %T: %v", err, err)
	}
	if ciErr.PRNumber != 55 {
		t.Errorf("expected PRNumber=55, got %d", ciErr.PRNumber)
	}
}

// executeMerge works as a standalone package function without a Manager,
// proving the merge pipeline can be composed without coupling to Manager fields.
func TestExecuteMerge_PackageFunc_MergesSuccessfully(t *testing.T) {
	stubCISleep(t)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 42,
			Branch: "ralph/test-pkg-func",
			Title:  "test PR",
			State:  PRStateOpen,
		}},
	})
	opts := ExecuteMergeOpts{
		PRNumber:       42,
		RepoURL:        "https://github.com/test/repo.git",
		WorktreeBranch: "ralph/test-pkg-func",
		WorkDir:        "/tmp/workdir",
		DefaultBranch:  "main",
		MergeOpts:      MergeOpts{DeleteBranch: true},
		AwaitCI: func(_ context.Context, _ int, _ string, _ time.Time) ([]CICheckResult, CIStatus, error) {
			return nil, CIPassed, nil
		},
	}

	merged, err := executeMerge(context.Background(), gh, opts, discardLog{})
	if err != nil {
		t.Fatalf("executeMerge: %v", err)
	}
	if !merged {
		t.Error("expected merged=true")
	}
}

// MergeWithRetry works as a standalone package function without a Manager,
// proving the retry pipeline can be composed without coupling to Manager fields.
func TestMergeWithRetry_PackageFunc_MergesSuccessfully(t *testing.T) {
	callCount := 0
	mergeFunc := func(_ context.Context) (bool, error) {
		callCount++
		return true, nil
	}

	merged, err := MergeWithRetry(context.Background(), mergeFunc, MergeRetryOpts{
		Logger: discardLog{},
	})
	if err != nil {
		t.Fatalf("MergeWithRetry: %v", err)
	}
	if !merged {
		t.Error("expected merged=true")
	}
	if callCount != 1 {
		t.Errorf("expected 1 merge call, got %d", callCount)
	}
}

// When CIFixApplied is returned repeatedly and MaxMergeAttempts is exhausted,
// MergeWithRetry returns CIFixExhaustedError rather than a generic error.
// This signals genuine test failures so the loop can leave the task open.
func TestMergeWithRetry_CIFixApplied_Exhausted_ReturnsCIFixExhaustedError(t *testing.T) {
	ciFixCalls := 0
	ciErr := &CIFailureError{PRNumber: 42, Failures: []CICheckResult{{Name: "tests", State: "FAILURE", Bucket: "fail"}}}

	mergeFunc := func(_ context.Context) (bool, error) {
		return false, ciErr
	}

	// Base: without CIFixApplied, returns generic error
	_, err := MergeWithRetry(context.Background(), mergeFunc, MergeRetryOpts{
		Logger: discardLog{},
		OnCIFailure: func(*CIFailureError) CIFixResult {
			return CIFixFailed // infra failure — no code applied
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var exhausted *CIFixExhaustedError
	if errors.As(err, &exhausted) {
		t.Error("expected non-CIFixExhaustedError when CIFixFailed returned (infra), got CIFixExhaustedError")
	}

	// Delta: with CIFixApplied, returns CIFixExhaustedError
	_, err = MergeWithRetry(context.Background(), mergeFunc, MergeRetryOpts{
		Logger: discardLog{},
		OnCIFailure: func(*CIFailureError) CIFixResult {
			ciFixCalls++
			return CIFixApplied // fix applied but CI still failing
		},
	})
	if err == nil {
		t.Fatal("expected error after exhaustion")
	}
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected CIFixExhaustedError when CIFixApplied exhausted, got %T: %v", err, err)
	}
	if exhausted.Attempts != MaxMergeAttempts {
		t.Errorf("expected Attempts=%d, got %d", MaxMergeAttempts, exhausted.Attempts)
	}
	if ciFixCalls != MaxMergeAttempts {
		t.Errorf("expected %d CI fix calls, got %d", MaxMergeAttempts, ciFixCalls)
	}
}

// MergeWithRetry package function invokes ResolveConflict from opts when
// a MergeConflictError occurs, without needing a Manager receiver.
func TestMergeWithRetry_PackageFunc_InvokesResolveConflictFromOpts(t *testing.T) {
	attempts := 0
	resolveConflictCalled := false

	mergeFunc := func(_ context.Context) (bool, error) {
		attempts++
		if attempts == 1 {
			return false, &MergeConflictError{PRNumber: 77}
		}
		return true, nil
	}
	resolveConflict := func(_ context.Context) error {
		resolveConflictCalled = true
		return nil
	}

	merged, err := MergeWithRetry(context.Background(), mergeFunc, MergeRetryOpts{
		Logger:          discardLog{},
		ResolveConflict: resolveConflict,
	})
	if err != nil {
		t.Fatalf("MergeWithRetry: %v", err)
	}
	if !merged {
		t.Error("expected merged=true after conflict resolution")
	}
	if !resolveConflictCalled {
		t.Error("expected ResolveConflict to be called from opts")
	}
}

// AutoMergeCurrentBranch skips merge and returns ErrStackedPRWaiting when the
// PR targets a non-main branch, deferring until the parent PR merges first.
func TestAutoMerge_StackedPR_WaitsForBase(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 150,
			Branch: "ralph/test/02-stacked-child",
			Base:   "ralph/parent-branch",
			State:  PRStateOpen,
		}},
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/02-stacked-child"),
	)

	_, err := repo.AutoMergeCurrentBranch(context.Background())
	if !errors.Is(err, ErrStackedPRWaiting) {
		t.Fatalf("expected ErrStackedPRWaiting, got: %v", err)
	}

	// PR must remain open — no merge attempted when base PR hasn't merged.
	pr, _ := gh.GetPR("", 150)
	if pr == nil || pr.State != PRStateOpen {
		t.Errorf("expected PR 150 to remain open (base PR not merged), got state=%v", pr)
	}
}

// branchNeedsUpdate returns true when origin/main is not an ancestor of HEAD,
// proving that a local ancestry check (not a GitHub API call) detects when
// main has moved ahead of the PR branch.
func TestBranchNeedsUpdate_ReturnsTrueWhenMainNotAncestorOfHEAD(t *testing.T) {
	runner := newStubRunner()
	runner.On("fetch", "", nil)
	runner.On("rev-parse --verify", "", nil)
	// merge-base --is-ancestor fails → origin/main is NOT an ancestor of HEAD
	runner.On("merge-base --is-ancestor", "", errors.New("not ancestor"))

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: discardLog{}},
		nil,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-needs-update"),
	)

	if !repo.branchNeedsUpdate() {
		t.Error("branchNeedsUpdate should return true when origin/main is not an ancestor of HEAD")
	}
}

// branchNeedsUpdate returns false when origin/main is an ancestor of HEAD,
// meaning the branch already includes all base branch changes.
func TestBranchNeedsUpdate_ReturnsFalseWhenMainIsAncestorOfHEAD(t *testing.T) {
	runner := newStubRunner()
	runner.On("fetch", "", nil)
	runner.On("rev-parse --verify", "", nil)
	// merge-base --is-ancestor succeeds → origin/main IS an ancestor of HEAD
	runner.On("merge-base --is-ancestor", "", nil)

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: discardLog{}},
		nil,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-up-to-date"),
	)

	if repo.branchNeedsUpdate() {
		t.Error("branchNeedsUpdate should return false when origin/main is an ancestor of HEAD")
	}
}

// AutoMergeCurrentBranch uses KnownPRNumber when set, skipping the
// FindOpenPR lookup. This prevents failures when the branch-based PR
// lookup fails (e.g. cross-project naming).
func TestAutoMerge_KnownPRNumber_SkipsFindOpenPR(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("rev-parse HEAD", "abc123", nil)

	// Note the PR is on a DIFFERENT branch than the worktree — FindOpenPR
	// would return 0 if called. KnownPRNumber bypasses the branch lookup.
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 42,
			Branch: "some-other-branch",
			Base:   "main",
			State:  PRStateOpen,
		}},
		Checks: map[int][]CICheckResult{42: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}}},
	})

	log := &testLog{}
	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: log},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-known-pr"),
		withKnownPRNumber(42),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected merge to succeed with KnownPRNumber, got: %v", err)
	}
	if !merged {
		t.Error("expected merged=true when KnownPRNumber is set and CI passes")
	}

	if log.contains("No PR found") {
		t.Error("should not attempt FindOpenPR when KnownPRNumber is set")
	}
}

// AutoMergeCurrentBranch skips rebase+push when CI is already passing on the
// current PR head SHA, avoiding the no-op push → stale filter → infinite poll cycle.
func TestAutoMerge_CIAlreadyPassing_SkipsPushAndMergesDirectly(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	// rev-parse HEAD returns the same SHA as the PR head
	runner.On("rev-parse HEAD", "sha-already-passing", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:  77,
			Branch:  "ralph/test/01-ci-fast-path",
			Title:   "already passing",
			HeadSHA: "sha-already-passing",
			State:   PRStateOpen,
		}},
		Checks: map[int][]CICheckResult{77: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}}},
	})

	log := &testLog{}
	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: log},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-ci-fast-path"),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected merge to succeed on fast path, got: %v", err)
	}
	if !merged {
		t.Error("expected merged=true on CI fast path")
	}

	// Push must not be called — the fast path skips rebase+push entirely.
	if runner.CalledWith("push") {
		t.Error("push should not be called when CI is already passing on the current SHA")
	}

	// PR is merged in the world — the SUT drove the state transition.
	pr, _ := gh.GetPR("", 77)
	if pr == nil || pr.State != PRStateMerged {
		t.Errorf("expected PR 77 to be merged via fast path, got state=%v", pr)
	}

	// Log must confirm the fast path was taken.
	if !log.contains("CI already passing on sha-already-passing") {
		t.Errorf("expected fast-path log message, got: %v", log.messages)
	}
}

// AutoMergeCurrentBranch falls through to the normal rebase+push+poll flow
// when local HEAD differs from the PR head SHA (new commits exist locally).
func TestAutoMerge_LocalHeadDiffersFromPRHead_UsesNormalFlow(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	// Local HEAD differs from PR head SHA → fast path does not trigger.
	runner.On("rev-parse HEAD", "sha-new-local-commit", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:  78,
			Branch:  "ralph/test/01-sha-mismatch",
			Title:   "new local commits",
			HeadSHA: "sha-remote-head",
			State:   PRStateOpen,
		}},
		Checks: map[int][]CICheckResult{78: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}}},
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-sha-mismatch"),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected merge to succeed via normal flow, got: %v", err)
	}
	if !merged {
		t.Error("expected merged=true via normal flow when SHA differs")
	}

	// Push must be called — normal flow requires it.
	if !runner.CalledWith("push") {
		t.Error("push should be called when local HEAD differs from PR head SHA")
	}
}

// AutoMergeCurrentBranch returns CIFailureError when the push is a no-op (same
// SHA before and after). Without the fix, the pushedAt filter would discard the
// pre-existing failing check (started before pushedAt), causing the loop to
// poll indefinitely and eventually evaluate only post-push checks — missing the
// failure. This is the regression test for PR #495 which merged despite a
// failing test check.
func TestAutoMerge_NoOpPush_CIFailureDetected(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	// rev-parse HEAD always returns the same SHA — push is a no-op.
	runner.On("rev-parse HEAD", "sha-no-change", nil)

	// The failing check started well before the push. Without the fix, the
	// pushedAt filter would discard it and the loop would see no checks.
	failStart := time.Now().Add(-5 * time.Minute)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:  88,
			Branch:  "ralph/test/01-no-op-push",
			Title:   "no-op push regression",
			HeadSHA: "sha-remote-head", // differs from local so fast path doesn't trigger
			State:   PRStateOpen,
		}},
		Checks: map[int][]CICheckResult{88: {{
			Name:      "test",
			State:     "FAILURE",
			Bucket:    "fail",
			StartedAt: failStart,
		}}},
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-no-op-push"),
	)

	_, err := repo.AutoMergeCurrentBranch(context.Background())
	var ciErr *CIFailureError
	if !errors.As(err, &ciErr) {
		t.Fatalf("expected CIFailureError when push is no-op and CI was failing, got: %v", err)
	}
	if len(ciErr.Failures) == 0 || ciErr.Failures[0].Name != "test" {
		t.Errorf("expected failing check 'test', got: %v", ciErr.Failures)
	}
}
