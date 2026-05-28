package agent

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/logging"
)

type testLogger struct{}

func (l *testLogger) Emit(_ logging.Opts, _ string, _ ...any)      {}
func (l *testLogger) AgentLog(_ logging.Domain, _ string, _ ...any) {}

// New should create a runner with no CmdFactory — the inner claude.Runner
// uses its default command construction.
func TestNew_NoCmdFactory(t *testing.T) {
	r := New(&testLogger{}, "/tmp/project")

	if r.inner.CmdFactory != nil {
		t.Error("expected CmdFactory to be nil")
	}
}

// New must forward projectDir to both the outer Runner and the inner
// claude.Runner so every spawn path enforces the workDir != projectDir
// invariant.
func TestNew_ForwardsProjectDir(t *testing.T) {
	const projectDir = "/tmp/project"
	r := New(&testLogger{}, projectDir)

	if r.ProjectDir != projectDir {
		t.Errorf("Runner.ProjectDir = %q, want %q", r.ProjectDir, projectDir)
	}
	if r.inner.ProjectDir != projectDir {
		t.Errorf("inner.ProjectDir = %q, want %q", r.inner.ProjectDir, projectDir)
	}
}

// Runner must satisfy the interface used by the loop: Run + StopStreaming.
func TestRunner_ImplementsClaudeRunnerInterface(t *testing.T) {
	var r interface {
		Run(claude.RunConfig) (claude.Result, error)
		StopStreaming()
	}
	r = New(&testLogger{}, "/tmp/project")
	_ = r
}

// Run must refuse to spawn an agent when WorkDir is empty. This is the
// structural defense against worktree setup silently failing and the loop
// continuing with an unconfigured cwd.
func TestRun_RefusesEmptyWorkDir(t *testing.T) {
	r := New(&testLogger{}, "/tmp/project")
	_, err := r.Run(claude.RunConfig{WorkDir: ""})
	if err == nil {
		t.Fatal("expected error when WorkDir is empty")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error should mention refusal, got: %v", err)
	}
}

// Run must refuse to spawn an agent when WorkDir equals ProjectDir. This
// is the chokepoint that prevents agents from writing into the project
// root when worktree setup has fallen back or been bypassed.
func TestRun_RefusesProjectDirAsWorkDir(t *testing.T) {
	const projectDir = "/tmp/project"
	r := New(&testLogger{}, projectDir)
	_, err := r.Run(claude.RunConfig{WorkDir: projectDir})
	if err == nil {
		t.Fatal("expected error when WorkDir == ProjectDir")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error should mention refusal, got: %v", err)
	}
}

// Query must refuse to spawn for the same reasons as Run.
func TestQuery_RefusesProjectDirAsWorkDir(t *testing.T) {
	const projectDir = "/tmp/project"
	r := New(&testLogger{}, projectDir)
	_, err := r.Query(context.Background(), projectDir, "test", "", nil)
	if err == nil {
		t.Fatal("expected error when workDir == projectDir")
	}
}

// Query must refuse to spawn when workDir is empty.
func TestQuery_RefusesEmptyWorkDir(t *testing.T) {
	r := New(&testLogger{}, "/tmp/project")
	_, err := r.Query(context.Background(), "", "test", "", nil)
	if err == nil {
		t.Fatal("expected error when workDir is empty")
	}
}

// Interactive must refuse to spawn an agent at the project root. Catches
// the recurring failure where review or task sessions fell back to the
// main checkout after a worktree setup error.
func TestInteractive_RefusesProjectDirAsWorkDir(t *testing.T) {
	const projectDir = "/tmp/project"
	r := New(&testLogger{}, projectDir)
	exitCode, err := r.Interactive(projectDir, "system")
	if err == nil {
		t.Fatal("expected error when workDir == projectDir")
	}
	if exitCode == 0 {
		t.Errorf("exit code should be non-zero on refusal, got %d", exitCode)
	}
}

// Interactive must refuse to spawn when workDir is empty.
func TestInteractive_RefusesEmptyWorkDir(t *testing.T) {
	r := New(&testLogger{}, "/tmp/project")
	_, err := r.Interactive("", "system")
	if err == nil {
		t.Fatal("expected error when workDir is empty")
	}
}

// Runner.stdout() and stderr() must return the custom writer when set,
// falling back to os.Stdout/os.Stderr. This is the mechanism that lets
// Interactive write trailing newlines to the correct destination.
func TestRunner_StdoutStderr_CustomWriters(t *testing.T) {
	var outBuf, errBuf bytes.Buffer
	r := New(&testLogger{}, "/tmp/project")
	r.Stdout = &outBuf
	r.Stderr = &errBuf

	if r.stdout() != &outBuf {
		t.Error("stdout() should return custom writer when Stdout is set")
	}
	if r.stderr() != &errBuf {
		t.Error("stderr() should return custom writer when Stderr is set")
	}
}

// When Stdout/Stderr are not set, the helpers must fall back to the
// os-level file descriptors so Interactive works with the terminal.
func TestRunner_StdoutStderr_Defaults(t *testing.T) {
	r := New(&testLogger{}, "/tmp/project")

	if r.stdout() != os.Stdout {
		t.Error("stdout() should default to os.Stdout")
	}
	if r.stderr() != os.Stderr {
		t.Error("stderr() should default to os.Stderr")
	}
}
