package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// PRDiff uses gh api with the diff Accept header, not gh pr diff.
// A fake gh binary records the invocation args; the test verifies the
// implementation sends "gh api repos/{nwo}/pulls/{num}" with the diff header.
func TestPRDiff_UsesGhAPI(t *testing.T) {
	bin := t.TempDir()
	logFile := filepath.Join(bin, "gh.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logFile + "\necho '+added line'\n"
	ghPath := filepath.Join(bin, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	g := &ghCLI{}
	diff, err := g.PRDiff("https://github.com/owner/repo.git", 42)
	if err != nil {
		t.Fatalf("PRDiff returned error: %v", err)
	}
	if !strings.Contains(diff, "+added line") {
		t.Errorf("unexpected diff output: %q", diff)
	}

	raw, readErr := os.ReadFile(logFile)
	if readErr != nil {
		t.Fatalf("gh was never called: %v", readErr)
	}
	invocation := string(raw)

	if !strings.Contains(invocation, "api") {
		t.Errorf("expected 'gh api' invocation, got: %q", invocation)
	}
	if !strings.Contains(invocation, "repos/owner/repo/pulls/42") {
		t.Errorf("expected repos/owner/repo/pulls/42 in args, got: %q", invocation)
	}
	if !strings.Contains(invocation, "application/vnd.github.diff") {
		t.Errorf("expected application/vnd.github.diff Accept header, got: %q", invocation)
	}
	if strings.Contains(invocation, "pr diff") {
		t.Errorf("found 'gh pr diff' invocation — must use gh api: %q", invocation)
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
	stub := &capturingGitHub{createPR: func(opts CreatePROpts) (int, error) {
		captured = opts
		return 42, nil
	}}

	opts := CreatePROpts{
		Head:  "feature-branch",
		Base:  "main",
		Title: "Add widget",
		Body:  "Automated PR",
		Repo:  "owner/repo",
		Dir:   "/tmp/work",
	}
	if _, err := stub.CreatePR(opts); err != nil {
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
	if result := stub.GetRunLog(42, "/tmp"); result != "" {
		t.Errorf("expected empty string from default stub, got %q", result)
	}

	stub.RunLogValue = "error TS2307: Cannot find module './Missing'"
	if result := stub.GetRunLog(42, "/tmp"); result != stub.RunLogValue {
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

	checks, err := stub.ListChecks(99, "owner/repo")
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

// GetPR returns a PRDetail with all fields populated and auto-generates
// a deterministic HeadSHA that changes on each call (simulating pushes).
func TestGetPR_ReturnsAllFields(t *testing.T) {
	stub := &StubGitHub{
		PRState: "OPEN",
		PRBase:  "main",
		PRHead:  "feature-branch",
		HeadSHA: "abc123",
	}

	pr, err := stub.GetPR("owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pr.State != "OPEN" {
		t.Errorf("State: want %q, got %q", "OPEN", pr.State)
	}
	if pr.BaseRef != "main" {
		t.Errorf("BaseRef: want %q, got %q", "main", pr.BaseRef)
	}
	if pr.HeadRef != "feature-branch" {
		t.Errorf("HeadRef: want %q, got %q", "feature-branch", pr.HeadRef)
	}
	if pr.HeadSHA != "abc123" {
		t.Errorf("HeadSHA: want %q, got %q", "abc123", pr.HeadSHA)
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

	result := mgr.GetCIFailureLog(42)
	if result != "test failure output line 1\nline 2" {
		t.Errorf("expected delegated log output, got %q", result)
	}
}

// CheckCopilotReviewEnabled returns true when the repo has a ruleset containing a
// copilot_code_review rule with review_on_push: true, proving auto-review detection works.
func TestCheckCopilotReviewEnabled_ReturnsTrueWhenRulePresent(t *testing.T) {
	bin := t.TempDir()
	logFile := filepath.Join(bin, "gh.log")
	response := `[{"id":1,"rules":[{"type":"copilot_code_review","parameters":{"review_on_push":true}}]}]`
	script := "#!/bin/sh\necho \"$@\" >> " + logFile + "\necho '" + response + "'\n"
	ghPath := filepath.Join(bin, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	g := &ghCLI{}
	enabled, err := g.CheckCopilotReviewEnabled("owner/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Error("expected enabled=true when copilot_code_review rule with review_on_push=true exists")
	}

	raw, _ := os.ReadFile(logFile)
	if !strings.Contains(string(raw), "repos/owner/repo/rulesets") {
		t.Errorf("expected rulesets endpoint, got: %q", string(raw))
	}
}

// CheckCopilotReviewEnabled returns false when review_on_push is false, proving
// disabled Copilot reviews are not treated as active.
func TestCheckCopilotReviewEnabled_ReturnsFalseWhenReviewOnPushFalse(t *testing.T) {
	bin := t.TempDir()
	response := `[{"id":1,"rules":[{"type":"copilot_code_review","parameters":{"review_on_push":false}}]}]`
	script := "#!/bin/sh\necho '" + response + "'\n"
	ghPath := filepath.Join(bin, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	g := &ghCLI{}
	enabled, err := g.CheckCopilotReviewEnabled("owner/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enabled {
		t.Error("expected enabled=false when review_on_push=false")
	}
}

// CheckCopilotReviewEnabled returns false when no copilot_code_review rule exists,
// proving non-Copilot rulesets don't trigger the flag.
func TestCheckCopilotReviewEnabled_ReturnsFalseWhenNoRule(t *testing.T) {
	bin := t.TempDir()
	response := `[{"id":1,"rules":[{"type":"required_status_checks","parameters":{}}]}]`
	script := "#!/bin/sh\necho '" + response + "'\n"
	ghPath := filepath.Join(bin, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	g := &ghCLI{}
	enabled, err := g.CheckCopilotReviewEnabled("owner/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enabled {
		t.Error("expected enabled=false when no copilot_code_review rule exists")
	}
}

// StubGitHub.CheckCopilotReviewEnabled returns the configured CopilotReviewEnabled
// value, proving tests can control the flag without shelling out.
func TestStubGitHub_CheckCopilotReviewEnabled(t *testing.T) {
	stub := &StubGitHub{CopilotReviewEnabled: true}
	enabled, err := stub.CheckCopilotReviewEnabled("owner/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Error("expected enabled=true from stub with CopilotReviewEnabled=true")
	}
}

// PollCopilotReview returns a CopilotReview with body and inline comments when
// copilot-pull-request-reviewer has submitted a review on the PR.
func TestPollCopilotReview_ReturnsCopilotReview(t *testing.T) {
	bin := t.TempDir()
	logFile := filepath.Join(bin, "gh.log")

	reviewJSON := `[{"id":1001,"user":{"login":"copilot-pull-request-reviewer"},"body":"LGTM with suggestions"}]`
	commentsJSON := `[{"path":"main.go","line":42,"body":"Consider using constants","pull_request_review_id":1001}]`
	script := "#!/bin/sh\necho \"$@\" >> " + logFile + "\nif echo \"$@\" | grep -q 'comments'; then\n  echo '" + commentsJSON + "'\nelse\n  echo '" + reviewJSON + "'\nfi\n"
	ghPath := filepath.Join(bin, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	g := &ghCLI{}
	review, err := g.PollCopilotReview("owner/repo", 42, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if review == nil {
		t.Fatal("expected non-nil review")
	}
	if review.Body != "LGTM with suggestions" {
		t.Errorf("Body: want %q, got %q", "LGTM with suggestions", review.Body)
	}
	if len(review.Comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(review.Comments))
	}
	if review.Comments[0].Path != "main.go" {
		t.Errorf("Path: want main.go, got %q", review.Comments[0].Path)
	}
	if review.Comments[0].Line != 42 {
		t.Errorf("Line: want 42, got %d", review.Comments[0].Line)
	}
	if review.Comments[0].Body != "Consider using constants" {
		t.Errorf("Body: want %q, got %q", "Consider using constants", review.Comments[0].Body)
	}
}

// PollCopilotReview returns nil without error when the timeout expires and no
// review from copilot-pull-request-reviewer has arrived, so the loop proceeds.
func TestPollCopilotReview_Timeout_ReturnsNil(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho '[{\"id\":999,\"user\":{\"login\":\"other-bot\"},\"body\":\"not copilot\"}]'\n"
	ghPath := filepath.Join(bin, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	g := &ghCLI{}
	review, err := g.PollCopilotReview("owner/repo", 42, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if review != nil {
		t.Errorf("expected nil review on timeout, got %+v", review)
	}
}

