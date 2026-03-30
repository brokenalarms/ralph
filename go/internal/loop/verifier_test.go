package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verify"
)

// promptCapturingFixRunner captures the prompt passed to Run so tests can
// verify that fix agents receive the correct context.
type promptCapturingFixRunner struct {
	onPrompt func(string)
	result   claude.Result
}

func (r *promptCapturingFixRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	if r.onPrompt != nil {
		r.onPrompt(cfg.Prompt)
	}
	return r.result, nil
}

func (r *promptCapturingFixRunner) StopStreaming() {}

func (r *promptCapturingFixRunner) InjectMessage(_ string) error { return nil }

// stubGitQuerier provides a minimal GitQuerier for Verifier tests,
// decoupling them from a real git.Manager.
type stubGitQuerier struct {
	headRev    string
	diffStat   string
	diffFull   string
	logOneline string
}

func (s *stubGitQuerier) HeadRev() string                  { return s.headRev }
func (s *stubGitQuerier) DiffStatRange(_, _ string) string { return s.diffStat }
func (s *stubGitQuerier) DiffFull(_, _ string) string      { return s.diffFull }
func (s *stubGitQuerier) LogOneline(_, _ string) string    { return s.logOneline }

func newTestVerifier(t *testing.T, opts ...func(*Verifier)) *Verifier {
	t.Helper()
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	st := newTestState(t, ralphDir)

	v := NewVerifier(VerifierConfig{
		VerifyDir:  dir,
		PromptsDir: promptsDir,
		RalphDir:   ralphDir,
	}, VerifierDeps{
		Logger:      logging.New(nil),
		Git:         &stubGitQuerier{headRev: "def456"},
		State:       st,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		Runner:      func() claudeRunner { return &stubRunner{} },
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
	if modelsUsed[0] != verify.ModelHaiku {
		t.Errorf("attempt 1: expected %s, got %s", verify.ModelHaiku, modelsUsed[0])
	}
	if modelsUsed[1] != verify.ModelSonnet {
		t.Errorf("attempt 2: expected %s, got %s", verify.ModelSonnet, modelsUsed[1])
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

// Test failures spawn a fix agent instead of using stdin injection.
// The fix agent receives the test output in its prompt and runs within
// the same OnSignal call.
func TestVerifier_OnSignal_TestFailure_SpawnsFixAgent(t *testing.T) {
	var fixPromptReceived string
	fixAgentCalled := false

	v := newTestVerifier(t, func(v *Verifier) {
		verifyDir := v.cfg.VerifyDir
		os.WriteFile(filepath.Join(verifyDir, "Makefile"), []byte("test:\n\t@echo 'FAIL: widget_test.go:42 expected 3 got 5' && exit 1\n"), 0o644)

		v.deps.NewRunner = func() claudeRunner {
			fixAgentCalled = true
			// Fix agent "fixes" the tests by removing the failing Makefile
			os.Remove(filepath.Join(verifyDir, "Makefile"))
			return &promptCapturingFixRunner{
				onPrompt: func(p string) { fixPromptReceived = p },
				result:   stubResult(true, "fixed tests"),
			}
		}
	})

	result := v.OnSignal(signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    t.TempDir(),
		rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID:     "test-fix1",
		nextTask:   "Build widget",
	})

	if !result {
		t.Fatal("expected OnSignal to return true after fix agent fixes tests")
	}
	if !fixAgentCalled {
		t.Fatal("expected test failure to spawn a fix agent, not use stdin injection")
	}
	if !strings.Contains(fixPromptReceived, "widget_test.go:42") {
		t.Errorf("fix agent prompt should contain test failure output, got: %q", fixPromptReceived)
	}
}

// Test fix agent that fails to signal causes OnSignal to return false,
// not start a new iteration.
func TestVerifier_OnSignal_TestFailure_FixAgentNoSignal_ReturnsFalse(t *testing.T) {
	v := newTestVerifier(t, func(v *Verifier) {
		os.WriteFile(filepath.Join(v.cfg.VerifyDir, "Makefile"), []byte("test:\n\t@echo 'FAIL' && exit 1\n"), 0o644)

		v.deps.NewRunner = func() claudeRunner {
			return &stubRunner{result: stubResult(false, "")}
		}
	})

	result := v.OnSignal(signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    t.TempDir(),
		rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID:     "test-fix2",
		nextTask:   "Build widget",
	})

	if result {
		t.Fatal("expected OnSignal to return false when fix agent fails to signal")
	}
}

// Test fix loop exhausts max attempts and returns false.
func TestVerifier_OnSignal_TestFailure_ExhaustsRetries(t *testing.T) {
	fixAttempts := 0

	v := newTestVerifier(t, func(v *Verifier) {
		os.WriteFile(filepath.Join(v.cfg.VerifyDir, "Makefile"), []byte("test:\n\t@echo 'FAIL' && exit 1\n"), 0o644)

		v.deps.NewRunner = func() claudeRunner {
			fixAttempts++
			// Fix agent signals success but tests keep failing
			return &stubRunner{result: stubResult(true, "attempted fix")}
		}
	})

	result := v.OnSignal(signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    t.TempDir(),
		rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID:     "test-fix3",
		nextTask:   "Build widget",
	})

	if result {
		t.Fatal("expected OnSignal to return false after exhausting test fix attempts")
	}
	if fixAttempts != maxTestFixAttempts {
		t.Fatalf("expected %d fix attempts, got %d", maxTestFixAttempts, fixAttempts)
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
