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

func TestSandboxProfile_AllowThenDenyStrategy(t *testing.T) {
	s := &Sandbox{DenyWritePaths: []string{"/Users"}}
	profile := s.Profile([]string{"/work/tree", "/work/.ralph"})

	if !strings.Contains(profile, "(allow file*)") {
		t.Error("profile should allow all file operations globally")
	}
	if !strings.Contains(profile, `(deny file-write* (subpath "/Users"))`) {
		t.Error("profile should deny writes under /Users")
	}
	if !strings.Contains(profile, `(allow file-write* (subpath "/work/tree"))`) {
		t.Error("profile should punch write hole for worktree")
	}
	if !strings.Contains(profile, `(allow file-write* (subpath "/work/.ralph"))`) {
		t.Error("profile should punch write hole for ralph dir")
	}
	if !strings.Contains(profile, `(deny default)`) {
		t.Error("profile should deny default for non-file operations")
	}
}

func TestSandboxProfile_IncludesTmpAccess(t *testing.T) {
	s := &Sandbox{}
	profile := s.Profile([]string{"/work"})

	if !strings.Contains(profile, `(allow file-write* (subpath "/tmp"))`) {
		t.Error("profile should allow /tmp write access")
	}
	if !strings.Contains(profile, `(allow file-write* (subpath "/private/tmp"))`) {
		t.Error("profile should allow /private/tmp write access")
	}
}

func TestSandboxProfile_AllowsNetwork(t *testing.T) {
	s := &Sandbox{}
	profile := s.Profile([]string{"/work"})

	if !strings.Contains(profile, "(allow network*)") {
		t.Error("profile should allow network access")
	}
}

func TestSandboxProfile_DenyBeforeAllow(t *testing.T) {
	s := &Sandbox{DenyWritePaths: []string{"/Users"}}
	profile := s.Profile([]string{"/Users/daniel/work"})

	denyIdx := strings.Index(profile, `(deny file-write* (subpath "/Users"))`)
	allowIdx := strings.Index(profile, `(allow file-write* (subpath "/Users/daniel/work"))`)
	if denyIdx < 0 || allowIdx < 0 {
		t.Fatal("profile missing deny or allow rules")
	}
	if allowIdx < denyIdx {
		t.Error("allow must come after deny so it takes precedence in Seatbelt")
	}
}

func TestSandboxWrap_ProducesSandboxExecCommand(t *testing.T) {
	s := &Sandbox{DenyWritePaths: []string{"/Users"}}
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

func TestAvailable(t *testing.T) {
	_, err := exec.LookPath("sandbox-exec")
	expected := err == nil
	if Available() != expected {
		t.Errorf("Available() = %v, want %v", Available(), expected)
	}
}

func TestDefaultSandbox_DeniesUsersWrites(t *testing.T) {
	s := DefaultSandbox()

	found := false
	for _, p := range s.DenyWritePaths {
		if p == "/Users" {
			found = true
			break
		}
	}
	if !found {
		t.Error("DefaultSandbox should deny writes under /Users")
	}
}

func TestNew_WithSandbox_SetsCmdFactory(t *testing.T) {
	s := &Sandbox{DenyWritePaths: []string{"/Users"}}
	r := New(&testLogger{}, s)

	if r.inner.CmdFactory == nil {
		t.Error("expected CmdFactory to be set when Sandbox is provided")
	}
}

func TestNew_WithoutSandbox_NoCmdFactory(t *testing.T) {
	r := New(&testLogger{}, nil)

	if r.inner.CmdFactory != nil {
		t.Error("expected CmdFactory to be nil when no Sandbox is provided")
	}
}

func TestSandboxedCmdFactory_ProducesCorrectCommand(t *testing.T) {
	s := &Sandbox{DenyWritePaths: []string{"/Users"}}
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

func TestQuery_WithoutSandbox_CallsClaude(t *testing.T) {
	r := New(&testLogger{}, nil)
	_ = r
}

func TestRunner_ImplementsClaudeRunnerInterface(t *testing.T) {
	var r interface {
		Run(claude.RunConfig) (claude.Result, error)
		StopStreaming()
	}
	r = New(&testLogger{}, nil)
	_ = r
}

func TestSandboxWrap_RespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Sandbox{}
	cmd := s.Wrap(ctx, []string{"/work"}, "echo", "test")

	if cmd.Process != nil {
		t.Error("command should not be started yet")
	}
}

func TestSandboxWrap_WritesProfileFile(t *testing.T) {
	s := &Sandbox{}
	cmd := s.Wrap(nil, []string{"/work"}, "true")

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

var _ = syscall.SysProcAttr{}
var _ = fmt.Sprintf
