package git

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// CICheckResult represents the status of a single CI check from gh pr checks.
// gh pr checks --json returns: name, state (SUCCESS/FAILURE/PENDING/CANCELLED),
// bucket (pass/fail/pending), startedAt.
type CICheckResult struct {
	Name       string    `json:"name"`
	State      string    `json:"state"`
	Bucket     string    `json:"bucket"`
	IsRequired bool      `json:"isRequired"`
	StartedAt  time.Time `json:"startedAt"`
}

// CIStatus summarizes the overall state of all CI checks on a PR.
type CIStatus int

const (
	CIPending CIStatus = iota
	CIPassed
	CIFailed
)

// CIFailureError is returned when CI checks fail on a PR.
type CIFailureError struct {
	PRNumber int
	Failures []CICheckResult
}

func (e *CIFailureError) Error() string {
	var names []string
	for _, f := range e.Failures {
		names = append(names, f.Name)
	}
	return fmt.Sprintf("CI checks failed on PR #%d: %s", e.PRNumber, strings.Join(names, ", "))
}

// DefaultCIPollInterval is the initial time between CI status checks.
// Each subsequent poll doubles this interval up to MaxCIPollInterval.
const DefaultCIPollInterval = 1 * time.Second

// MaxCIPollInterval caps the exponential backoff so polls don't grow too far apart.
const MaxCIPollInterval = 5 * time.Second

// DefaultCIPollTimeout is the maximum time to wait for CI checks to complete.
const DefaultCIPollTimeout = 10 * time.Minute

// ciSleep is the function used to create timer channels in waitForCI.
// Tests override this to avoid real sleeps.
var ciSleep = func(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// evaluateChecks determines the overall CI status from individual check results.
// All checks are blocking — any failure returns CIFailed, any pending check
// returns CIPending. CIPassed is only returned when every check has resolved
// successfully.
func evaluateChecks(checks []CICheckResult) CIStatus {
	if len(checks) == 0 {
		return CIPending
	}

	allResolved := true
	for _, c := range checks {
		if c.Bucket == "fail" || c.State == "FAILURE" || c.State == "CANCELLED" {
			return CIFailed
		}
		if c.Bucket == "pending" || c.State == "PENDING" || c.State == "IN_PROGRESS" {
			allResolved = false
		}
	}

	if allResolved {
		return CIPassed
	}
	return CIPending
}

// failedChecks returns only the checks that did not succeed.
func failedChecks(checks []CICheckResult) []CICheckResult {
	var failed []CICheckResult
	for _, c := range checks {
		if c.Bucket == "fail" || c.State == "FAILURE" || c.State == "CANCELLED" {
			failed = append(failed, c)
		}
	}
	return failed
}

// isInfrastructureFailure checks the GitHub Actions API to determine if CI
// failed due to infrastructure (billing, runner allocation) rather than actual
// test failures. A job with zero steps executed indicates it never ran.
func (m *Manager) isInfrastructureFailure(ctx context.Context, prNumber int) bool {
	nwo := NWOFromRemote(m.RemoteURL())
	if nwo == "" {
		return false
	}
	gh := m.gh()
	if gh == nil || !gh.Available() {
		return false
	}
	steps, err := gh.GetJobStepCount(nwo, prNumber)
	if err != nil {
		return false
	}
	return steps == 0
}

// RequiredFailedChecks returns failed checks that the fix agent should address.
// Currently returns all failed checks — gh pr checks does not expose an
// isRequired field, so we treat every check as required.
func RequiredFailedChecks(checks []CICheckResult) []CICheckResult {
	var failed []CICheckResult
	for _, c := range checks {
		if c.Bucket == "fail" {
			failed = append(failed, c)
		}
	}
	return failed
}

// ErrStackedPRWaiting is returned when a PR targets a non-main branch
// and must wait for the base PR to merge first. This is not a failure —
// it's expected stacking behavior and should not count as a merge failure.
var ErrStackedPRWaiting = fmt.Errorf("stacked PR waiting for base to merge")

// MergeConflictError is returned when a PR cannot be merged due to conflicts.
type MergeConflictError struct {
	PRNumber int
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("PR #%d has merge conflicts with the base branch", e.PRNumber)
}

// UnresolvedConflictError is returned when a merge conflict could not be
// auto-resolved by rebasing. Retrying will not help — the conflict requires
// manual or agent-driven resolution.
type UnresolvedConflictError struct {
	PRNumber int
}

func (e *UnresolvedConflictError) Error() string {
	return fmt.Sprintf("PR #%d has unresolvable merge conflicts — auto-resolve failed", e.PRNumber)
}



// CIFetchFunc is the signature for fetching PR check status.
type CIFetchFunc func(prNumber int, repoURL string) ([]CICheckResult, error)

// AwaitCI fetches CI check status for a PR and polls until checks resolve.
// When pushedAt is non-zero, filters out checks that started before the push
// so only fresh CI results are evaluated. This prevents stale results from a
// previous push from gating the merge.
func (m *Manager) AwaitCI(ctx context.Context, prNumber int, repoURL string, pushedAt time.Time) ([]CICheckResult, CIStatus, error) {
	nwo := NWOFromRemote(repoURL)
	gh := m.gh()

	fetch := gh.ListChecks
	if !pushedAt.IsZero() {
		m.Logger.Emit(logging.Opts{Domain: logging.CI, Link: logging.PRLinkOpt(nwo, prNumber)}, "Waiting for fresh CI checks (pushed at %s)...", pushedAt.Format("15:04:05"))
		baseFetch := fetch
		fetch = func(prNumber int, repoURL string) ([]CICheckResult, error) {
			checks, err := baseFetch(prNumber, repoURL)
			if err != nil {
				return nil, err
			}
			var fresh []CICheckResult
			for _, c := range checks {
				if !c.StartedAt.IsZero() && c.StartedAt.Before(pushedAt) {
					continue
				}
				fresh = append(fresh, c)
			}
			return fresh, nil
		}
	}

	checks, fetchErr := fetch(prNumber, repoURL)
	if fetchErr != nil || len(checks) == 0 {
		m.Logger.Emit(logging.Opts{Domain: logging.CI, Link: logging.PRLinkOpt(nwo, prNumber)}, "CI checks not available yet — waiting...")
		return waitForCI(ctx, fetch, prNumber, repoURL, nwo, DefaultCIPollInterval, DefaultCIPollTimeout, m.Logger)
	}
	status := evaluateChecks(checks)
	if status != CIPending {
		return checks, status, nil
	}
	return waitForCI(ctx, fetch, prNumber, repoURL, nwo, DefaultCIPollInterval, DefaultCIPollTimeout, m.Logger)
}

// waitForCI polls PR checks until they complete or timeout is reached.
// Uses exponential backoff starting at interval, doubling each poll up to
// MaxCIPollInterval. Emits a single in-place log line that grows as polls
// accumulate (e.g. "CI polled 1s..2s..4s"), finalizing it to the log file
// on completion. Emits nothing when CI resolves on the first fetch.
func waitForCI(ctx context.Context, fetch CIFetchFunc, prNumber int, repoURL, nwo string, interval, timeout time.Duration, log Log) ([]CICheckResult, CIStatus, error) {
	deadline := time.Now().Add(timeout)
	prLink := logging.PRLinkOpt(nwo, prNumber)

	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}

	currentInterval := interval
	var polled bool

	emitPoll := func(duration string) {
		if !polled {
			log.EmitInPlace(logging.Opts{Domain: logging.CI, Link: prLink}, "CI polled %s", duration)
		} else {
			log.EmitAppend("..%s", duration)
		}
		polled = true
	}

	finalize := func() {
		if polled {
			log.EmitFinalInPlace()
		}
	}

	for {
		checks, err := fetch(prNumber, repoURL)
		if err != nil {
			if time.Now().After(deadline) {
				finalize()
				return nil, CIPending, fmt.Errorf("CI checks not available within %v: %w", timeout, err)
			}
			emitPoll(formatDuration(currentInterval))
			select {
			case <-done:
				finalize()
				return nil, CIPending, fmt.Errorf("interrupted")
			case <-ciSleep(currentInterval):
			}
			currentInterval = nextBackoff(currentInterval)
			continue
		}

		status := evaluateChecks(checks)
		switch status {
		case CIPassed:
			finalize()
			return checks, CIPassed, nil
		case CIFailed:
			finalize()
			return checks, CIFailed, nil
		}

		if time.Now().After(deadline) {
			finalize()
			return checks, CIPending, fmt.Errorf("CI checks did not complete within %v", timeout)
		}

		emitPoll(formatDuration(currentInterval))
		select {
		case <-done:
			finalize()
			return nil, CIPending, fmt.Errorf("interrupted")
		case <-ciSleep(currentInterval):
		}
		currentInterval = nextBackoff(currentInterval)
	}
}

// nextBackoff doubles the interval, capping at MaxCIPollInterval.
func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > MaxCIPollInterval {
		return MaxCIPollInterval
	}
	return next
}

// formatDuration returns a compact human-readable duration (e.g. "1s", "2s", "15s").
func formatDuration(d time.Duration) string {
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
