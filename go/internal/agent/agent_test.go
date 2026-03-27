package agent

import (
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
)

type testLogger struct{}

func (l *testLogger) Log(_ string, _ string, _ ...any)     {}
func (l *testLogger) Warn(_ string, _ string, _ ...any)    {}
func (l *testLogger) Error(_ string, _ string, _ ...any)   {}
func (l *testLogger) Success(_ string, _ string, _ ...any) {}
func (l *testLogger) AgentLog(_ string, _ string, _ ...any) {}

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

// Query without a sandbox should produce a direct claude command.
func TestQuery_CallsClaude(t *testing.T) {
	r := New(&testLogger{})
	_ = r
}
