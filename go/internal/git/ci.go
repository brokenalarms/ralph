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

// DefaultCIPollInterval is the time between CI status checks.
const DefaultCIPollInterval = 15 * time.Second

// DefaultCIPollTimeout is the maximum time to wait for CI checks to complete.
const DefaultCIPollTimeout = 10 * time.Minute

// evaluateChecks determines the overall CI status from individual check results.
// If any check has failed, returns CIFailed. If all non-neutral checks have
// completed (pass or fail), returns accordingly. Pending checks are only
// blocking if no check has passed yet — this avoids hanging on deployment
// checks (e.g. Netlify rules) that can stay pending indefinitely.
func evaluateChecks(checks []CICheckResult) CIStatus {
	if len(checks) == 0 {
		return CIPending
	}

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

// waitForCI polls PR checks until they complete or timeout is reached.
// The fetch parameter controls how checks are retrieved — production code
// passes fetchPRChecks; tests can inject a stub.
func waitForCI(ctx context.Context, fetch CIFetchFunc, prNumber, repoURL string, interval, timeout time.Duration, log Log) ([]CICheckResult, CIStatus, error) {
	deadline := time.Now().Add(timeout)

	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}

	for {
		checks, err := fetch(prNumber, repoURL)
		if err != nil {
			if time.Now().After(deadline) {
				return nil, CIPending, fmt.Errorf("CI checks not available within %v: %w", timeout, err)
			}
			log.Log("ci", "CI checks not available yet for PR #%s, polling in %v...", prNumber, interval)
			select {
			case <-done:
				return nil, CIPending, fmt.Errorf("interrupted")
			case <-time.After(interval):
			}
			continue
		}

		status := evaluateChecks(checks)
		switch status {
		case CIPassed:
			log.Log("ci", "CI checks passed for PR #%s", prNumber)
			return checks, CIPassed, nil
		case CIFailed:
			log.Warn("ci", "CI checks failed for PR #%s", prNumber)
			return checks, CIFailed, nil
		}

		if time.Now().After(deadline) {
			log.Warn("ci", "CI poll timeout reached for PR #%s", prNumber)
			return checks, CIPending, fmt.Errorf("CI checks did not complete within %v", timeout)
		}

		log.Log("ci", "CI checks pending for PR #%s, polling in %v...", prNumber, interval)
		select {
		case <-done:
			return nil, CIPending, fmt.Errorf("interrupted")
		case <-time.After(interval):
		}
	}
}
