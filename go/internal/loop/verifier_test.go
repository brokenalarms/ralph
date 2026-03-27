package loop

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
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
		VerifyDir:  dir,
		PromptsDir: promptsDir,
		RalphDir:   ralphDir,
	}, VerifierDeps{
		Logger:      logging.New(nil),
		Git:         &stubGitQuerier{headRev: "def456"},
		State:       st,
		TaskBackend: &stubBackend{remaining: 1, total: 1, description: "test task"},
		Runner:      func() claudeRunner { return &injectCapturingRunner{} },
		Signals:     claude.DefaultSignalPaths(ralphDir),
		NewRunner:   func() claudeRunner { return &stubRunner{result: stubResult(false, "")} },
		LLMVerify: func(ctx context.Context, gq verify.GitQuerier, workDir, promptsDir, taskID, headBefore, beadTitle, beadDescription, beadAcceptance string, gh git.GitHub, queryFn verify.QueryFunc, model ...string) verify.Result {
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
		v.deps.LLMVerify = func(ctx context.Context, gq verify.GitQuerier, workDir, promptsDir, taskID, headBefore, beadTitle, beadDescription, beadAcceptance string, gh git.GitHub, queryFn verify.QueryFunc, model ...string) verify.Result {
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
		v.deps.LLMVerify = func(ctx context.Context, gq verify.GitQuerier, workDir, promptsDir, taskID, headBefore, beadTitle, beadDescription, beadAcceptance string, gh git.GitHub, queryFn verify.QueryFunc, model ...string) verify.Result {
			llmCalls++
			if len(model) > 0 {
				modelsUsed = append(modelsUsed, model[0])
			}
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

// Hog mode delegates to fix agent when LLM returns NoDiff, proving
// the feature-existence path goes through the Verifier.
func TestVerifier_HogMode_SpawnsVerifier(t *testing.T) {
	runnerSpawned := false

	v := newTestVerifier(t, func(v *Verifier) {
		v.cfg.VerifyLevel = "hog"
		v.deps.LLMVerify = func(ctx context.Context, gq verify.GitQuerier, workDir, promptsDir, taskID, headBefore, beadTitle, beadDescription, beadAcceptance string, gh git.GitHub, queryFn verify.QueryFunc, model ...string) verify.Result {
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
