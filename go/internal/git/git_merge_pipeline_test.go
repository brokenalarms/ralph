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
		Checks:       map[int][]CICheckResult{42: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}}},
		JobStepCount: 1, // CI ran and passed; not an infra outage
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
	pr, _ := gh.GetPR(context.Background(), "", 42)
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
		hooks: &stubShipHooks{
			PushFn: func(ctx context.Context) error { pushedCalled = true; return nil },
		},
		logger: discardLog{},
	}

	result, err := shipPR(context.Background(), gh, "/wt", "ralph/gmxa-ship", "https://github.com/test/repo.git", opts, infra)
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

// shipPR skips CreatePR and returns an empty result when branchHasUnmergedWork
// reports the branch has no commits ahead of main — prevents spurious 422 API
// errors on empty/already-merged branches.
func TestShip_SkipsCreatePRWhenNotAheadOfMain(t *testing.T) {
	// Empty world: if CreatePR were called, a PR would appear. We verify it
	// wasn't by checking ListAllPRs is empty after shipPR returns.
	gh := newStubGitHub(StubGitHubConfig{Available: true})

	opts := ShipOpts{}
	infra := shipInfra{
		hooks: &stubShipHooks{
			BranchHasUnmergedWorkFn: func(string) bool { return false },
		},
		logger: discardLog{},
	}

	result, err := shipPR(context.Background(), gh, "/wt", "ralph/test-branch", "", opts, infra)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prs, _ := gh.ListAllPRs(context.Background(), "")
	if len(prs) != 0 {
		t.Errorf("CreatePR must not be called when branch has no commits ahead of main; world has PRs: %+v", prs)
	}
	if result.PRNumber != 0 {
		t.Errorf("PRNumber = %d, want 0 (no-op)", result.PRNumber)
	}
}

// shipPR creates a PR when the branch is diverged from main — has commits
// ahead of main AND main has commits not on the branch. This is the fix for
// the bug where Ship silently orphaned work on diverged branches.
func TestShip_CreatesPRWhenDivergedFromMain(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 42,
			Branch: "ralph/e63z-diverged",
			Title:  "diverged branch work",
			State:  PRStateOpen,
		}},
	})

	opts := ShipOpts{
		TaskID:    "ralph-e63z",
		TaskTitle: "diverged branch work",
	}
	// Branch is diverged: has commits ahead of main (branchHasUnmergedWork=true)
	// but is NOT a linear ancestor of main (BranchIsAheadOfMain would be false).
	infra := shipInfra{
		hooks: &stubShipHooks{
			BranchHasUnmergedWorkFn: func(string) bool { return true },
		},
		logger: discardLog{},
	}

	result, err := shipPR(context.Background(), gh, "/wt", "ralph/e63z-diverged", "https://github.com/test/repo.git", opts, infra)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42; diverged branch with unmerged work must create a PR", result.PRNumber)
	}
}

// AutoMergeCurrentBranch proceeds to merge when CI reports failure but all
// jobs executed zero steps (infrastructure/billing outage). The pre-AwaitCI
// infra check detects this and skips polling entirely — no CIFailureError.
func TestAutoMerge_InfraFailure_ProceedsToMerge(t *testing.T) {
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

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected nil error for infra CI failure, got: %v", err)
	}
	if !merged {
		t.Error("expected merged=true when CI failure is infrastructure-only (zero job steps)")
	}
}

// AutoMergeCurrentBranch does not call AwaitCI when isInfrastructureFailure
// is true before polling begins — it jumps directly to executeMerge.
func TestAutoMerge_InfraFailure_SkipsCIWait(t *testing.T) {
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

	inner := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 55,
			Branch: "ralph/test/01-skip-ci-wait",
			Title:  "skip ci wait test",
			State:  PRStateOpen,
		}},
		JobStepCount: 0,
	})
	counter := &listChecksCounter{gitHub: inner}

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		counter,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-skip-ci-wait"),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if !merged {
		t.Error("expected merged=true when infra failure detected before AwaitCI")
	}
	if counter.n != 0 {
		t.Errorf("AwaitCI must not be called when infra failure is detected pre-poll (ListChecks calls=%d, want 0)", counter.n)
	}
}

// AutoMergeCurrentBranch falls through to executeMerge when AwaitCI times out
// and isInfrastructureFailure returns true (zero job steps) — the timeout is
// an infra symptom, not a real CI failure.
func TestAutoMerge_CITimeout_InfraFailure_ProceedsToMerge(t *testing.T) {
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

	// First GetJobStepCount call (pre-AwaitCI check) returns an error so the
	// pre-check does not short-circuit. Second call (post-timeout check) returns
	// 0, classifying the timeout as an infrastructure failure → proceed to merge.
	inner := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 77,
			Branch: "ralph/test/01-ci-timeout-infra",
			Title:  "ci timeout infra test",
			State:  PRStateOpen,
		}},
		ListChecksErr: errors.New("simulated CI unavailable"),
	})
	seq := &sequencedJobStepGH{
		gitHub: inner,
		responses: []struct {
			count int
			err   error
		}{
			{0, errors.New("step count unavailable before poll")}, // pre-AwaitCI: infra check fails → poll runs
			{0, nil}, // post-timeout: infra check succeeds → merge
		},
	}

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}, CIPollTimeout: 1 * time.Millisecond},
		seq,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-ci-timeout-infra"),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected nil error when CI times out and infra failure detected, got: %v", err)
	}
	if !merged {
		t.Error("expected merged=true when AwaitCI timeout is classified as infrastructure failure")
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
		Checks:       map[int][]CICheckResult{100: {{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
		JobStepCount: 1, // real failure: jobs executed steps, not an infra outage
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

// Ship with AutoMerge=true proceeds to merge when CI reports zero job steps
// (infrastructure/billing outage). AutoMergeCurrentBranch detects the outage
// before polling and jumps directly to MergePR — the PR merges rather than
// being left open. CIFailure is false because the merge succeeded.
func TestShip_InfrastructureFailure_MergesInsteadOfFailing(t *testing.T) {
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
	if !result.Merged {
		t.Error("expected Merged=true: infra failure should bypass CI and merge")
	}
	if result.CIFailure {
		t.Error("expected CIFailure=false: merge succeeded, no CI failure to report")
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

// Ship logs that no review is pending and the loop is continuing to a
// CI-gated merge when PollReview returns nil — it must not claim a timeout
// occurred (PollReview returns immediately, without waiting, when the bot was
// never a requested reviewer) or that a merge is happening next (the loop
// still has to rebase, push, and wait on CI before any merge attempt).
func TestShip_NoReviewPending_LogsCIGatedMergeNotTimeoutOrImmediateMerge(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("rev-parse HEAD", "sha-review-log", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:  101,
			Branch:  "ralph/test/01-ship-review-log",
			Base:    "main",
			HeadSHA: "sha-review-log",
			State:   PRStateOpen,
		}},
		Checks: map[int][]CICheckResult{101: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}}},
		// PollReviewResult/PollReviewErr default to nil — no review arrives.
	})

	log := &testLog{}
	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: log},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-ship-review-log"),
	)

	result, err := repo.Ship(context.Background(), ShipOpts{
		AutoMerge: true,
		PRNumber:  101,
		Reviewers: []Reviewer{{BotUsername: "copilot-pull-request-reviewer", DefaultTimeout: time.Millisecond}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Merged {
		t.Error("expected Merged=true once CI passes with no review pending")
	}

	if log.contains("proceeding to merge") {
		t.Error("log must not say 'proceeding to merge' — a CI-gated merge attempt follows, not an immediate merge")
	}
	if log.contains("within timeout") {
		t.Error("log must not claim a timeout occurred — PollReview returns immediately when the bot isn't a requested reviewer")
	}
	if !log.contains("No copilot-pull-request-reviewer review pending") {
		t.Errorf("expected log to state no review is pending, got: %v", log.messages)
	}
	if !log.contains("CI-gated merge") {
		t.Errorf("expected log to describe the CI-gated merge that follows, got: %v", log.messages)
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
		// GetJobStepCountErr makes isInfrastructureFailure return false so the
		// timeout is treated as a real timeout (not an infra outage), leaving the PR open.
		ListChecksErr:      fmt.Errorf("API unavailable"),
		GetJobStepCountErr: fmt.Errorf("API unavailable"),
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
	pr, _ := gh.GetPR(context.Background(), "", 102)
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
	pr, _ := gh.GetPR(context.Background(), "", prNum)
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

	pr, _ := gh.GetPR(context.Background(), "", prNum)
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

// MergeWithRetry returns CIFailureError to the caller immediately (no
// internal retry), so the loop can decide whether to spawn a fix agent and
// call Ship again.
func TestMergeWithRetry_CIFailure_ReturnsErrorImmediately(t *testing.T) {
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
		Checks:       map[int][]CICheckResult{55: {{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
		JobStepCount: 1, // real failure: jobs executed steps, not an infra outage
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-ci-no-callback"),
	)

	_, err := repo.MergeWithRetry(context.Background(), MergeRetryOpts{})
	if err == nil {
		t.Fatal("expected CIFailureError")
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
		CI: &stubCIPoller{
			AwaitCIFn: func(_ context.Context, _ int, _ string, _ time.Time) ([]CICheckResult, CIStatus, error) {
				return nil, CIPassed, nil
			},
		},
	}

	_, merged, err := executeMerge(context.Background(), gh, opts, discardLog{})
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

// MergeWithRetry package function invokes ResolveConflict via opts.Resolver
// when a MergeConflictError occurs, without needing a Manager receiver.
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
	resolver := &stubConflictResolver{
		ResolveConflictFn: func(_ context.Context) error {
			resolveConflictCalled = true
			return nil
		},
	}

	merged, err := MergeWithRetry(context.Background(), mergeFunc, MergeRetryOpts{
		Logger:   discardLog{},
		Resolver: resolver,
	})
	if err != nil {
		t.Fatalf("MergeWithRetry: %v", err)
	}
	if !merged {
		t.Error("expected merged=true after conflict resolution")
	}
	if !resolveConflictCalled {
		t.Error("expected ResolveConflict to be called from opts.Resolver")
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
	pr, _ := gh.GetPR(context.Background(), "", 150)
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
	pr, _ := gh.GetPR(context.Background(), "", 77)
	if pr == nil || pr.State != PRStateMerged {
		t.Errorf("expected PR 77 to be merged via fast path, got state=%v", pr)
	}

	// Log must confirm the fast path was taken.
	if !log.contains("CI already passing on sha-already-passing") {
		t.Errorf("expected fast-path log message, got: %v", log.messages)
	}
}

// AutoMergeCurrentBranch logs that it is waiting for CI and will merge only if
// it passes, not "Auto-merging..." — which reads as an immediate merge before
// the CI-gated rebase/push/AwaitCI sequence has even started.
func TestAutoMergeCurrentBranch_LogsWaitingForCINotImmediateMerge(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("rev-parse HEAD", "sha-wait-ci-log", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:  81,
			Branch:  "ralph/test/01-wait-ci-log",
			Title:   "wait for ci wording",
			HeadSHA: "sha-wait-ci-log",
			State:   PRStateOpen,
		}},
		Checks: map[int][]CICheckResult{81: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}}},
	})

	log := &testLog{}
	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: log},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-wait-ci-log"),
	)

	if _, err := repo.AutoMergeCurrentBranch(context.Background()); err != nil {
		t.Fatalf("expected merge to succeed, got: %v", err)
	}

	if log.contains("Auto-merging...") {
		t.Error("log must not contain 'Auto-merging...' — it overstates an immediate merge")
	}
	if !log.contains("Waiting for CI") || !log.contains("merge only if it passes") {
		t.Errorf("expected log to convey waiting for CI and merging only if it passes, got: %v", log.messages)
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
		JobStepCount: 1, // real failure: jobs executed steps, not an infra outage
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

// AutoMergeCurrentBranch with --admin-merge-on-ci-infra-failure set merges
// with Admin:true when isInfrastructureFailure is true — bypassing branch protection.
func TestAutoMerge_AdminMergeOnCIInfraFailure_MergesWithAdmin(t *testing.T) {
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
			Number:  42,
			Branch:  "ralph/test/01-admin-infra",
			Title:   "admin infra test",
			State:   PRStateOpen,
			Blocked: true, // branch protection blocks merge without Admin
		}},
		Checks:       map[int][]CICheckResult{42: {{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
		JobStepCount: 0, // zero job steps → infra failure, not real test failure
	})

	repo := newRepoForTest(
		Config{
			ProjectDir:                 "/project",
			WorkDir:                    "/project/wt",
			BaseBranch:                 "main",
			Logger:                     &testLog{},
			AdminMergeOnCIInfraFailure: true,
		},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-admin-infra"),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected nil error with admin-merge-on-ci-infra-failure set and infra failure, got: %v", err)
	}
	if !merged {
		t.Error("expected merged=true when Admin bypass used for infra-only CI failure")
	}
	pr, _ := gh.GetPR(context.Background(), "test/repo", 42)
	if pr == nil || pr.State != PRStateMerged {
		t.Errorf("expected PR 42 merged via admin override, got state=%v", pr)
	}
}

// AutoMergeCurrentBranch with --admin-merge-on-ci-infra-failure NOT set and a blocked
// PR falls through with the existing behavior: executeMerge returns an error because
// branch protection is not bypassed.
func TestAutoMerge_AdminMergeOnCIInfraFailure_NotSet_BlockedNotBypassed(t *testing.T) {
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
			Number:  43,
			Branch:  "ralph/test/01-no-admin-infra",
			Title:   "no admin infra test",
			State:   PRStateOpen,
			Blocked: true, // branch protection blocks merge
		}},
		Checks:       map[int][]CICheckResult{43: {{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
		JobStepCount: 0, // zero job steps → infra failure, not real test failure
	})

	repo := newRepoForTest(
		Config{
			ProjectDir: "/project",
			WorkDir:    "/project/wt",
			BaseBranch: "main",
			Logger:     &testLog{},
			// AdminMergeOnCIInfraFailure not set
		},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-no-admin-infra"),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if err == nil {
		t.Fatal("expected error when branch protection blocks merge and admin flag is not set")
	}
	if merged {
		t.Error("expected merged=false when branch protection not bypassed")
	}
	pr, _ := gh.GetPR(context.Background(), "test/repo", 43)
	if pr != nil && pr.State == PRStateMerged {
		t.Error("PR must not be merged when admin flag is not set")
	}
}

// AutoMergeCurrentBranch never calls MergePR with Admin:true when CI fails with
// non-zero job steps (real test failure) — the flag is scoped to infra failures only.
func TestAutoMerge_AdminMergeOnCIInfraFailure_NoEffectOnRealFailure(t *testing.T) {
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
			Number:  44,
			Branch:  "ralph/test/01-real-failure",
			Title:   "real failure test",
			State:   PRStateOpen,
			Blocked: true,
		}},
		Checks:       map[int][]CICheckResult{44: {{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
		JobStepCount: 3, // non-zero: real test failure, not infra
	})

	repo := newRepoForTest(
		Config{
			ProjectDir:                 "/project",
			WorkDir:                    "/project/wt",
			BaseBranch:                 "main",
			Logger:                     &testLog{},
			AdminMergeOnCIInfraFailure: true,
		},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-real-failure"),
	)

	_, err := repo.AutoMergeCurrentBranch(context.Background())
	var ciErr *CIFailureError
	if !errors.As(err, &ciErr) {
		t.Fatalf("expected CIFailureError for real test failure regardless of flag, got: %v", err)
	}
	pr, _ := gh.GetPR(context.Background(), "test/repo", 44)
	if pr != nil && pr.State == PRStateMerged {
		t.Error("PR must not be merged when CI failure is real (non-zero job steps)")
	}
}

// When no GitHub CI checks are present after the grace period, the loop must
// run the locally-detected test suite before merging. A failing test suite
// blocks the merge and returns an error — unverified code must never ship.
func TestAutoMerge_NoCIConfigured_TestsFail_BlocksMerge(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	// Return the PR's HeadSHA so the fast path is taken (skips rebase+push).
	runner.On("rev-parse HEAD", "stub-sha-201", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 201,
			Branch: "ralph/test/01-noci-fail",
			Title:  "no-CI tests-fail test",
			State:  PRStateOpen,
		}},
		// No Checks entry — ListChecks returns nil, simulating no CI configured.
	})

	workDir := t.TempDir()
	repo := newRepoForTest(
		Config{
			ProjectDir:      workDir,
			WorkDir:         workDir,
			BaseBranch:      "main",
			Logger:          discardLog{},
			NoCIGracePeriod: 5 * time.Millisecond,
			CIPollTimeout:   200 * time.Millisecond,
			ConfigVerify:    "/usr/bin/false",
		},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-noci-fail"),
		withKnownPRNumber(201),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if merged {
		t.Error("expected merged=false when local tests fail with no CI")
	}
	if err == nil {
		t.Fatal("expected error when local tests fail with no CI")
	}
	// The PR must remain open — failing tests must block the merge.
	pr, _ := gh.GetPR(context.Background(), "", 201)
	if pr == nil || pr.State != PRStateOpen {
		t.Errorf("expected PR 201 to remain open after local test failure, got state=%v", pr.State)
	}
}

// When no GitHub CI checks are present and no test command is detected in the
// project, the merge is allowed — absence of tests is not treated as failure.
func TestAutoMerge_NoCIConfigured_NoTestCommand_Merges(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("rev-parse HEAD", "stub-sha-202", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 202,
			Branch: "ralph/test/01-noci-notests",
			Title:  "no-CI no-tests test",
			State:  PRStateOpen,
		}},
	})

	// Empty temp dir: no package.json, no Makefile → DetectTestCommand returns nil.
	workDir := t.TempDir()
	repo := newRepoForTest(
		Config{
			ProjectDir:      workDir,
			WorkDir:         workDir,
			BaseBranch:      "main",
			Logger:          discardLog{},
			NoCIGracePeriod: 5 * time.Millisecond,
			CIPollTimeout:   200 * time.Millisecond,
			ConfigVerify:    "", // no verify command
		},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-noci-notests"),
		withKnownPRNumber(202),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected no error when no test command detected, got: %v", err)
	}
	if !merged {
		t.Error("expected PR to be merged when no CI and no test command")
	}
	pr, _ := gh.GetPR(context.Background(), "", 202)
	if pr == nil || pr.State != PRStateMerged {
		t.Errorf("expected PR 202 to be merged when no tests configured, got state=%v", pr.State)
	}
}

// When no GitHub CI checks are present and the locally-detected test suite
// passes, the merge proceeds normally.
func TestAutoMerge_NoCIConfigured_TestsPass_Merges(t *testing.T) {
	stubCISleep(t)

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("rev-parse HEAD", "stub-sha-203", nil)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 203,
			Branch: "ralph/test/01-noci-pass",
			Title:  "no-CI tests-pass test",
			State:  PRStateOpen,
		}},
	})

	workDir := t.TempDir()
	repo := newRepoForTest(
		Config{
			ProjectDir:      workDir,
			WorkDir:         workDir,
			BaseBranch:      "main",
			Logger:          discardLog{},
			NoCIGracePeriod: 5 * time.Millisecond,
			CIPollTimeout:   200 * time.Millisecond,
			ConfigVerify:    "/usr/bin/true", // always exits 0
		},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-noci-pass"),
		withKnownPRNumber(203),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected no error when tests pass with no CI, got: %v", err)
	}
	if !merged {
		t.Error("expected PR to be merged when no CI and tests pass")
	}
	pr, _ := gh.GetPR(context.Background(), "", 203)
	if pr == nil || pr.State != PRStateMerged {
		t.Errorf("expected PR 203 to be merged when tests pass, got state=%v", pr.State)
	}
}
