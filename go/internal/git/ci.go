package git

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CICheckResult represents the status of a single CI check from gh pr checks.
// gh pr checks --json returns: name, state (SUCCESS/FAILURE/PENDING/CANCELLED),
// bucket (pass/fail/pending).
type CICheckResult struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Bucket string `json:"bucket"`
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
	PRNumber string
	Failures []CICheckResult
}

func (e *CIFailureError) Error() string {
	var names []string
	for _, f := range e.Failures {
		names = append(names, f.Name)
	}
	return fmt.Sprintf("CI checks failed on PR #%s: %s", e.PRNumber, strings.Join(names, ", "))
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

// ErrStackedPRWaiting is returned when a PR targets a non-main branch
// and must wait for the base PR to merge first. This is not a failure —
// it's expected stacking behavior and should not count as a merge failure.
var ErrStackedPRWaiting = fmt.Errorf("stacked PR waiting for base to merge")

// MergeConflictError is returned when a PR cannot be merged due to conflicts.
type MergeConflictError struct {
	PRNumber string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("PR #%s has merge conflicts with the base branch", e.PRNumber)
}

// UnresolvedConflictError is returned when a merge conflict could not be
// auto-resolved by rebasing. Retrying will not help — the conflict requires
// manual or agent-driven resolution.
type UnresolvedConflictError struct {
	PRNumber string
}

func (e *UnresolvedConflictError) Error() string {
	return fmt.Sprintf("PR #%s has unresolvable merge conflicts — auto-resolve failed", e.PRNumber)
}

// isCIGatedError returns true if the merge error indicates branch protection
// is blocking the merge (typically because CI checks haven't passed yet).
func isCIGatedError(mergeOutput string) bool {
	lower := strings.ToLower(mergeOutput)
	patterns := []string{
		"base branch policy prohibits the merge",
		"required status check",
		"merge requirements were not satisfied",
		"pull request review is required",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// isMergeConflictError returns true if the merge error indicates the PR
// has conflicts with the base branch that prevent merging.
func isMergeConflictError(mergeOutput string) bool {
	lower := strings.ToLower(mergeOutput)
	patterns := []string{
		"merge conflict",
		"not mergeable",
		"pull request is not mergeable",
		"head branch was behind the base branch",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// CIFetchFunc is the signature for fetching PR check status.
type CIFetchFunc func(prNumber, repoURL string) ([]CICheckResult, error)

// AwaitCI fetches CI check status for a PR and polls until checks resolve.
// Returns the final checks, their aggregate status, and any polling error.
// Reusable by AutoMerge, fix-CI flows, and any caller that needs to wait
// for CI to complete on a PR.
func (m *Manager) AwaitCI(ctx context.Context, prNumber, repoURL string) ([]CICheckResult, CIStatus, error) {
	fetch := m.gh().ListChecks
	checks, fetchErr := fetch(prNumber, repoURL)
	if fetchErr != nil || len(checks) == 0 {
		m.Logger.Log("ci", "CI checks not available yet for PR #%s — waiting...", prNumber)
		return waitForCI(ctx, fetch, prNumber, repoURL, DefaultCIPollInterval, DefaultCIPollTimeout, m.Logger)
	}
	status := evaluateChecks(checks)
	if status != CIPending {
		return checks, status, nil
	}
	return waitForCI(ctx, fetch, prNumber, repoURL, DefaultCIPollInterval, DefaultCIPollTimeout, m.Logger)
}

// AwaitFreshCI waits for CI checks that match the given commit SHA.
// After a force-push, old CI results are stale — this polls until checks
// for the new HEAD appear and resolve.
func (m *Manager) AwaitFreshCI(ctx context.Context, prNumber, repoURL, expectedSHA string) ([]CICheckResult, CIStatus, error) {
	if expectedSHA == "" {
		return m.AwaitCI(ctx, prNumber, repoURL)
	}
	gh := m.gh()
	fetch := func(pr, repo string) ([]CICheckResult, error) {
		// Check if the PR's HEAD matches the expected SHA.
		currentSHA, _ := gh.GetPRHeadSHA(m.WorkDir, pr)
		if currentSHA != expectedSHA {
			// GitHub hasn't registered the push yet.
			return nil, fmt.Errorf("PR HEAD is %s, waiting for %s", currentSHA[:min(7, len(currentSHA))], expectedSHA[:min(7, len(expectedSHA))])
		}
		return gh.ListChecks(pr, repo)
	}
	m.Logger.Log("ci", "Waiting for fresh CI on PR #%s (commit %s)...", prNumber, expectedSHA[:min(7, len(expectedSHA))])
	return waitForCI(ctx, fetch, prNumber, repoURL, DefaultCIPollInterval, DefaultCIPollTimeout, m.Logger)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// waitForCI polls PR checks until they complete or timeout is reached.
// Uses exponential backoff starting at interval, doubling each poll up to
// MaxCIPollInterval. Logs a single updating line showing accumulated poll
// durations instead of one line per poll.
func waitForCI(ctx context.Context, fetch CIFetchFunc, prNumber, repoURL string, interval, timeout time.Duration, log Log) ([]CICheckResult, CIStatus, error) {
	deadline := time.Now().Add(timeout)

	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}

	currentInterval := interval
	var pollDurations []string

	for {
		checks, err := fetch(prNumber, repoURL)
		if err != nil {
			if time.Now().After(deadline) {
				return nil, CIPending, fmt.Errorf("CI checks not available within %v: %w", timeout, err)
			}
			pollDurations = append(pollDurations, formatDuration(currentInterval))
			select {
			case <-done:
				return nil, CIPending, fmt.Errorf("interrupted")
			case <-ciSleep(currentInterval):
			}
			currentInterval = nextBackoff(currentInterval)
			continue
		}

		status := evaluateChecks(checks)
		switch status {
		case CIPassed:
			if len(pollDurations) > 0 {
				log.Log("ci", "CI polled %s for PR #%s", strings.Join(pollDurations, ".."), prNumber)
			}
			return checks, CIPassed, nil
		case CIFailed:
			if len(pollDurations) > 0 {
				log.Log("ci", "CI polled %s for PR #%s", strings.Join(pollDurations, ".."), prNumber)
			}
			return checks, CIFailed, nil
		}

		if time.Now().After(deadline) {
			if len(pollDurations) > 0 {
				log.Log("ci", "CI polled %s for PR #%s", strings.Join(pollDurations, ".."), prNumber)
			}
			return checks, CIPending, fmt.Errorf("CI checks did not complete within %v", timeout)
		}

		pollDurations = append(pollDurations, formatDuration(currentInterval))
		select {
		case <-done:
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
