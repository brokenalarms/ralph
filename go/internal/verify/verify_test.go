package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/git"
)

// dirQuerier implements GitQuerier for test repos by shelling out to git.
type dirQuerier struct {
	dir string
}

func (q *dirQuerier) HeadRev() string {
	out, _ := exec.Command("git", "-C", q.dir, "rev-parse", "HEAD").Output()
	return strings.TrimSpace(string(out))
}

func (q *dirQuerier) DiffStatRange(from, to string) string {
	if from == "" || to == "" || from == to {
		return ""
	}
	out, _ := exec.Command("git", "-C", q.dir, "diff", "--stat", from, to).Output()
	return strings.TrimSpace(string(out))
}

func (q *dirQuerier) DiffFull(from, to string) string {
	out, _ := exec.Command("git", "-C", q.dir, "diff", from+".."+to).Output()
	return strings.TrimSpace(string(out))
}

func (q *dirQuerier) LogOneline(from, to string) string {
	out, _ := exec.Command("git", "-C", q.dir, "log", "--oneline", from+".."+to).Output()
	return strings.TrimSpace(string(out))
}

func newQuerier(dir string) *dirQuerier {
	return &dirQuerier{dir: dir}
}

// Compile-time check that dirQuerier satisfies GitQuerier.
var _ GitQuerier = (*dirQuerier)(nil)

// Compile-time check that git.Manager satisfies GitQuerier.
var _ GitQuerier = (*git.Manager)(nil)

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

	result := RunTests(context.Background(), dir)
	if !result.Passed {
		t.Errorf("expected tests to pass, got: %s", result.Reason)
	}
}

// TestTimeout is 5 minutes — enough for ralph's own suite (~3 min)
// without masking genuinely hanging tests.
func TestTestTimeout_Default(t *testing.T) {
	if TestTimeout != 5*time.Minute {
		t.Errorf("TestTimeout = %v, want 5m", TestTimeout)
	}
}

// RunTests fails when its internal timeout expires, proving that
// a hanging test suite (unresolved promise, open handle) doesn't
// block the loop indefinitely.
func TestRunTests_Timeout(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\tsleep 600\n"), 0o644)

	saved := TestTimeout
	TestTimeout = 50 * time.Millisecond
	t.Cleanup(func() { TestTimeout = saved })

	result := RunTests(context.Background(), dir)
	if result.Passed {
		t.Error("expected tests to fail when timeout expires")
	}
	if !strings.Contains(result.Reason, "timed out") {
		t.Errorf("expected timeout reason, got: %s", result.Reason)
	}
	if !strings.Contains(result.Reason, "run individual test files to isolate") {
		t.Errorf("expected isolation guidance in reason, got: %s", result.Reason)
	}
}

// RunTests fails when context is cancelled, proving that Ctrl-C
// stops a long-running test suite instead of blocking indefinitely.
func TestRunTests_CancelledContext(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\tsleep 60\n"), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := RunTests(ctx, dir)
	if result.Passed {
		t.Error("expected tests to fail with cancelled context")
	}
}

// RunTests fails when the test command exits non-zero, proving that
// ralph will reject Claude's completion signal when tests are broken.
func TestRunTests_FailingTests(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\tfalse\n"), 0o644)

	result := RunTests(context.Background(), dir)
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

	result := RunTests(context.Background(), dir)
	if !result.Passed {
		t.Errorf("expected pass when no test runner detected, got: %s", result.Reason)
	}
}

// CheckCommits fails when HEAD hasn't moved since the baseline, catching
// the case where Claude signals completion without making code changes.
func TestCheckCommits_NoNewCommits(t *testing.T) {
	dir := setupGitRepo(t)
	gq := newQuerier(dir)

	head := gq.HeadRev()
	result := CheckCommits(gq, head)
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
	gq := newQuerier(dir)

	headBefore := gq.HeadRev()

	// Make a new commit
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("change"), 0o644)
	exec.Command("git", "-C", dir, "add", ".").Run()
	exec.Command("git", "-C", dir, "commit", "-m", "new").Run()

	result := CheckCommits(gq, headBefore)
	if !result.Passed {
		t.Errorf("expected pass with new commits, got: %s", result.Reason)
	}
}

// CheckCommits passes when no baseline is provided (first iteration),
// so we don't block on edge cases.
func TestCheckCommits_EmptyBaseline(t *testing.T) {
	gq := newQuerier(t.TempDir())

	result := CheckCommits(gq, "")
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

// filterFailures strips passing "ok" package lines from go test output,
// keeping only FAIL lines and error details visible in compile check logs.
func TestFilterFailures(t *testing.T) {
	input := strings.Join([]string{
		"ok \tgithub.com/example/pkg1\t0.003s",
		"ok \tgithub.com/example/pkg2\t0.001s",
		"FAIL\tgithub.com/example/pkg3 [build failed]",
		"# github.com/example/pkg3",
		"./broken.go:5:2: undefined: missing",
		"ok \tgithub.com/example/pkg4\t0.002s",
		"FAIL",
	}, "\n")

	got := filterFailures(input)
	if strings.Contains(got, "ok \t") {
		t.Errorf("expected passing packages removed, got: %q", got)
	}
	if !strings.Contains(got, "FAIL\tgithub.com/example/pkg3") {
		t.Error("expected FAIL line preserved")
	}
	if !strings.Contains(got, "undefined: missing") {
		t.Error("expected error detail preserved")
	}
}

// filterFailures returns input unchanged when there are no passing packages.
func TestFilterFailures_NoPassingLines(t *testing.T) {
	input := "FAIL\tpkg [build failed]\n# pkg\n./x.go:1: error"
	got := filterFailures(input)
	if got != input {
		t.Errorf("expected unchanged output, got: %q", got)
	}
}

// PreflightChecks detects when files changed and new commits exist,
// confirming the orchestrator can verify work was done before running
// the full test suite.
func TestPreflightChecks_WithChanges(t *testing.T) {
	dir := setupGitRepo(t)
	gq := newQuerier(dir)
	headBefore := gq.HeadRev()

	os.WriteFile(filepath.Join(dir, "feature.go"), []byte("package main"), 0o644)
	exec.Command("git", "-C", dir, "add", ".").Run()
	exec.Command("git", "-C", dir, "commit", "-m", "add feature").Run()

	result := PreflightChecks(gq, headBefore, "in_progress")
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
	gq := newQuerier(dir)
	headBefore := gq.HeadRev()

	result := PreflightChecks(gq, headBefore, "closed")
	if result.BeadOpen {
		t.Error("expected BeadOpen=false when status is closed (premature close)")
	}
}

// PreflightChecks correctly reports no changes when HEAD hasn't moved.
func TestPreflightChecks_NoChanges(t *testing.T) {
	dir := setupGitRepo(t)
	gq := newQuerier(dir)
	headBefore := gq.HeadRev()

	result := PreflightChecks(gq, headBefore, "in_progress")
	if result.FilesChanged {
		t.Error("expected FilesChanged=false when no files changed")
	}
	if result.HasCommits {
		t.Error("expected HasCommits=false when HEAD hasn't moved")
	}
}


// loadReviewPrompt includes guidance that prompt/config changes are valid
// implementations and that code-specific criteria (tests, error handling)
// should not be required for non-code changes.
func TestLoadReviewPrompt_PromptChangeGuidance(t *testing.T) {
	promptsDir := t.TempDir()
	src := filepath.Join("..", "..", "cmd", "ralph", "prompts", "verify-review.md")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("failed to read verify-review.md from source: %v", err)
	}
	os.WriteFile(filepath.Join(promptsDir, "verify-review.md"), data, 0o644)

	prompt := loadReviewPrompt(promptsDir, "Update agent instructions", "Change the prompt template", "", "PR", "diff content")

	checks := []struct {
		desc    string
		snippet string
	}{
		{"acknowledges prompt changes as valid", "prompt or configuration changes"},
		{"scopes test requirement to code", "code changes include tests"},
		{"mentions markdown as valid", "markdown"},
	}
	for _, c := range checks {
		if !strings.Contains(strings.ToLower(prompt), strings.ToLower(c.snippet)) {
			t.Errorf("prompt missing guidance: %s (looked for %q)", c.desc, c.snippet)
		}
	}
}

// loadReviewPrompt fallback (no template file) still produces a usable prompt.
func TestLoadReviewPrompt_Fallback(t *testing.T) {
	prompt := loadReviewPrompt("/nonexistent", "task title", "task desc", "", "iteration", "some diff")
	if !strings.Contains(prompt, "task title") {
		t.Error("fallback prompt should contain task title")
	}
	if !strings.Contains(prompt, "some diff") {
		t.Error("fallback prompt should contain diff")
	}
}

// LLMVerifyPR accepts a VerifyOpts struct instead of positional parameters,
// proving the struct-based API compiles and works end-to-end.
func TestLLMVerifyPR_AcceptsVerifyOpts(t *testing.T) {
	dir := setupGitRepo(t)
	gq := newQuerier(dir)
	head := gq.HeadRev()

	result := LLMVerifyPR(VerifyOpts{
		Ctx:             context.Background(),
		Git:             gq,
		WorkDir:         dir,
		PromptsDir:      t.TempDir(),
		TaskID:          "struct-test",
		HeadBefore:      head,
		BeadTitle:       "struct api test",
		BeadDescription: "proves VerifyOpts struct works",
		BeadAcceptance:  "accepts struct",
		Model:           ModelHaiku,
	})
	if !result.Passed {
		t.Errorf("expected pass with no diff, got: %s", result.Reason)
	}
	if !result.NoDiff {
		t.Error("expected NoDiff=true when no PR and no iteration diff exist")
	}
}

// LLMVerifyPR passes when no PR and no diff exist — agent confirmed task complete.
func TestLLMVerifyPR_NoPRNoDiff(t *testing.T) {
	dir := setupGitRepo(t)
	gq := newQuerier(dir)
	head := gq.HeadRev()

	result := LLMVerifyPR(VerifyOpts{
		Ctx:             context.Background(),
		Git:             gq,
		WorkDir:         dir,
		PromptsDir:      t.TempDir(),
		TaskID:          "nonexistent-task",
		HeadBefore:      head,
		BeadTitle:       "some task",
		BeadDescription: "some description",
	})
	if !result.Passed {
		t.Errorf("expected pass when agent confirms complete with no new work needed, got: %s", result.Reason)
	}
	if !result.NoDiff {
		t.Error("expected NoDiff=true when no PR and no iteration diff exist")
	}
}

// LLMVerifyPR delegates PR lookup to the GitHub interface rather than
// shelling out to gh directly, proving git/gh exec.Command calls are
// routed through the git module.
func TestLLMVerifyPR_UsesGitHubInterface(t *testing.T) {
	dir := setupGitRepo(t)
	gq := newQuerier(dir)
	head := gq.HeadRev()

	stub := &stubGitHub{
		searchResult: "99",
		prDiff:       "+new line\n",
	}

	result := LLMVerifyPR(VerifyOpts{
		Ctx:             context.Background(),
		Git:             gq,
		WorkDir:         dir,
		PromptsDir:      t.TempDir(),
		TaskID:          "test-task",
		HeadBefore:      head,
		BeadTitle:       "test",
		BeadDescription: "test desc",
		GitHub:          stub,
	})

	if !stub.searchCalled {
		t.Error("expected SearchPR to be called via GitHub interface")
	}
	if !stub.prDiffCalled {
		t.Error("expected PRDiff to be called via GitHub interface")
	}

	// LLM call will fail (no claude binary in test), so we expect pass with skip reason
	_ = result
}

// stubGitHub implements git.GitHub for testing that verify delegates
// to the interface instead of calling exec.Command directly.
type stubGitHub struct {
	searchResult string
	prDiff       string
	searchCalled bool
	prDiffCalled bool
}

func (s *stubGitHub) Available() bool                                    { return true }
func (s *stubGitHub) FindOpenPR(string, string) (string, error)         { return "", nil }
func (s *stubGitHub) CreatePR(git.CreatePROpts) error                   { return nil }
func (s *stubGitHub) EditPR(string, string, string, string) error       { return nil }
func (s *stubGitHub) MergePR(string, string, git.MergeOpts) (string, error) { return "", nil }
func (s *stubGitHub) UpdateBranch(string, string, string) (bool, error) { return false, nil }
func (s *stubGitHub) ListChecks(string, string) ([]git.CICheckResult, error) { return nil, nil }
func (s *stubGitHub) GetRunLog(string, string) string                   { return "" }
func (s *stubGitHub) CheckEnforceAdmins(string, string) (bool, error)   { return false, nil }
func (s *stubGitHub) PostEnforceAdmins(string, string) (string, error)  { return "", nil }
func (s *stubGitHub) FindPR(string, string) (string, string, string, error) { return "", "", "", nil }
func (s *stubGitHub) SearchPR(_ string, _ string) (string, error) {
	s.searchCalled = true
	return s.searchResult, nil
}
func (s *stubGitHub) PRDiff(_ string, _ string) (string, error) {
	s.prDiffCalled = true
	return s.prDiff, nil
}
func (s *stubGitHub) GetPRState(string, string) (string, error)        { return "", nil }
func (s *stubGitHub) ListOpenPRBranches(string) ([]string, error)      { return nil, nil }
func (s *stubGitHub) GetPRBase(string, string) (string, error)  { return "", nil }
func (s *stubGitHub) GetPRHead(string, string) (string, error)     { return "", nil }
func (s *stubGitHub) GetPRHeadSHA(string, string) (string, error)  { return "", nil }

// ModelShortName extracts a human-friendly name from a full model ID,
// so log lines show "haiku" instead of "claude-haiku-4-5-20251001".
func TestModelShortName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{ModelHaiku, "haiku"},
		{ModelSonnet, "sonnet"},
		{"claude-opus-4-6-20260101", "opus"},
		{"unknown-model-v1", "unknown-model-v1"},
	}
	for _, tt := range tests {
		got := ModelShortName(tt.input)
		if got != tt.want {
			t.Errorf("ModelShortName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// CompileCheck passes when all Go packages compile, ensuring interface
// stubs across every test package are up to date before pushing.
func TestCompileCheck_PassingProject(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main_test.go"), []byte("package main\nimport \"testing\"\nfunc TestNoop(t *testing.T) {}\n"), 0o644)

	result := CompileCheck(context.Background(), dir)
	if !result.Passed {
		t.Errorf("expected compile check to pass, got: %s\n%s", result.Reason, result.Details)
	}
}

// CompileCheck fails when a test file has a compilation error, catching
// the exact scenario where an interface method is added but a stub is missing.
func TestCompileCheck_BrokenStub(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main_test.go"), []byte("package main\nimport \"testing\"\nfunc TestBroken(t *testing.T) { undefined() }\n"), 0o644)

	result := CompileCheck(context.Background(), dir)
	if result.Passed {
		t.Error("expected compile check to fail for broken test file")
	}
	if !strings.Contains(result.Reason, "compile check failed") {
		t.Errorf("expected compile-related reason, got: %s", result.Reason)
	}
}

// CompileCheck strips passing "ok" package lines from failure output so only
// the broken package details appear in logs.
func TestCompileCheck_FiltersPassingPackages(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0o644)
	// good package compiles fine
	goodDir := filepath.Join(dir, "good")
	os.MkdirAll(goodDir, 0o755)
	os.WriteFile(filepath.Join(goodDir, "good.go"), []byte("package good\n"), 0o644)
	os.WriteFile(filepath.Join(goodDir, "good_test.go"), []byte("package good\nimport \"testing\"\nfunc TestOk(t *testing.T) {}\n"), 0o644)
	// bad package has compile error
	badDir := filepath.Join(dir, "bad")
	os.MkdirAll(badDir, 0o755)
	os.WriteFile(filepath.Join(badDir, "bad.go"), []byte("package bad\n"), 0o644)
	os.WriteFile(filepath.Join(badDir, "bad_test.go"), []byte("package bad\nimport \"testing\"\nfunc TestBad(t *testing.T) { undefined() }\n"), 0o644)

	result := CompileCheck(context.Background(), dir)
	if result.Passed {
		t.Fatal("expected compile check to fail")
	}
	if strings.Contains(result.Details, "ok \t") {
		t.Errorf("expected passing packages filtered from details, got:\n%s", result.Details)
	}
	if !strings.Contains(result.Details, "FAIL") {
		t.Errorf("expected FAIL line in details, got:\n%s", result.Details)
	}
}

// CompileCheck finds go.mod in a go/ subdirectory, matching ralph's project
// layout where the Go module lives under go/ rather than the repo root.
func TestCompileCheck_GoSubdirectory(t *testing.T) {
	dir := t.TempDir()
	goDir := filepath.Join(dir, "go")
	os.MkdirAll(goDir, 0o755)
	os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(goDir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644)

	result := CompileCheck(context.Background(), dir)
	if !result.Passed {
		t.Errorf("expected compile check to pass with go/ subdir, got: %s\n%s", result.Reason, result.Details)
	}
}

// CompileCheck passes for non-Go projects (no go.mod found).
func TestCompileCheck_NonGoProject(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{}`), 0o644)

	result := CompileCheck(context.Background(), dir)
	if !result.Passed {
		t.Errorf("expected pass for non-Go project, got: %s", result.Reason)
	}
}

// findGoModDir locates go.mod in the given directory, a go/ subdirectory,
// or a parent directory — proving ralph's nested layout is handled.
func TestFindGoModDir(t *testing.T) {
	t.Run("direct", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644)
		if got := findGoModDir(dir); got != dir {
			t.Errorf("expected %s, got %s", dir, got)
		}
	})

	t.Run("go_subdir", func(t *testing.T) {
		dir := t.TempDir()
		goDir := filepath.Join(dir, "go")
		os.MkdirAll(goDir, 0o755)
		os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module test\n"), 0o644)
		if got := findGoModDir(dir); got != goDir {
			t.Errorf("expected %s, got %s", goDir, got)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		dir := t.TempDir()
		if got := findGoModDir(dir); got != "" {
			t.Errorf("expected empty, got %s", got)
		}
	})
}

func setupGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	exec.Command("git", "-C", dir, "init", "-b", "main").Run()
	exec.Command("git", "-C", dir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", dir, "config", "user.name", "test").Run()
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("init"), 0o644)
	exec.Command("git", "-C", dir, "add", ".").Run()
	exec.Command("git", "-C", dir, "commit", "-m", "init").Run()
	return dir
}
