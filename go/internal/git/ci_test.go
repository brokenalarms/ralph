package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubCINow replaces ciNow with a fake clock that advances by step on every
// call, so tests can simulate elapsed time deterministically (no-progress
// budget resets, minute-cadence status lines) without real sleeps.
func stubCINow(t *testing.T, step time.Duration) {
	t.Helper()
	origNow := ciNow
	current := time.Now()
	ciNow = func() time.Time {
		current = current.Add(step)
		return current
	}
	t.Cleanup(func() { ciNow = origNow })
}

// CICheckResult.Failed and Pending expose the normalized verdict as methods,
// so callers can switch on behavior instead of re-testing the raw bucket value.
func TestCICheckResult_FailedAndPending(t *testing.T) {
	pass := CICheckResult{Bucket: CIBucketPass}
	fail := CICheckResult{Bucket: CIBucketFail}
	pending := CICheckResult{Bucket: CIBucketPending}

	if pass.Failed() || pass.Pending() {
		t.Errorf("pass bucket: expected Failed()=false Pending()=false, got Failed()=%v Pending()=%v", pass.Failed(), pass.Pending())
	}
	if !fail.Failed() || fail.Pending() {
		t.Errorf("fail bucket: expected Failed()=true Pending()=false, got Failed()=%v Pending()=%v", fail.Failed(), fail.Pending())
	}
	if pending.Failed() || !pending.Pending() {
		t.Errorf("pending bucket: expected Failed()=false Pending()=true, got Failed()=%v Pending()=%v", pending.Failed(), pending.Pending())
	}
}

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

// evaluateChecks returns CIPending when any check is still pending,
// even if other checks have passed — all checks must resolve.
func TestEvaluateChecks_PendingBlocksEvenWhenOthersPassed(t *testing.T) {
	checks := []CICheckResult{
		{Name: "test", State: "SUCCESS", Bucket: "pass"},
		{Name: "deploy", State: "PENDING", Bucket: "pending"},
	}
	if got := evaluateChecks(checks); got != CIPending {
		t.Errorf("expected CIPending (deploy still pending), got %v", got)
	}
}

// evaluateChecks treats non-required check failures as blocking,
// not just required ones — all CI signals matter.
func TestEvaluateChecks_NonRequiredFailureBlocks(t *testing.T) {
	checks := []CICheckResult{
		{Name: "test", State: "SUCCESS", Bucket: "pass"},
		{Name: "netlify", State: "FAILURE", Bucket: "fail"},
	}
	if got := evaluateChecks(checks); got != CIFailed {
		t.Errorf("expected CIFailed (non-required check failed), got %v", got)
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

// RequiredFailedChecks returns only failures marked as required by branch
// protection, filtering out optional/deploy checks that fix agents can't fix.
// RequiredFailedChecks returns all checks with bucket=fail since gh pr checks
// does not expose isRequired.
func TestRequiredFailedChecks_ReturnsAllFailed(t *testing.T) {
	checks := []CICheckResult{
		{Name: "test", State: "FAILURE", Bucket: "fail"},
		{Name: "lint", State: "FAILURE", Bucket: "fail"},
		{Name: "deploy/netlify", State: "SUCCESS", Bucket: "pass"},
		{Name: "Pages changed", State: "FAILURE", Bucket: "fail"},
	}
	required := RequiredFailedChecks(checks)
	if len(required) != 3 {
		t.Fatalf("expected 3 failed checks, got %d", len(required))
	}
}

// RequiredFailedChecks returns empty when no checks have bucket=fail.
func TestRequiredFailedChecks_NoneFailedReturnsEmpty(t *testing.T) {
	checks := []CICheckResult{
		{Name: "deploy/netlify", State: "SUCCESS", Bucket: "pass"},
		{Name: "Header rules", State: "SUCCESS", Bucket: "pass"},
	}
	required := RequiredFailedChecks(checks)
	if len(required) != 0 {
		t.Fatalf("expected 0 failed checks, got %d", len(required))
	}
}

// When IsRequired is populated, RequiredFailedChecks only returns required failures.
func TestRequiredFailedChecks_FiltersByIsRequired(t *testing.T) {
	checks := []CICheckResult{
		{Name: "test", State: "FAILURE", Bucket: "fail", IsRequired: true},
		{Name: "deploy/preview", State: "FAILURE", Bucket: "fail", IsRequired: false},
		{Name: "lint", State: "SUCCESS", Bucket: "pass", IsRequired: true},
	}
	required := RequiredFailedChecks(checks)
	if len(required) != 1 {
		t.Fatalf("expected 1 required failed check, got %d", len(required))
	}
	if required[0].Name != "test" {
		t.Errorf("expected 'test', got %q", required[0].Name)
	}
}

// CIFailureError produces a human-readable message listing the failed
// check names, suitable for feedback to the next iteration.
func TestCIFailureError_Message(t *testing.T) {
	err := &CIFailureError{
		PRNumber: 42,
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

// MergeConflictError produces a message identifying the PR with conflicts.
func TestMergeConflictError_Message(t *testing.T) {
	err := &MergeConflictError{PRNumber: 55}
	msg := err.Error()
	if msg != "PR #55 has merge conflicts with the base branch" {
		t.Errorf("unexpected error message: %s", msg)
	}
}

// mergeOpts sets DeleteBranch.
func TestMergeOpts_Defaults(t *testing.T) {
	repo := newRepoForTest(Config{}, nil)
	opts := repo.mergeOpts()
	if !opts.DeleteBranch {
		t.Error("expected DeleteBranch=true")
	}
}

func TestWaitForCI_PollsUntilPassed(t *testing.T) {
	var calls atomic.Int32
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		n := calls.Add(1)
		if n < 3 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	checks, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Second, 5*time.Second, 0, nil, discardLog{})
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
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		return []CICheckResult{
			{Name: "test", State: "FAILURE", Bucket: "fail"},
		}, nil
	}

	_, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Second, 5*time.Second, 0, nil, discardLog{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIFailed {
		t.Errorf("expected CIFailed, got %v", status)
	}
}

// waitForCI abandons the wait once the fetched check state is frozen (no new
// check, no status transition) for the full no-progress budget, even though
// the hard cap is far from expiring — proving the timeout is progress-aware,
// not a flat wall-clock deadline.
func TestWaitForCI_TimesOut(t *testing.T) {
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
	}

	_, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Millisecond, 5*time.Second, 0, nil, discardLog{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var incomplete *CIIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("expected a *CIIncompleteError, got: %v", err)
	}
	if incomplete.Waited != 5*time.Millisecond {
		t.Errorf("expected the error to carry the no-progress budget (5ms), got %v", incomplete.Waited)
	}
	if status != CIPending {
		t.Errorf("expected CIPending on timeout, got %v", status)
	}
}

// waitForCI treats a live GitHub Actions job step reported by stepFetch as
// progress even when the fetched check state itself is frozen — a single
// required check whose status stays "PENDING" for its whole run (the case of
// a long-running job) is waited out well past noProgressTimeout instead of
// tripping the no-progress budget, because each cadence-gated stepFetch call
// keeps observing a running step and resets the clock.
func TestWaitForCI_LiveStepResetsNoProgressBudget(t *testing.T) {
	stubCISleep(t)
	stubCINow(t, 90*time.Second)

	var calls atomic.Int32
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		n := calls.Add(1)
		if n < 20 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}
	stepFetch := func() ([]JobStepStatus, error) {
		return []JobStepStatus{{JobName: "test", StepName: "Run npm test", StepIndex: 11, StepTotal: 15}}, nil
	}

	// noProgressTimeout is 5m; the fake clock advances 90s per poll, so by
	// poll 20 (~30 minutes elapsed) a frozen-state implementation would have
	// aborted long ago. Because stepFetch reports a running step on every
	// cadence tick, the budget keeps resetting and all 19 pending polls
	// complete successfully.
	_, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Minute, time.Hour, 0, stepFetch, discardLog{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
	if calls.Load() < 20 {
		t.Errorf("expected at least 20 polls, got %d", calls.Load())
	}
}

// waitForCI still times out on the exact same frozen check state and clock
// advance as TestWaitForCI_LiveStepResetsNoProgressBudget when stepFetch is
// absent — proving the fix only changes behavior when live step data is
// actually observed, not the frozen-state fallback.
func TestWaitForCI_TimesOutWithoutStepData(t *testing.T) {
	stubCISleep(t)
	stubCINow(t, 90*time.Second)

	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
	}

	_, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Minute, time.Hour, 0, nil, discardLog{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var incomplete *CIIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("expected a *CIIncompleteError, got: %v", err)
	}
	if incomplete.Waited != 5*time.Minute {
		t.Errorf("expected the error to carry the no-progress budget (5m0s), got %v", incomplete.Waited)
	}
	if status != CIPending {
		t.Errorf("expected CIPending on timeout, got %v", status)
	}
}

// waitForCI uses exponential backoff, doubling the interval each poll up
// to MaxCIPollInterval, so early polls are fast and later polls don't spam.
func TestWaitForCI_BackoffDoubles(t *testing.T) {
	var sleeps []time.Duration
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		sleeps = append(sleeps, d)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { ciSleep = origSleep }()

	callCount := 0
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		callCount++
		if callCount < 6 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	_, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Second, 5*time.Minute, 5*time.Minute, 0, nil, discardLog{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
	// 5 pending polls → sleeps of 1s, 2s, 4s, 8s, 16s (below the 30s cap)
	if len(sleeps) != 5 {
		t.Fatalf("expected 5 sleeps, got %d: %v", len(sleeps), sleeps)
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	for i, w := range want {
		if sleeps[i] != w {
			t.Errorf("sleep[%d] = %v, want %v", i, sleeps[i], w)
		}
	}
}

// waitForCI's exponential backoff caps at MaxCIPollInterval (30s) rather than
// doubling indefinitely, so a long-hung CI run doesn't end up polling minutes
// apart.
func TestWaitForCI_BackoffCapsAt30Seconds(t *testing.T) {
	var sleeps []time.Duration
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		sleeps = append(sleeps, d)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { ciSleep = origSleep }()

	callCount := 0
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		callCount++
		if callCount < 8 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	_, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Second, 5*time.Minute, 5*time.Minute, 0, nil, discardLog{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
	// 7 pending polls → sleeps of 1s, 2s, 4s, 8s, 16s, then capped at 30s, 30s.
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("expected %d sleeps, got %d: %v", len(want), len(sleeps), sleeps)
	}
	for i, w := range want {
		if sleeps[i] != w {
			t.Errorf("sleep[%d] = %v, want %v", i, sleeps[i], w)
		}
	}
}

// waitForCI emits a single minute-cadence status line for a burst of fast
// polls — the first line is emitted immediately when polling begins, and
// with a faked instant ciSleep no real wall-clock minute ever elapses to
// trigger a second one, so 10 polls still produce exactly one status line
// (replacing the old per-poll "..Ns" counters).
func TestWaitForCI_SingleLogLine(t *testing.T) {
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { ciSleep = origSleep }()

	callCount := 0
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		callCount++
		if callCount < 11 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	log := &testLog{}
	_, _, err := waitForCI(context.Background(), fetch, 42, "", "", 1*time.Second, 5*time.Minute, 5*time.Minute, 0, nil, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(log.messages) != 1 {
		t.Fatalf("expected exactly 1 log message for 10 polls, got %d: %v", len(log.messages), log.messages)
	}
	if !strings.Contains(log.messages[0], "0/1 checks complete") || !strings.Contains(log.messages[0], "in progress: test") {
		t.Errorf("expected checks snapshot in status line, got: %s", log.messages[0])
	}
}

// waitForCI emits no log line when CI resolves on the first fetch
// without any polling being needed.
func TestWaitForCI_NoLogLineWhenResolvedImmediately(t *testing.T) {
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	log := &testLog{}
	_, status, err := waitForCI(context.Background(), fetch, 42, "", "", 1*time.Second, 5*time.Minute, 5*time.Minute, 0, nil, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
	if len(log.messages) != 0 {
		t.Fatalf("expected no log messages when resolved immediately, got %d: %v", len(log.messages), log.messages)
	}
}

// waitForCI resets the no-progress budget every time the fetched check state
// changes, so a CI run whose checks keep transitioning is waited out
// regardless of total duration — even when each individual gap between polls
// would have exceeded a frozen-state budget on its own.
func TestWaitForCI_ProgressResetsNoProgressBudget(t *testing.T) {
	stubCISleep(t)
	stubCINow(t, 1*time.Second)

	var calls atomic.Int32
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		n := calls.Add(1)
		if n < 10 {
			return []CICheckResult{{Name: "test", State: fmt.Sprintf("STEP_%d", n), Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	// noProgressTimeout is 5s — with the fake clock advancing 1s per poll, a
	// frozen-state implementation would abandon the wait after 5 polls. Because
	// each poll reports a new check state, the budget resets every time and
	// all 9 pending polls complete successfully.
	_, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Second, time.Hour, 0, nil, discardLog{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
	if calls.Load() < 10 {
		t.Errorf("expected at least 10 polls, got %d", calls.Load())
	}
}

// waitForCI's hard cap (ci_max_wait) bounds the total wait even when checks
// keep transitioning and the no-progress budget never fires — genuinely hung
// CI must still be abandoned eventually.
func TestWaitForCI_HardCapExpiresDuringContinuousProgress(t *testing.T) {
	var calls atomic.Int32
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		n := calls.Add(1)
		return []CICheckResult{{Name: "test", State: fmt.Sprintf("STEP_%d", n), Bucket: "pending"}}, nil
	}

	// noProgressTimeout is large enough it never fires — checks change on
	// every poll. Only the tiny hard cap bounds the wait.
	_, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Second, 5*time.Millisecond, 0, nil, discardLog{})
	if err == nil {
		t.Fatal("expected hard-cap timeout error")
	}
	var incomplete *CIIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("expected a *CIIncompleteError, got: %v", err)
	}
	if incomplete.Waited != 5*time.Millisecond {
		t.Errorf("expected the error to carry the hard cap (5ms), got %v", incomplete.Waited)
	}
	if status != CIPending {
		t.Errorf("expected CIPending on hard-cap timeout, got %v", status)
	}
	if calls.Load() < 2 {
		t.Errorf("expected multiple polls before the hard cap expired, got %d", calls.Load())
	}
}

// waitForCI emits a status line once per elapsed minute (using the fake
// clock's advancing "now") in addition to the first line at poll start, each
// reporting elapsed time and a completed/total checks snapshot.
func TestWaitForCI_MinuteCadenceStatusLine(t *testing.T) {
	stubCISleep(t)
	stubCINow(t, 40*time.Second)

	var calls atomic.Int32
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		n := calls.Add(1)
		if n < 5 {
			return []CICheckResult{
				{Name: "build", State: "SUCCESS", Bucket: "pass"},
				{Name: "e2e", State: "PENDING", Bucket: "pending"},
			}, nil
		}
		return []CICheckResult{
			{Name: "build", State: "SUCCESS", Bucket: "pass"},
			{Name: "e2e", State: "SUCCESS", Bucket: "pass"},
		}, nil
	}

	log := &testLog{}
	_, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Minute, time.Hour, 0, nil, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}

	// The fake clock advances 40s per poll, crossing the 1-minute cadence
	// boundary more than once across 4 pending polls — expect more than the
	// single first-line status message.
	if len(log.messages) < 2 {
		t.Fatalf("expected at least 2 minute-cadence status lines, got %d: %v", len(log.messages), log.messages)
	}
	if !strings.Contains(log.messages[0], "1/2 checks complete") || !strings.Contains(log.messages[0], "in progress: e2e") {
		t.Errorf("expected checks snapshot in status line, got: %s", log.messages[0])
	}
}

// waitForCI's minute-cadence status line names the currently running GitHub
// Actions step of an in-progress job, with its 1-based index and the job's
// total step count, instead of just the bare check name.
func TestWaitForCI_StatusLineNamesRunningStep(t *testing.T) {
	stubCISleep(t)
	stubCINow(t, 40*time.Second)

	var calls atomic.Int32
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		n := calls.Add(1)
		if n < 3 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}
	stepFetch := func() ([]JobStepStatus, error) {
		return []JobStepStatus{{JobName: "test", StepName: "Run Playwright tests", StepIndex: 5, StepTotal: 9}}, nil
	}

	log := &testLog{}
	_, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Minute, time.Hour, 0, stepFetch, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
	if len(log.messages) == 0 {
		t.Fatal("expected at least one status line")
	}
	if !strings.Contains(log.messages[0], "test → Run Playwright tests (step 5/9)") {
		t.Errorf("expected step detail in status line, got: %s", log.messages[0])
	}
	if strings.Contains(log.messages[0], "in progress:") {
		t.Errorf("expected step rendering to replace the bare 'in progress:' prefix, got: %s", log.messages[0])
	}
}

// waitForCI's minute-cadence status line comma-separates multiple
// in-progress jobs, each with its own running step and index/total.
func TestWaitForCI_StatusLineMultiJobRendering(t *testing.T) {
	stubCISleep(t)
	stubCINow(t, 40*time.Second)

	var calls atomic.Int32
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		n := calls.Add(1)
		if n < 3 {
			return []CICheckResult{
				{Name: "test", State: "PENDING", Bucket: "pending"},
				{Name: "lint", State: "PENDING", Bucket: "pending"},
			}, nil
		}
		return []CICheckResult{
			{Name: "test", State: "SUCCESS", Bucket: "pass"},
			{Name: "lint", State: "SUCCESS", Bucket: "pass"},
		}, nil
	}
	stepFetch := func() ([]JobStepStatus, error) {
		return []JobStepStatus{
			{JobName: "test", StepName: "Run Playwright tests", StepIndex: 5, StepTotal: 9},
			{JobName: "lint", StepName: "Run eslint", StepIndex: 2, StepTotal: 3},
		}, nil
	}

	log := &testLog{}
	_, _, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Minute, time.Hour, 0, stepFetch, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(log.messages) == 0 {
		t.Fatal("expected at least one status line")
	}
	want := "test → Run Playwright tests (step 5/9), lint → Run eslint (step 2/3)"
	if !strings.Contains(log.messages[0], want) {
		t.Errorf("expected multi-job step rendering %q, got: %s", want, log.messages[0])
	}
}

// waitForCI falls back to the plain check-name rendering when the step
// fetch errors, so a transient GitHub Actions API failure never blanks the
// status line or crashes the poll.
func TestWaitForCI_StatusLineFallsBackOnStepFetchError(t *testing.T) {
	stubCISleep(t)
	stubCINow(t, 40*time.Second)

	var calls atomic.Int32
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		n := calls.Add(1)
		if n < 3 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}
	stepFetch := func() ([]JobStepStatus, error) {
		return nil, fmt.Errorf("actions API unavailable")
	}

	log := &testLog{}
	_, _, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Minute, time.Hour, 0, stepFetch, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(log.messages) == 0 {
		t.Fatal("expected at least one status line")
	}
	if !strings.Contains(log.messages[0], "in progress: test") {
		t.Errorf("expected fallback check-name rendering, got: %s", log.messages[0])
	}
}

// waitForCI falls back to a check's bare name when the step fetch succeeds
// but returns no matching job — the case of a non-GitHub-Actions check
// (e.g. an external CI provider) mixed in among Actions jobs.
func TestWaitForCI_StatusLineFallsBackForNonActionsCheck(t *testing.T) {
	stubCISleep(t)
	stubCINow(t, 40*time.Second)

	var calls atomic.Int32
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		n := calls.Add(1)
		if n < 3 {
			return []CICheckResult{
				{Name: "test", State: "PENDING", Bucket: "pending"},
				{Name: "netlify", State: "PENDING", Bucket: "pending"},
			}, nil
		}
		return []CICheckResult{
			{Name: "test", State: "SUCCESS", Bucket: "pass"},
			{Name: "netlify", State: "SUCCESS", Bucket: "pass"},
		}, nil
	}
	stepFetch := func() ([]JobStepStatus, error) {
		return []JobStepStatus{{JobName: "test", StepName: "Run Playwright tests", StepIndex: 5, StepTotal: 9}}, nil
	}

	log := &testLog{}
	_, _, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Minute, time.Hour, 0, stepFetch, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(log.messages) == 0 {
		t.Fatal("expected at least one status line")
	}
	want := "test → Run Playwright tests (step 5/9), netlify"
	if !strings.Contains(log.messages[0], want) {
		t.Errorf("expected mixed rendering %q, got: %s", want, log.messages[0])
	}
}

// waitForCI returns immediately when the context is cancelled, proving
// that Ctrl-C interrupts CI polling instead of blocking until timeout.
func TestWaitForCI_CancelledContext(t *testing.T) {
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, status, err := waitForCI(ctx, fetch, 1, "", "", 1*time.Second, 10*time.Second, 10*time.Second, 0, nil, discardLog{})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if status != CIPending {
		t.Errorf("expected CIPending, got %v", status)
	}
}



// waitForCI returns CIPassed after the no-CI grace period when all fetches
// return zero checks — repos with no CI configured should not block auto-merge.
func TestWaitForCI_ZeroChecksAfterGracePeriodReturnsCIPassed(t *testing.T) {
	stubCISleep(t)

	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		return nil, nil
	}

	checks, status, err := waitForCI(
		context.Background(), fetch, 1, "", "",
		1*time.Millisecond, 5*time.Second, 5*time.Second, 5*time.Millisecond,
		nil, discardLog{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed after grace period with no CI checks, got %v", status)
	}
	if len(checks) != 0 {
		t.Errorf("expected empty checks for no-CI repo, got %v", checks)
	}
}

// waitForCI continues polling when zero-check grace period is 0 (disabled),
// proving the grace period opt-in does not change existing behavior.
func TestWaitForCI_ZeroGracePeriodTimesOutNormally(t *testing.T) {
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		return nil, nil
	}

	_, status, err := waitForCI(
		context.Background(), fetch, 1, "", "",
		1*time.Millisecond, 5*time.Millisecond, 5*time.Second, 0,
		nil, discardLog{},
	)
	if err == nil {
		t.Fatal("expected timeout error when grace period is 0 and no checks ever appear")
	}
	if status != CIPending {
		t.Errorf("expected CIPending on timeout (no grace period), got %v", status)
	}
}

// AwaitCI returns CIPassed after the no-CI grace period for repos with no CI,
// proving greenfield projects do not block auto-merge indefinitely.
func TestAwaitCI_NoCIChecksReturnsPassedAfterGracePeriod(t *testing.T) {
	stubCISleep(t)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		Checks:    nil,
	})
	repo := newRepoForTest(Config{
		Logger:          &testLog{},
		CIPollTimeout:   5 * time.Second,
		NoCIGracePeriod: 5 * time.Millisecond,
	}, gh)

	_, status, err := repo.AwaitCI(context.Background(), 1, "repo", time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed for no-CI repo after grace period, got %v", status)
	}
}

// AwaitCI returns CIPassed immediately when checks already pass,
// without entering the polling loop.
func TestAwaitCI_PassedImmediately(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		Checks:    map[int][]CICheckResult{1: {{Name: "test", State: "SUCCESS", Bucket: "pass"}}},
	})
	repo := newRepoForTest(Config{Logger: &testLog{}}, gh)

	checks, status, err := repo.AwaitCI(context.Background(), 1, "repo", time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
	if len(checks) != 1 || checks[0].Name != "test" {
		t.Errorf("unexpected checks: %v", checks)
	}
}

// AwaitCI returns CIFailed immediately when checks have already failed,
// without entering the polling loop.
func TestAwaitCI_FailedImmediately(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		Checks:    map[int][]CICheckResult{1: {{Name: "lint", State: "FAILURE", Bucket: "fail"}}},
	})
	repo := newRepoForTest(Config{Logger: &testLog{}}, gh)

	checks, status, err := repo.AwaitCI(context.Background(), 1, "repo", time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIFailed {
		t.Errorf("expected CIFailed, got %v", status)
	}
	if len(checks) != 1 || checks[0].Name != "lint" {
		t.Errorf("unexpected checks: %v", checks)
	}
}

// AwaitCI keeps polling (rather than aborting) when ListChecks returns an
// error. Static-error world proves the error branch: errors don't crash or
// short-circuit; the poll loop persists until timeout.
func TestAwaitCI_FetchErrorKeepsPolling(t *testing.T) {
	stubCISleep(t)

	gh := newStubGitHub(StubGitHubConfig{
		Available:     true,
		ListChecksErr: fmt.Errorf("no checks yet"),
	})
	repo := newRepoForTest(Config{Logger: &testLog{}, CIPollTimeout: 5 * time.Millisecond}, gh)

	_, _, err := repo.AwaitCI(context.Background(), 1, "repo", time.Time{})
	if err == nil {
		t.Fatal("expected timeout error when ListChecks always fails")
	}
}

// AwaitCI log output uses a PRLink hyperlink when it emits the "CI checks
// not available yet" line during polling. Static-error world drives the
// poll path; the log line appears and must contain the clickable hyperlink.
func TestAwaitCI_LogUsesPRLink(t *testing.T) {
	stubCISleep(t)

	gh := newStubGitHub(StubGitHubConfig{
		Available:     true,
		ListChecksErr: fmt.Errorf("no checks yet"),
	})
	log := &testLog{}
	repo := newRepoForTest(Config{Logger: log, CIPollTimeout: 5 * time.Millisecond}, gh)

	_, _, _ = repo.AwaitCI(context.Background(), 99, "https://github.com/owner/repo", time.Time{})

	// The "CI checks not available yet" log line must contain a clickable
	// hyperlink (OSC 8 sequence with the GitHub URL), not plain "PR #99".
	found := false
	for _, msg := range log.messages {
		if strings.Contains(msg, "not available yet") {
			if !strings.Contains(msg, "github.com/owner/repo/pull/99") {
				t.Errorf("expected PRLink hyperlink in log, got: %s", msg)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'not available yet' log line, got: %v", log.messages)
	}
}

// AwaitCI with pushedAt logs a PRLink hyperlink in the "Waiting for fresh CI"
// line when fresh checks are returned.
func TestAwaitCI_PushedAtLogUsesPRLink(t *testing.T) {
	stubCISleep(t)

	now := time.Now()
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		Checks: map[int][]CICheckResult{88: {
			{Name: "test", State: "SUCCESS", Bucket: "pass", StartedAt: now.Add(time.Second)},
		}},
	})
	log := &testLog{}
	repo := newRepoForTest(Config{Logger: log}, gh)

	_, status, err := repo.AwaitCI(context.Background(), 88, "https://github.com/owner/repo", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}

	// "Waiting for fresh CI" line must contain a clickable link.
	for _, msg := range log.messages {
		if strings.Contains(msg, "Waiting for fresh CI") &&
			!strings.Contains(msg, "github.com/owner/repo/pull/88") {
			t.Errorf("expected PRLink hyperlink in log, got: %s", msg)
		}
	}
}

// waitForCI's minute-cadence status log lines use PRLink for clickable links.
func TestWaitForCI_LogUsesPRLink(t *testing.T) {
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { ciSleep = origSleep }()

	callCount := 0
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		callCount++
		if callCount < 3 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	log := &testLog{}
	_, _, err := waitForCI(context.Background(), fetch, 77, "https://github.com/owner/repo", "owner/repo", 1*time.Second, 5*time.Minute, 5*time.Minute, 0, nil, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, msg := range log.messages {
		if strings.Contains(msg, "checks complete") {
			found = true
			if !strings.Contains(msg, "github.com/owner/repo/pull/77") {
				t.Errorf("expected PRLink hyperlink in status log, got: %s", msg)
			}
		}
	}
	if !found {
		t.Fatalf("expected a status log line, got: %v", log.messages)
	}
}

// AwaitCI filters out stale checks (StartedAt before pushedAt) rather than
// returning them as a satisfied result. Static world with only-stale checks
// proves filtering happens: if filtering didn't run, the passing check would
// return CIPassed immediately; instead, the poll times out waiting for fresh.
func TestAwaitCI_AllStaleChecksTimesOut(t *testing.T) {
	stubCISleep(t)

	pushedAt := time.Now()
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		Checks: map[int][]CICheckResult{1: {
			// The check passes, but was started before pushedAt → stale.
			{Name: "test", State: "SUCCESS", Bucket: "pass", StartedAt: pushedAt.Add(-time.Minute)},
		}},
	})
	repo := newRepoForTest(Config{Logger: &testLog{}, CIPollTimeout: 5 * time.Millisecond}, gh)

	_, status, err := repo.AwaitCI(context.Background(), 1, "repo", pushedAt)
	if err == nil {
		t.Fatal("expected timeout error when only stale checks exist (filter must hide them)")
	}
	if status != CIPending {
		t.Errorf("expected CIPending on timeout (stale check filtered out), got %v", status)
	}
}

// AwaitCI with zero pushedAt skips filtering and returns results immediately.
func TestAwaitCI_ZeroPushedAtSkipsFiltering(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		Checks:    map[int][]CICheckResult{1: {{Name: "test", State: "SUCCESS", Bucket: "pass"}}},
	})
	repo := newRepoForTest(Config{Logger: &testLog{}}, gh)

	checks, status, err := repo.AwaitCI(context.Background(), 1, "repo", time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
	if len(checks) != 1 {
		t.Errorf("unexpected checks: %v", checks)
	}
}

// setupAutoMergeManager creates a Manager with the given gitHub stub and
// a real bare repo in a temp dir so AutoMergeCurrentBranch can run without
// a real gh CLI. The worktree branch is renamed to "ralph/test-feature"
// so tests know exactly which branch FindOpenPR will be queried with.
func setupAutoMergeManager(t *testing.T, gh gitHub) *repo {
	t.Helper()
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	// setupAutoMergeManager exercises real git against initBareRepo — SetupWorktree,
	// RenameBranchForTask, and AutoMergeCurrentBranch all issue real git commands.
	// Override the default no-op runner with the real exec-backed runner.
	repo := newRepoForTest(
		Config{ProjectDir: project, BaseBranch: "main", RalphDir: ralphDir, Logger: &testLog{}},
		gh,
		withRunner(&execRunner{}),
	)
	if err := repo.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	repo.RenameBranchForTask("test feature", "")
	return repo
}

// AutoMergeCurrentBranch returns a MergeConflictError when the PR has merge
// conflicts with its base, so the caller can rebase and retry.
func TestAutoMerge_MergeConflictReturnsTypedError(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:     42,
			Branch:     "ralph/test-feature",
			State:      PRStateOpen,
			Conflicted: true, // world state: this PR has conflicts
		}},
		Checks: map[int][]CICheckResult{42: {{Name: "test", State: "SUCCESS", Bucket: "pass"}}},
	})
	repo := setupAutoMergeManager(t, gh)

	_, err := repo.AutoMergeCurrentBranch(context.Background())
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
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 42,
			Branch: "ralph/test-feature",
			State:  PRStateOpen,
		}},
		Checks:       map[int][]CICheckResult{42: {{Name: "test", State: "FAILURE", Bucket: "fail"}}},
		JobStepCount: 1, // real failure: jobs executed steps, not an infra outage
	})
	repo := setupAutoMergeManager(t, gh)

	_, err := repo.AutoMergeCurrentBranch(context.Background())
	if err == nil {
		t.Fatal("expected error from CI failure")
	}

	var ciErr *CIFailureError
	if !errors.As(err, &ciErr) {
		t.Errorf("expected CIFailureError, got %T: %v", err, err)
	}
}

// AwaitCI uses Manager.CIPollTimeout when non-zero, falling back to
// DefaultCIPollTimeout when zero. This proves the config value is wired
// through to the polling loop rather than always using the constant.
func TestAwaitCI_UsesManagerCIPollTimeout(t *testing.T) {
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { ciSleep = origSleep }()

	// Manager with a very short custom timeout — pending checks should time out quickly.
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		Checks:    map[int][]CICheckResult{1: {{Name: "test", State: "PENDING", Bucket: "pending"}}},
	})
	repo := newRepoForTest(Config{Logger: &testLog{}, CIPollTimeout: 1 * time.Millisecond}, gh)

	_, status, err := repo.AwaitCI(context.Background(), 1, "repo", time.Time{})
	if err == nil {
		t.Fatal("expected timeout error with 1ms CIPollTimeout")
	}
	if status != CIPending {
		t.Errorf("expected CIPending on timeout, got %v", status)
	}
}

// AwaitCI falls back to DefaultCIPollTimeout when Manager.CIPollTimeout is zero,
// preserving backwards compatibility for tests that construct Manager directly.
func TestAwaitCI_ZeroCIPollTimeoutFallsBackToDefault(t *testing.T) {
	// Manager with zero CIPollTimeout and checks that pass immediately —
	// if the fallback is working, it won't time out before the first poll.
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		Checks:    map[int][]CICheckResult{1: {{Name: "test", State: "SUCCESS", Bucket: "pass"}}},
	})
	repo := newRepoForTest(Config{Logger: &testLog{}}, gh)

	_, status, err := repo.AwaitCI(context.Background(), 1, "repo", time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
}

// AwaitCI filters to only required status checks when branch protection
// configures them, so non-required checks (Netlify, tag workflows) do not
// gate merging. A failing non-required check is ignored; the required check
// that passes causes CIPassed to be returned.
func TestAwaitCI_RequiredChecksFilter_IgnoresNonRequired(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		Checks: map[int][]CICheckResult{1: {
			{Name: "test", State: "SUCCESS", Bucket: "pass"},
			{Name: "netlify", State: "FAILURE", Bucket: "fail"}, // not required
		}},
		RequiredChecks: []string{"test"}, // only "test" is required
	})

	repo := newRepoForTest(Config{Logger: &testLog{}, BaseBranch: "main"}, gh)

	_, status, err := repo.AwaitCI(context.Background(), 1, "https://github.com/owner/repo.git", time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed (non-required netlify failure should be ignored), got %v", status)
	}
}

// AwaitCI returns CIFailed when a required check fails, even when non-required
// checks are also present, proving required-only evaluation is applied correctly.
func TestAwaitCI_RequiredChecksFilter_RequiredFailureBlocks(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		Checks: map[int][]CICheckResult{1: {
			{Name: "test", State: "FAILURE", Bucket: "fail"},    // required, failing
			{Name: "lint", State: "SUCCESS", Bucket: "pass"},    // required, passing
			{Name: "netlify", State: "SUCCESS", Bucket: "pass"}, // not required
		}},
		RequiredChecks: []string{"test", "lint"},
	})

	repo := newRepoForTest(Config{Logger: &testLog{}, BaseBranch: "main"}, gh)

	_, status, _ := repo.AwaitCI(context.Background(), 1, "https://github.com/owner/repo.git", time.Time{})
	if status != CIFailed {
		t.Errorf("expected CIFailed (required check 'test' failing), got %v", status)
	}
}

// AwaitCI evaluates all checks when GetRequiredChecks returns empty, preserving
// existing behavior for repos without branch protection rules configured.
func TestAwaitCI_RequiredChecksFilter_FallsBackToAllChecksWhenEmpty(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		Checks: map[int][]CICheckResult{1: {
			{Name: "test", State: "SUCCESS", Bucket: "pass"},
			{Name: "netlify", State: "FAILURE", Bucket: "fail"},
		}},
		RequiredChecks: nil, // no required checks configured
	})

	repo := newRepoForTest(Config{Logger: &testLog{}, BaseBranch: "main"}, gh)

	_, status, _ := repo.AwaitCI(context.Background(), 1, "https://github.com/owner/repo.git", time.Time{})
	if status != CIFailed {
		t.Errorf("expected CIFailed (no required checks → evaluate all, netlify fails), got %v", status)
	}
}

