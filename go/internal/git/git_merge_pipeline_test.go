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

	mgr := &Manager{
		ProjectDir:     project,
		BaseBranch:     "main",
		WorkDir:        wtDir,
		WorktreeBranch: "ralph/test-rebase-push",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	if err := mgr.Push(context.Background()); err != nil {
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
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("rev-parse HEAD", "abc123", nil)

	gh := &StubGitHub{
		IsAvailable:  true,
		OpenPR:       "42",
		PRTitle:      "some PR",
		PRHeadSHA:    "abc123",
		Checks:       []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		UpdateResult: true, // branch needs update → main moved
	}

	mgr := &Manager{
		ProjectDir:     "/project",
		WorkDir:        "/project/wt",
		WorktreeBranch: "ralph/test/01-main-moved",
		BaseBranch:     "main",
		Runner:         runner,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         &testLog{},
	}

	_, err := mgr.AutoMergeCurrentBranch(context.Background())
	if err == nil {
		t.Fatal("expected error when main moved while CI was running")
	}

	var conflictErr *MergeConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected MergeConflictError, got %T: %v", err, err)
	}
	if conflictErr.PRNumber != "42" {
		t.Errorf("expected PRNumber=42, got %s", conflictErr.PRNumber)
	}

	if gh.MergeCalls > 0 {
		t.Error("MergePR should not be called when branch needs update")
	}
}

// executeMerge classifies "Pull request is not mergeable" as a merge conflict
// rather than a CI gate error. This documents current behavior: isMergeConflictError
// is checked before isCIGatedError, so ambiguous messages that match both patterns
// are treated as conflicts. After the orchestrator refactor, this should be
// reconsidered — the "not mergeable" error from GitHub often means CI hasn't
// passed, not that there's a content conflict.
// After ralph-laun fix, executeMerge checks CI-gated first. "not mergeable"
// still matches isMergeConflictError but only fires if isCIGatedError didn't
// match first. This test has no CI-gated patterns, so it falls through to
// merge conflict.
func TestExecuteMerge_NotMergeableClassifiedAsConflictWhenNotCIGated(t *testing.T) {
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

	gh := &StubGitHub{
		IsAvailable:  true,
		OpenPR:       "88",
		PRTitle:      "some PR",
		PRHeadSHA:    "abc123",
		MergeOutput:  "Pull request is not mergeable",
		MergeErr:     fmt.Errorf("merge failed"),
		Checks:       []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		UpdateResult: false,
	}

	mgr := &Manager{
		ProjectDir:     "/project",
		WorkDir:        "/project/wt",
		WorktreeBranch: "ralph/test/01-not-mergeable",
		BaseBranch:     "main",
		Runner:         runner,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         &testLog{},
	}

	_, err := mgr.AutoMergeCurrentBranch(context.Background())
	if err == nil {
		t.Fatal("expected error for non-mergeable PR")
	}

	var conflictErr *MergeConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("'not mergeable' should be MergeConflictError when CI-gated doesn't match first, got %T: %v", err, err)
	}
}

// AutoMergeCurrentBranch bypasses CI failure and proceeds to merge when
// local tests passed and CI failed due to infrastructure (zero steps executed),
// so billing outages don't block merges.
func TestAutoMerge_InfraFailureBypass_MergesWhenLocalTestsPassed(t *testing.T) {
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

	gh := &StubGitHub{
		IsAvailable:  true,
		OpenPR:       "99",
		PRTitle:      "infra failure test",
		PRHeadSHA:    "abc123",
		Checks:       []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}},
		UpdateResult: false,
		JobStepCount: 0,
	}

	mgr := &Manager{
		ProjectDir:       "/project",
		WorkDir:          "/project/wt",
		WorktreeBranch:   "ralph/test/01-infra-bypass",
		BaseBranch:       "main",
		Runner:           runner,
		GitHub:           gh,
		State:            newMemState(),
		Logger:           &testLog{},
		LocalTestsPassed: true,
	}

	merged, err := mgr.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected merge to proceed despite CI infra failure, got: %v", err)
	}
	if !merged {
		t.Error("expected merged=true when local tests passed and CI is infra failure")
	}
	if gh.MergeCalls == 0 {
		t.Error("expected MergePR to be called (merge should proceed)")
	}
}

// AutoMergeCurrentBranch does NOT bypass CI failure when local tests did not
// pass, even if the failure is an infrastructure issue.
func TestAutoMerge_InfraFailureNoBypass_WhenLocalTestsNotPassed(t *testing.T) {
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

	gh := &StubGitHub{
		IsAvailable:  true,
		OpenPR:       "100",
		PRTitle:      "infra failure no bypass",
		PRHeadSHA:    "abc123",
		Checks:       []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}},
		JobStepCount: 0,
	}

	mgr := &Manager{
		ProjectDir:       "/project",
		WorkDir:          "/project/wt",
		WorktreeBranch:   "ralph/test/01-infra-no-bypass",
		BaseBranch:       "main",
		Runner:           runner,
		GitHub:           gh,
		State:            newMemState(),
		Logger:           &testLog{},
		LocalTestsPassed: false,
	}

	_, err := mgr.AutoMergeCurrentBranch(context.Background())
	if err == nil {
		t.Fatal("expected CIFailureError when local tests not passed")
	}

	var ciErr *CIFailureError
	if !errors.As(err, &ciErr) {
		t.Fatalf("expected CIFailureError, got %T: %v", err, err)
	}
}

// CreatePR targets PrevBranch (the parent in a stacked PR) instead of main,
// so GitHub shows the correct diff between the child and parent branch.
func TestCreatePR_StackedPRTargetsParentBranch(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)

	var capturedOpts CreatePROpts
	gh := &capturingGitHub{
		StubGitHub: StubGitHub{IsAvailable: true, CreatedPR: "200"},
	}
	gh.createPR = func(opts CreatePROpts) error {
		capturedOpts = opts
		gh.StubGitHub.OpenPR = "200"
		return nil
	}

	mgr := &Manager{
		ProjectDir:     "/project",
		WorkDir:        "/project/wt",
		WorktreeBranch: "ralph/test/02-child-task",
		BaseBranch:     "main",
		PrevBranch:     "ralph/parent-task",
		Runner:         runner,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         discardLog{},
	}

	prNum, err := mgr.CreatePR(context.Background(), "ralph-abc", "child task description", "body")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if prNum == "" {
		t.Fatal("expected non-empty PR number")
	}
	if capturedOpts.Base != "ralph/parent-task" {
		t.Errorf("CreatePR base = %q, want %q (should target parent branch, not main)", capturedOpts.Base, "ralph/parent-task")
	}
	if capturedOpts.Head != "ralph/test/02-child-task" {
		t.Errorf("CreatePR head = %q, want %q", capturedOpts.Head, "ralph/test/02-child-task")
	}
}

// CreatePR targets main when PrevBranch is not set (non-stacked case).
func TestCreatePR_NonStackedTargetsMain(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)

	var capturedOpts CreatePROpts
	gh := &capturingGitHub{
		StubGitHub: StubGitHub{IsAvailable: true, CreatedPR: "201"},
		createPR: func(opts CreatePROpts) error {
			capturedOpts = opts
			return nil
		},
	}

	mgr := &Manager{
		ProjectDir:     "/project",
		WorkDir:        "/project/wt",
		WorktreeBranch: "ralph/test/01-solo-task",
		BaseBranch:     "main",
		Runner:         runner,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         discardLog{},
	}

	_, err := mgr.CreatePR(context.Background(), "ralph-xyz", "solo task", "body")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if capturedOpts.Base != "main" {
		t.Errorf("CreatePR base = %q, want %q (should target main when no PrevBranch)", capturedOpts.Base, "main")
	}
}

// PostMergeUpdateMain rebases the worktree onto the updated main after a merge
// so the worktree picks up the merged changes for the next task.
func TestPostMergeUpdateMain_RebasesWorktree(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		BaseBranch: "main",
		State:      st,
		Logger:     &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("post merge task", "")

	// Add a commit in the worktree so it diverges from main.
	writeFile(t, mgr.WorkDir, "worktree-file.txt", "worktree content\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "worktree commit")

	worktreeCommitBefore := gitOutput(mgr.WorkDir, "rev-parse", "HEAD")

	// Advance origin/main (simulating a merged PR).
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	run(t, "git", "-C", tmpClone, "config", "user.name", "test")
	run(t, "git", "-C", tmpClone, "config", "user.email", "test@test")
	writeFile(t, tmpClone, "merged-file.txt", "merged content\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	originMainSHA := strings.TrimSpace(cmdOutput(t, "git", "-C", tmpClone, "rev-parse", "HEAD"))

	mgr.PostMergeUpdateMain()

	// Local main should match origin/main.
	localMain := gitOutput(project, "rev-parse", "main")
	if localMain != originMainSHA {
		t.Errorf("local main = %s, want %s (should match origin/main)", localMain, originMainSHA)
	}

	// Worktree HEAD should have changed (rebased).
	worktreeCommitAfter := gitOutput(mgr.WorkDir, "rev-parse", "HEAD")
	if worktreeCommitAfter == worktreeCommitBefore {
		t.Error("worktree HEAD should have changed after rebase onto new main")
	}

	// origin/main should be an ancestor of the worktree HEAD (rebase succeeded).
	if gitCmdErr(mgr.WorkDir, "merge-base", "--is-ancestor", originMainSHA, "HEAD") != nil {
		t.Error("origin/main should be ancestor of worktree HEAD after rebase")
	}

	// The worktree file should still exist (rebase preserved the worktree commit).
	if gitCmdErr(mgr.WorkDir, "show", "HEAD:worktree-file.txt") != nil {
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

	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      "55",
		PRHeadSHA:   "abc123",
		Checks:      []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}},
	}

	mgr := &Manager{
		ProjectDir:     "/project",
		WorkDir:        "/project/wt",
		WorktreeBranch: "ralph/test/01-ci-no-callback",
		BaseBranch:     "main",
		Runner:         runner,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         &testLog{},
	}

	_, err := mgr.MergeWithRetry(context.Background(), MergeRetryOpts{})
	if err == nil {
		t.Fatal("expected CIFailureError when no callback provided")
	}

	var ciErr *CIFailureError
	if !errors.As(err, &ciErr) {
		t.Fatalf("expected CIFailureError, got %T: %v", err, err)
	}
	if ciErr.PRNumber != "55" {
		t.Errorf("expected PRNumber=55, got %s", ciErr.PRNumber)
	}
}

// MergeWithRetry uses exponential backoff for infrastructure failures (CIFixNoCommits)
// and does not consume the code-fix attempt budget.
func TestMergeWithRetry_InfraRetryBackoff(t *testing.T) {
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

	ciCallCount := 0
	gh := &infraRetryGitHub{
		StubGitHub: StubGitHub{
			IsAvailable: true,
			OpenPR:      "101",
			PRTitle:     "infra retry",
			PRHeadSHA:   "abc123",
		},
		ciResults: func() []CICheckResult {
			ciCallCount++
			if ciCallCount <= 3 {
				return []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}}
			}
			return []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}
		},
	}

	mgr := &Manager{
		ProjectDir:     "/project",
		WorkDir:        "/project/wt",
		WorktreeBranch: "ralph/test/01-infra-retry",
		BaseBranch:     "main",
		Runner:         runner,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         &testLog{},
	}

	var sleepDurations []time.Duration
	infraFixCalls := 0
	merged, err := mgr.MergeWithRetry(context.Background(), MergeRetryOpts{
		OnCIFailure: func(ciErr *CIFailureError) CIFixResult {
			infraFixCalls++
			return CIFixNoCommits
		},
		SleepFunc: func(d time.Duration) {
			sleepDurations = append(sleepDurations, d)
		},
	})
	if err != nil {
		t.Fatalf("expected merge to succeed after infra retries, got: %v", err)
	}
	if !merged {
		t.Error("expected merged=true after infra retries resolved")
	}
	if infraFixCalls != 3 {
		t.Errorf("expected 3 infra fix calls, got %d", infraFixCalls)
	}
	if len(sleepDurations) != 3 {
		t.Fatalf("expected 3 sleep calls, got %d: %v", len(sleepDurations), sleepDurations)
	}
	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}
	for i, w := range want {
		if sleepDurations[i] != w {
			t.Errorf("sleep[%d] = %v, want %v", i, sleepDurations[i], w)
		}
	}
}

// infraRetryGitHub returns dynamic CI results to simulate infrastructure retries.
type infraRetryGitHub struct {
	StubGitHub
	ciResults func() []CICheckResult
}

func (g *infraRetryGitHub) ListChecks(prNumber, repoURL string) ([]CICheckResult, error) {
	return g.ciResults(), nil
}

func (g *infraRetryGitHub) MergePR(prNumber, repoURL string, opts MergeOpts) (string, error) {
	g.StubGitHub.MergeCalls++
	return "merged", nil
}

// executeMerge handles the CI-gated retry path: when MergePR returns a branch
// protection error, it waits for CI then retries the merge.
func TestExecuteMerge_CIGatedRetryPath(t *testing.T) {
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

	mergeCalls := 0
	gh := &ciGatedMergeGitHub{
		StubGitHub: StubGitHub{
			IsAvailable:  true,
			OpenPR:       "120",
			PRTitle:      "CI gated test",
			PRHeadSHA:    "abc123",
			UpdateResult: false,
		},
		mergeFunc: func() (string, error) {
			mergeCalls++
			if mergeCalls == 1 {
				return "Base branch policy prohibits the merge", fmt.Errorf("merge blocked")
			}
			return "merged", nil
		},
		checksFunc: func(call int) []CICheckResult {
			if call <= 1 {
				return []CICheckResult{{Name: "ci", State: "PENDING", Bucket: "pending"}}
			}
			return []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}
		},
	}

	mgr := &Manager{
		ProjectDir:     "/project",
		WorkDir:        "/project/wt",
		WorktreeBranch: "ralph/test/01-ci-gated",
		BaseBranch:     "main",
		Runner:         runner,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         &testLog{},
	}

	merged, err := mgr.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected merge to succeed after CI gate cleared, got: %v", err)
	}
	if !merged {
		t.Error("expected merged=true")
	}
	if mergeCalls != 2 {
		t.Errorf("expected 2 merge calls (blocked + retry), got %d", mergeCalls)
	}
}

// ciGatedMergeGitHub simulates a merge blocked by branch protection that
// succeeds after CI passes.
type ciGatedMergeGitHub struct {
	StubGitHub
	mergeFunc  func() (string, error)
	checksFunc func(call int) []CICheckResult
	checkCalls int
}

func (g *ciGatedMergeGitHub) MergePR(prNumber, repoURL string, opts MergeOpts) (string, error) {
	return g.mergeFunc()
}

func (g *ciGatedMergeGitHub) ListChecks(prNumber, repoURL string) ([]CICheckResult, error) {
	g.checkCalls++
	return g.checksFunc(g.checkCalls), nil
}

// AutoMergeCurrentBranch skips merge and returns ErrStackedPRWaiting when the
// PR targets a non-main branch, deferring until the parent PR merges first.
func TestAutoMerge_StackedPR_WaitsForBase(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)

	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      "150",
		PRBase:      "ralph/parent-branch",
	}

	mgr := &Manager{
		ProjectDir:     "/project",
		WorkDir:        "/project/wt",
		WorktreeBranch: "ralph/test/02-stacked-child",
		BaseBranch:     "main",
		Runner:         runner,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         &testLog{},
	}

	_, err := mgr.AutoMergeCurrentBranch(context.Background())
	if !errors.Is(err, ErrStackedPRWaiting) {
		t.Fatalf("expected ErrStackedPRWaiting, got: %v", err)
	}

	if gh.MergeCalls > 0 {
		t.Error("should not attempt merge when base PR hasn't merged")
	}
}
