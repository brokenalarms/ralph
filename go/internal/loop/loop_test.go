package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verifier"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// newTestModules returns the Modules literal that loop.New expects, populated
// from the values the test already has in scope. It is the single entry point
// for constructing a Modules value in tests — tests do not build Modules
// literals directly. An optional logger may be passed as the final argument;
// when omitted the helper uses a no-op logger.
//
// The helper exists so tests can stub out all-but-one module without
// repeating every field name on every call site, and so future fields added
// to Modules land in exactly one place in the test code.
func newTestModules(t *testing.T, st *state.Store, gm git.Ops, backend tasks.Backend, loggerOpt ...*logging.Logger) Modules {
	t.Helper()
	logger := logging.New(nil)
	if len(loggerOpt) > 0 && loggerOpt[0] != nil {
		logger = loggerOpt[0]
	}
	vrf := verifier.New(verifier.Config{
		Signals: claude.SignalPaths{},
	}, logger, nil)
	return Modules{State: st, Git: gm, TaskBackend: backend, Logger: logger, Verifier: vrf}
}

// syncVerifierWithConfig reconstructs l.verifier so its internal Config mirrors
// the loop's Config fields that the verifier cares about (VerifyDir, ProjectDir,
// VerifyModel, etc.). Call after loop.New in tests that exercise verifier
// operations relying on these fields — e.g. pre-iteration tests (which early-
// return on empty VerifyDir) or model selection.
//
// newRunner is the fix-agent runner factory the test wants verifier to use;
// pass nil to get the production default (agent.New).
func syncVerifierWithConfig(t *testing.T, l *Loop, newRunner verifier.RunnerFactory) {
	t.Helper()
	l.verifier = verifier.New(verifier.Config{
		VerifyDir:             l.cfg.VerifyDir,
		ProjectDir:            l.cfg.Dirs.ProjectDir,
		VerifyModel:           l.cfg.VerifyModel,
		VerifyEscalationModel: l.cfg.VerifyEscalationModel,
		ModelCap:              l.cfg.ModelCap,
		PromptsDir:            l.cfg.Dirs.PromptsDir,
		RalphDir:              l.cfg.Dirs.RalphDir,
		IdleTimeout:           l.cfg.IdleTimeout,
		TestTimeout:           l.cfg.TestTimeout,
		CompileCheckTimeout:   l.cfg.CompileCheckTimeout,
		Signals:               l.signals,
	}, l.logger, newRunner)
}

type stubRunner struct {
	onRun    func()
	onRunCfg func(cfg claude.RunConfig)
	result   claude.Result
}

func (s *stubRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	if s.onRun != nil {
		s.onRun()
	}
	if s.onRunCfg != nil {
		s.onRunCfg(cfg)
	}
	return s.result, nil
}

func (s *stubRunner) StopStreaming() {}

func (s *stubRunner) InjectMessage(_ string) error { return nil }

func setupTestDir(t *testing.T) (string, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	// Projects must have a ralph:verify script; create a passing Makefile target
	// so tests with VerifyDir set don't fail at startup or verification.
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\ttrue\n"), 0o644)
	st := state.NewStore(ralphDir)
	st.Init(5)
	return dir, st
}

// TestRun_ExitsOnGitHubUnreachable proves that the loop exits immediately with
// a diagnostic error when GitHub is unreachable at startup instead of hanging.
func TestRun_ExitsOnGitHubUnreachable(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		RemoteURLValue: "https://github.com/owner/repo.git",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}, newTestModules(t, st, gm, &testutil.TrackingBackend{}))

	l.cfg.IsOnline = func() bool { return true }
	l.cfg.WaitForInternet = func(context.Context, *logging.Logger) bool { return true }
	l.cfg.CheckGitHub = func(context.Context) error {
		return errors.New("GitHub connectivity check timed out after 10s")
	}

	err := l.Run(context.Background())
	if err == nil {
		t.Fatal("expected error when GitHub is unreachable, got nil")
	}
	if !strings.Contains(err.Error(), "Cannot reach GitHub") {
		t.Errorf("error should contain 'Cannot reach GitHub', got: %v", err)
	}
	if !strings.Contains(err.Error(), "VPN") {
		t.Errorf("error should mention VPN as a possible cause, got: %v", err)
	}
}

// --- test helpers ---

// createPromptTemplates creates minimal prompt template files so the loop
// can build prompts without errors.
func createPromptTemplates(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	for _, name := range []string{"shared.md", "internal.md", "reflection.md", "signal.md", "feedback.md", "refactor.md", "refactor-style.md", "execution-bd.md", "bead-creation.md"} {
		os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o644)
	}
}

// (l *Loop).taskDescription returns the backend's description for the
// given task ID, or empty string for missing or nil backend cases.
func TestLoopTaskDescription_Standalone(t *testing.T) {
	dir, st := setupTestDir(t)
	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}
	backend := &testutil.StubBackend{Description: "Fix auth middleware"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
		MaxIterations: 1,
		CallsPerHour:  80,
	}, newTestModules(t, st, gm, backend))

	if got := l.taskDescription("ralph-abc"); got != "Fix auth middleware" {
		t.Errorf("expected description, got %q", got)
	}
	if got := l.taskDescription(""); got != "" {
		t.Errorf("empty taskID should return empty, got %q", got)
	}

	// nil backend → empty
	lNil := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
		MaxIterations: 1,
		CallsPerHour:  80,
	}, newTestModules(t, st, gm, nil))
	if got := lNil.taskDescription("ralph-abc"); got != "" {
		t.Errorf("nil backend should return empty, got %q", got)
	}
}
