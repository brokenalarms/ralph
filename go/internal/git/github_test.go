package git

import (
	"fmt"
	"testing"
)

// MergePR with a 200 response signals a successful squash-merge.
func TestMergePR_HTTP200_ReturnsMerged(t *testing.T) {
	output := "HTTP/2.0 200 OK\r\n\r\n{}"
	result := classifyMergeStatus(output, nil)
	if !result.Merged {
		t.Errorf("expected Merged=true for HTTP 200, got %+v", result)
	}
	if result.Blocked || result.Conflict {
		t.Errorf("expected no Blocked/Conflict flags for HTTP 200, got %+v", result)
	}
}

// MergePR with a 405 response means branch protection is blocking the merge.
func TestMergePR_HTTP405_ReturnsBlocked(t *testing.T) {
	output := "HTTP/2.0 405 Method Not Allowed\r\n\r\n{\"message\":\"Pull Request is not mergeable\"}"
	result := classifyMergeStatus(output, fmt.Errorf("exit status 1"))
	if !result.Blocked {
		t.Errorf("expected Blocked=true for HTTP 405, got %+v", result)
	}
	if result.Merged || result.Conflict {
		t.Errorf("expected no Merged/Conflict flags for HTTP 405, got %+v", result)
	}
}

// MergePR with a 409 response means the PR has unresolvable merge conflicts.
func TestMergePR_HTTP409_ReturnsConflict(t *testing.T) {
	output := "HTTP/2.0 409 Conflict\r\n\r\n{\"message\":\"Merge conflict\"}"
	result := classifyMergeStatus(output, fmt.Errorf("exit status 1"))
	if !result.Conflict {
		t.Errorf("expected Conflict=true for HTTP 409, got %+v", result)
	}
	if result.Merged || result.Blocked {
		t.Errorf("expected no Merged/Blocked flags for HTTP 409, got %+v", result)
	}
}

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

