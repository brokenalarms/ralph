package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type discardLog struct{}

func (discardLog) Log(string, ...any)  {}
func (discardLog) Warn(string, ...any) {}
func (discardLog) Error(string, ...any) {}

// evaluateChecks returns CIPassed when all checks have completed successfully,
// verifying the happy path for CI-gated merges.
func TestEvaluateChecks_AllPassed(t *testing.T) {
	checks := []CICheckResult{
		{Name: "test", State: "SUCCESS", Bucket: "pass"},
		{Name: "lint", State: "SUCCESS", Bucket: "pass"},
	}
	if got := evaluateChecks(checks); got != CIPassed {
		t.Errorf("expected CIPassed, got %v", got)
	}
}

// evaluateChecks returns CIPassed when at least one check passed and
// none failed — pending deployment checks don't block.
func TestEvaluateChecks_PassedWithPendingDeploy(t *testing.T) {
	checks := []CICheckResult{
		{Name: "test", State: "SUCCESS", Bucket: "pass"},
		{Name: "deploy", State: "PENDING", Bucket: "pending"},
	}
	if got := evaluateChecks(checks); got != CIPassed {
		t.Errorf("expected CIPassed (test passed, deploy pending), got %v", got)
	}
}

// evaluateChecks returns CIPending when all checks are pending
// and none have passed yet.
func TestEvaluateChecks_AllPending(t *testing.T) {
	checks := []CICheckResult{
		{Name: "test", State: "PENDING", Bucket: "pending"},
	}
	if got := evaluateChecks(checks); got != CIPending {
		t.Errorf("expected CIPending, got %v", got)
	}
}

// evaluateChecks returns CIFailed when any check has a failure conclusion,
// so the merge is aborted and failure feedback is provided.
func TestEvaluateChecks_HasFailure(t *testing.T) {
	checks := []CICheckResult{
		{Name: "test", State: "FAILURE", Bucket: "fail"},
		{Name: "lint", State: "SUCCESS", Bucket: "pass"},
	}
	if got := evaluateChecks(checks); got != CIFailed {
		t.Errorf("expected CIFailed, got %v", got)
	}
}

// evaluateChecks returns CIFailed for cancelled checks, treating cancellation
// the same as failure for merge gating purposes.
func TestEvaluateChecks_Cancelled(t *testing.T) {
	checks := []CICheckResult{
		{Name: "test", State: "CANCELLED", Bucket: "fail"},
	}
	if got := evaluateChecks(checks); got != CIFailed {
		t.Errorf("expected CIFailed for cancelled check, got %v", got)
	}
}

// evaluateChecks returns CIFailed for timed-out checks.
func TestEvaluateChecks_TimedOut(t *testing.T) {
	checks := []CICheckResult{
		{Name: "test", State: "TIMED_OUT", Bucket: "fail"},
	}
	if got := evaluateChecks(checks); got != CIFailed {
		t.Errorf("expected CIFailed for timed out check, got %v", got)
	}
}

// evaluateChecks returns CIPending when no checks are present,
// since an empty check list means checks haven't been registered yet.
func TestEvaluateChecks_Empty(t *testing.T) {
	if got := evaluateChecks(nil); got != CIPending {
		t.Errorf("expected CIPending for empty checks, got %v", got)
	}
}

// evaluateChecks treats NEUTRAL and SKIPPED conclusions as passing,
// since these indicate checks that opted out rather than failed.
func TestEvaluateChecks_NeutralAndSkipped(t *testing.T) {
	checks := []CICheckResult{
		{Name: "optional", State: "SUCCESS", Bucket: "pass"},
		{Name: "skipped", State: "SKIPPED", Bucket: "pass"},
	}
	if got := evaluateChecks(checks); got != CIPassed {
		t.Errorf("expected CIPassed for neutral/skipped checks, got %v", got)
	}
}

// failedChecks returns only checks that didn't succeed, filtering out
// SUCCESS, NEUTRAL, and SKIPPED conclusions.
func TestFailedChecks_FiltersCorrectly(t *testing.T) {
	checks := []CICheckResult{
		{Name: "test", State: "FAILURE", Bucket: "fail"},
		{Name: "lint", State: "SUCCESS", Bucket: "pass"},
		{Name: "optional", State: "SUCCESS", Bucket: "pass"},
		{Name: "deploy", State: "CANCELLED", Bucket: "fail"},
	}
	failed := failedChecks(checks)
	if len(failed) != 2 {
		t.Fatalf("expected 2 failed checks, got %d", len(failed))
	}
	if failed[0].Name != "test" || failed[1].Name != "deploy" {
		t.Errorf("unexpected failed checks: %v", failed)
	}
}

// isCIGatedError recognizes the standard GitHub branch protection error
// message that indicates CI checks are blocking the merge.
func TestIsCIGatedError_Matches(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"GraphQL: Base branch policy prohibits the merge (mergeError)", true},
		{"required status check \"test\" is expected", true},
		{"Merge requirements were not satisfied", true},
		{"Pull request review is required", true},
		{"some other error", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isCIGatedError(tc.msg); got != tc.want {
			t.Errorf("isCIGatedError(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// CIFailureError produces a human-readable message listing the failed
// check names, suitable for feedback to the next iteration.
func TestCIFailureError_Message(t *testing.T) {
	err := &CIFailureError{
		PRNumber: "42",
		Failures: []CICheckResult{
			{Name: "test", Bucket: "fail"},
			{Name: "lint", Bucket: "fail"},
		},
	}
	msg := err.Error()
	if msg != "CI checks failed on PR #42: test, lint" {
		t.Errorf("unexpected error message: %s", msg)
	}
}

// isMergeConflictError recognizes GitHub merge conflict messages that
// indicate a PR cannot be merged due to conflicts with the base branch.
func TestIsMergeConflictError_Matches(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"Pull request is not mergeable", true},
		{"Merge conflict in the base branch", true},
		{"There is a merge conflict", true},
		{"not mergeable: the head branch was out of date", true},
		{"Head branch was behind the base branch", true},
		{"required status check \"test\" is expected", false},
		{"some other error", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isMergeConflictError(tc.msg); got != tc.want {
			t.Errorf("isMergeConflictError(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// MergeConflictError produces a message identifying the PR with conflicts.
func TestMergeConflictError_Message(t *testing.T) {
	err := &MergeConflictError{PRNumber: "55"}
	msg := err.Error()
	if msg != "PR #55 has merge conflicts with the base branch" {
		t.Errorf("unexpected error message: %s", msg)
	}
}

// mergeOpts includes Admin when MergeAdmin is set, allowing admin
// users to bypass branch protection when desired.
func TestMergeOpts_IncludesAdmin(t *testing.T) {
	mgr := &Manager{
		MergeAdmin: true,
	}
	opts := mgr.mergeOpts()
	if !opts.Admin {
		t.Error("expected Admin=true when MergeAdmin is set")
	}
	if !opts.DeleteBranch {
		t.Error("expected DeleteBranch=true")
	}
}

// mergeOpts omits Admin when MergeAdmin is false, preserving
// the default behavior of respecting branch protection.
func TestMergeOpts_OmitsAdminByDefault(t *testing.T) {
	mgr := &Manager{}
	opts := mgr.mergeOpts()
	if opts.Admin {
		t.Error("expected Admin=false by default")
	}
}

func TestWaitForCI_PollsUntilPassed(t *testing.T) {
	var calls atomic.Int32
	fetch := func(pr, repo string) ([]CICheckResult, error) {
		n := calls.Add(1)
		if n < 3 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	checks, status, err := waitForCI(context.Background(), fetch, "1", "", 1*time.Millisecond, 5*time.Second, discardLog{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
	if len(checks) != 1 || checks[0].State != "SUCCESS" {
		t.Errorf("unexpected checks: %v", checks)
	}
	if calls.Load() < 3 {
		t.Errorf("expected at least 3 polls, got %d", calls.Load())
	}
}

func TestWaitForCI_ReturnsFailedImmediately(t *testing.T) {
	fetch := func(pr, repo string) ([]CICheckResult, error) {
		return []CICheckResult{
			{Name: "test", State: "FAILURE", Bucket: "fail"},
		}, nil
	}

	_, status, err := waitForCI(context.Background(), fetch, "1", "", 1*time.Millisecond, 5*time.Second, discardLog{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIFailed {
		t.Errorf("expected CIFailed, got %v", status)
	}
}

func TestWaitForCI_TimesOut(t *testing.T) {
	fetch := func(pr, repo string) ([]CICheckResult, error) {
		return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
	}

	_, status, err := waitForCI(context.Background(), fetch, "1", "", 1*time.Millisecond, 5*time.Millisecond, discardLog{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if status != CIPending {
		t.Errorf("expected CIPending on timeout, got %v", status)
	}
}

// waitForCI returns immediately when the context is cancelled, proving
// that Ctrl-C interrupts CI polling instead of blocking until timeout.
func TestWaitForCI_CancelledContext(t *testing.T) {
	fetch := func(pr, repo string) ([]CICheckResult, error) {
		return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, status, err := waitForCI(ctx, fetch, "1", "", 1*time.Second, 10*time.Second, discardLog{})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if status != CIPending {
		t.Errorf("expected CIPending, got %v", status)
	}
}

// gh() returns the default ghCLI when no GitHub interface is injected.
func TestGh_DefaultsToGhCLI(t *testing.T) {
	mgr := &Manager{}
	gh := mgr.gh()
	if gh == nil {
		t.Fatal("gh() returned nil")
	}
}

// gh() returns the injected GitHub interface when one is set.
func TestGh_UsesInjectedGitHub(t *testing.T) {
	stub := &stubGitHub{}
	mgr := &Manager{GitHub: stub}
	if mgr.gh() != stub {
		t.Error("gh() should return the injected GitHub interface")
	}
}

// stubGitHub is a test double for the GitHub interface.
type stubGitHub struct {
	available    bool
	openPR       string
	findPRErr    error
	createPRErr  error
	mergeOutput  string
	mergeErr     error
	updateResult bool
	updateErr    error
	checks       []CICheckResult
	checksErr    error
	mergeCalls   int
	mergeOpts    MergeOpts
}

func (s *stubGitHub) Available() bool { return s.available }
func (s *stubGitHub) FindOpenPR(branch, repoURL string) (string, error) {
	return s.openPR, s.findPRErr
}
func (s *stubGitHub) CreatePR(opts CreatePROpts) error {
	return s.createPRErr
}
func (s *stubGitHub) MergePR(prNumber, repoURL string, opts MergeOpts) (string, error) {
	s.mergeCalls++
	s.mergeOpts = opts
	return s.mergeOutput, s.mergeErr
}
func (s *stubGitHub) UpdateBranch(dir, nwo, prNumber string) (bool, error) {
	return s.updateResult, s.updateErr
}
func (s *stubGitHub) ListChecks(prNumber, repoURL string) ([]CICheckResult, error) {
	return s.checks, s.checksErr
}
func (s *stubGitHub) GetRunLog(prNumber, workDir string) string {
	return ""
}

// setupAutoMergeManager creates a Manager with a stubGitHub and real git repos
// so AutoMergeCurrentBranch can run without a real gh CLI.
func setupAutoMergeManager(t *testing.T, gh *stubGitHub) *Manager {
	t.Helper()
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		GitHub:      gh,
		State:       st,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	mgr.RenameBranchForTask("test feature", "")
	return mgr
}

// AutoMergeCurrentBranch returns a MergeConflictError when the gh merge
// command reports merge conflicts, so the caller can rebase and retry.
func TestAutoMerge_MergeConflictReturnsTypedError(t *testing.T) {
	gh := &stubGitHub{
		available:   true,
		openPR:      "42",
		checks:      []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}},
		mergeOutput: "Pull request is not mergeable",
		mergeErr:    fmt.Errorf("exit status 1"),
	}
	mgr := setupAutoMergeManager(t, gh)

	_, err := mgr.AutoMergeCurrentBranch(context.Background())
	if err == nil {
		t.Fatal("expected error from merge conflict")
	}

	var conflictErr *MergeConflictError
	if !errors.As(err, &conflictErr) {
		t.Errorf("expected MergeConflictError, got %T: %v", err, err)
	}
}

// AutoMergeCurrentBranch returns a CIFailureError when CI checks fail,
// so the caller can spawn a fix agent and retry.
func TestAutoMerge_CIFailureReturnsTypedError(t *testing.T) {
	gh := &stubGitHub{
		available: true,
		openPR:    "42",
		checks: []CICheckResult{
			{Name: "test", State: "FAILURE", Bucket: "fail"},
		},
	}
	mgr := setupAutoMergeManager(t, gh)

	_, err := mgr.AutoMergeCurrentBranch(context.Background())
	if err == nil {
		t.Fatal("expected error from CI failure")
	}

	var ciErr *CIFailureError
	if !errors.As(err, &ciErr) {
		t.Errorf("expected CIFailureError, got %T: %v", err, err)
	}
}

// AutoMergeCurrentBranch passes MergeOpts from Manager config to the
// GitHub interface, so admin settings are respected and branches are deleted.
func TestAutoMerge_PassesMergeOptsToGitHub(t *testing.T) {
	gh := &stubGitHub{
		available: true,
		openPR:    "42",
		checks:    []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}},
	}

	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		MergeAdmin:  true,
		GitHub:      gh,
		State:       st,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("test feature", "")
	mgr.AutoMergeCurrentBranch(context.Background())

	if !gh.mergeOpts.DeleteBranch {
		t.Error("merge should always set DeleteBranch")
	}
	if !gh.mergeOpts.Admin {
		t.Error("MergeAdmin=true should set Admin in merge opts")
	}
}
