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
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verifier"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// newTestVerifier constructs a *verifier.Verifier for tests, deriving the
// verifier's config from the loop's Config. It's the only module where
// construction has enough boilerplate (10 fields to copy from loop.Config)
// to justify a helper — state/git/backend/logger are 1-liners that tests
// construct directly. Tests then build Modules{} inline.
//
// Optional stubs override the production defaults for verifier's two
// sub-modules: newRunner (fix-agent runner factory) and querier. Both
// default to agent.New(logger) when nil.
func newTestVerifier(t *testing.T, cfg Config, logger *logging.Logger, stubs ...verifierTestStubs) *verifier.Verifier {
	t.Helper()
	var s verifierTestStubs
	if len(stubs) > 0 {
		s = stubs[0]
	}
	q := s.querier
	if q == nil && s.queryResponse != "" {
		resp := s.queryResponse
		q = &stubQuerier{fn: func(_ context.Context, _, _, _ string) (string, error) {
			return resp, nil
		}}
	}
	return verifier.New(verifier.Config{
		ProjectDir:            cfg.Dirs.ProjectDir,
		ConfigVerify:          cfg.Verify,
		VerifyModel:           cfg.VerifyModel,
		VerifyEscalationModel: cfg.VerifyEscalationModel,
		FixModel:              cfg.FixModel,
		PromptsDir:            cfg.Dirs.PromptsDir,
		RalphDir:              cfg.Dirs.RalphDir,
		Timeouts:              cfg.Timeouts,
		TestTimeout:           cfg.TestTimeout,
		CompileCheckTimeout:   cfg.CompileCheckTimeout,
		Signals:               claude.DefaultSignalPaths(cfg.Dirs.RalphDir),
	}, logger, s.newRunner, q)
}

// verifierTestStubs bundles optional verifier sub-module stubs. Zero
// values fall back to production defaults. queryResponse is a convenience
// shorthand: when set, it creates a stub querier that always returns that
// response (overridden if querier is also set).
type verifierTestStubs struct {
	newRunner     verifier.RunnerFactory
	querier       verifier.Querier
	queryResponse string
}

type stubRunner struct {
	onRun    func()
	onRunCfg func(cfg claude.RunConfig)
	result   claude.Result
	// queryFn is the per-test override for the Query method.
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

// Query satisfies claudeRunner for tests that control one-shot LLM responses.
func (s *stubRunner) Query(ctx context.Context, workDir, prompt, model string, _ []string) (string, error) {
	if s.queryFn != nil {
		return s.queryFn(ctx, workDir, prompt, model)
	}
	return "NO: stub runner default", nil
}

// stubQuerier satisfies verifier.Querier for tests that need to control
// LLM verification responses without going through the full agent runner.
type stubQuerier struct {
	fn func(ctx context.Context, workDir, prompt, model string) (string, error)
}

func (s *stubQuerier) Query(ctx context.Context, workDir, prompt, model string, _ []string) (string, error) {
	if s.fn == nil {
		return "YES: stub querier default", nil
	}
	return s.fn(ctx, workDir, prompt, model)
}

// stubConnectivity satisfies the Connectivity interface for tests.
// The default zero value reports online + GitHub reachable + waits
// succeed instantly — the configuration most tests want. Tests that
// need to exercise offline / unreachable paths set the corresponding
// override fields.
type stubConnectivity struct {
	githubErr      error
	offline        bool
	waitDeclined   bool
	onWaitInternet func()
}

func (c *stubConnectivity) CheckGitHub(_ context.Context) error { return c.githubErr }
func (c *stubConnectivity) IsOnline() bool                      { return !c.offline }
func (c *stubConnectivity) WaitForInternet(ctx context.Context, _ *logging.Logger) bool {
	if c.onWaitInternet != nil {
		c.onWaitInternet()
	}
	// Honor the Connectivity contract: a cancelled context must produce
	// a false return so cancellation-dependent code paths are exercised
	// rather than masked.
	if ctx.Err() != nil {
		return false
	}
	return !c.waitDeclined
}

// onlineStubConnectivity returns a Connectivity stub that reports
// fully-online state — the configuration most loop tests want when they
// just need to skip the real GitHub / network checks.
func onlineStubConnectivity() *stubConnectivity { return &stubConnectivity{} }

// stubVerifyHook satisfies VerifyHook by returning the configured
// (passed, reason) result on every call. Used by tests that bypass the
// runVerifyPipeline / runSimpleVerifyCompletion paths.
type stubVerifyHook struct {
	passed bool
	reason string
	onCall func(ctx context.Context, dir, headBefore string)
}

func (s *stubVerifyHook) Verify(ctx context.Context, dir, headBefore string) (bool, string) {
	if s.onCall != nil {
		s.onCall(ctx, dir, headBefore)
	}
	return s.passed, s.reason
}

// passingVerifyHook is the most common test stub: returns (true, "")
// from Verify so the caller's verification step is treated as a pass.
func passingVerifyHook() *stubVerifyHook { return &stubVerifyHook{passed: true} }

// stubPostTaskHook satisfies PostTaskHook by recording each call into
// the provided fn. Tests use this to assert that post-task fired with
// the expected arguments.
type stubPostTaskHook struct {
	fn func(ctx context.Context, taskID string, prNumber int, merged bool)
}

func (s *stubPostTaskHook) OnPostTask(ctx context.Context, taskID string, prNumber int, merged bool) {
	if s.fn != nil {
		s.fn(ctx, taskID, prNumber, merged)
	}
}

// stubWaitHook satisfies WaitHook by calling the provided fn on every
// OnWait. Tests use this to detect that the wait-for-tasks path was
// reached.
type stubWaitHook struct {
	fn func()
}

func (s *stubWaitHook) OnWait() {
	if s.fn != nil {
		s.fn()
	}
}

// stubIterationHook satisfies IterationHook by calling the provided fn
// on every OnIterationStart.
type stubIterationHook struct {
	fn func()
}

func (s *stubIterationHook) OnIterationStart() {
	if s.fn != nil {
		s.fn()
	}
}

// stubBinaryHasher satisfies BinaryHasher for evolve tests.
// Calls is incremented on each Hash() call so tests can observe
// how many times the binary was checked. Hashes is a sequence of
// byte slices to return on successive Hash() calls; when exhausted
// the last element is repeated. A single-element Hashes slice
// simulates an unchanged binary; a two-element slice with different
// values simulates a swap after the first call.
type stubBinaryHasher struct {
	Hashes [][]byte
	Calls  int
}

func (h *stubBinaryHasher) Hash() ([]byte, error) {
	if len(h.Hashes) == 0 {
		return []byte("unchanged"), nil
	}
	idx := h.Calls
	if idx >= len(h.Hashes) {
		idx = len(h.Hashes) - 1
	}
	h.Calls++
	return h.Hashes[idx], nil
}

// unchangedBinaryHasher returns a BinaryHasher that always reports the same
// hash — simulating a loop run where no new binary was installed.
func unchangedBinaryHasher() *stubBinaryHasher {
	return &stubBinaryHasher{Hashes: [][]byte{[]byte("same-hash")}}
}

// changedBinaryHasher returns a BinaryHasher that reports a different hash
// on every call past the first — simulating a post-task binary swap.
func changedBinaryHasher() *stubBinaryHasher {
	return &stubBinaryHasher{Hashes: [][]byte{[]byte("old-hash"), []byte("new-hash")}}
}

func setupTestDir(t *testing.T) (string, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	// Projects must have a ralph:verify script; create a passing Makefile target
	// so verification in tests uses the stub's WorkDir and finds a passing script.
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

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		RemoteURL:  "https://github.com/owner/repo.git",
	})
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
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  &testutil.TrackingBackend{},
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: &stubConnectivity{githubErr: errors.New("GitHub connectivity check timed out after 10s")},
	})

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
	names := []string{
		"shared.md", "internal.md", "reflection.md", "signal.md", "feedback.md",
		"execution-bd.md", "bead-creation.md", "verify-review.md",
		"status-tests-pass.md", "status-tests-failing.md",
		"status-build-failing.md", "status-build-broken.md",
		"status-tests-failure-context.md", "status-build-failure-context.md",
	}
	for _, name := range names {
		os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o644)
	}
}

// (l *Loop).taskDescription returns the backend's description for the
// given task ID, or empty string for missing or nil backend cases.
func TestLoopTaskDescription_Standalone(t *testing.T) {
	dir, st := setupTestDir(t)
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	backend := &testutil.StubBackend{Description: "Fix auth middleware"}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
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
	loggerNil := logging.New(nil)
	lNil := New(cfgNil, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: nil,
		Logger:      loggerNil,
		Verifier:    newTestVerifier(t, cfgNil, loggerNil),
	})
	if got := lNil.taskDescription("ralph-abc"); got != "" {
		t.Errorf("nil backend should return empty, got %q", got)
	}
}
