package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/verify"
)

// stubGitQuerier provides a minimal GitQuerier for Verifier tests,
// decoupling them from a real git.Manager.
type stubGitQuerier struct {
	headRev     string
	diffStat    string
	diffFull    string
	logOneline  string
}

func (s *stubGitQuerier) HeadRev() string                       { return s.headRev }
func (s *stubGitQuerier) DiffStatRange(_, _ string) string      { return s.diffStat }
func (s *stubGitQuerier) DiffFull(_, _ string) string           { return s.diffFull }
func (s *stubGitQuerier) LogOneline(_, _ string) string         { return s.logOneline }

func newTestVerifier(t *testing.T, opts ...func(*Verifier)) *Verifier {
	t.Helper()
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	st := newTestState(t, ralphDir)

	v := NewVerifier(VerifierConfig{
		VerifyDir:             dir,
		VerifyModel:           config.DefaultVerifyModel,
		VerifyEscalationModel: config.DefaultVerifyEscalationModel,
		PromptsDir:            promptsDir,
		RalphDir:              ralphDir,
	}, VerifierDeps{
		Logger:      logging.New(nil),
		Git:         &stubGitQuerier{headRev: "def456"},
		State:       st,
		TaskBackend: &stubBackend{remaining: 1, total: 1, description: "test task"},
		Runner:      func() claudeRunner { return &injectCapturingRunner{} },
		Signals:     claude.DefaultSignalPaths(ralphDir),
		NewRunner:   func() claudeRunner { return &stubRunner{result: stubResult(false, "")} },
		LLMVerify: func(opts verify.VerifyOpts) verify.Result {
			return verify.Result{Passed: true, Reason: "looks good"}
		},
		SkipTask: func(id, reason string) {},
	})

	for _, opt := range opts {
		opt(v)
	}
	return v
}

func newTestState(t *testing.T, ralphDir string) *state.Store {
	t.Helper()
	st := state.NewStore(ralphDir)
	st.Init(5)
	return st
}

// Verifier.OnSignal returns true when both tests and LLM pass, proving
// the Verifier type owns the happy-path verification flow independently.
func TestVerifier_OnSignal_HappyPath(t *testing.T) {
	v := newTestVerifier(t)

	result := v.OnSignal(signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    t.TempDir(),
		rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID:     "test-123",
		nextTask:   "Test task",
	})

	if !result {
		t.Fatal("expected OnSignal to return true when verification passes")
	}
}

// Verifier exhausts LLM retries and calls SkipTask, proving retry logic
// and skip behavior are owned by the Verifier type.
func TestVerifier_OnSignal_LLMExhaustsRetries_SkipsTask(t *testing.T) {
	var skippedID string
	v := newTestVerifier(t, func(v *Verifier) {
		v.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
			return verify.Result{Passed: false, Details: "diff doesn't match bead"}
		}
		v.deps.SkipTask = func(id, reason string) { skippedID = id }
	})

	var result bool
	for i := 0; i < maxLLMVerifyAttempts; i++ {
		result = v.OnSignal(signalParams{
			ctx: context.Background(), headBefore: "abc123",
			workDir: t.TempDir(), rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
			taskID: "test-skip", nextTask: "Skip test",
		})
	}

	if result {
		t.Fatal("expected OnSignal to return false after exhausting retries")
	}
	if skippedID != "test-skip" {
		t.Fatalf("expected task test-skip to be skipped, got %q", skippedID)
	}
}

// Model escalation works through Verifier: first attempt uses Haiku,
// subsequent attempts use Sonnet.
func TestVerifier_ModelEscalation(t *testing.T) {
	var modelsUsed []string
	llmCalls := 0

	v := newTestVerifier(t, func(v *Verifier) {
		v.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
			llmCalls++
			modelsUsed = append(modelsUsed, opts.Model)
			if llmCalls <= 2 {
				return verify.Result{Passed: false, Details: "needs work"}
			}
			return verify.Result{Passed: true, Reason: "approved"}
		}
	})

	params := signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: t.TempDir(), rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID: "test-escalation", nextTask: "Escalation test",
	}

	v.OnSignal(params)
	v.OnSignal(params)
	v.OnSignal(params)

	if len(modelsUsed) != 3 {
		t.Fatalf("expected 3 LLM calls, got %d", len(modelsUsed))
	}
	if modelsUsed[0] != config.DefaultVerifyModel {
		t.Errorf("attempt 1: expected %s, got %s", config.DefaultVerifyModel, modelsUsed[0])
	}
	if modelsUsed[1] != config.DefaultVerifyEscalationModel {
		t.Errorf("attempt 2: expected %s, got %s", config.DefaultVerifyEscalationModel, modelsUsed[1])
	}
}

// ResetCounters clears attempt state so task transitions start fresh.
func TestVerifier_ResetCounters(t *testing.T) {
	v := newTestVerifier(t)
	v.testFixAttempts = 5
	v.llmVerifyAttempts = 3

	v.ResetCounters()

	if v.testFixAttempts != 0 || v.llmVerifyAttempts != 0 {
		t.Fatalf("expected counters to be 0, got test=%d llm=%d", v.testFixAttempts, v.llmVerifyAttempts)
	}
}

// When tests fail post-signal, feedback is injected via stdin and no fix
// agent is spawned — the main agent fixes its own work with full context.
func TestVerifier_OnSignal_TestFailure_InjectsViaStdin_NoFixAgent(t *testing.T) {
	injector := &injectCapturingRunner{}
	fixAgentSpawned := false

	dir := t.TempDir()
	// Create a Makefile with a failing test target so RunTests fails.
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\t@echo 'FAIL: broken test' && exit 1\n"), 0o644)

	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := newTestState(t, ralphDir)

	v := NewVerifier(VerifierConfig{
		VerifyDir:  dir,
		PromptsDir: filepath.Join(dir, "prompts"),
		RalphDir:   ralphDir,
	}, VerifierDeps{
		Logger:      logging.New(nil),
		Git:         &stubGitQuerier{headRev: "def456"},
		State:       st,
		TaskBackend: &stubBackend{remaining: 1, total: 1, description: "test task"},
		Runner:      func() claudeRunner { return injector },
		Signals:     claude.DefaultSignalPaths(ralphDir),
		NewRunner: func() claudeRunner {
			fixAgentSpawned = true
			return &stubRunner{result: stubResult(true, "fixed")}
		},
		LLMVerify: func(opts verify.VerifyOpts) verify.Result {
			return verify.Result{Passed: true, Reason: "ok"}
		},
		SkipTask: func(id, reason string) {},
	})

	result := v.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(dir, "raw.log"),
		taskID: "test-fail", nextTask: "Failing test task",
	})

	if result {
		t.Fatal("expected OnSignal to return false when tests fail")
	}
	if len(injector.injected) == 0 {
		t.Fatal("expected test failure to be injected via stdin")
	}
	if !strings.Contains(injector.injected[0], "Tests failed") {
		t.Errorf("expected injected message about test failure, got: %s", injector.injected[0])
	}
	if fixAgentSpawned {
		t.Fatal("fix agent should NOT be spawned for post-signal test failures — main agent handles it via stdin")
	}
}

// When LLM rejects post-signal, feedback is injected via stdin and no fix
// agent is spawned — the main agent addresses feedback with full context.
func TestVerifier_OnSignal_LLMRejection_InjectsViaStdin_NoFixAgent(t *testing.T) {
	injector := &injectCapturingRunner{}
	fixAgentSpawned := false

	v := newTestVerifier(t, func(v *Verifier) {
		v.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
			return verify.Result{Passed: false, Details: "missing error handling"}
		}
		v.deps.Runner = func() claudeRunner { return injector }
		v.deps.NewRunner = func() claudeRunner {
			fixAgentSpawned = true
			return &stubRunner{result: stubResult(true, "fixed")}
		}
	})

	result := v.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: t.TempDir(), rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID: "test-llm-reject", nextTask: "LLM rejection test",
	})

	if result {
		t.Fatal("expected OnSignal to return false when LLM rejects")
	}
	if len(injector.injected) == 0 {
		t.Fatal("expected LLM rejection feedback to be injected via stdin")
	}
	if !strings.Contains(injector.injected[0], "LLM verification rejected") {
		t.Errorf("expected injected message about LLM rejection, got: %s", injector.injected[0])
	}
	if fixAgentSpawned {
		t.Fatal("fix agent should NOT be spawned for post-signal LLM rejections — main agent handles it via stdin")
	}
}

// TryFixCI still spawns a separate fix agent because post-merge CI failures
// occur after the main agent has exited — there is no stdin pipe to inject into.
func TestVerifier_TryFixCI_SpawnsFixAgent(t *testing.T) {
	fixAgentSpawned := false
	mainRunner := &injectCapturingRunner{}

	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := newTestState(t, ralphDir)

	v := NewVerifier(VerifierConfig{
		VerifyDir:  dir,
		PromptsDir: filepath.Join(dir, "prompts"),
		RalphDir:   ralphDir,
	}, VerifierDeps{
		Logger:      logging.New(nil),
		Git:         &stubGitQuerier{headRev: "def456"},
		State:       st,
		TaskBackend: &stubBackend{remaining: 1, total: 1},
		Runner:      func() claudeRunner { return mainRunner },
		Signals:     claude.DefaultSignalPaths(ralphDir),
		NewRunner: func() claudeRunner {
			fixAgentSpawned = true
			return &stubRunner{result: stubResult(true, "CI fixed")}
		},
		LLMVerify: func(opts verify.VerifyOpts) verify.Result {
			return verify.Result{Passed: true}
		},
		SkipTask: func(id, reason string) {},
	})

	ciErr := &git.CIFailureError{PRNumber: "42"}
	ok := v.TryFixCI(context.Background(), "test output: FAIL", ciErr, "Fix CI", dir, filepath.Join(dir, "raw.log"))

	if !fixAgentSpawned {
		t.Fatal("TryFixCI must spawn a fix agent — main agent is not running for post-merge CI failures")
	}
	if !ok {
		t.Fatal("expected TryFixCI to return true when fix agent signals completion")
	}
}

// Hog mode delegates to fix agent when LLM returns NoDiff, proving
// the feature-existence path goes through the Verifier.
func TestVerifier_HogMode_SpawnsVerifier(t *testing.T) {
	runnerSpawned := false

	v := newTestVerifier(t, func(v *Verifier) {
		v.cfg.VerifyLevel = "hog"
		v.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
			return verify.Result{Passed: true, NoDiff: true, Reason: "no diff"}
		}
		v.deps.NewRunner = func() claudeRunner {
			runnerSpawned = true
			return &stubRunner{result: stubResult(true, "feature exists")}
		}
	})

	result := v.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: t.TempDir(), rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID: "test-hog", nextTask: "Hog test",
	})

	if !result {
		t.Fatal("hog mode should pass when verification agent confirms feature exists")
	}
	if !runnerSpawned {
		t.Fatal("hog mode should spawn a verification agent on no-diff")
	}
}
