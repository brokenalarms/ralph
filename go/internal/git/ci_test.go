package git

import (
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

// evaluateChecks returns CIPending when any check is still running,
// so the polling loop continues waiting.
func TestEvaluateChecks_StillPending(t *testing.T) {
	checks := []CICheckResult{
		{Name: "test", State: "SUCCESS", Bucket: "pass"},
		{Name: "deploy", State: "PENDING", Bucket: "pending"},
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

// ghMergeArgs includes --admin when MergeAdmin is set, allowing admin
// users to bypass branch protection when desired.
func TestGhMergeArgs_IncludesAdmin(t *testing.T) {
	mgr := &Manager{
		BranchStrategy: BranchStacked,
		MergeAdmin:     true,
	}
	args := mgr.ghMergeArgs("123", "https://github.com/org/repo")
	found := false
	for _, a := range args {
		if a == "--admin" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected --admin in args: %v", args)
	}
}

// ghMergeArgs omits --admin when MergeAdmin is false, preserving
// the default behavior of respecting branch protection.
func TestGhMergeArgs_OmitsAdminByDefault(t *testing.T) {
	mgr := &Manager{
		BranchStrategy: BranchStacked,
	}
	args := mgr.ghMergeArgs("123", "https://github.com/org/repo")
	for _, a := range args {
		if a == "--admin" {
			t.Errorf("--admin should not be present by default: %v", args)
		}
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

	checks, status, err := waitForCI(fetch, "1", "", 1*time.Millisecond, 5*time.Second, discardLog{})
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

	_, status, err := waitForCI(fetch, "1", "", 1*time.Millisecond, 5*time.Second, discardLog{})
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

	_, status, err := waitForCI(fetch, "1", "", 1*time.Millisecond, 5*time.Millisecond, discardLog{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if status != CIPending {
		t.Errorf("expected CIPending on timeout, got %v", status)
	}
}

func TestCIFetcher_DefaultsToFetchPRChecks(t *testing.T) {
	mgr := &Manager{}
	f := mgr.ciFetcher()
	if f == nil {
		t.Fatal("ciFetcher returned nil")
	}
}

func TestCIFetcher_UsesInjectedFunc(t *testing.T) {
	called := false
	mgr := &Manager{
		FetchPRChecks: func(pr, repo string) ([]CICheckResult, error) {
			called = true
			return nil, nil
		},
	}
	f := mgr.ciFetcher()
	f("1", "")
	if !called {
		t.Error("injected FetchPRChecks was not called")
	}
}
