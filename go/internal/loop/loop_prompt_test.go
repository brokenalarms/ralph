package loop

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// loopForPromptTest constructs a minimal Loop with the given backend and
// dirs for the buildTaskPrompt tests, which read l.taskBackend,
// l.cfg.Dirs.PromptsDir, and l.cfg.Dirs.RalphDir via the receiver.
func loopForPromptTest(t *testing.T, dir, ralphDir, promptsDir string, backend *testutil.StubBackend) *Loop {
	t.Helper()
	_, st := setupTestDir(t)
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	return New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})
}

// Verifies that buildTaskPrompt includes the bd ID when one is present,
// matching the shell's task prompt format.
func TestLoop_BuildTaskPrompt(t *testing.T) {
	dir := t.TempDir()
	l := loopForPromptTest(t, dir, filepath.Join(dir, ".ralph"), "", &testutil.StubBackend{})

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
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	ssDir := filepath.Join(ralphDir, "screenshots")
	os.MkdirAll(ssDir, 0o755)
	os.WriteFile(filepath.Join(ssDir, "ralph-abc-01-broken-modal.png"), []byte("img"), 0o644)

	pDir, err := filepath.Abs(filepath.Join("..", "..", "cmd", "ralph", "prompts"))
	if err != nil {
		t.Fatalf("resolve prompts dir: %v", err)
	}

	backend := &testutil.StubBackend{NextID: "ralph-abc", NextTask: "Fix modal", FullContext: "○ ralph-abc · Fix modal [● P3 · OPEN]"}
	l := loopForPromptTest(t, dir, ralphDir, pDir, backend)

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
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")

	pDir, err := filepath.Abs(filepath.Join("..", "..", "cmd", "ralph", "prompts"))
	if err != nil {
		t.Fatalf("resolve prompts dir: %v", err)
	}

	backend := &testutil.StubBackend{NextID: "ralph-xyz", NextTask: "Fix layout", FullContext: "○ ralph-xyz · Fix layout [● P3 · OPEN]"}
	l := loopForPromptTest(t, dir, ralphDir, pDir, backend)

	got := l.buildTaskPrompt("Fix layout", "ralph-xyz")

	if strings.Contains(got, "## Screenshots") {
		t.Error("task prompt should not include screenshots section when none exist")
	}
}

// TestBuildPrompt_LogsComponentSizes verifies that buildPrompt emits a log entry
// with byte sizes for taskPrompt, attemptHistory, testStatus, tasksContext, total,
// and template overhead — needed to diagnose compaction regressions.
func TestBuildPrompt_LogsComponentSizes(t *testing.T) {
	dir := t.TempDir()
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	var buf bytes.Buffer
	logger := logging.NewWithWriter(&buf)

	_, st := setupTestDir(t)
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{},
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})
	l.runner = &stubRunner{}

	_, err := l.buildPrompt("task prompt content", "attempt history text", "tests passing")
	if err != nil {
		t.Fatalf("buildPrompt failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Prompt sizes") {
		t.Errorf("expected 'Prompt sizes' log entry, got: %s", output)
	}
	for _, field := range []string{"taskPrompt:", "attemptHistory:", "testStatus:", "tasksContext:", "total:", "template overhead:"} {
		if !strings.Contains(output, field) {
			t.Errorf("expected log to contain %q, got: %s", field, output)
		}
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
// substituted with the orchestrator's baseline test results,
// so the agent knows the test status without running the suite itself.
func TestLoop_TestStatusIncludedInPrompt(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	// Write templates with {{TEST_STATUS}} in internal.md
	for _, name := range []string{"shared.md", "reflection.md", "signal.md", "feedback.md", "execution-bd.md", "bead-creation.md"} {
		os.WriteFile(filepath.Join(promptsDir, name), []byte("test"), 0o644)
	}
	os.WriteFile(filepath.Join(promptsDir, "internal.md"),
		[]byte("Assumptions\n{{TEST_STATUS}}\n{{TASK_INSTRUCTIONS}}\n{{ATTEMPT_HISTORY}}"), 0o644)
	os.WriteFile(filepath.Join(promptsDir, "status-tests-pass.md"),
		[]byte("- Test suite status: all tests passing as of start."), 0o644)

	// Create Makefile with passing tests
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\ttrue\n"), 0o644)

	var capturedPrompt string
	backend := &testutil.StubBackend{
		Remaining:    1,
		Total:        1,
		NextTask:     "Do task",
		NextID:       "ralph-ts",
		BackendLabel: "beads",
	}

	runner := &stubRunner{
		onRun: func() {
			backend.Remaining = 0
			backend.Completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})

	// Capture the prompt passed to Claude
	l.runner = &stubRunner{
		onRun: func() {
			backend.Remaining = 0
			backend.Completed = 1
		},
		result: runner.result,
	}
	origRunner := l.runner
	l.runner = &promptCapturingRunner{
		inner:    origRunner,
		captured: &capturedPrompt,
	}

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

	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix login", NextID: "ralph-xyz"}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})
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

// Verifies that idle timeout selection is driven by log activity, not git state.
//
// A real claude.Runner (with CmdFactory) is used so poll() actually executes.
// The subprocess writes a plaintext line to the raw log after 100ms, flipping
// activitySeen in poll(). That causes the short IdleTimeoutProgress to fire
// rather than the long IdleTimeout, proving log activity — not git state —
// controls which timeout is active.
func TestLoop_HasProgress_LogActivityDrivesTimeout(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)
	os.MkdirAll(ralphDir, 0o755)

	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix login", NextID: "ralph-abc"}
	// Pre-existing diff and HEAD movement must not trigger the short timeout —
	// only log activity should.
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		HasDiff:    true,
		HeadRev:    "stub-head-0",
	})

	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		Timeouts: claude.Timeouts{
			Idle:         30 * time.Second,       // long — must not fire
			IdleProgress: 100 * time.Millisecond, // short — fires once activity seen
		},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	// Real runner with a CmdFactory: subprocess writes a plaintext line after
	// 100ms then sleeps. poll() detects the content on the first 2s tick,
	// flips activitySeen, then fires IdleTimeoutProgress (100ms) on the next tick.
	l.runner = &claude.Runner{
		Logger: logger,
		CmdFactory: func(rc claude.RunConfig, rawLog *os.File) *exec.Cmd {
			cmd := exec.Command("sh", "-c", "sleep 0.1; printf 'agent output\\n'; sleep 30")
			cmd.Dir = rc.WorkDir
			cmd.Stdout = rawLog
			cmd.Stderr = rawLog
			return cmd
		},
	}

	// Simulate HEAD moving mid-run — must not affect timeout selection. The
	// commit must land after the agent is underway, so wait until the
	// subprocess has written "agent output" to the raw log before committing.
	activeRawLog := logging.ActiveLogPath(ralphDir, "raw")
	go func() {
		testutil.WaitFor(t, 10*time.Second, "agent output in raw log", func() bool {
			data, err := os.ReadFile(activeRawLog)
			return err == nil && strings.Contains(string(data), "agent output")
		})
		gm.CommitAll("simulated commit during run")
	}()

	start := time.Now()
	l.runAgent(context.Background(), taskContext{id: "ralph-abc", title: "Fix login"}, 0)
	elapsed := time.Since(start)

	// With PollInterval=2s: activity detected at ~2s, then IdleTimeoutProgress
	// (100ms) fires at ~4s. The long IdleTimeout (30s) must not have fired.
	if elapsed >= 10*time.Second {
		t.Errorf("log activity should trigger short progress timeout (~4s), but took %s — long idle timeout may have fired", elapsed)
	}
}
