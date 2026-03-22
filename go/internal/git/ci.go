package git

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CICheckResult represents the status of a single CI check from gh pr checks.
type CICheckResult struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Conclusion string `json:"conclusion"`
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

// fetchPRChecks queries gh for the current CI check status of a PR.
func fetchPRChecks(prNumber, repoURL string) ([]CICheckResult, error) {
	args := []string{"pr", "checks", prNumber, "--json", "name,state,conclusion"}
	if repoURL != "" {
		args = append(args, "-R", repoURL)
	}
	cmd := exec.Command("gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr checks failed: %w", err)
	}

	var checks []CICheckResult
	if err := json.Unmarshal(out, &checks); err != nil {
		return nil, fmt.Errorf("parsing check results: %w", err)
	}
	return checks, nil
}

// evaluateChecks determines the overall CI status from individual check results.
func evaluateChecks(checks []CICheckResult) CIStatus {
	if len(checks) == 0 {
		return CIPending
	}
	for _, c := range checks {
		if c.State != "COMPLETED" {
			return CIPending
		}
	}
	for _, c := range checks {
		if c.Conclusion == "FAILURE" || c.Conclusion == "CANCELLED" || c.Conclusion == "TIMED_OUT" {
			return CIFailed
		}
	}
	return CIPassed
}

// failedChecks returns only the checks that did not succeed.
func failedChecks(checks []CICheckResult) []CICheckResult {
	var failed []CICheckResult
	for _, c := range checks {
		if c.Conclusion != "SUCCESS" && c.Conclusion != "NEUTRAL" && c.Conclusion != "SKIPPED" {
			failed = append(failed, c)
		}
	}
	return failed
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

// waitForCI polls PR checks until they complete or timeout is reached.
// Returns the final check results and overall status.
func waitForCI(prNumber, repoURL string, interval, timeout time.Duration, log Log) ([]CICheckResult, CIStatus, error) {
	deadline := time.Now().Add(timeout)

	for {
		checks, err := fetchPRChecks(prNumber, repoURL)
		if err != nil {
			return nil, CIPending, err
		}

		status := evaluateChecks(checks)
		switch status {
		case CIPassed:
			log.Log("CI checks passed for PR #%s", prNumber)
			return checks, CIPassed, nil
		case CIFailed:
			log.Warn("CI checks failed for PR #%s", prNumber)
			return checks, CIFailed, nil
		}

		if time.Now().After(deadline) {
			log.Warn("CI poll timeout reached for PR #%s", prNumber)
			return checks, CIPending, fmt.Errorf("CI checks did not complete within %v", timeout)
		}

		log.Log("CI checks pending for PR #%s, polling in %v...", prNumber, interval)
		time.Sleep(interval)
	}
}
