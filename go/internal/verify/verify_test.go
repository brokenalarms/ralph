package verify

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// DetectTestCommand finds a Makefile test target when present,
// proving ralph can auto-detect the project's test runner.
func TestDetectTestCommand_Makefile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\tgo test ./...\n"), 0o644)

	tc := DetectTestCommand(dir)
	if tc == nil {
		t.Fatal("expected test command, got nil")
	}
	if tc.Cmd != "make" || len(tc.Args) != 1 || tc.Args[0] != "test" {
		t.Errorf("expected make test, got %s %v", tc.Cmd, tc.Args)
	}
}

// DetectTestCommand finds npm test when package.json has a test script,
// supporting Node.js projects.
func TestDetectTestCommand_NPM(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"test":"jest"}}`), 0o644)

	tc := DetectTestCommand(dir)
	if tc == nil {
		t.Fatal("expected test command, got nil")
	}
	if tc.Cmd != "npm" {
		t.Errorf("expected npm, got %s", tc.Cmd)
	}
}

// DetectTestCommand finds go test when go.mod exists,
// supporting Go projects without a Makefile.
func TestDetectTestCommand_GoMod(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644)

	tc := DetectTestCommand(dir)
	if tc == nil {
		t.Fatal("expected test command, got nil")
	}
	if tc.Cmd != "go" {
		t.Errorf("expected go, got %s", tc.Cmd)
	}
}

// DetectTestCommand returns nil when no test runner is found,
// so verification doesn't block projects without tests.
func TestDetectTestCommand_None(t *testing.T) {
	dir := t.TempDir()

	tc := DetectTestCommand(dir)
	if tc != nil {
		t.Errorf("expected nil for empty dir, got %s %v", tc.Cmd, tc.Args)
	}
}

// Makefile targets are detected only when the exact target name appears,
// not when a similarly-named target like "test-integration" is present.
func TestDetectTestCommand_MakefileNoTarget(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("build:\n\tgo build ./...\n"), 0o644)

	tc := DetectTestCommand(dir)
	if tc != nil && tc.Cmd == "make" {
		t.Error("should not detect make test when Makefile has no test target")
	}
}

// RunTests passes when a test command succeeds, proving the happy path
// where Claude's fix is verified by the test suite.
func TestRunTests_PassingTests(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\ttrue\n"), 0o644)

	result := RunTests(dir)
	if !result.Passed {
		t.Errorf("expected tests to pass, got: %s", result.Reason)
	}
}

// RunTests fails when the test command exits non-zero, proving that
// ralph will reject Claude's completion signal when tests are broken.
func TestRunTests_FailingTests(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\tfalse\n"), 0o644)

	result := RunTests(dir)
	if result.Passed {
		t.Error("expected tests to fail")
	}
	if !strings.Contains(result.Reason, "test suite failed") {
		t.Errorf("unexpected reason: %s", result.Reason)
	}
}

// RunTests passes when no test runner is detected, avoiding false
// negatives for projects that don't have a test framework.
func TestRunTests_NoTestRunner(t *testing.T) {
	dir := t.TempDir()

	result := RunTests(dir)
	if !result.Passed {
		t.Errorf("expected pass when no test runner detected, got: %s", result.Reason)
	}
}

// CheckCommits fails when HEAD hasn't moved since the baseline, catching
// the case where Claude signals completion without making code changes.
func TestCheckCommits_NoNewCommits(t *testing.T) {
	dir := setupGitRepo(t)

	head := gitHeadRev(dir)
	result := CheckCommits(dir, head)
	if result.Passed {
		t.Error("expected failure when HEAD hasn't moved")
	}
	if !strings.Contains(result.Reason, "no new commits") {
		t.Errorf("unexpected reason: %s", result.Reason)
	}
}

// CheckCommits passes when HEAD has advanced, confirming that Claude
// produced actual code changes before signaling completion.
func TestCheckCommits_WithNewCommits(t *testing.T) {
	dir := setupGitRepo(t)

	headBefore := gitHeadRev(dir)

	// Make a new commit
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("change"), 0o644)
	exec.Command("git", "-C", dir, "add", ".").Run()
	exec.Command("git", "-C", dir, "commit", "-m", "new").Run()

	result := CheckCommits(dir, headBefore)
	if !result.Passed {
		t.Errorf("expected pass with new commits, got: %s", result.Reason)
	}
}

// CheckCommits passes when no baseline is provided (first iteration),
// so we don't block on edge cases.
func TestCheckCommits_EmptyBaseline(t *testing.T) {
	dir := t.TempDir()

	result := CheckCommits(dir, "")
	if !result.Passed {
		t.Errorf("expected pass with empty baseline, got: %s", result.Reason)
	}
}

func TestLastNLines(t *testing.T) {
	input := "line1\nline2\nline3\nline4\nline5"
	got := lastNLines(input, 3)
	if got != "line3\nline4\nline5" {
		t.Errorf("expected last 3 lines, got: %q", got)
	}
}

func TestLastNLines_ShortInput(t *testing.T) {
	input := "line1\nline2"
	got := lastNLines(input, 5)
	if got != input {
		t.Errorf("expected full input, got: %q", got)
	}
}

// PreflightChecks detects when files changed and new commits exist,
// confirming the orchestrator can verify work was done before running
// the full test suite.
func TestPreflightChecks_WithChanges(t *testing.T) {
	dir := setupGitRepo(t)
	headBefore := gitHeadRev(dir)

	os.WriteFile(filepath.Join(dir, "feature.go"), []byte("package main"), 0o644)
	exec.Command("git", "-C", dir, "add", ".").Run()
	exec.Command("git", "-C", dir, "commit", "-m", "add feature").Run()

	result := PreflightChecks(dir, headBefore, "in_progress")
	if !result.FilesChanged {
		t.Error("expected FilesChanged=true after adding a file")
	}
	if !result.HasCommits {
		t.Error("expected HasCommits=true after committing")
	}
	if !result.BeadOpen {
		t.Error("expected BeadOpen=true when status is in_progress")
	}
}

// PreflightChecks detects premature bead close when the agent
// closes the task before the orchestrator verifies it.
func TestPreflightChecks_PrematureClose(t *testing.T) {
	dir := setupGitRepo(t)
	headBefore := gitHeadRev(dir)

	result := PreflightChecks(dir, headBefore, "closed")
	if result.BeadOpen {
		t.Error("expected BeadOpen=false when status is closed (premature close)")
	}
}

// PreflightChecks correctly reports no changes when HEAD hasn't moved.
func TestPreflightChecks_NoChanges(t *testing.T) {
	dir := setupGitRepo(t)
	headBefore := gitHeadRev(dir)

	result := PreflightChecks(dir, headBefore, "in_progress")
	if result.FilesChanged {
		t.Error("expected FilesChanged=false when no files changed")
	}
	if result.HasCommits {
		t.Error("expected HasCommits=false when HEAD hasn't moved")
	}
}


// LLMVerifyPR passes when no PR and no diff exist (tests already passed).
func TestLLMVerifyPR_NoPRNoDiff(t *testing.T) {
	dir := setupGitRepo(t)
	head := gitHeadRev(dir)

	result := LLMVerifyPR(dir, "nonexistent-task", head, "some task", "some description")
	if !result.Passed {
		t.Errorf("expected pass when no PR and no diff (tests passed), got: %s", result.Reason)
	}
}

func setupGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	exec.Command("git", "-C", dir, "init").Run()
	exec.Command("git", "-C", dir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", dir, "config", "user.name", "test").Run()
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("init"), 0o644)
	exec.Command("git", "-C", dir, "add", ".").Run()
	exec.Command("git", "-C", dir, "commit", "-m", "init").Run()
	return dir
}
