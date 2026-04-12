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

// mergeOpts always sets DeleteBranch and never sets Admin (removed).
func TestMergeOpts_Defaults(t *testing.T) {
	mgr := &Repo{}
	opts := mgr.mergeOpts()
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

	checks, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Second, discardLog{})
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

	_, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Second, discardLog{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIFailed {
		t.Errorf("expected CIFailed, got %v", status)
	}
}

func TestWaitForCI_TimesOut(t *testing.T) {
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
	}

	_, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Millisecond, 5*time.Millisecond, discardLog{})
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
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		callCount++
		if callCount < 6 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	_, status, err := waitForCI(context.Background(), fetch, 1, "", "", 1*time.Second, 5*time.Minute, discardLog{})
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

// waitForCI emits exactly one log line regardless of poll count — each poll
// appends ~4 chars rather than rewriting the line, so 10 polls produce one line.
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
	_, _, err := waitForCI(context.Background(), fetch, 42, "", "", 1*time.Second, 5*time.Minute, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 10 pending polls produce exactly one "CI polled" line in the log.
	if len(log.messages) != 1 {
		t.Fatalf("expected exactly 1 log message for 10 polls, got %d: %v", len(log.messages), log.messages)
	}
	if !strings.Contains(log.messages[0], "polled 1s..2s..4s") {
		t.Errorf("expected polling summary with backoff schedule, got: %s", log.messages[0])
	}
}

// waitForCI emits no log line when CI resolves on the first fetch
// without any polling being needed.
func TestWaitForCI_NoLogLineWhenResolvedImmediately(t *testing.T) {
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	log := &testLog{}
	_, status, err := waitForCI(context.Background(), fetch, 42, "", "", 1*time.Second, 5*time.Minute, log)
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

// waitForCI returns immediately when the context is cancelled, proving
// that Ctrl-C interrupts CI polling instead of blocking until timeout.
func TestWaitForCI_CancelledContext(t *testing.T) {
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, status, err := waitForCI(ctx, fetch, 1, "", "", 1*time.Second, 10*time.Second, discardLog{})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if status != CIPending {
		t.Errorf("expected CIPending, got %v", status)
	}
}

// gh() returns the default ghCLI when no GitHub interface is injected.
func TestGh_DefaultsToGhCLI(t *testing.T) {
	mgr := &Repo{}
	gh := mgr.gh()
	if gh == nil {
		t.Fatal("gh() returned nil")
	}
}

// gh() returns the injected GitHub interface when one is set.
func TestGh_UsesInjectedGitHub(t *testing.T) {
	stub := NewStubGitHub()
	mgr := &Repo{github: stub}
	if mgr.gh() != stub {
		t.Error("gh() should return the injected GitHub interface")
	}
}


// AwaitCI returns CIPassed immediately when checks already pass,
// without entering the polling loop.
func TestAwaitCI_PassedImmediately(t *testing.T) {
	gh := NewStubGitHub()
	gh.Checks = []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}
	mgr := &Repo{github: gh, logger: &testLog{}}

	checks, status, err := mgr.AwaitCI(context.Background(), 1, "repo", time.Time{})
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
	gh := NewStubGitHub()
	gh.Checks = []CICheckResult{{Name: "lint", State: "FAILURE", Bucket: "fail"}}
	mgr := &Repo{github: gh, logger: &testLog{}}

	checks, status, err := mgr.AwaitCI(context.Background(), 1, "repo", time.Time{})
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
	gh := NewStubGitHub()
	mgr := &Repo{github: gh, logger: &testLog{}}

	// Override ListChecks to transition from pending to passed.
	pollGH := &pollableGitHub{
		StubGitHub: *gh,
		listChecks: func(pr int, repo string) ([]CICheckResult, error) {
			n := calls.Add(1)
			if n < 3 {
				return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
			}
			return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
		},
	}
	mgr.github = pollGH

	_, status, err := mgr.AwaitCI(context.Background(), 1, "repo", time.Time{})
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
		StubGitHub: *NewStubGitHub(),
		listChecks: func(pr int, repo string) ([]CICheckResult, error) {
			n := calls.Add(1)
			if n < 2 {
				return nil, fmt.Errorf("no checks yet")
			}
			return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
		},
	}
	mgr := &Repo{github: pollGH, logger: &testLog{}}

	_, status, err := mgr.AwaitCI(context.Background(), 1, "repo", time.Time{})
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
		StubGitHub: *NewStubGitHub(),
		listChecks: func(pr int, repo string) ([]CICheckResult, error) {
			n := calls.Add(1)
			if n < 2 {
				return nil, fmt.Errorf("no checks yet")
			}
			return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
		},
	}
	log := &testLog{}
	mgr := &Repo{github: pollGH, logger: log}

	_, status, err := mgr.AwaitCI(context.Background(), 99, "https://github.com/owner/repo", time.Time{})
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

// AwaitCI with pushedAt logs PRLink clickable terminal links.
func TestAwaitCI_PushedAtLogUsesPRLink(t *testing.T) {
	stubCISleep(t)

	now := time.Now()
	pollGH := &pollableGitHub{
		StubGitHub: *NewStubGitHub(),
		listChecks: func(pr int, repo string) ([]CICheckResult, error) {
			return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass", StartedAt: now.Add(time.Second)}}, nil
		},
	}
	log := &testLog{}
	mgr := &Repo{github: pollGH, logger: log}

	_, status, err := mgr.AwaitCI(context.Background(), 88, "https://github.com/owner/repo", now)
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
	fetch := func(pr int, repo string) ([]CICheckResult, error) {
		callCount++
		if callCount < 3 {
			return []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}, nil
		}
		return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}, nil
	}

	log := &testLog{}
	_, _, err := waitForCI(context.Background(), fetch, 77, "https://github.com/owner/repo", "owner/repo", 1*time.Second, 5*time.Minute, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, msg := range log.messages {
		if strings.Contains(msg, "polled") && !strings.Contains(msg, "github.com/owner/repo/pull/77") {
			t.Errorf("expected PRLink hyperlink in polling log, got: %s", msg)
		}
	}
}

// AwaitCI with pushedAt filters out stale checks (started before push)
// and polls until fresh checks appear.
func TestAwaitCI_FiltersStaleChecks(t *testing.T) {
	stubCISleep(t)

	pushedAt := time.Now()
	var calls atomic.Int32
	pollGH := &pollableGitHub{
		StubGitHub: *NewStubGitHub(),
		listChecks: func(pr int, repo string) ([]CICheckResult, error) {
			n := calls.Add(1)
			if n < 3 {
				// Only stale checks exist — started before the push.
				return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass", StartedAt: pushedAt.Add(-time.Minute)}}, nil
			}
			// Fresh checks appear.
			return []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass", StartedAt: pushedAt.Add(time.Second)}}, nil
		},
	}
	log := &testLog{}
	mgr := &Repo{github: pollGH, logger: log}

	checks, status, err := mgr.AwaitCI(context.Background(), 1, "repo", pushedAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != CIPassed {
		t.Errorf("expected CIPassed, got %v", status)
	}
	if len(checks) != 1 || checks[0].Name != "test" {
		t.Errorf("unexpected checks: %v", checks)
	}
	if calls.Load() < 3 {
		t.Errorf("expected at least 3 polls (2 stale + 1 fresh), got %d", calls.Load())
	}
}

// AwaitCI with zero pushedAt skips filtering and returns results immediately.
func TestAwaitCI_ZeroPushedAtSkipsFiltering(t *testing.T) {
	gh := NewStubGitHub()
	gh.Checks = []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}
	mgr := &Repo{github: gh, logger: &testLog{}}

	checks, status, err := mgr.AwaitCI(context.Background(), 1, "repo", time.Time{})
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

// pollableGitHub wraps StubGitHub but allows overriding ListChecks
// with a function that changes behavior across calls.
type pollableGitHub struct {
	StubGitHub
	listChecks func(int, string) ([]CICheckResult, error)
}

func (p *pollableGitHub) ListChecks(prNumber int, repoURL string) ([]CICheckResult, error) {
	if p.listChecks != nil {
		return p.listChecks(prNumber, repoURL)
	}
	return p.StubGitHub.ListChecks(prNumber, repoURL)
}

// setupAutoMergeManager creates a Manager with a StubGitHub and real git repos
// so AutoMergeCurrentBranch can run without a real gh CLI.
func setupAutoMergeManager(t *testing.T, gh *StubGitHub) *Repo {
	t.Helper()
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Repo{
		ProjectDir: project,
		baseBranch: "main",
		ralphDir:   ralphDir,
		github:     gh,
		state:      st,
		logger:     &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	mgr.RenameBranchForTask("test feature", "")
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
	gh := NewStubGitHub()
	gh.OpenPR = 10
	gh.Checks = []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}
	mgr := setupAutoMergeManager(t, gh)

	seqGH := NewStubGitHub()
	seqGH.OpenPR = gh.OpenPR
	seqGH.Checks = gh.Checks
	seqGH.MergeResults = []MergeResult{
		{Blocked: true, Message: "Base branch policy prohibits the merge"},
		{Merged: true},
	}
	seqGH.OnMerge = func() { calls++ }
	mgr.github = seqGH

	merged, err := mgr.executeMerge(context.Background(), 10, "https://github.com/test/repo.git")
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
	gh := NewStubGitHub()
	gh.Checks = []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}
	gh.MergeResult = MergeResult{Conflict: true, Message: "merge conflict"}
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
	gh := NewStubGitHub()
	gh.Checks = []CICheckResult{{Name: "test", State: "FAILURE", Bucket: "fail"}}
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
	gh := NewStubGitHub()
	gh.Checks = []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}

	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Repo{
		ProjectDir: project,
		baseBranch: "main",
		ralphDir:   ralphDir,
		github:     gh,
		state:      st,
		logger:     &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("test feature", "")
	mgr.AutoMergeCurrentBranch(context.Background())

	if !gh.LastMergeOpts.DeleteBranch {
		t.Error("merge should always set DeleteBranch")
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
	gh := NewStubGitHub()
	gh.Checks = []CICheckResult{{Name: "test", State: "PENDING", Bucket: "pending"}}
	mgr := &Repo{github: gh, logger: &testLog{}, ciPollTimeout: 1 * time.Millisecond}

	_, status, err := mgr.AwaitCI(context.Background(), 1, "repo", time.Time{})
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
	gh := NewStubGitHub()
	gh.Checks = []CICheckResult{{Name: "test", State: "SUCCESS", Bucket: "pass"}}
	mgr := &Repo{github: gh, logger: &testLog{}}

	_, status, err := mgr.AwaitCI(context.Background(), 1, "repo", time.Time{})
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
	gh := NewStubGitHub()
	gh.Checks = []CICheckResult{
		{Name: "test", State: "SUCCESS", Bucket: "pass"},
		{Name: "netlify", State: "FAILURE", Bucket: "fail"}, // not required
	}
	gh.RequiredChecks = []string{"test"} // only "test" is required

	mgr := &Repo{github: gh, baseBranch: "main", logger: &testLog{}}

	_, status, err := mgr.AwaitCI(context.Background(), 1, "https://github.com/owner/repo.git", time.Time{})
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
	gh := NewStubGitHub()
	gh.Checks = []CICheckResult{
		{Name: "test", State: "FAILURE", Bucket: "fail"},   // required, failing
		{Name: "lint", State: "SUCCESS", Bucket: "pass"},   // required, passing
		{Name: "netlify", State: "SUCCESS", Bucket: "pass"}, // not required
	}
	gh.RequiredChecks = []string{"test", "lint"}

	mgr := &Repo{github: gh, baseBranch: "main", logger: &testLog{}}

	_, status, _ := mgr.AwaitCI(context.Background(), 1, "https://github.com/owner/repo.git", time.Time{})
	if status != CIFailed {
		t.Errorf("expected CIFailed (required check 'test' failing), got %v", status)
	}
}

// AwaitCI evaluates all checks when GetRequiredChecks returns empty, preserving
// existing behavior for repos without branch protection rules configured.
func TestAwaitCI_RequiredChecksFilter_FallsBackToAllChecksWhenEmpty(t *testing.T) {
	gh := NewStubGitHub()
	gh.Checks = []CICheckResult{
		{Name: "test", State: "SUCCESS", Bucket: "pass"},
		{Name: "netlify", State: "FAILURE", Bucket: "fail"},
	}
	gh.RequiredChecks = nil // no required checks configured

	mgr := &Repo{github: gh, baseBranch: "main", logger: &testLog{}}

	_, status, _ := mgr.AwaitCI(context.Background(), 1, "https://github.com/owner/repo.git", time.Time{})
	if status != CIFailed {
		t.Errorf("expected CIFailed (no required checks → evaluate all, netlify fails), got %v", status)
	}
}

