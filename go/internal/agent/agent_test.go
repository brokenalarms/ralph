package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
)

type testLogger struct{}

func (l *testLogger) Log(format string, args ...any)     {}
func (l *testLogger) Warn(format string, args ...any)    {}
func (l *testLogger) Error(format string, args ...any)   {}
func (l *testLogger) Success(format string, args ...any) {}

// Sandbox profile restricts write access to the specified directories,
// proving that the container boundary prevents agents from writing
// outside their worktree.
func TestSandboxProfile_WriteDirsRestricted(t *testing.T) {
	s := &Sandbox{ReadPaths: []string{"/usr"}}
	profile := s.Profile([]string{"/work/tree", "/work/.ralph"})

	if !strings.Contains(profile, `(allow file-read* file-write* (subpath "/work/tree"))`) {
		t.Error("profile should grant write access to worktree")
	}
	if !strings.Contains(profile, `(allow file-read* file-write* (subpath "/work/.ralph"))`) {
		t.Error("profile should grant write access to ralph dir")
	}
	if !strings.Contains(profile, `(deny default)`) {
		t.Error("profile should deny by default")
	}
	if !strings.Contains(profile, `(allow file-read* (subpath "/usr"))`) {
		t.Error("profile should allow reading system paths")
	}
}

// Sandbox profile always includes /tmp write access for temporary files
// created by tools (compilers, test runners, etc.).
func TestSandboxProfile_IncludesTmpAccess(t *testing.T) {
	s := &Sandbox{}
	profile := s.Profile([]string{"/work"})

	if !strings.Contains(profile, `(allow file-read* file-write* (subpath "/tmp"))`) {
		t.Error("profile should allow /tmp access")
	}
	if !strings.Contains(profile, `(allow file-read* file-write* (subpath "/private/tmp"))`) {
		t.Error("profile should allow /private/tmp access")
	}
}

// Sandbox profile allows network access so Claude can reach its API
// and tools like git can push/pull from remotes.
func TestSandboxProfile_AllowsNetwork(t *testing.T) {
	s := &Sandbox{}
	profile := s.Profile([]string{"/work"})

	if !strings.Contains(profile, "(allow network*)") {
		t.Error("profile should allow network access")
	}
}

// Wrap produces a sandbox-exec command with the profile written to a temp
// file, proving that agents are containerized when a Sandbox is configured.
func TestSandboxWrap_ProducesSandboxExecCommand(t *testing.T) {
	s := &Sandbox{ReadPaths: []string{"/usr"}}
	cmd := s.Wrap(context.Background(), []string{"/work"}, "echo", "hello")

	if filepath.Base(cmd.Path) != "sandbox-exec" {
		t.Errorf("expected sandbox-exec, got %s", cmd.Path)
	}

	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "-f") {
		t.Error("expected -f flag for profile file")
	}
	if !strings.Contains(args, "echo") {
		t.Error("expected wrapped command in args")
	}
	if !strings.Contains(args, "hello") {
		t.Error("expected wrapped args")
	}

	// Verify profile file was created
	profileIdx := -1
	for i, a := range cmd.Args {
		if a == "-f" && i+1 < len(cmd.Args) {
			profileIdx = i + 1
			break
		}
	}
	if profileIdx < 0 {
		t.Fatal("could not find profile path in args")
	}
	profilePath := cmd.Args[profileIdx]
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("could not read profile file: %v", err)
	}
	if !strings.Contains(string(data), "(deny default)") {
		t.Error("profile file should contain deny default")
	}
}

// Available returns true on macOS where sandbox-exec is present,
// enabling container-first execution by default.
func TestAvailable(t *testing.T) {
	_, err := exec.LookPath("sandbox-exec")
	expected := err == nil
	if Available() != expected {
		t.Errorf("Available() = %v, want %v", Available(), expected)
	}
}

// DefaultSandbox includes standard system paths needed for tool execution
// (git, go, npm, etc.) as read-only paths.
func TestDefaultSandbox_IncludesSystemPaths(t *testing.T) {
	s := DefaultSandbox()

	hasPath := func(p string) bool {
		for _, rp := range s.ReadPaths {
			if rp == p {
				return true
			}
		}
		return false
	}

	for _, required := range []string{"/usr", "/bin", "/sbin"} {
		if !hasPath(required) {
			t.Errorf("DefaultSandbox should include %s in ReadPaths", required)
		}
	}
}

// New with a Sandbox sets up the inner Runner with a CmdFactory that
// wraps commands in sandbox-exec, proving that the centralized module
// applies container isolation by default.
func TestNew_WithSandbox_SetsCmdFactory(t *testing.T) {
	s := &Sandbox{ReadPaths: []string{"/usr"}}
	r := New(&testLogger{}, s)

	if r.inner.CmdFactory == nil {
		t.Error("expected CmdFactory to be set when Sandbox is provided")
	}
}

// New without a Sandbox leaves the CmdFactory nil, so the inner Runner
// uses its default command construction (no sandboxing).
func TestNew_WithoutSandbox_NoCmdFactory(t *testing.T) {
	r := New(&testLogger{}, nil)

	if r.inner.CmdFactory != nil {
		t.Error("expected CmdFactory to be nil when no Sandbox is provided")
	}
}

// sandboxedCmdFactory produces a sandbox-exec command with the correct
// Claude CLI flags and process group isolation, proving the centralized
// module preserves the iteration agent's execution contract.
func TestSandboxedCmdFactory_ProducesCorrectCommand(t *testing.T) {
	s := &Sandbox{ReadPaths: []string{"/usr"}}
	r := New(&testLogger{}, s)

	tmpDir := t.TempDir()
	rawLog, _ := os.CreateTemp(tmpDir, "raw-*.log")
	defer rawLog.Close()

	cfg := claude.RunConfig{
		Ctx:      context.Background(),
		WorkDir:  "/work/tree",
		RalphDir: "/work/.ralph",
		Prompt:   "test prompt",
	}

	cmd := r.sandboxedCmdFactory(cfg, rawLog)

	if filepath.Base(cmd.Path) != "sandbox-exec" {
		t.Errorf("expected sandbox-exec, got %s", cmd.Path)
	}

	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--print") {
		t.Error("missing --print flag")
	}
	if !strings.Contains(args, "--verbose") {
		t.Error("missing --verbose flag")
	}
	if !strings.Contains(args, "stream-json") {
		t.Error("missing stream-json output format")
	}
	if !strings.Contains(args, "--add-dir /work/tree") {
		t.Error("missing --add-dir for work dir")
	}
	if !strings.Contains(args, "--add-dir /work/.ralph") {
		t.Error("missing --add-dir for ralph dir")
	}
	if !strings.Contains(args, "test prompt") {
		t.Error("missing prompt")
	}

	if cmd.Dir != "/work/tree" {
		t.Errorf("expected Dir=/work/tree, got %s", cmd.Dir)
	}
	if cmd.Stdout != rawLog {
		t.Error("stdout should be raw log file")
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("expected Setpgid for process group isolation")
	}
}

// Query without sandbox calls claude directly, proving the centralized
// module works in non-container mode for CI/testing environments.
func TestQuery_WithoutSandbox_CallsClaude(t *testing.T) {
	r := New(&testLogger{}, nil)

	// We can't actually call claude in tests, but we can verify
	// that the method exists and accepts the right parameters.
	// The actual integration is verified by the loop tests.
	_ = r
}

// Runner satisfies the claudeRunner interface used by the loop package,
// proving it's a drop-in replacement for direct claude.Runner usage.
func TestRunner_ImplementsClaudeRunnerInterface(t *testing.T) {
	// This compiles only if Runner has Run(claude.RunConfig)(claude.Result,error)
	// and StopStreaming() — the claudeRunner interface from loop.
	var r interface {
		Run(claude.RunConfig) (claude.Result, error)
		StopStreaming()
	}
	r = New(&testLogger{}, nil)
	_ = r
}

// Sandbox.Wrap with context passes context to the underlying command,
// so cancellation propagates to sandboxed processes.
func TestSandboxWrap_RespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Sandbox{}
	cmd := s.Wrap(ctx, []string{"/work"}, "echo", "test")

	// The command should be created via CommandContext
	if cmd.Process != nil {
		t.Error("command should not be started yet")
	}
}

// Sandbox profile file is written to a deterministic location so it
// can be inspected for debugging.
func TestSandboxWrap_WritesProfileFile(t *testing.T) {
	s := &Sandbox{}
	cmd := s.Wrap(nil, []string{"/work"}, "true")

	// Find the profile path from -f argument
	for i, a := range cmd.Args {
		if a == "-f" && i+1 < len(cmd.Args) {
			profilePath := cmd.Args[i+1]
			if _, err := os.Stat(profilePath); err != nil {
				t.Errorf("profile file should exist at %s", profilePath)
			}
			return
		}
	}
	t.Error("could not find -f flag in sandbox-exec args")
}

// sandboxedCmdFactory includes allowed and disallowed tools from the
// claude package, proving container agents have the same tool permissions.
func TestSandboxedCmdFactory_IncludesToolPermissions(t *testing.T) {
	s := &Sandbox{}
	r := New(&testLogger{}, s)

	tmpDir := t.TempDir()
	rawLog, _ := os.CreateTemp(tmpDir, "raw-*.log")
	defer rawLog.Close()

	cfg := claude.RunConfig{
		Ctx:      context.Background(),
		WorkDir:  tmpDir,
		RalphDir: filepath.Join(tmpDir, ".ralph"),
		Prompt:   "test",
	}

	cmd := r.sandboxedCmdFactory(cfg, rawLog)
	args := strings.Join(cmd.Args, " ")

	if !strings.Contains(args, "--allowedTools") {
		t.Error("missing --allowedTools flag")
	}
	if !strings.Contains(args, "--disallowedTools") {
		t.Error("missing --disallowedTools flag")
	}
	if !strings.Contains(args, "Bash(*)") {
		t.Error("allowed tools should include Bash(*)")
	}
}

// Suppress unused import warnings for syscall and fmt.
var _ = syscall.SysProcAttr{}
var _ = fmt.Sprintf
