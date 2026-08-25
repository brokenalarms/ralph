package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/logging"
)

// TestMain disables the real execve for the whole package: every test that
// drives handleLoop on darwin would otherwise replace the test binary with
// taskpolicy. The stub reports failure so the loop proceeds unclamped, which
// is the path those tests exercise.
func TestMain(m *testing.M) {
	execve = func(string, []string, []string) error { return errors.New("execve disabled in tests") }
	os.Exit(m.Run())
}

type execRecorder struct {
	calls int
	path  string
	argv  []string
	env   []string
}

func (r *execRecorder) install(t *testing.T) {
	t.Helper()
	prev := execve
	execve = func(path string, argv, env []string) error {
		r.calls++
		r.path, r.argv, r.env = path, argv, env
		return errors.New("recorded")
	}
	t.Cleanup(func() { execve = prev })
}

func requireDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("taskpolicy re-exec is darwin-only")
	}
}

// Proves: with the default clamp, the loop re-execs itself exactly once via
// taskpolicy -c utility <self> <args>, carrying the marker env so the
// clamped process does not exec again.
func TestReexecUnderQoSClamp_ExecsTaskpolicyWithMarker(t *testing.T) {
	requireDarwin(t)
	t.Setenv(qosClampEnv, "")
	rec := &execRecorder{}
	rec.install(t)

	reexecUnderQoSClamp(config.QoSClampUtility, []string{"loop", "--auto-merge"}, logging.New(io.Discard))

	if rec.calls != 1 {
		t.Fatalf("execve calls = %d, want 1", rec.calls)
	}
	if rec.path != taskpolicyPath {
		t.Errorf("exec path = %q, want %q", rec.path, taskpolicyPath)
	}
	exe, _ := os.Executable()
	want := []string{"taskpolicy", "-c", "utility", exe, "loop", "--auto-merge"}
	if strings.Join(rec.argv, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", rec.argv, want)
	}
	found := false
	for _, kv := range rec.env {
		if kv == qosClampEnv+"=utility" {
			found = true
		}
	}
	if !found {
		t.Errorf("env missing %s=utility: %v", qosClampEnv, rec.env)
	}
}

// Proves: a process already carrying the marker never re-execs, which is what
// stops the clamped process — and the evolve restart that inherits its env —
// from looping.
func TestReexecUnderQoSClamp_SkipsWhenAlreadyClamped(t *testing.T) {
	requireDarwin(t)
	t.Setenv(qosClampEnv, "utility")
	rec := &execRecorder{}
	rec.install(t)

	reexecUnderQoSClamp(config.QoSClampUtility, []string{"loop"}, logging.New(io.Discard))

	if rec.calls != 0 {
		t.Errorf("execve calls = %d, want 0", rec.calls)
	}
}

// Proves: qos_clamp = "none" leaves the loop unclamped.
func TestReexecUnderQoSClamp_NoneDisables(t *testing.T) {
	t.Setenv(qosClampEnv, "")
	rec := &execRecorder{}
	rec.install(t)

	reexecUnderQoSClamp(config.QoSClampNone, []string{"loop"}, logging.New(io.Discard))

	if rec.calls != 0 {
		t.Errorf("execve calls = %d, want 0", rec.calls)
	}
}

// Proves: a failed exec is reported once as a warning and the caller
// continues — the loop runs unclamped rather than refusing to start.
func TestReexecUnderQoSClamp_ExecFailureWarnsAndReturns(t *testing.T) {
	requireDarwin(t)
	t.Setenv(qosClampEnv, "")
	rec := &execRecorder{}
	rec.install(t)
	var buf strings.Builder
	log := logging.NewWithWriter(&buf)

	reexecUnderQoSClamp(config.QoSClampUtility, []string{"loop"}, log)

	if rec.calls != 1 {
		t.Fatalf("execve calls = %d, want 1", rec.calls)
	}
	if !strings.Contains(buf.String(), "QoS clamp not applied") {
		t.Errorf("expected warning in log output, got: %q", buf.String())
	}
}

// Proves: handleLoop applies the clamp after config validation and before the
// pidfile check — the re-exec fires for a valid loop invocation, and the loop
// proceeds when it fails.
func TestHandleLoop_ReexecsUnderQoSClampAfterValidate(t *testing.T) {
	requireDarwin(t)
	t.Setenv(qosClampEnv, "")
	dir := t.TempDir()
	runCmd(t, "git", "-C", dir, "init")
	runCmd(t, "git", "-C", dir, "commit", "--allow-empty", "-m", "init")
	os.MkdirAll(filepath.Join(dir, ".ralph"), 0o755)

	rec := &execRecorder{}
	rec.install(t)
	sub := config.Subcommand{Name: "loop", Dir: dir, Args: []string{"--base-branch", "main"}}
	_ = handleLoop(sub, logging.New(io.Discard))

	if rec.calls != 1 {
		t.Fatalf("execve calls = %d, want 1", rec.calls)
	}
	if len(rec.argv) < 3 || rec.argv[2] != "utility" {
		t.Errorf("argv = %v, want taskpolicy -c utility …", rec.argv)
	}
}

// Proves: a loop that fails config validation never reaches the re-exec.
func TestHandleLoop_NoReexecWhenValidateFails(t *testing.T) {
	t.Setenv(qosClampEnv, "")
	dir := t.TempDir()
	runCmd(t, "git", "-C", dir, "init")
	runCmd(t, "git", "-C", dir, "commit", "--allow-empty", "-m", "init")

	rec := &execRecorder{}
	rec.install(t)
	sub := config.Subcommand{Name: "loop", Dir: dir, Args: []string{"--base-branch", "main", "--evolve"}}
	code := handleLoop(sub, logging.New(io.Discard))

	if code != 1 {
		t.Errorf("exit code = %d, want 1 (--evolve requires --auto-merge)", code)
	}
	if rec.calls != 0 {
		t.Errorf("execve calls = %d, want 0", rec.calls)
	}
}

// Proves: interactive subcommands never touch the clamp.
func TestInteractiveSubcommands_NoReexec(t *testing.T) {
	t.Setenv(qosClampEnv, "")
	rec := &execRecorder{}
	rec.install(t)
	for _, args := range [][]string{{"task", "-h"}, {"review", "-h"}, {"merge", "-h"}} {
		_ = run(args)
	}
	if rec.calls != 0 {
		t.Errorf("execve calls = %d, want 0", rec.calls)
	}
}

// Proves: qos_clamp accepts the four values case-insensitively (via the
// RALPH_QOS_CLAMP env var, the same path config.toml takes) and Validate
// rejects anything else with an error naming them.
func TestQoSClampFlag_Validation(t *testing.T) {
	for _, v := range []string{"utility", "Background", "MAINTENANCE", "none"} {
		t.Setenv("RALPH_QOS_CLAMP", v)
		cfg, err := config.Parse([]string{"--base-branch", "main"})
		if err != nil {
			t.Errorf("qos_clamp %q: unexpected error %v", v, err)
			continue
		}
		if cfg.QoSClamp != strings.ToLower(v) {
			t.Errorf("qos_clamp %q parsed as %q", v, cfg.QoSClamp)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("qos_clamp %q: Validate failed: %v", v, err)
		}
	}

	t.Setenv("RALPH_QOS_CLAMP", "")
	cfg := config.Defaults()
	cfg.BaseBranch = "main"
	if cfg.QoSClamp != config.QoSClampUtility {
		t.Errorf("default qos_clamp = %q, want utility", cfg.QoSClamp)
	}
	cfg.QoSClamp = "turbo"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "utility, background, maintenance, none") {
		t.Errorf("qos_clamp turbo: want Validate error naming accepted values, got %v", err)
	}
}

// Proves: the startup banner names the clamp the loop is running under, so a
// loop that failed to clamp is distinguishable from one that succeeded.
func TestActiveQoSClampLabel(t *testing.T) {
	cases := []struct{ marker, want string }{
		{"utility", "utility — P-cores, below UI band"},
		{"background", "background — E-cores only"},
		{"maintenance", "maintenance — E-cores, lowest priority"},
		{"", "none"},
		{"custom", "custom"},
	}
	for _, c := range cases {
		t.Setenv(qosClampEnv, c.marker)
		if got := activeQoSClampLabel(); got != c.want {
			t.Errorf("marker %q: label = %q, want %q", c.marker, got, c.want)
		}
	}
}
