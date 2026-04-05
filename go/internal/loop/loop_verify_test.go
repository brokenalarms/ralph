package loop

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		VerifyDir:     dir,
	}, st, gm, logger)

	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		return verify.Result{Passed: true, Reason: "looks good"}
	}
	l.cfg.OnVerify = func(ctx context.Context, dir, headBefore string) (bool, string) {
		return true, ""
	}

	result := l.verifier.OnSignal(signalParams{
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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		VerifyDir:     dir,
	}, st, gm, logger)

	l.verifier.deps.NewRunner = func() claudeRunner {
		return &stubRunner{result: stubResult(true, "attempted fix")}
	}

	llmCalls := 0
	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		llmCalls++
		return verify.Result{Passed: false, Details: "diff doesn't match bead"}
	}

	result := l.verifier.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-123", nextTask: "Test task",
	})
	if result {
		t.Fatal("expected onSignal to return false when LLM verification exhausts retries")
	}
	if llmCalls != maxLLMVerifyAttempts {
		t.Fatalf("expected %d LLM verify calls, got %d", maxLLMVerifyAttempts, llmCalls)
	}
	if backend.SkippedTask != "test-123" {
		t.Fatalf("expected test-123 deferred in backend, got %q", backend.SkippedTask)
	}
}

// All verification attempts use Sonnet regardless of attempt number.
func TestOnSignal_LLMVerify_ModelEscalation(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		VerifyDir:     dir,
	}, st, gm, logger)

	l.verifier.deps.NewRunner = func() claudeRunner {
		return &stubRunner{result: stubResult(true, "attempted fix")}
	}

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

	l.verifier.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-escalation", nextTask: "Escalation test",
	})

	if len(modelsUsed) != 3 {
		t.Fatalf("expected 3 LLM calls, got %d", len(modelsUsed))
	}
	if modelsUsed[0] != verify.ModelSonnet {
		t.Errorf("attempt 1: expected %s, got %s", verify.ModelSonnet, modelsUsed[0])
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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	customEscalation := "claude-sonnet-custom"

	l := New(Config{
		Dirs:                  workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:         5,
		CallsPerHour:          80,
		TaskBackend:           &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		VerifyDir:             dir,
		VerifyEscalationModel: customEscalation,
	}, st, gm, logger)

	l.verifier.deps.NewRunner = func() claudeRunner {
		return &stubRunner{result: stubResult(true, "attempted fix")}
	}

	var modelsUsed []string
	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		modelsUsed = append(modelsUsed, opts.Model)
		if len(modelsUsed) < 3 {
			return verify.Result{Passed: false, Details: "needs work"}
		}
		return verify.Result{Passed: true, Reason: "approved"}
	}

	l.verifier.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-config-models", nextTask: "Config models test",
	})

	if len(modelsUsed) != 3 {
		t.Fatalf("expected 3 LLM calls, got %d", len(modelsUsed))
	}
	if modelsUsed[0] != customEscalation {
		t.Errorf("attempt 1: expected %s, got %s", customEscalation, modelsUsed[0])
	}
	if modelsUsed[1] != customEscalation {
		t.Errorf("attempt 2: expected %s, got %s", customEscalation, modelsUsed[1])
	}
	if modelsUsed[2] != customEscalation {
		t.Errorf("attempt 3: expected %s, got %s", customEscalation, modelsUsed[2])
	}
}

// LLM rejects once, fix agent runs, LLM approves — all within a single
// onSignal call. Proves verification rejection spawns a fix agent and
// re-verifies within one iteration.
func TestOnSignal_LLMReject_FixAgent_PassesOnReVerify(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task", Acceptance: "must handle errors"},
		VerifyDir:     dir,
	}, st, gm, logger)

	fixAgentSpawned := false
	l.verifier.deps.NewRunner = func() claudeRunner {
		fixAgentSpawned = true
		return &stubRunner{result: stubResult(true, "fixed error handling")}
	}

	llmCalls := 0
	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		llmCalls++
		if llmCalls == 1 {
			return verify.Result{Passed: false, Details: "missing error handling"}
		}
		return verify.Result{Passed: true, Reason: "looks good after fix"}
	}

	result := l.verifier.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-retry", nextTask: "Retry test",
	})

	if !result {
		t.Fatal("expected onSignal to return true after fix agent + re-verify")
	}
	if llmCalls != 2 {
		t.Fatalf("expected 2 LLM verify calls within single onSignal, got %d", llmCalls)
	}
	if !fixAgentSpawned {
		t.Fatal("expected fix agent to be spawned on LLM rejection")
	}
}

// Fix agent receives the rejection reason in its prompt so it knows what
// to fix. Captured via the prompt string passed to the stub runner.
func TestOnSignal_LLMReject_FixAgent_ReceivesRejectionReason(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task", Acceptance: "must handle errors"},
		VerifyDir:     dir,
	}, st, gm, logger)

	var capturedPrompt string
	l.verifier.deps.NewRunner = func() claudeRunner {
		return &promptCapturingRunner{
			inner:    &stubRunner{result: stubResult(true, "fixed")},
			captured: &capturedPrompt,
		}
	}

	rejectionMsg := "function foo() ignores the error return from bar()"
	llmCalls := 0
	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		llmCalls++
		if llmCalls == 1 {
			return verify.Result{Passed: false, Details: rejectionMsg}
		}
		return verify.Result{Passed: true, Reason: "approved"}
	}

	l.verifier.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-prompt", nextTask: "Prompt test",
	})

	if !strings.Contains(capturedPrompt, rejectionMsg) {
		t.Fatalf("fix agent prompt should contain rejection reason %q, got:\n%s", rejectionMsg, capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "must handle errors") {
		t.Fatalf("fix agent prompt should contain acceptance criteria, got:\n%s", capturedPrompt)
	}
}

// When the fix agent exits without signaling, onSignal returns false —
// the fix attempt failed.
func TestOnSignal_LLMReject_FixAgentNoSignal_ReturnsFalse(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		VerifyDir:     dir,
	}, st, gm, logger)

	l.verifier.deps.NewRunner = func() claudeRunner {
		return &stubRunner{result: stubResult(false, "")}
	}

	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		return verify.Result{Passed: false, Details: "bad code"}
	}

	result := l.verifier.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-nosignal", nextTask: "No signal test",
	})

	if result {
		t.Fatal("expected onSignal to return false when fix agent exits without signal")
	}
}

// In fire mode (default), a no-diff LLM result passes without spawning
// a verification agent.
func TestOnSignal_FireMode_NoDiffAccepted(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		VerifyDir:     dir,
	}, st, gm, logger)

	l.verifier.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
		return verify.Result{Passed: true, NoDiff: true, Reason: "no diff"}
	}

	runnerSpawned := false
	l.verifier.deps.NewRunner = func() claudeRunner {
		runnerSpawned = true
		return &stubRunner{result: stubResult(true, "confirmed")}
	}

	result := l.verifier.OnSignal(signalParams{
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


func stubResult(signal bool, summary string) claude.Result {
	return claude.Result{
		SignalDetected: signal,
		Summary:        summary,
	}
}

// tryFixReviewComments logs each actionable comment as "reviewer: file:line — first line"
// before spawning the fix agent, giving the operator visibility into what the
// agent will address without requiring them to check GitHub.
func TestTryFixReviewComments_LogsEachActionableComment(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.NewWithWriter(&buf)

	v := newTestVerifier(t, func(v *Verifier) {
		v.deps.Logger = logger
		v.deps.NewRunner = func() claudeRunner {
			return &stubRunner{result: stubResult(true, "fixed")}
		}
	})

	gm := &testutil.StubGit{HeadRevValue: "abc123"}
	review := &git.AutoReview{
		Comments: []git.ReviewComment{
			{Path: "src/foo.go", Line: 42, Body: "Should use pointer receiver for consistency\nMore detail here"},
			{Path: "pkg/bar.go", Line: 7, Body: "Missing nil check before dereferencing ptr"},
		},
	}

	tryFixReviewComments(context.Background(), gm, v, logger, "copilot-pull-request-reviewer", review, 1, "task", t.TempDir(), t.TempDir()+"/raw.log")

	output := buf.String()
	if !strings.Contains(output, "src/foo.go:42") {
		t.Errorf("expected log to contain src/foo.go:42, got: %s", output)
	}
	if !strings.Contains(output, "Should use pointer receiver for consistency") {
		t.Errorf("expected log to contain first line of first comment, got: %s", output)
	}
	if strings.Contains(output, "More detail here") {
		t.Errorf("expected log to contain only first line of body, not full body, got: %s", output)
	}
	if !strings.Contains(output, "pkg/bar.go:7") {
		t.Errorf("expected log to contain pkg/bar.go:7, got: %s", output)
	}
	if !strings.Contains(output, "Missing nil check before dereferencing ptr") {
		t.Errorf("expected log to contain first line of second comment, got: %s", output)
	}
}
