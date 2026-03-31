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

// MergeConflictError produces a message identifying the PR with conflicts.
func TestMergeConflictError_Message(t *testing.T) {
	err := &MergeConflictError{PRNumber: "55"}
	msg := err.Error()
	if msg != "PR #55 has merge conflicts with the base branch" {
		t.Errorf("unexpected error message: %s", msg)
	}
}

// mergeOpts always sets DeleteBranch and never sets Admin (removed).
func TestMergeOpts_Defaults(t *testing.T) {
	mgr := &Manager{}
	opts := mgr.mergeOpts()
	if !opts.DeleteBranch {
		t.Error("expected DeleteBranch=true")
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

	checks, status, err := waitForCI(context.Background(), fetch, "1", "", "", 1*time.Millisecond, 5*time.Second, discardLog{})
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

	_, status, err := waitForCI(context.Background(), fetch, "1", "", "", 1*time.Millisecond, 5*time.Second, discardLog{})
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

	_, status, err := waitForCI(context.Background(), fetch, "1", "", "", 1*time.Millisecond, 5*time.Millisecond, discardLog{})
	if err == nil {
		t.Fatal("expected timeout error")
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
	fetch := func(pr, repo string) ([]CICheckResult, error) {
		callCount++
		if callCount < 6 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	_, status, err := waitForCI(context.Background(), fetch, "1", "", "", 1*time.Second, 5*time.Minute, discardLog{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
	// 5 pending polls → sleeps of 1s, 2s, 4s, 5s, 5s (capped)
	if len(sleeps) != 5 {
		t.Fatalf("expected 5 sleeps, got %d: %v", len(sleeps), sleeps)
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for i, w := range want {
		if sleeps[i] != w {
			t.Errorf("sleep[%d] = %v, want %v", i, sleeps[i], w)
		}
	}
}

// waitForCI logs a single summary line per PR showing accumulated poll
// durations, not one line per poll cycle.
func TestWaitForCI_SingleLogLine(t *testing.T) {
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { ciSleep = origSleep }()

	callCount := 0
	fetch := func(pr, repo string) ([]CICheckResult, error) {
		callCount++
		if callCount < 4 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	log := &testLog{}
	_, _, err := waitForCI(context.Background(), fetch, "42", "", "", 1*time.Second, 5*time.Minute, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have exactly 1 log line: the polling summary. Callers handle
	// the "passed" line with more context (e.g. "CI passed — merging").
	if len(log.messages) != 1 {
		t.Fatalf("expected 1 log message (polling summary only), got %d: %v", len(log.messages), log.messages)
	}

	if !strings.Contains(log.messages[0], "polled 1s..2s..4s") {
		t.Errorf("expected polling line with backoff schedule, got: %s", log.messages[0])
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

	_, status, err := waitForCI(ctx, fetch, "1", "", "", 1*time.Second, 10*time.Second, discardLog{})
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
	stub := &StubGitHub{}
	mgr := &Manager{GitHub: stub}
	if mgr.gh() != stub {
		t.Error("gh() should return the injected GitHub interface")
	}
}


// AwaitCI returns CIPassed immediately when checks already pass,
// without entering the polling loop.
func TestAwaitCI_PassedImmediately(t *testing.T) {
	gh := &StubGitHub{
		Checks: []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}},
	}
	mgr := &Manager{GitHub: gh, Logger: &testLog{}}

	checks, status, err := mgr.AwaitCI(context.Background(), "1", "repo", "")
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
	gh := &StubGitHub{
		Checks: []CICheckResult{{Name: "lint", State: "FAILURE", Bucket: "fail"}},
	}
	mgr := &Manager{GitHub: gh, Logger: &testLog{}}

	checks, status, err := mgr.AwaitCI(context.Background(), "1", "repo", "")
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

// AwaitCI polls until pending checks resolve, proving it delegates to
// waitForCI when the initial fetch returns pending results.
func TestAwaitCI_PollsWhenPending(t *testing.T) {
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { ciSleep = origSleep }()

	var calls atomic.Int32
	gh := &StubGitHub{}
	mgr := &Manager{GitHub: gh, Logger: &testLog{}}

	// Override ListChecks to transition from pending to passed.
	pollGH := &pollableGitHub{
		StubGitHub: *gh,
		listChecks: func(pr, repo string) ([]CICheckResult, error) {
			n := calls.Add(1)
			if n < 3 {
				return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
			}
			return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
		},
	}
	mgr.GitHub = pollGH

	_, status, err := mgr.AwaitCI(context.Background(), "1", "repo", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
	if calls.Load() < 3 {
		t.Errorf("expected at least 3 polls, got %d", calls.Load())
	}
}

// AwaitCI polls when the initial fetch returns an error (checks not yet
// registered), proving it handles the "CI not available" case.
func TestAwaitCI_PollsWhenFetchErrors(t *testing.T) {
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { ciSleep = origSleep }()

	var calls atomic.Int32
	pollGH := &pollableGitHub{
		StubGitHub: StubGitHub{},
		listChecks: func(pr, repo string) ([]CICheckResult, error) {
			n := calls.Add(1)
			if n < 2 {
				return nil, fmt.Errorf("no checks yet")
			}
			return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
		},
	}
	mgr := &Manager{GitHub: pollGH, Logger: &testLog{}}

	_, status, err := mgr.AwaitCI(context.Background(), "1", "repo", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
}

// AwaitCI log output uses logging.PRLink for clickable terminal links
// when a valid GitHub repoURL is provided.
func TestAwaitCI_LogUsesPRLink(t *testing.T) {
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { ciSleep = origSleep }()

	var calls atomic.Int32
	pollGH := &pollableGitHub{
		StubGitHub: StubGitHub{},
		listChecks: func(pr, repo string) ([]CICheckResult, error) {
			n := calls.Add(1)
			if n < 2 {
				return nil, fmt.Errorf("no checks yet")
			}
			return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
		},
	}
	log := &testLog{}
	mgr := &Manager{GitHub: pollGH, Logger: log}

	_, status, err := mgr.AwaitCI(context.Background(), "99", "https://github.com/owner/repo", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}

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

// AwaitCI with expectedSHA logs PRLink clickable terminal links during SHA polling.
func TestAwaitCI_SHAPollLogUsesPRLink(t *testing.T) {
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { ciSleep = origSleep }()

	pollGH := &pollableGitHub{
		StubGitHub: StubGitHub{
			Checks: []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}},
		},
		listChecks: func(pr, repo string) ([]CICheckResult, error) {
			return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
		},
		getPRHeadSHA: func(workDir, prNumber string) (string, error) {
			return "newsha123", nil
		},
	}
	log := &testLog{}
	mgr := &Manager{GitHub: pollGH, Logger: log}

	_, status, err := mgr.AwaitCI(context.Background(), "88", "https://github.com/owner/repo", "newsha123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}

	// "Waiting for" and "HEAD confirmed" lines must contain clickable links.
	for _, msg := range log.messages {
		if (strings.Contains(msg, "Waiting for") || strings.Contains(msg, "HEAD confirmed")) &&
			!strings.Contains(msg, "github.com/owner/repo/pull/88") {
			t.Errorf("expected PRLink hyperlink in log, got: %s", msg)
		}
	}
}

// waitForCI polling summary log lines use PRLink for clickable links.
func TestWaitForCI_LogUsesPRLink(t *testing.T) {
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { ciSleep = origSleep }()

	callCount := 0
	fetch := func(pr, repo string) ([]CICheckResult, error) {
		callCount++
		if callCount < 3 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	log := &testLog{}
	_, _, err := waitForCI(context.Background(), fetch, "77", "https://github.com/owner/repo", "owner/repo", 1*time.Second, 5*time.Minute, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, msg := range log.messages {
		if strings.Contains(msg, "polled") && !strings.Contains(msg, "github.com/owner/repo/pull/77") {
			t.Errorf("expected PRLink hyperlink in polling log, got: %s", msg)
		}
	}
}

// AwaitCI with an expected SHA polls until the PR HEAD matches that SHA
// before returning CI results, preventing stale results after a push.
func TestAwaitCI_WaitsForExpectedSHA(t *testing.T) {
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { ciSleep = origSleep }()

	var shaCalls atomic.Int32
	pollGH := &pollableGitHub{
		StubGitHub: StubGitHub{
			Checks: []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}},
		},
		listChecks: func(pr, repo string) ([]CICheckResult, error) {
			return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
		},
		getPRHeadSHA: func(workDir, prNumber string) (string, error) {
			n := shaCalls.Add(1)
			if n < 3 {
				return "stalesha", nil
			}
			return "expectedsha", nil
		},
	}
	log := &testLog{}
	mgr := &Manager{GitHub: pollGH, Logger: log}

	checks, status, err := mgr.AwaitCI(context.Background(), "1", "repo", "expectedsha")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
	if len(checks) != 1 || checks[0].Name != "test" {
		t.Errorf("unexpected checks: %v", checks)
	}
	// Must have polled GetPRHeadSHA at least 3 times (2 stale + 1 match).
	if shaCalls.Load() < 3 {
		t.Errorf("expected at least 3 SHA polls, got %d", shaCalls.Load())
	}
}

// AwaitCI with empty expectedSHA skips SHA verification and returns
// results immediately when checks are already resolved.
func TestAwaitCI_EmptySHASkipsVerification(t *testing.T) {
	gh := &StubGitHub{
		Checks: []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}},
	}
	mgr := &Manager{GitHub: gh, Logger: &testLog{}}

	checks, status, err := mgr.AwaitCI(context.Background(), "1", "repo", "")
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

// awaitHeadSHA logs progress messages while polling when the SHA does not
// match immediately, so long-running retarget waits are not silent.
func TestAwaitHeadSHA_LogsProgressWhilePolling(t *testing.T) {
	stubCISleep(t)

	origInterval := awaitHeadSHAProgressInterval
	awaitHeadSHAProgressInterval = 0 // trigger progress log on every poll
	t.Cleanup(func() { awaitHeadSHAProgressInterval = origInterval })

	var shaCalls atomic.Int32
	pollGH := &pollableGitHub{
		StubGitHub: StubGitHub{
			Checks: []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		},
		listChecks: func(pr, repo string) ([]CICheckResult, error) {
			return []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}, nil
		},
		getPRHeadSHA: func(workDir, prNumber string) (string, error) {
			n := shaCalls.Add(1)
			if n < 3 {
				return "stalesha", nil
			}
			return "targetsha", nil
		},
	}
	log := &testLog{}
	mgr := &Manager{GitHub: pollGH, Logger: log}

	_, status, err := mgr.AwaitCI(context.Background(), "55", "https://github.com/owner/repo", "targetsha")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}

	// At least one "Still waiting" progress log must appear — the SHA polling
	// loop is not silent during a long retarget wait.
	if !log.contains("Still waiting") {
		t.Errorf("expected progress log during SHA polling, got: %v", log.messages)
	}
}

// pollableGitHub wraps StubGitHub but allows overriding ListChecks
// with a function that changes behavior across calls.
type pollableGitHub struct {
	StubGitHub
	listChecks   func(string, string) ([]CICheckResult, error)
	getPRHeadSHA func(string, string) (string, error)
}

func (p *pollableGitHub) ListChecks(prNumber, repoURL string) ([]CICheckResult, error) {
	if p.listChecks != nil {
		return p.listChecks(prNumber, repoURL)
	}
	return p.StubGitHub.ListChecks(prNumber, repoURL)
}

func (p *pollableGitHub) GetPRHeadSHA(workDir, prNumber string) (string, error) {
	if p.getPRHeadSHA != nil {
		return p.getPRHeadSHA(workDir, prNumber)
	}
	return p.StubGitHub.GetPRHeadSHA(workDir, prNumber)
}

// setupAutoMergeManager creates a Manager with a StubGitHub and real git repos
// so AutoMergeCurrentBranch can run without a real gh CLI.
func setupAutoMergeManager(t *testing.T, gh *StubGitHub) *Manager {
	t.Helper()
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:  project,
		BaseBranch: "main",
		RalphDir:    ralphDir,
				GitHub:      gh,
		State:       st,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	mgr.RenameBranchForTask("test feature", "")
	// Set PRHeadSHA to match the worktree HEAD so AwaitCI's SHA verification
	// doesn't block forever in tests.
	gh.PRHeadSHA = mgr.gitOutput(mgr.WorkDir, "rev-parse", "HEAD")
	return mgr
}

// executeMerge retries the merge after CI passes when the initial merge
// is blocked by branch protection.
func TestExecuteMerge_RetriesAfterCIGate(t *testing.T) {
	origSleep := ciSleep
	ciSleep = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	defer func() { ciSleep = origSleep }()

	calls := 0
	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      "10",
		Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
	}
	mgr := setupAutoMergeManager(t, gh)

	seqGH := &StubGitHub{
		IsAvailable: gh.IsAvailable,
		OpenPR:      gh.OpenPR,
		Checks:      gh.Checks,
		MergeResults: []MergeResult{
			{Blocked: true, Message: "Base branch policy prohibits the merge"},
			{Merged: true},
		},
		OnMerge: func() { calls++ },
	}
	mgr.GitHub = seqGH

	merged, err := mgr.executeMerge(context.Background(), "10", "https://github.com/test/repo.git")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !merged {
		t.Error("expected merge to succeed on retry")
	}
	if calls != 2 {
		t.Errorf("expected 2 merge calls (initial + retry), got %d", calls)
	}
}

// AutoMergeCurrentBranch returns a MergeConflictError when the gh merge
// command reports merge conflicts, so the caller can rebase and retry.
func TestAutoMerge_MergeConflictReturnsTypedError(t *testing.T) {
	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      "42",
		Checks:      []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}},
		MergeResult: MergeResult{Conflict: true, Message: "merge conflict"},
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
	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      "42",
		Checks: []CICheckResult{
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
	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      "42",
		Checks:      []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}},
	}

	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir: project,
		BaseBranch: "main",
		RalphDir:   ralphDir,
		GitHub:     gh,
		State:      st,
		Logger:     &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("test feature", "")
	gh.PRHeadSHA = mgr.gitOutput(mgr.WorkDir, "rev-parse", "HEAD")
	mgr.AutoMergeCurrentBranch(context.Background())

	if !gh.LastMergeOpts.DeleteBranch {
		t.Error("merge should always set DeleteBranch")
	}
}

