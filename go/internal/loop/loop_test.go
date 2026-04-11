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

// newTestModules constructs a Modules literal for tests. The cfg argument
// is used to derive the verifier's config — verifier is built once with
// the right config from the start, so no post-construction syncing is
// needed. Tests that need stub sub-modules pass them via the testStubs
// struct (zero values get production defaults).
//
// This is the single entry point for constructing a Modules value in
// tests — tests do not build Modules literals directly. Future fields
// added to Modules land in exactly one place in the test code.
func newTestModules(t *testing.T, cfg Config, st *state.Store, gm git.Ops, backend tasks.Backend, stubs ...testStubs) Modules {
	t.Helper()
	var s testStubs
	if len(stubs) > 0 {
		s = stubs[0]
	}
	logger := s.logger
	if logger == nil {
		logger = logging.New(nil)
	}
	vrf := verifier.New(verifier.Config{
		VerifyDir:             cfg.VerifyDir,
		ProjectDir:            cfg.Dirs.ProjectDir,
		VerifyModel:           cfg.VerifyModel,
		VerifyEscalationModel: cfg.VerifyEscalationModel,
		ModelCap:              cfg.ModelCap,
		PromptsDir:            cfg.Dirs.PromptsDir,
		RalphDir:              cfg.Dirs.RalphDir,
		IdleTimeout:           cfg.IdleTimeout,
		TestTimeout:           cfg.TestTimeout,
		CompileCheckTimeout:   cfg.CompileCheckTimeout,
		Signals:               claude.DefaultSignalPaths(cfg.Dirs.RalphDir),
	}, logger, s.newRunner, s.querier)
	return Modules{State: st, Git: gm, TaskBackend: backend, Logger: logger, Verifier: vrf}
}

// testStubs bundles optional test stubs for newTestModules. Zero values
// fall back to production defaults.
type testStubs struct {
	logger    *logging.Logger
	newRunner verifier.RunnerFactory
	querier   verifier.Querier
}

type stubRunner struct {
	onRun    func()
	onRunCfg func(cfg claude.RunConfig)
	result   claude.Result
	// queryFn is the per-test override for the Query method. When nil,
	// Query returns "NO: stub" so refactor checks short-circuit without
	// an actual LLM call.
	queryFn func(ctx context.Context, workDir, prompt, model string) (string, error)
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

// Query satisfies claudeRunner. Tests that exercise the refactor-decision
// path set s.queryFn to control the response; the default returns "NO" so
// refactor checks short-circuit without an actual LLM call.
func (s *stubRunner) Query(ctx context.Context, workDir, prompt, model string) (string, error) {
	if s.queryFn != nil {
		return s.queryFn(ctx, workDir, prompt, model)
	}
	return "NO: stub runner default", nil
}

// stubQuerier satisfies verifier.Querier for tests that need to control
// LLM verification responses without going through the full agent runner.
// Tests construct &stubQuerier{fn: func(...) (string, error) { ... }} and
// pass it into newTestModules.
type stubQuerier struct {
	fn func(ctx context.Context, workDir, prompt, model string) (string, error)
}

func (s *stubQuerier) Query(ctx context.Context, workDir, prompt, model string) (string, error) {
	if s.fn == nil {
		return "YES: stub querier default", nil
	}
	return s.fn(ctx, workDir, prompt, model)
}

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
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	l := New(cfg, newTestModules(t, cfg, st, gm, &testutil.TrackingBackend{}))

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
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	l := New(cfg, newTestModules(t, cfg, st, gm, backend))

	if got := l.taskDescription("ralph-abc"); got != "Fix auth middleware" {
		t.Errorf("expected description, got %q", got)
	}
	if got := l.taskDescription(""); got != "" {
		t.Errorf("empty taskID should return empty, got %q", got)
	}
	// nil backend → empty
	cfgNil := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	lNil := New(cfgNil, newTestModules(t, cfgNil, st, gm, nil))
	if got := lNil.taskDescription("ralph-abc"); got != "" {
		t.Errorf("nil backend should return empty, got %q", got)
	}
}
