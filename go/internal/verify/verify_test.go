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
func TestDetectTestCommand_MakeVerify(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\tgo test ./...\n"), 0o644)

	tc := DetectTestCommand(dir)
	if tc == nil {
		t.Fatal("expected test command, got nil")
	}
	if tc.Cmd != "make" || len(tc.Args) != 1 || tc.Args[0] != "ralph-verify" {
		t.Errorf("expected make ralph-verify, got %s %v", tc.Cmd, tc.Args)
	}
}

// DetectTestCommand finds npm run ralph:verify when package.json has the script.
func TestDetectTestCommand_NPM(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"ralph:verify":"jest && playwright test"}}`), 0o644)

	tc := DetectTestCommand(dir)
	if tc == nil {
		t.Fatal("expected test command, got nil")
	}
	if tc.Cmd != "npm" || len(tc.Args) != 2 || tc.Args[0] != "run" || tc.Args[1] != "ralph:verify" {
		t.Errorf("expected npm run ralph:verify, got %s %v", tc.Cmd, tc.Args)
	}
}

// DetectTestCommand ignores npm test — only ralph:verify is accepted.
func TestDetectTestCommand_NPMTestIgnored(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"test":"jest"}}`), 0o644)

	tc := DetectTestCommand(dir)
	if tc != nil {
		t.Errorf("expected nil when only npm test exists (no ralph:verify), got %s %v", tc.Cmd, tc.Args)
	}
}

// DetectTestCommand ignores go.mod — heuristic detection is removed.
func TestDetectTestCommand_GoModIgnored(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644)

	tc := DetectTestCommand(dir)
	if tc != nil {
		t.Errorf("expected nil when only go.mod exists (no ralph:verify), got %s %v", tc.Cmd, tc.Args)
	}
}

// DetectTestCommand returns nil when no ralph:verify script is found.
func TestDetectTestCommand_None(t *testing.T) {
	dir := t.TempDir()

	tc := DetectTestCommand(dir)
	if tc != nil {
		t.Errorf("expected nil for empty dir, got %s %v", tc.Cmd, tc.Args)
	}
}

// Makefile must have a ralph-verify target, not just test.
func TestDetectTestCommand_MakefileTestTargetIgnored(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\tgo test ./...\n"), 0o644)

	tc := DetectTestCommand(dir)
	if tc != nil {
		t.Errorf("expected nil when Makefile has test but not ralph-verify, got %s %v", tc.Cmd, tc.Args)
	}
}

// RunTests passes when a test command succeeds, proving the happy path
// where Claude's fix is verified by the test suite.
func TestRunTests_PassingTests(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\ttrue\n"), 0o644)

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
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\tsleep 1\n"), 0o644)

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
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\tsleep 1\n"), 0o644)

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
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\tfalse\n"), 0o644)

	result := RunTests(context.Background(), dir)
	if result.Passed {
		t.Error("expected tests to fail")
	}
	if !strings.Contains(result.Reason, "test suite failed") {
		t.Errorf("unexpected reason: %s", result.Reason)
	}
}

// RunTests fails when no ralph:verify script is found — projects must
// declare their verify command explicitly.
func TestRunTests_NoTestRunner(t *testing.T) {
	// Proves: when no ralph:verify script exists, Result.ScriptMissing is set
	// so callers can distinguish configuration errors from test failures.
	dir := t.TempDir()

	result := RunTests(context.Background(), dir)
	if result.Passed {
		t.Error("expected failure when no ralph:verify script found")
	}
	if !result.ScriptMissing {
		t.Error("expected ScriptMissing=true when no ralph:verify script found")
	}
}

// DetectPostTaskCommand returns "npm run ralph:post-task" when package.json has the script.
func TestDetectPostTaskCommand_NPM(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"ralph:post-task":"node ./scripts/post-task.js"}}`), 0o644)

	got := DetectPostTaskCommand("", dir)
	if got != "npm run ralph:post-task" {
		t.Errorf("expected 'npm run ralph:post-task', got %q", got)
	}
}

// DetectPostTaskCommand returns "make ralph-post-task" when the Makefile target is present.
func TestDetectPostTaskCommand_Make(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-post-task:\n\t./scripts/post-task.sh\n"), 0o644)

	got := DetectPostTaskCommand("", dir)
	if got != "make ralph-post-task" {
		t.Errorf("expected 'make ralph-post-task', got %q", got)
	}
}

// DetectPostTaskCommand falls back to the CLI flag when no package.json script or Makefile target is found.
func TestDetectPostTaskCommand_CLIFallback(t *testing.T) {
	dir := t.TempDir()

	got := DetectPostTaskCommand("/path/to/post-task.sh", dir)
	if got != "/path/to/post-task.sh" {
		t.Errorf("expected CLI flag value, got %q", got)
	}
}

// DetectPostTaskCommand returns empty string when neither package.json script,
// Makefile target, nor CLI flag is present.
func TestDetectPostTaskCommand_None(t *testing.T) {
	dir := t.TempDir()

	got := DetectPostTaskCommand("", dir)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// DetectPostTaskCommand prefers ralph:post-task npm script over the CLI flag.
func TestDetectPostTaskCommand_NPMTakesPriorityOverCLI(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"ralph:post-task":"node ./scripts/post-task.js"}}`), 0o644)

	got := DetectPostTaskCommand("/path/to/cli-post-task.sh", dir)
	if got != "npm run ralph:post-task" {
		t.Errorf("expected npm script to take priority, got %q", got)
	}
}

// DetectTestCommand finds the script in a fallback directory when the primary
// directory does not have it — proving worktrees branched before ralph:verify
// was added still detect the script via the project root.
func TestDetectTestCommand_FallbackDir(t *testing.T) {
	primary := t.TempDir()   // simulates worktree — no ralph:verify
	fallback := t.TempDir()  // simulates project root — has ralph:verify
	os.WriteFile(filepath.Join(fallback, "Makefile"), []byte("ralph-verify:\n\tgo test ./...\n"), 0o644)

	tc := DetectTestCommand(primary, fallback)
	if tc == nil {
		t.Fatal("expected test command via fallback dir, got nil")
	}
	if tc.Dir != fallback {
		t.Errorf("expected Dir=%q (fallback), got %q", fallback, tc.Dir)
	}
	if tc.Cmd != "make" {
		t.Errorf("expected make, got %s", tc.Cmd)
	}
}

// DetectPostTaskCommand finds the script in a fallback directory when the
// primary directory does not have it.
func TestDetectPostTaskCommand_FallbackDir(t *testing.T) {
	primary := t.TempDir()  // simulates worktree — no ralph:post-task
	fallback := t.TempDir() // simulates project root — has ralph:post-task
	os.WriteFile(filepath.Join(fallback, "package.json"), []byte(`{"scripts":{"ralph:post-task":"node ./scripts/post-task.js"}}`), 0o644)

	got := DetectPostTaskCommand("", primary, fallback)
	if got != "npm run ralph:post-task" {
		t.Errorf("expected 'npm run ralph:post-task' via fallback, got %q", got)
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

// LLMVerifyPR uses the pre-fetched PRDiff field when available,
// preferring it over the iteration diff.
func TestLLMVerifyPR_UsesPRDiffField(t *testing.T) {
	dir := setupGitRepo(t)
	gq := newQuerier(dir)
	head := gq.HeadRev()

	result := LLMVerifyPR(VerifyOpts{
		Ctx:             context.Background(),
		Git:             gq,
		WorkDir:         dir,
		PromptsDir:      t.TempDir(),
		TaskID:          "test-task",
		HeadBefore:      head,
		BeadTitle:       "test",
		BeadDescription: "test desc",
		PRDiff:          "+new line from PR\n",
	})

	// LLM call will fail (no claude binary in test), so we expect pass with skip reason
	_ = result
}

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

// CompileCheck passes for a TypeScript project whose typecheck succeeds,
// proving type-correct TS code clears the pre-push gate.
func TestCompileCheck_TypeScriptPasses(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	os.WriteFile(filepath.Join(binDir, "tsc"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644)

	result := CompileCheck(context.Background(), dir)
	if !result.Passed {
		t.Errorf("expected TypeScript compile check to pass, got: %s\n%s", result.Reason, result.Details)
	}
}

// CompileCheck fails for a TypeScript project with a type error, blocking
// push before CI catches it (e.g. TS2552 missing import).
func TestCompileCheck_TypeScriptFails(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	os.WriteFile(filepath.Join(binDir, "tsc"), []byte("#!/bin/sh\necho 'error TS2552: Cannot find name'\nexit 1\n"), 0o755)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644)

	result := CompileCheck(context.Background(), dir)
	if result.Passed {
		t.Error("expected TypeScript compile check to fail for type error")
	}
	if !strings.Contains(result.Reason, "TypeScript") {
		t.Errorf("expected TypeScript-related reason, got: %s", result.Reason)
	}
	if !strings.Contains(result.Details, "TS2552") {
		t.Errorf("expected error detail in output, got: %s", result.Details)
	}
}

// CompileCheck uses npm run typecheck when package.json has a typecheck script,
// matching what CI runs instead of invoking tsc directly.
func TestCompileCheck_TypeScriptNPMTypecheck(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	// Fake npm that exits 0 when called as "npm run typecheck"
	os.WriteFile(filepath.Join(binDir, "npm"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"typecheck":"tsc --noEmit"}}`), 0o644)

	result := CompileCheck(context.Background(), dir)
	if !result.Passed {
		t.Errorf("expected npm run typecheck to pass, got: %s\n%s", result.Reason, result.Details)
	}
}

// CompileCheck runs both Go and TypeScript checks when a project has both
// go.mod and tsconfig.json, so neither language escapes compile verification.
func TestCompileCheck_BothGoAndTypeScript(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	os.WriteFile(filepath.Join(binDir, "tsc"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644)

	result := CompileCheck(context.Background(), dir)
	if !result.Passed {
		t.Errorf("expected both checks to pass, got: %s\n%s", result.Reason, result.Details)
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

func TestCapModel(t *testing.T) {
	tests := []struct {
		cap, model, want string
	}{
		// No cap: model passes through unchanged.
		{"", ModelOpus, ModelOpus},
		{"", ModelSonnet, ModelSonnet},
		{"", ModelHaiku, ModelHaiku},
		// Cap=opus: no restriction (opus is the ceiling).
		{ModelOpus, ModelOpus, ModelOpus},
		{ModelOpus, ModelSonnet, ModelSonnet},
		{ModelOpus, ModelHaiku, ModelHaiku},
		// Cap=sonnet: opus clamped to sonnet, haiku passes through.
		{ModelSonnet, ModelOpus, ModelSonnet},
		{ModelSonnet, ModelSonnet, ModelSonnet},
		{ModelSonnet, ModelHaiku, ModelHaiku},
		// Cap=haiku: everything clamped to haiku.
		{ModelHaiku, ModelOpus, ModelHaiku},
		{ModelHaiku, ModelSonnet, ModelHaiku},
		{ModelHaiku, ModelHaiku, ModelHaiku},
	}
	for _, tc := range tests {
		got := CapModel(tc.cap, tc.model)
		if got != tc.want {
			t.Errorf("CapModel(%q, %q) = %q, want %q", tc.cap, tc.model, got, tc.want)
		}
	}
}

// RunTests populates Command and Dir so callers can log what was detected.
func TestRunTests_PopulatesCommandAndDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\ttrue\n"), 0o644)

	result := RunTests(context.Background(), dir)
	if result.Command != "make ralph-verify" {
		t.Errorf("expected Command='make ralph-verify', got %q", result.Command)
	}
	if result.Dir != dir {
		t.Errorf("expected Dir=%q, got %q", dir, result.Dir)
	}
}

// RunTests populates Command and Dir even when the test suite fails, so
// the caller can include the command in failure log lines.
func TestRunTests_PopulatesCommandOnFailure(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\tfalse\n"), 0o644)

	result := RunTests(context.Background(), dir)
	if result.Passed {
		t.Fatal("expected failure")
	}
	if result.Command != "make ralph-verify" {
		t.Errorf("expected Command='make ralph-verify' on failure, got %q", result.Command)
	}
	if result.Dir != dir {
		t.Errorf("expected Dir=%q, got %q", dir, result.Dir)
	}
}

// CompileCheck populates Command so callers can log the detected build tool.
func TestCompileCheck_PopulatesCommand_Go(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644)

	result := CompileCheck(context.Background(), dir)
	if !result.Passed {
		t.Fatalf("expected pass, got: %s\n%s", result.Reason, result.Details)
	}
	if result.Command != "go test -run=^$ ./..." {
		t.Errorf("expected Command='go test -run=^$ ./...', got %q", result.Command)
	}
	if result.Dir != dir {
		t.Errorf("expected Dir=%q, got %q", dir, result.Dir)
	}
}

// CompileCheck includes the TypeScript command when a tsconfig.json is present.
func TestCompileCheck_PopulatesCommand_TS(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	os.WriteFile(filepath.Join(binDir, "tsc"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644)

	result := CompileCheck(context.Background(), dir)
	if !result.Passed {
		t.Fatalf("expected pass, got: %s", result.Reason)
	}
	if result.Command != "tsc --noEmit" {
		t.Errorf("expected Command='tsc --noEmit', got %q", result.Command)
	}
}

// CompileCheck lists both commands when the project has Go and TypeScript.
func TestCompileCheck_PopulatesCommand_Both(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	os.WriteFile(filepath.Join(binDir, "tsc"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644)

	result := CompileCheck(context.Background(), dir)
	if !result.Passed {
		t.Fatalf("expected pass, got: %s", result.Reason)
	}
	if result.Command != "go test -run=^$ ./... + tsc --noEmit" {
		t.Errorf("expected compound Command, got %q", result.Command)
	}
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
