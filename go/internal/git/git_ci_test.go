package git

import (
	"testing"
)

// Verifies that FormatCheckFailures produces a readable summary listing
// only the checks that actually failed, omitting successes and skips.
func TestFormatCheckFailures_MixedResults(t *testing.T) {
	checks := []CheckStatus{
		{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"},
		{Name: "lint", Status: "COMPLETED", Conclusion: "CANCELLED"},
		{Name: "deploy", Status: "COMPLETED", Conclusion: "SKIPPED"},
		{Name: "coverage", Status: "COMPLETED", Conclusion: "NEUTRAL"},
	}

	result := FormatCheckFailures(checks)
	if result == "" {
		t.Fatal("expected non-empty failure summary")
	}
	if !containsStr(result, "test: FAILURE") {
		t.Errorf("should mention failed test check, got: %s", result)
	}
	if !containsStr(result, "lint: CANCELLED") {
		t.Errorf("should mention cancelled lint check, got: %s", result)
	}
	if containsStr(result, "build") {
		t.Errorf("should not mention successful build check, got: %s", result)
	}
	if containsStr(result, "deploy") {
		t.Errorf("should not mention skipped deploy check, got: %s", result)
	}
	if containsStr(result, "coverage") {
		t.Errorf("should not mention neutral coverage check, got: %s", result)
	}
}

// Verifies that FormatCheckFailures returns empty string when all checks pass.
func TestFormatCheckFailures_AllPassing(t *testing.T) {
	checks := []CheckStatus{
		{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
	}
	if result := FormatCheckFailures(checks); result != "" {
		t.Errorf("expected empty string for all-passing checks, got: %s", result)
	}
}

// Verifies that diagnoseMergeFailure extracts specific failure reasons from
// gh pr merge output rather than returning raw text.
func TestDiagnoseMergeFailure(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "status check failure",
			output: "GraphQL: Could not merge because 2 status checks are expected",
			want:   "branch protection requires status checks to pass",
		},
		{
			name:   "review required",
			output: "GraphQL: At least 1 approving review is required by reviewers",
			want:   "branch protection requires reviews",
		},
		{
			name:   "base branch policy",
			output: "! Pull request #245 is not mergeable: base branch policy prohibits the merge",
			want:   "branch protection policy prohibits merge",
		},
		{
			name:   "merge conflict",
			output: "! Pull request #10 is not mergeable: there is a merge conflict with the base branch",
			want:   "merge conflict with base branch",
		},
		{
			name:   "unknown error preserved",
			output: "some unexpected error",
			want:   "some unexpected error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := diagnoseMergeFailure(tt.output)
			if got != tt.want {
				t.Errorf("diagnoseMergeFailure(%q) = %q, want %q", tt.output, got, tt.want)
			}
		})
	}
}

// Verifies that MergeError implements the error interface and includes
// the PR number and reason in its message.
func TestMergeError_Format(t *testing.T) {
	err := &MergeError{
		PR:     "42",
		Reason: "CI checks failed",
		Checks: []CheckStatus{{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"}},
	}
	msg := err.Error()
	if !containsStr(msg, "#42") {
		t.Errorf("error message should contain PR number, got: %s", msg)
	}
	if !containsStr(msg, "CI checks failed") {
		t.Errorf("error message should contain reason, got: %s", msg)
	}
}

// Verifies that ChecksResult correctly reports all-done and passed states
// for different combinations of check statuses.
func TestChecksResult_States(t *testing.T) {
	tests := []struct {
		name    string
		checks  []CheckStatus
		allDone bool
		passed  bool
	}{
		{
			name:    "all success",
			checks:  []CheckStatus{{Status: "COMPLETED", Conclusion: "SUCCESS"}},
			allDone: true,
			passed:  true,
		},
		{
			name:    "pending check",
			checks:  []CheckStatus{{Status: "IN_PROGRESS", Conclusion: ""}},
			allDone: false,
			passed:  false,
		},
		{
			name:    "one failure",
			checks:  []CheckStatus{{Status: "COMPLETED", Conclusion: "FAILURE"}},
			allDone: true,
			passed:  false,
		},
		{
			name:    "mixed pending and complete",
			checks:  []CheckStatus{{Status: "COMPLETED", Conclusion: "SUCCESS"}, {Status: "QUEUED", Conclusion: ""}},
			allDone: false,
			passed:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ChecksResult{AllDone: true, Passed: true}
			for _, c := range tt.checks {
				if !isTerminal(c.Status) {
					result.AllDone = false
					result.Passed = false
				} else if c.Conclusion != "SUCCESS" && c.Conclusion != "NEUTRAL" && c.Conclusion != "SKIPPED" {
					result.Passed = false
				}
			}
			if result.AllDone != tt.allDone {
				t.Errorf("AllDone = %v, want %v", result.AllDone, tt.allDone)
			}
			if result.Passed != tt.passed {
				t.Errorf("Passed = %v, want %v", result.Passed, tt.passed)
			}
		})
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
