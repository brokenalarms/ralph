package git

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CheckStatus represents the outcome of a single CI check.
type CheckStatus struct {
	Name       string
	Status     string // QUEUED, IN_PROGRESS, COMPLETED, WAITING, PENDING, REQUESTED
	Conclusion string // SUCCESS, FAILURE, CANCELLED, TIMED_OUT, ACTION_REQUIRED, STALE, NEUTRAL, SKIPPED, STARTUP_FAILURE, ""
}

// ChecksResult summarizes the state of all PR checks.
type ChecksResult struct {
	Checks  []CheckStatus
	AllDone bool
	Passed  bool
}

// MergeError wraps an auto-merge failure with structured details about what
// went wrong, allowing the loop to feed actionable information back.
type MergeError struct {
	PR      string
	Reason  string
	Checks  []CheckStatus
	RawText string
}

func (e *MergeError) Error() string {
	return fmt.Sprintf("auto-merge failed for PR #%s: %s", e.PR, e.Reason)
}

// PRChecks queries the current check status for a PR via gh pr checks.
func (m *Manager) PRChecks(prNumber, repoURL string) (ChecksResult, error) {
	cmd := exec.Command("gh", "pr", "checks", prNumber,
		"--json", "name,status,conclusion",
		"-R", repoURL)
	out, err := cmd.Output()
	if err != nil {
		return ChecksResult{}, fmt.Errorf("gh pr checks: %w", err)
	}

	var raw []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return ChecksResult{}, fmt.Errorf("parsing checks JSON: %w", err)
	}

	result := ChecksResult{AllDone: true, Passed: true}
	for _, r := range raw {
		cs := CheckStatus{
			Name:       r.Name,
			Status:     r.Status,
			Conclusion: r.Conclusion,
		}
		result.Checks = append(result.Checks, cs)

		if !isTerminal(r.Status) {
			result.AllDone = false
			result.Passed = false
		} else if r.Conclusion != "SUCCESS" && r.Conclusion != "NEUTRAL" && r.Conclusion != "SKIPPED" {
			result.Passed = false
		}
	}
	return result, nil
}

// WaitForPRChecks polls PR checks until all complete or the timeout expires.
// Returns the final checks result. If no checks exist, returns immediately
// with Passed=true (no checks to gate on).
func (m *Manager) WaitForPRChecks(prNumber, repoURL string, timeout time.Duration) (ChecksResult, error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}

	deadline := time.Now().Add(timeout)
	pollInterval := 15 * time.Second
	firstPoll := true

	for {
		result, err := m.PRChecks(prNumber, repoURL)
		if err != nil {
			if firstPoll {
				return ChecksResult{AllDone: true, Passed: true}, nil
			}
			return result, err
		}

		if len(result.Checks) == 0 {
			return ChecksResult{AllDone: true, Passed: true}, nil
		}
		firstPoll = false

		if result.AllDone {
			return result, nil
		}

		if time.Now().Add(pollInterval).After(deadline) {
			result.Passed = false
			return result, fmt.Errorf("CI checks timed out after %s", timeout)
		}

		pending := 0
		for _, c := range result.Checks {
			if !isTerminal(c.Status) {
				pending++
			}
		}
		m.Logger.Log("Waiting for CI: %d/%d checks pending...", pending, len(result.Checks))

		time.Sleep(pollInterval)
	}
}

// FormatCheckFailures produces a human-readable summary of failed checks.
func FormatCheckFailures(checks []CheckStatus) string {
	var failures []string
	for _, c := range checks {
		if isTerminal(c.Status) && c.Conclusion != "SUCCESS" && c.Conclusion != "NEUTRAL" && c.Conclusion != "SKIPPED" {
			failures = append(failures, fmt.Sprintf("- %s: %s", c.Name, c.Conclusion))
		}
	}
	if len(failures) == 0 {
		return ""
	}
	return "Failed CI checks:\n" + strings.Join(failures, "\n")
}

func isTerminal(status string) bool {
	return status == "COMPLETED" || status == ""
}
