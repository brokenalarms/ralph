package loop

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/verify"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies Loop.onSignal delegates to Verifier and returns true when both
// test verification and LLM verification pass on the first attempt.
func TestOnSignal_HappyPath(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   &stubBackend{remaining: 1, total: 1, description: "test task"},
		VerifyDir:     dir,
	}, st, gm, logger)

	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		return verify.Result{Passed: true, Reason: "looks good"}
	}
	l.verifyFunc = func(ctx context.Context, dir, headBefore string) (bool, string) {
		return true, ""
	}

	result := l.onSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-123", nextTask: "Test task",
	})
	if !result {
		t.Fatal("expected onSignal to return true when verification passes")
	}
}

// LLM rejects every attempt — verification loop exhausts maxLLMVerifyAttempts
// and skips the task.
func TestOnSignal_LLMReject_ExhaustsRetries_SkipsTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)
	backend := &stubBackend{remaining: 1, total: 1, description: "test task"}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		VerifyDir:     dir,
	}, st, gm, logger)

	l.verifier.deps.Runner = func() claudeRunner { return &injectCapturingRunner{} }

	llmCalls := 0
	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		llmCalls++
		return verify.Result{Passed: false, Details: "diff doesn't match bead"}
	}

	params := signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-123", nextTask: "Test task",
	}

	var result bool
	for i := 0; i < maxLLMVerifyAttempts; i++ {
		result = l.onSignal(params)
	}
	if result {
		t.Fatal("expected onSignal to return false when LLM verification exhausts retries")
	}
	if llmCalls != maxLLMVerifyAttempts {
		t.Fatalf("expected %d LLM verify calls, got %d", maxLLMVerifyAttempts, llmCalls)
	}
	if backend.skippedTask != "test-123" {
		t.Fatalf("expected test-123 deferred in backend, got %q", backend.skippedTask)
	}
}

// First verification uses Haiku; after rejection + fix, re-verification
// escalates to Sonnet.
func TestOnSignal_LLMVerify_ModelEscalation(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   &stubBackend{remaining: 1, total: 1, description: "test task"},
		VerifyDir:     dir,
	}, st, gm, logger)

	l.verifier.deps.Runner = func() claudeRunner { return &injectCapturingRunner{} }

	var modelsUsed []string
	llmCalls := 0
	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		llmCalls++
		modelsUsed = append(modelsUsed, opts.Model)
		if llmCalls <= 2 {
			return verify.Result{Passed: false, Details: "needs work"}
		}
		return verify.Result{Passed: true, Reason: "approved"}
	}

	params := signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-escalation", nextTask: "Escalation test",
	}

	l.onSignal(params)
	l.onSignal(params)
	l.onSignal(params)

	if len(modelsUsed) != 3 {
		t.Fatalf("expected 3 LLM calls, got %d", len(modelsUsed))
	}
	if modelsUsed[0] != verify.ModelHaiku {
		t.Errorf("attempt 1: expected %s, got %s", verify.ModelHaiku, modelsUsed[0])
	}
	if modelsUsed[1] != verify.ModelSonnet {
		t.Errorf("attempt 2: expected %s, got %s", verify.ModelSonnet, modelsUsed[1])
	}
	if modelsUsed[2] != verify.ModelSonnet {
		t.Errorf("attempt 3: expected %s, got %s", verify.ModelSonnet, modelsUsed[2])
	}
}

// Config-driven model selection overrides defaults.
func TestOnSignal_ConfigDrivenModels(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	customModel := "claude-haiku-custom"
	customEscalation := "claude-sonnet-custom"

	l := New(Config{
		Dirs:                  workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:         5,
		CallsPerHour:          80,
		TaskBackend:           &stubBackend{remaining: 1, total: 1, description: "test task"},
		VerifyDir:             dir,
		VerifyModel:           customModel,
		VerifyEscalationModel: customEscalation,
	}, st, gm, logger)

	l.verifier.deps.Runner = func() claudeRunner { return &injectCapturingRunner{} }

	var modelsUsed []string
	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		modelsUsed = append(modelsUsed, opts.Model)
		if len(modelsUsed) < 3 {
			return verify.Result{Passed: false, Details: "needs work"}
		}
		return verify.Result{Passed: true, Reason: "approved"}
	}

	params := signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-config-models", nextTask: "Config models test",
	}

	l.onSignal(params)
	l.onSignal(params)
	l.onSignal(params)

	if len(modelsUsed) != 3 {
		t.Fatalf("expected 3 LLM calls, got %d", len(modelsUsed))
	}
	if modelsUsed[0] != customModel {
		t.Errorf("attempt 1: expected %s, got %s", customModel, modelsUsed[0])
	}
	if modelsUsed[1] != customEscalation {
		t.Errorf("attempt 2: expected %s, got %s", customEscalation, modelsUsed[1])
	}
	if modelsUsed[2] != customEscalation {
		t.Errorf("attempt 3: expected %s, got %s", customEscalation, modelsUsed[2])
	}
}

// LLM rejects once, fix agent fixes, LLM approves on second attempt.
func TestOnSignal_LLMReject_PassesOnRetry(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   &stubBackend{remaining: 1, total: 1, description: "test task"},
		VerifyDir:     dir,
	}, st, gm, logger)

	l.verifier.deps.Runner = func() claudeRunner { return &injectCapturingRunner{} }

	llmCalls := 0
	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		llmCalls++
		if llmCalls == 1 {
			return verify.Result{Passed: false, Details: "missing error handling"}
		}
		return verify.Result{Passed: true, Reason: "looks good after fix"}
	}

	params := signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-retry", nextTask: "Retry test",
	}

	result := l.onSignal(params)
	if result {
		t.Fatal("expected first onSignal to return false when LLM rejects")
	}
	result = l.onSignal(params)
	if !result {
		t.Fatal("expected onSignal to pass when LLM approves on retry")
	}
	if llmCalls != 2 {
		t.Fatalf("expected 2 LLM verify calls, got %d", llmCalls)
	}
}

// When the fix agent exits without signaling, the verification loop stops.
func TestOnSignal_LLMReject_FixAgentNoSignal_StopsLoop(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   &stubBackend{remaining: 1, total: 1, description: "test task"},
		VerifyDir:     dir,
	}, st, gm, logger)

	l.verifier.deps.Runner = func() claudeRunner { return &injectFailRunner{result: claude.Result{}} }

	llmCalls := 0
	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		llmCalls++
		return verify.Result{Passed: false, Details: "bad code"}
	}

	l.verifier.deps.NewRunner = func() claudeRunner {
		return &stubRunner{result: stubResult(false, "")}
	}

	result := l.onSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-nosignal", nextTask: "No signal test",
	})

	if result {
		t.Fatal("expected onSignal to return false when fix agent doesn't signal")
	}
	if llmCalls != 1 {
		t.Fatalf("expected 1 LLM call (no retry after fix agent failure), got %d", llmCalls)
	}
}

// In fire mode (default), a no-diff LLM result passes without spawning
// a verification agent.
func TestOnSignal_FireMode_NoDiffAccepted(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   &stubBackend{remaining: 1, total: 1, description: "test task"},
		VerifyDir:     dir,
		VerifyLevel:   "fire",
	}, st, gm, logger)

	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		return verify.Result{Passed: true, NoDiff: true, Reason: "no diff"}
	}

	runnerSpawned := false
	l.verifier.deps.NewRunner = func() claudeRunner {
		runnerSpawned = true
		return &stubRunner{result: stubResult(true, "confirmed")}
	}

	result := l.onSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-fire", nextTask: "Fire test",
	})

	if !result {
		t.Fatal("fire mode should accept no-diff completion")
	}
	if runnerSpawned {
		t.Fatal("fire mode should not spawn a verification agent")
	}
}

// In hog mode, a no-diff LLM result spawns a verification agent.
func TestOnSignal_HogMode_NoDiffSpawnsVerifier_Passes(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   &stubBackend{remaining: 1, total: 1, description: "test task"},
		VerifyDir:     dir,
		VerifyLevel:   "hog",
	}, st, gm, logger)

	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		return verify.Result{Passed: true, NoDiff: true, Reason: "no diff"}
	}

	runnerSpawned := false
	l.verifier.deps.NewRunner = func() claudeRunner {
		runnerSpawned = true
		return &stubRunner{result: stubResult(true, "feature exists")}
	}

	result := l.onSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-hog", nextTask: "Hog test",
	})

	if !result {
		t.Fatal("hog mode should pass when verification agent confirms feature exists")
	}
	if !runnerSpawned {
		t.Fatal("hog mode should spawn a verification agent on no-diff")
	}
}

// In hog mode, when the verification agent exits without signaling,
// onSignal rejects and the task is skipped.
func TestOnSignal_HogMode_NoDiffVerifierRejects(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)
	backend := &stubBackend{remaining: 1, total: 1, description: "test task"}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		VerifyDir:     dir,
		VerifyLevel:   "hog",
	}, st, gm, logger)

	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		return verify.Result{Passed: true, NoDiff: true, Reason: "no diff"}
	}

	l.verifier.deps.NewRunner = func() claudeRunner {
		return &stubRunner{result: stubResult(false, "")}
	}

	result := l.onSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-hog-reject", nextTask: "Hog reject test",
	})

	if result {
		t.Fatal("hog mode should reject when verification agent does not confirm")
	}
}

func stubResult(signal bool, summary string) claude.Result {
	return claude.Result{
		SignalDetected: signal,
		Summary:        summary,
	}
}
