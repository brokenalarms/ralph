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
	Name     string `json:"name"`
	State    string `json:"state"`
	Bucket   string `json:"bucket"`
	Required bool   `json:"required"`
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
const MaxCIPollInterval = 15 * time.Second

// DefaultCIPollTimeout is the maximum time to wait for CI checks to complete.
const DefaultCIPollTimeout = 10 * time.Minute

// ciSleep is the function used to create timer channels in waitForCI.
// Tests override this to avoid real sleeps.
var ciSleep = func(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// evaluateChecks determines the overall CI status from individual check results.
// Only required checks are blocking — non-required failures (e.g. Netlify
// deploy previews) are ignored. If any required check has failed, returns
// CIFailed. If all required checks pass, returns CIPassed even if optional
// checks fail.
func evaluateChecks(checks []CICheckResult) CIStatus {
	if len(checks) == 0 {
		return CIPending
	}

	hasRequired := false
	requiredFailed := false
	requiredPassed := false
	requiredAllResolved := true

	for _, c := range checks {
		failed := c.Bucket == "fail" || c.State == "FAILURE" || c.State == "CANCELLED"
		passed := c.Bucket == "pass" || c.State == "SUCCESS"
		pending := c.Bucket == "pending" || c.State == "PENDING" || c.State == "IN_PROGRESS"

		if c.Required {
			hasRequired = true
			if failed {
				requiredFailed = true
			} else if passed {
				requiredPassed = true
			} else if pending {
				requiredAllResolved = false
			}
		}
	}

	// If no checks are marked required, fall back to treating all as required.
	if !hasRequired {
		return evaluateAllChecks(checks)
	}

	if requiredFailed {
		return CIFailed
	}
	if requiredPassed && requiredAllResolved {
		return CIPassed
	}
	if requiredAllResolved {
		return CIPassed
	}
	return CIPending
}

// evaluateAllChecks is the fallback when no checks are marked required.
func evaluateAllChecks(checks []CICheckResult) CIStatus {
	hasFailed := false
	hasPassed := false
	allResolved := true
	for _, c := range checks {
		if c.Bucket == "fail" || c.State == "FAILURE" || c.State == "CANCELLED" {
			hasFailed = true
		} else if c.Bucket == "pass" || c.State == "SUCCESS" {
			hasPassed = true
		} else if c.Bucket == "pending" || c.State == "PENDING" || c.State == "IN_PROGRESS" {
			allResolved = false
		}
	}
	if hasFailed {
		return CIFailed
	}
	if hasPassed && !hasFailed {
		return CIPassed
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

// MergeConflictError is returned when a PR cannot be merged due to conflicts.
type MergeConflictError struct {
	PRNumber string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("PR #%s has merge conflicts with the base branch", e.PRNumber)
}

// DeferredMergeError indicates a PR targets a non-default branch and
// cannot be merged until its base branch is merged first.
type DeferredMergeError struct {
	PRNumber string
	PRBase   string
}

func (e *DeferredMergeError) Error() string {
	return fmt.Sprintf("PR #%s targets %s — waiting for base PRs to merge first", e.PRNumber, e.PRBase)
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
	m.Logger.Log("ci", "CI checks pending on PR #%s — waiting for completion...", prNumber)
	return waitForCI(ctx, fetch, prNumber, repoURL, DefaultCIPollInterval, DefaultCIPollTimeout, m.Logger)
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
				log.Log("ci", "CI checks pending for PR #%s (polled %s)", prNumber, strings.Join(pollDurations, ".."))
			}
			log.Log("ci", "CI checks passed for PR #%s", prNumber)
			return checks, CIPassed, nil
		case CIFailed:
			if len(pollDurations) > 0 {
				log.Log("ci", "CI checks pending for PR #%s (polled %s)", prNumber, strings.Join(pollDurations, ".."))
			}
			log.Warn("ci", "CI checks failed for PR #%s", prNumber)
			return checks, CIFailed, nil
		}

		if time.Now().After(deadline) {
			if len(pollDurations) > 0 {
				log.Log("ci", "CI checks pending for PR #%s (polled %s)", prNumber, strings.Join(pollDurations, ".."))
			}
			log.Warn("ci", "CI poll timeout reached for PR #%s", prNumber)
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
