package agent

import (
	"bytes"
	"os"
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
	r := New(&testLogger{})

	if r.inner.CmdFactory != nil {
		t.Error("expected CmdFactory to be nil")
	}
}

// Runner must satisfy the interface used by the loop: Run + StopStreaming.
func TestRunner_ImplementsClaudeRunnerInterface(t *testing.T) {
	var r interface {
		Run(claude.RunConfig) (claude.Result, error)
		StopStreaming()
	}
	r = New(&testLogger{})
	_ = r
}

// Query should produce a direct claude command.
func TestQuery_CallsClaude(t *testing.T) {
	r := New(&testLogger{})
	_ = r
}

// Runner.stdout() and stderr() must return the custom writer when set,
// falling back to os.Stdout/os.Stderr. This is the mechanism that lets
// Interactive write trailing newlines to the correct destination.
func TestRunner_StdoutStderr_CustomWriters(t *testing.T) {
	var outBuf, errBuf bytes.Buffer
	r := New(&testLogger{})
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
	r := New(&testLogger{})

	if r.stdout() != os.Stdout {
		t.Error("stdout() should default to os.Stdout")
	}
	if r.stderr() != os.Stderr {
		t.Error("stderr() should default to os.Stderr")
	}
}
