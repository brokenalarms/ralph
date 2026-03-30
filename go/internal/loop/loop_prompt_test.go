package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies that buildTaskPrompt includes the bd ID when one is present,
// matching the shell's task prompt format.
func TestLoop_BuildTaskPrompt(t *testing.T) {
	l := &Loop{}

	got := l.buildTaskPrompt("Implement feature X", "ralph-abc")
	if got != "Complete this task (bd id: ralph-abc): Implement feature X" {
		t.Errorf("unexpected prompt with ID: %q", got)
	}

	got = l.buildTaskPrompt("Implement feature X", "")
	if got != "Complete this task: Implement feature X" {
		t.Errorf("unexpected prompt without ID: %q", got)
	}
}

// Verifies that buildTaskPrompt appends screenshot paths when screenshots
// exist in the ralph screenshots directory for the given bead ID.
func TestLoop_BuildTaskPrompt_WithScreenshots(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	ssDir := filepath.Join(ralphDir, "screenshots")
	os.MkdirAll(ssDir, 0o755)
	os.WriteFile(filepath.Join(ssDir, "ralph-abc-01-broken-modal.png"), []byte("img"), 0o644)

	pDir, err := filepath.Abs(filepath.Join("..", "..", "cmd", "ralph", "prompts"))
	if err != nil {
		t.Fatalf("resolve prompts dir: %v", err)
	}

	backend := &stubBackend{nextID: "ralph-abc", nextTask: "Fix modal", fullContext: "○ ralph-abc · Fix modal [● P3 · OPEN]"}
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: pDir,
		},
		CallsPerHour: 80,
		TaskBackend:  backend,
	}, st, gm, logging.New(nil))

	got := l.buildTaskPrompt("Fix modal", "ralph-abc")

	if !strings.Contains(got, "## Screenshots") {
		t.Error("task prompt should include screenshots section when screenshots exist")
	}
	if !strings.Contains(got, "ralph-abc-01-broken-modal.png") {
		t.Error("task prompt should include the screenshot filename")
	}
}

// Verifies that buildTaskPrompt omits the screenshots section when no
// screenshots exist for the bead.
func TestLoop_BuildTaskPrompt_NoScreenshots(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	pDir, err := filepath.Abs(filepath.Join("..", "..", "cmd", "ralph", "prompts"))
	if err != nil {
		t.Fatalf("resolve prompts dir: %v", err)
	}

	backend := &stubBackend{nextID: "ralph-xyz", nextTask: "Fix layout", fullContext: "○ ralph-xyz · Fix layout [● P3 · OPEN]"}
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: pDir,
		},
		CallsPerHour: 80,
		TaskBackend:  backend,
	}, st, gm, logging.New(nil))

	got := l.buildTaskPrompt("Fix layout", "ralph-xyz")

	if strings.Contains(got, "## Screenshots") {
		t.Error("task prompt should not include screenshots section when none exist")
	}
}

// Verifies that fileLineCount correctly counts newlines in a file.
func TestFileLineCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0o644)
	if got := fileLineCount(path); got != 3 {
		t.Errorf("expected 3 lines, got %d", got)
	}

	if got := fileLineCount(filepath.Join(dir, "nonexistent")); got != 0 {
		t.Errorf("expected 0 for nonexistent file, got %d", got)
	}
}

// Verifies that readLogFrom skips the specified number of lines.
func TestReadLogFrom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	os.WriteFile(path, []byte("line1\nline2\nline3\nline4\n"), 0o644)

	got := readLogFrom(path, 2)
	if got != "line3\nline4\n" {
		t.Errorf("expected 'line3\\nline4\\n', got %q", got)
	}
}

// Verifies that {{TEST_STATUS}} placeholder in internal.md gets
// substituted with the orchestrator's pre-iteration test results,
// so the agent knows the test status without running the suite itself.
func TestLoop_TestStatusIncludedInPrompt(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	// Write templates with {{TEST_STATUS}} in internal.md
	for _, name := range []string{"shared.md", "reflection.md", "signal.md", "feedback.md", "execution-bd.md"} {
		os.WriteFile(filepath.Join(promptsDir, name), []byte("test"), 0o644)
	}
	os.WriteFile(filepath.Join(promptsDir, "internal.md"),
		[]byte("Assumptions\n{{TEST_STATUS}}\n{{TASK_INSTRUCTIONS}}\n{{ATTEMPT_HISTORY}}"), 0o644)

	// Create Makefile with passing tests
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\ttrue\n"), 0o644)

	var capturedPrompt string
	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Do task",
		nextID:    "ralph-ts",
		label:     "beads",
	}

	runner := &stubRunner{
		onRun: func() {
			backend.remaining = 0
			backend.completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		VerifyDir:     dir,
	}, st, gm, logging.New(nil))

	// Capture the prompt passed to Claude
	l.runner = &stubRunner{
		onRun: func() {
			backend.remaining = 0
			backend.completed = 1
		},
		result: runner.result,
	}
	origRunner := l.runner
	l.runner = &promptCapturingRunner{
		inner:    origRunner,
		captured: &capturedPrompt,
	}
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

	_ = l.Run(context.Background())

	if !strings.Contains(capturedPrompt, "tests passing") {
		t.Errorf("prompt should include test status, got: %s", capturedPrompt[:min(200, len(capturedPrompt))])
	}
	// Should not contain the unsubstituted placeholder
	if strings.Contains(capturedPrompt, "{{TEST_STATUS}}") {
		t.Error("prompt should not contain unsubstituted {{TEST_STATUS}} placeholder")
	}
}

type configCapturingRunner struct {
	inner    claudeRunner
	captured *claude.RunConfig
}

func (c *configCapturingRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	*c.captured = cfg
	return c.inner.Run(cfg)
}

func (c *configCapturingRunner) StopStreaming() {
	c.inner.StopStreaming()
}

func (c *configCapturingRunner) InjectMessage(msg string) error {
	return c.inner.InjectMessage(msg)
}

// promptCapturingRunner wraps a claude runner to capture the prompt.

type promptCapturingRunner struct {
	inner    claudeRunner
	captured *string
}

func (p *promptCapturingRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	*p.captured = cfg.Prompt
	return p.inner.Run(cfg)
}

func (p *promptCapturingRunner) StopStreaming() {
	p.inner.StopStreaming()
}

func (p *promptCapturingRunner) InjectMessage(msg string) error {
	return p.inner.InjectMessage(msg)
}

// Verifies that push is called after signal detection. The sync guard
// (fetch + rebase) is enforced internally by PushAndCreatePR's EnsureUpToDate
// — tested in git module.

// prepareAndBuildPrompt: verifies it returns a non-empty prompt and
// the expected head/log state.
func TestLoop_PrepareAndBuildPrompt_ReturnsPrompt(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{remaining: 1, total: 1, nextTask: "Fix login", nextID: "ralph-xyz"}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}

	prep, ok := l.prepareAndBuildPrompt(context.Background(), "ralph-xyz", "Fix login")
	if !ok {
		t.Fatal("expected ok=true from prepareAndBuildPrompt")
	}
	if prep.fullPrompt == "" {
		t.Error("expected non-empty prompt")
	}
	if prep.workDir != dir {
		t.Errorf("expected workDir=%s, got %s", dir, prep.workDir)
	}
}
