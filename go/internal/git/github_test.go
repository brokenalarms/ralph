package git

import (
	"testing"
)

// The StubGitHub type satisfies the GitHub interface, proving that test stubs
// can replace all GitHub CLI operations without shelling out.
func TestStubGitHub_SatisfiesInterface(t *testing.T) {
	var _ GitHub = &StubGitHub{}
}

// CreatePROpts carries all parameters in a single struct so callers avoid
// positional-parameter mistakes.
func TestCreatePROpts_FieldsPreserved(t *testing.T) {
	var captured CreatePROpts
	stub := &capturingGitHub{createPR: func(opts CreatePROpts) error {
		captured = opts
		return nil
	}}

	opts := CreatePROpts{
		Head:  "feature-branch",
		Base:  "main",
		Title: "Add widget",
		Body:  "Automated PR",
		Repo:  "owner/repo",
		Dir:   "/tmp/work",
	}
	if err := stub.CreatePR(opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if captured != opts {
		t.Errorf("CreatePR opts not passed through:\ngot:  %+v\nwant: %+v", captured, opts)
	}
}

// GetRunLog returns the stub's configured RunLogValue, proving the interface
// method can be stubbed for CI failure log testing.
func TestGetRunLog_Stubbable(t *testing.T) {
	stub := &StubGitHub{}
	if result := stub.GetRunLog("42", "/tmp"); result != "" {
		t.Errorf("expected empty string from default stub, got %q", result)
	}

	stub.RunLogValue = "error TS2307: Cannot find module './Missing'"
	if result := stub.GetRunLog("42", "/tmp"); result != stub.RunLogValue {
		t.Errorf("expected configured RunLogValue, got %q", result)
	}
}

// ListChecks replaces the former FetchChecks method, returning CI check results
// that callers use to determine merge readiness.
func TestListChecks_Stubbable(t *testing.T) {
	stub := &StubGitHub{
		Checks: []CICheckResult{
			{Name: "build", State: "SUCCESS", Bucket: "pass"},
			{Name: "lint", State: "FAILURE", Bucket: "fail"},
		},
	}

	checks, err := stub.ListChecks("99", "owner/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(checks))
	}
	if checks[1].Name != "lint" {
		t.Errorf("expected second check name 'lint', got %q", checks[1].Name)
	}
}

// isHarmlessUpdateBranchError suppresses known-benign 422 responses from the
// GitHub update-branch API so they don't surface as warnings in the loop log.
func TestIsHarmlessUpdateBranchError(t *testing.T) {
	harmless := []struct {
		name, output string
	}{
		{"already up to date", `{"message":"Validation Failed","errors":[{"message":"already up to date"}]}`},
		{"expected_head_sha", `{"message":"Update is not a fast forward","errors":[{"field":"expected_head_sha"}]}`},
		{"no new commits", `{"message":"Validation Failed","errors":[{"message":"There are no new commits on the base branch to update the pull request with."}]}`},
	}
	for _, tc := range harmless {
		t.Run(tc.name, func(t *testing.T) {
			if !isHarmlessUpdateBranchError(tc.output) {
				t.Errorf("expected harmless=true for %q", tc.output)
			}
		})
	}

	t.Run("real error is not harmless", func(t *testing.T) {
		if isHarmlessUpdateBranchError(`{"message":"Not Found"}`) {
			t.Error("expected harmless=false for a real error")
		}
	})
}

// Manager.GetCIFailureLog delegates to the injected GitHub interface's GetRunLog,
// confirming that loop code can get CI logs without shelling out.
func TestManager_GetCIFailureLog_DelegatesToGitHub(t *testing.T) {
	stub := &StubGitHub{RunLogValue: "test failure output line 1\nline 2"}
	mgr := &Manager{
		BaseBranch: "main",
		GitHub: stub,
	}

	result := mgr.GetCIFailureLog("42")
	if result != "test failure output line 1\nline 2" {
		t.Errorf("expected delegated log output, got %q", result)
	}
}

