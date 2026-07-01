package loop

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verifier"
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				return "YES: looks good", nil
			}},
		}),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})

	verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-123", nextTask: "Test task",
	})
	if skipReason != "" {
		l.skipTask("test-123", skipReason)
	}
	if !verified {
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, DiffFullResult: "+ stub diff"})

	backend := &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	llmCalls := 0
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: stubResult(true, "attempted fix")}
			},
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				llmCalls++
				return "NO: diff doesn't match bead", nil
			}},
		}),
	})

	verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-123", nextTask: "Test task",
	})
	if skipReason != "" {
		l.skipTask("test-123", skipReason)
	}
	if verified {
		t.Fatal("expected onSignal to return false when LLM verification exhausts retries")
	}
	// runFixLoop's generic contract evaluates checks once more than it spawns
	// fix agents (maxAttempts spawns bound the retries; the final evaluation
	// after the last spawn is what actually detects exhaustion), so a full
	// exhaustion makes maxLLMVerifyAttempts()+1 LLM calls.
	wantCalls := l.maxLLMVerifyAttempts() + 1
	if llmCalls != wantCalls {
		t.Fatalf("expected %d LLM verify calls, got %d", wantCalls, llmCalls)
	}
	if backend.SkippedTask != "test-123" {
		t.Fatalf("expected test-123 deferred in backend, got %q", backend.SkippedTask)
	}
}

// First LLM verification attempt uses haiku; subsequent attempts escalate to sonnet.
func TestOnSignal_LLMVerify_ModelEscalation(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, DiffFullResult: "+ stub diff"})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	var modelsUsed []string
	llmCalls := 0
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: stubResult(true, "attempted fix")}
			},
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				llmCalls++
				modelsUsed = append(modelsUsed, model)
				if llmCalls <= 2 {
					return "NO: needs work", nil
				}
				return "YES: approved", nil
			}},
		}),
	})

	verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-escalation", nextTask: "Escalation test",
	})
	if skipReason != "" {
		l.skipTask("test-escalation", skipReason)
	}
	_ = verified

	if len(modelsUsed) != 3 {
		t.Fatalf("expected 3 LLM calls, got %d", len(modelsUsed))
	}
	if modelsUsed[0] != verify.ModelHaiku {
		t.Errorf("attempt 1: expected %s (haiku), got %s", verify.ModelHaiku, modelsUsed[0])
	}
	if modelsUsed[1] != verify.ModelSonnet {
		t.Errorf("attempt 2: expected %s (sonnet escalation), got %s", verify.ModelSonnet, modelsUsed[1])
	}
	if modelsUsed[2] != verify.ModelSonnet {
		t.Errorf("attempt 3: expected %s (sonnet escalation), got %s", verify.ModelSonnet, modelsUsed[2])
	}
}

// Config-driven model selection: first attempt uses VerifyModel, subsequent attempts use VerifyEscalationModel.
func TestOnSignal_ConfigDrivenModels(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, DiffFullResult: "+ stub diff"})

	customFirst := "claude-haiku-custom"
	customEscalation := "claude-sonnet-custom"
	cfg := Config{
		Dirs:                  workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:         5,
		CallsPerHour:          80,
		VerifyModel:           customFirst,
		VerifyEscalationModel: customEscalation,
	}
	var modelsUsed []string
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: stubResult(true, "attempted fix")}
			},
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				modelsUsed = append(modelsUsed, model)
				if len(modelsUsed) < 3 {
					return "NO: needs work", nil
				}
				return "YES: approved", nil
			}},
		}),
	})

	verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-config-models", nextTask: "Config models test",
	})
	if skipReason != "" {
		l.skipTask("test-config-models", skipReason)
	}
	_ = verified

	if len(modelsUsed) != 3 {
		t.Fatalf("expected 3 LLM calls, got %d", len(modelsUsed))
	}
	if modelsUsed[0] != customFirst {
		t.Errorf("attempt 1: expected %s, got %s", customFirst, modelsUsed[0])
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, DiffFullResult: "+ stub diff"})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	fixAgentSpawned := false
	llmCalls := 0
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task", Acceptance: "must handle errors"},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				fixAgentSpawned = true
				return &stubRunner{result: stubResult(true, "fixed error handling")}
			},
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				llmCalls++
				if llmCalls == 1 {
					return "NO: missing error handling", nil
				}
				return "YES: looks good after fix", nil
			}},
		}),
	})

	verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-retry", nextTask: "Retry test",
	})
	if skipReason != "" {
		l.skipTask("test-retry", skipReason)
	}

	if !verified {
		t.Fatal("expected onSignal to return true after fix agent + re-verify")
	}
	if llmCalls != 2 {
		t.Fatalf("expected 2 LLM verify calls within single onSignal, got %d", llmCalls)
	}
	if !fixAgentSpawned {
		t.Fatal("expected fix agent to be spawned on LLM rejection")
	}
}

// A fix agent spawned to address an LLM rejection can itself break the
// tests. This must be retryable — the failing test output feeds into the
// next fix attempt (bounded by maxLLMVerifyAttempts) — not a terminal bail
// of the whole verification. (The old runLLMVerifyFixLoop re-ran tests once
// after the fix agent and returned false, "" outright on failure, discarding
// the fix agent's LLM-approved progress and any remaining retry budget.)
func TestOnSignal_LLMReject_FixAgent_TestFailureAfterFix_RetriesInsteadOfBailing(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	// Tests pass initially so the earlier test-fix-loop pipeline stage is a
	// no-op and verification reaches the LLM-verify stage directly.
	passingMakefile := []byte("ralph-verify:\n\ttrue\n")
	failingMakefile := []byte("ralph-verify:\n\t@echo 'FAIL: broken by fix agent' && exit 1\n")
	os.WriteFile(filepath.Join(dir, "Makefile"), passingMakefile, 0o644)

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, DiffFullResult: "+ stub diff"})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	llmCalls := 0
	fixAgentSpawns := 0
	var lastFixPrompt string
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				fixAgentSpawns++
				// Attempt 1's fix agent "addresses" the LLM's feedback but
				// breaks the tests. Attempt 2's fix agent fixes them.
				if fixAgentSpawns == 1 {
					os.WriteFile(filepath.Join(dir, "Makefile"), failingMakefile, 0o644)
				} else {
					os.WriteFile(filepath.Join(dir, "Makefile"), passingMakefile, 0o644)
				}
				return &promptCapturingRunner{
					inner:    &stubRunner{result: stubResult(true, "attempted fix")},
					captured: &lastFixPrompt,
				}
			},
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				llmCalls++
				if llmCalls == 1 {
					return "NO: missing error handling", nil
				}
				return "YES: looks good", nil
			}},
		}),
	})

	verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-retry-after-break", nextTask: "Retry after break test",
	})
	if skipReason != "" {
		l.skipTask("test-retry-after-break", skipReason)
	}

	if !verified {
		t.Fatal("expected verification to retry past the post-fix test failure and eventually pass, not bail")
	}
	if fixAgentSpawns != 2 {
		t.Fatalf("expected 2 fix agent spawns (1 for the LLM rejection, 1 for the post-fix test failure), got %d", fixAgentSpawns)
	}
	if llmCalls != 3 {
		t.Fatalf("expected 3 LLM verify calls (reject, approve-but-tests-broke, approve-after-retest), got %d", llmCalls)
	}
	if !strings.Contains(lastFixPrompt, "broken by fix agent") {
		t.Errorf("expected the retry fix agent's prompt to contain the failing test output, got:\n%s", lastFixPrompt)
	}
}

// Fix agent receives the rejection reason in its prompt so it knows what
// to fix. Captured via the prompt string passed to the stub runner.
func TestOnSignal_LLMReject_FixAgent_ReceivesRejectionReason(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, DiffFullResult: "+ stub diff"})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	var capturedPrompt string
	rejectionMsg := "function foo() ignores the error return from bar()"
	llmCalls := 0
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task", Acceptance: "must handle errors"},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &promptCapturingRunner{
					inner:    &stubRunner{result: stubResult(true, "fixed")},
					captured: &capturedPrompt,
				}
			},
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				llmCalls++
				if llmCalls == 1 {
					return "NO: " + rejectionMsg, nil
				}
				return "YES: approved", nil
			}},
		}),
	})

	verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-prompt", nextTask: "Prompt test",
	})
	if skipReason != "" {
		l.skipTask("test-prompt", skipReason)
	}
	_ = verified

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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, DiffFullResult: "+ stub diff"})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: stubResult(false, "")}
			},
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				return "NO: bad code", nil
			}},
		}),
	})

	verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-nosignal", nextTask: "No signal test",
	})
	if skipReason != "" {
		l.skipTask("test-nosignal", skipReason)
	}

	if verified {
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	// With no PR diff and no iteration diff, LLMVerifyPR short-circuits
	// to NoDiff=true without invoking QueryFn.
	runnerSpawned := false
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				runnerSpawned = true
				return &stubRunner{result: stubResult(true, "confirmed")}
			},
		}),
	})

	verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
		ctx: context.Background(), headBefore: "abc123",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-fire", nextTask: "Fire test",
	})
	if skipReason != "" {
		l.skipTask("test-fire", skipReason)
	}

	if !verified {
		t.Fatal("fire mode should accept no-diff completion")
	}
	if runnerSpawned {
		t.Fatal("fire mode should not spawn a verification agent")
	}
}

// When headBefore == HeadRev (no iteration-local commits) but the branch is
// ahead of origin/main (prior-iteration commits exist), runVerifyPipeline
// must proceed to verification instead of rejecting with "No commits found".
// This prevents stagnation when iteration N commits but exits without
// signaling, and iteration N+1 signals without new commits.
func TestOnSignal_PriorIterationCommits_Proceeds(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	// DiffFromBaseResult is set to simulate realistic multi-iteration work: prior
	// iterations committed something, so the branch diff is non-empty even though
	// headBefore == HeadRev (this iteration added nothing new). An empty
	// DiffFromBase with commits ahead is a tooling fault (caught by the
	// empty-diff guard in runVerifyPipeline), not this path.
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:         dir,
		WorkDir:            dir,
		HeadRev:            "same-sha",
		DiffFromBaseResult: "+prior iteration work",
		LogOnelineResult:   "abc1234 prior iteration commit",
	})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				return "YES: looks good", nil
			}},
		}),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})

	verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
		ctx: context.Background(), headBefore: "same-sha",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-prior", nextTask: "Prior iteration test",
	})
	if skipReason != "" {
		l.skipTask("test-prior", skipReason)
	}
	if !verified {
		t.Fatal("expected verification to proceed when prior-iteration commits exist ahead of origin/main")
	}
}

// When headBefore == HeadRev AND no prior-iteration commits exist (branch is
// not ahead of origin/main), runVerifyPipeline must still reject.
func TestOnSignal_NoPriorCommits_Rejects(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:       dir,
		WorkDir:          dir,
		HeadRev:          "same-sha",
		LogOnelineResult: "", // no prior-iteration commits
	})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})

	verified, _ := l.runVerifyPipeline(verifyPipelineInput{
		ctx: context.Background(), headBefore: "same-sha",
		workDir: dir, rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID: "test-none", nextTask: "No commits test",
	})
	if verified {
		t.Fatal("expected verification to reject when no commits exist at all")
	}
}

// Proves: every runAgent call uses cfg.WorkingModel — no model escalation exists.
func TestAgentModelEscalation(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})

	const baseModel = verify.ModelSonnet
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		WorkingModel:  baseModel,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix login", NextID: "ralph-abc"},
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	var capturedModels []string
	l.runner = &stubRunner{
		onRunCfg: func(cfg claude.RunConfig) {
			capturedModels = append(capturedModels, cfg.Model)
		},
	}

	task := taskContext{id: "ralph-abc", title: "Fix login"}

	// First call: no prior attempts — uses cfg.WorkingModel.
	l.runAgent(context.Background(), task, 0)

	// Second call: with in-memory prior attempt — still uses cfg.WorkingModel, not escalation model.
	l.taskAttempts = append(l.taskAttempts, AttemptEvent{Summary: "first try failed", Analysis: "continue"})
	l.runAgent(context.Background(), task, 1)

	if len(capturedModels) != 2 {
		t.Fatalf("expected 2 runner calls, got %d", len(capturedModels))
	}
	if capturedModels[0] != baseModel {
		t.Errorf("attempt 1: expected %s (sonnet), got %s", baseModel, capturedModels[0])
	}
	if capturedModels[1] != baseModel {
		t.Errorf("attempt 2: expected %s (sonnet, not escalation), got %s", baseModel, capturedModels[1])
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

	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := git.NewStub(git.StubRepoConfig{HeadRev: "abc123", ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: stubResult(true, "fixed")}
			},
		}),
	})

	review := &git.AutoReview{
		Comments: []git.ReviewComment{
			{Path: "src/foo.go", Line: 42, Body: "Should use pointer receiver for consistency\nMore detail here"},
			{Path: "pkg/bar.go", Line: 7, Body: "Missing nil check before dereferencing ptr"},
		},
	}

	l.tryFixReviewComments(context.Background(), "copilot-pull-request-reviewer", review, 1, "task", t.TempDir(), t.TempDir()+"/raw.log")

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

// Proves that tryFixReviewComments still replies-to-and-resolves the review
// threads after a successful push now that fixLoopSpec has no onPushed
// callback field — the sequencing moved to plain code in the wrapper
// (call fixLoop, then act on its result) rather than a func field.
func TestTryFixReviewComments_RepliesAndResolvesAfterPush(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	// HasUncommitted=true causes fixLoop to call CommitAll, advancing headRev —
	// simulating the fix agent committing changes for the review comment fix.
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        dir,
		HeadRev:        "task-head",
		HasUncommitted: true,
	})

	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:  st,
		Git:    gm,
		Logger: logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: stubResult(true, "addressed review feedback")}
			},
		}),
	})

	review := &git.AutoReview{
		Comments: []git.ReviewComment{
			{Path: "src/foo.go", Line: 42, Body: "Missing nil check before dereferencing ptr"},
		},
	}

	applied := l.tryFixReviewComments(context.Background(), "copilot-pull-request-reviewer", review, 1, "task", dir, filepath.Join(ralphDir, "raw.log"))
	if !applied {
		t.Fatalf("expected tryFixReviewComments to report the fix as applied")
	}
	if calls := gm.(git.StubInspector).GetReplyToAndResolveCommentsCalls(); calls != 1 {
		t.Errorf("expected ReplyToAndResolveComments to be called once after push, got %d calls", calls)
	}
}

// Proves: SpawnFixAgent uses cfg.FixModel on both attempt 1 and attempt 2 —
// fix agents no longer escalate models across attempts. The runner factory
// stub captures the Model field from the RunConfig passed to runner.Run.
func TestFixModelSameAcrossAttempts(t *testing.T) {
	const fixModel = verify.ModelOpus

	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	var capturedModels []string
	vrf := verifier.New(verifier.Config{
		FixModel:   fixModel,
		RalphDir:   ralphDir,
		PromptsDir: promptsDir,
	}, logging.New(nil), func() verifier.Runner {
		return &stubRunner{
			result: claude.Result{SignalDetected: true},
			onRunCfg: func(cfg claude.RunConfig) {
				capturedModels = append(capturedModels, cfg.Model)
			},
		}
	}, nil)

	base := verifier.FixAgentInput{
		Ctx:         context.Background(),
		Template:    "verify-tests.md",
		Vars:        map[string]string{"{{TASK_TITLE}}": "test task", "{{TASK_DESCRIPTION}}": "desc", "{{TEST_OUTPUT}}": "out"},
		WorkDir:     dir,
		RawLogPath:  filepath.Join(ralphDir, "raw.log"),
		Description: "test failures",
	}

	base.Attempt = 1
	vrf.SpawnFixAgent(base)
	base.Attempt = 2
	vrf.SpawnFixAgent(base)

	if len(capturedModels) != 2 {
		t.Fatalf("expected 2 runner calls, got %d", len(capturedModels))
	}
	if capturedModels[0] != fixModel {
		t.Errorf("attempt 1: expected %s, got %s", fixModel, capturedModels[0])
	}
	if capturedModels[1] != fixModel {
		t.Errorf("attempt 2: expected %s (no escalation), got %s", fixModel, capturedModels[1])
	}
}

// Proves: when FixModel is unset in Config, it defaults to ModelOpus on
// attempt 1 (no sonnet warm-up).
func TestFixModelDefaultOpusFromFirstAttempt(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	var capturedModels []string
	vrf := verifier.New(verifier.Config{
		RalphDir: ralphDir,
	}, logging.New(nil), func() verifier.Runner {
		return &stubRunner{
			result: claude.Result{SignalDetected: true},
			onRunCfg: func(cfg claude.RunConfig) {
				capturedModels = append(capturedModels, cfg.Model)
			},
		}
	}, nil)

	base := verifier.FixAgentInput{
		Ctx:         context.Background(),
		Template:    "verify-tests.md",
		Vars:        map[string]string{"{{TASK_TITLE}}": "t", "{{TASK_DESCRIPTION}}": "ac", "{{TEST_OUTPUT}}": "out"},
		WorkDir:     dir,
		Description: "test failures",
	}
	base.Attempt = 1
	vrf.SpawnFixAgent(base)

	if len(capturedModels) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(capturedModels))
	}
	if capturedModels[0] != verify.ModelOpus {
		t.Errorf("default attempt 1: expected %s (opus), got %s", verify.ModelOpus, capturedModels[0])
	}
}

// Proves that a CI fix agent committing changes to a file outside the task
// diff is preserved and pushed, not reverted to origin/<default>. Previously
// an onPushReady hook in tryFixCI would call RevertFilesToRef on any file not
// in the task diff, undoing legitimate boy-scout fixes and leaving CI red.
func TestTryFixCI_OutOfScopeFixPreservedAndPushed(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	// HasUncommitted=true causes fixLoop to call CommitAll, advancing headRev
	// from "task-head" to "stub-head-1". This simulates the fix agent modifying
	// a file (outside the task diff) and leaving it uncommitted.
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        dir,
		HeadRev:        "task-head",
		HasUncommitted: true,
	})

	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:  st,
		Git:    gm,
		Logger: logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: stubResult(true, "fixed flaky test in unrelated file")}
			},
		}),
	})

	ciErr := &git.CIFailureError{
		PRNumber: 42,
		Failures: []git.CICheckResult{{Name: "test / unit", Bucket: "fail"}},
	}
	result := l.tryFixCI(context.Background(), ciErr, "Fix login bug", dir, filepath.Join(ralphDir, "raw.log"))
	if result != git.CIFixApplied {
		t.Errorf("expected CIFixApplied when fix agent commits out-of-scope changes, got %v", result)
	}
	// Exactly one new commit (the fix). A revert would produce headRev "stub-head-2".
	if gm.HeadRev() != "stub-head-1" {
		t.Errorf("expected fix commit to be pushed as-is (stub-head-1), got %s — a revert commit was added", gm.HeadRev())
	}
}

// Proves the verify gate (post-signal and pre-iteration) uses l.git.GetWorkDir()
// — the live per-task worktree — not cfg.Dirs.ProjectDir. When the worktree has
// a passing verify script and projectDir has a failing one, the gate must pass.
func TestVerifyGate_RunsInWorktreeNotProjectDir(t *testing.T) {
	projectDir := t.TempDir()
	worktreeDir := t.TempDir()
	ralphDir := filepath.Join(projectDir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	// projectDir has a FAILING ralph-verify target.
	os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte("ralph-verify:\n\t@false\n"), 0o644)
	// worktreeDir has a PASSING ralph-verify target — the live worktree is green.
	os.WriteFile(filepath.Join(worktreeDir, "Makefile"), []byte("ralph-verify:\n\t@true\n"), 0o644)

	st := state.NewStore(ralphDir)
	st.Init(5)

	// Git stub: GetWorkDir() returns worktreeDir (the live per-task worktree).
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:       projectDir,
		WorkDir:          worktreeDir,
		HeadRev:          "abc123",
		LogOnelineResult: "abc123 some prior commit",
	})

	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: projectDir,
			WorkDir:    worktreeDir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TestTimeout:   5 * time.Second,
	}
	logger := logging.New(nil)
	vrf := verifier.New(verifier.Config{
		ProjectDir:  projectDir,
		TestTimeout: 5 * time.Second,
	}, logger, nil, &stubQuerier{fn: func(_ context.Context, _, _, _ string) (string, error) {
		return "YES: looks good", nil
	}})

	l := New(cfg, Modules{
		State:    st,
		Git:      gm,
		Logger:   logger,
		Verifier: vrf,
	})

	// runSimpleVerifyCompletion is the post-signal gate path. It must use
	// l.git.GetWorkDir() (worktreeDir = green), not projectDir (red).
	passed, reason := l.runSimpleVerifyCompletion(context.Background(), "different-sha")
	if !passed {
		t.Fatalf("verify gate should pass when worktree tests pass — got failure: %s", reason)
	}
}

// Proves the signal → assert → verify ordering:
// When the agent commits and signals (signalTimeHead captured), but the branch
// is subsequently reset to origin/<base> (dropping the commit), the verify
// pipeline must abort as an infrastructure error — not a rejection — so the
// task is not skipped and the LLM verifier is never invoked.
//
// AC5 integration test: agent commits → branch reset → verification attempt
// aborts as infra error, task not skipped, LLM not called.
func TestRunVerifyPipeline_SignalCommitDropped_InfraAbort(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	// Simulate: agent committed "signal-sha", then the worktree was reset
	// to origin/main. CommitDroppedFromBranch causes IsCommitAncestorOf to
	// return false, simulating that signal-sha is no longer reachable from HEAD.
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:              dir,
		WorkDir:                 dir,
		DiffFullResult:          "+stub agent work",
		CommitDroppedFromBranch: true,
	})

	llmCalled := false
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		// MaxLLMVerifyAttempts left at default (3) so the skip threshold is clear.
	}
	logger := logging.New(nil)
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			querier: &stubQuerier{fn: func(_ context.Context, _, _, _ string) (string, error) {
				llmCalled = true
				return "YES: looks good", nil
			}},
		}),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})

	// signal → assert (infra check) → verify ordering:
	// signalTimeHead is captured at signal time (before reset).
	verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
		ctx:            context.Background(),
		headBefore:     "old-sha",
		signalTimeHead: "signal-sha",
		workDir:        dir,
		rawLogPath:     filepath.Join(ralphDir, "raw.log"),
		taskID:         "test-infra",
		nextTask:       "Test infra abort",
	})
	if skipReason != "" {
		l.skipTask("test-infra", skipReason)
	}

	if verified {
		t.Error("verification must fail when signal-time commit is dropped from the branch")
	}
	// Infrastructure abort: no skipReason means the task is NOT added to
	// skipped_tasks — it will retry on the next iteration.
	if skipReason != "" {
		t.Errorf("infra abort must not produce a skip reason, got %q", skipReason)
	}
	// The LLM verifier must not be invoked — the assert fires before verify.
	if llmCalled {
		t.Error("LLM verifier must not be invoked when signal-time commit is absent from HEAD")
	}
	// The task must not be recorded as skipped in the backend.
	if backend.SkippedTask != "" {
		t.Errorf("task must not be skipped on infra abort, got SkippedTask=%q", backend.SkippedTask)
	}
}

// Reproduces ralph-732q: a task worktree branch is configured, but git
// operations have fallen back to the project checkout (GetWorkDir() ==
// GetProjectDir()). Every diff/HEAD read would then reflect projectDir's
// HEAD — the PREVIOUS task's merged commit — so the verifier would be handed
// the prior task's diff and falsely reject this task's correct work. The
// pipeline must abort as an infrastructure error before any LLM call: no
// skip, no attempt consumed, LLM never invoked.
func TestRunVerifyPipeline_WorktreeBranchButProjectDir_InfraAbort(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	// Contamination state: the run is configured to use a per-task worktree
	// (cfg.Dirs.WorkDir below points at a separate dir), but git operations
	// have fallen back to the project checkout — GetWorkDir() == GetProjectDir()
	// == dir. DiffFullResult is the prior task's diff that would be fed to the
	// verifier if the guard did not fire.
	worktreeDir := filepath.Join(dir, "worktree-ralph-732q")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/ralph-732q-task",
		DiffFullResult: "+prior task diff (loop_iteration.go)",
		HeadRev:        "prior-task-merged-sha",
	})

	llmCalled := false
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: worktreeDir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			querier: &stubQuerier{fn: func(_ context.Context, _, _, _ string) (string, error) {
				llmCalled = true
				return "NO: diff has no task-manager.md changes", nil
			}},
		}),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})

	verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
		ctx:            context.Background(),
		headBefore:     "old-sha",
		signalTimeHead: "signal-sha",
		workDir:        dir,
		rawLogPath:     filepath.Join(ralphDir, "raw.log"),
		taskID:         "ralph-732q",
		nextTask:       "startup skip-triage",
	})
	if skipReason != "" {
		l.skipTask("ralph-732q", skipReason)
	}

	if verified {
		t.Error("verification must fail (infra abort) when a worktree branch is configured but git ops resolve to projectDir")
	}
	if skipReason != "" {
		t.Errorf("infra abort must not produce a skip reason, got %q", skipReason)
	}
	if llmCalled {
		t.Error("LLM verifier must not be invoked when the worktree invariant is violated — a stale projectDir diff must never reach the verifier")
	}
	if backend.SkippedTask != "" {
		t.Errorf("task must not be skipped on infra abort, got SkippedTask=%q", backend.SkippedTask)
	}
}

// When the verify diff is empty but the branch has commits ahead of base,
// runVerifyPipeline must abort as a tooling error — not pass silently or set
// a skip reason. Reproduces the case where unfetched origin, wrong workdir,
// or base misresolution produces an empty diff on a branch with real work.
func TestRunVerifyPipeline_EmptyDiffAheadOfBase_AbortsAsToolingError(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:         dir,
		WorkDir:            dir,
		DiffFromBaseResult: "",
		DiffFullResult:     "",
		LogOnelineResult:   "abc123 fix something",
	})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	llmCalls := 0
	logger := logging.New(nil)
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			querier: &stubQuerier{fn: func(_ context.Context, _, _, _ string) (string, error) {
				llmCalls++
				return "YES: looks good", nil
			}},
		}),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})

	verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "test-123",
		nextTask:   "Test task",
	})
	if skipReason != "" {
		l.skipTask("test-123", skipReason)
	}

	if verified {
		t.Fatal("expected runVerifyPipeline to abort (return false) on empty diff with branch ahead of base")
	}
	if skipReason != "" {
		t.Fatalf("infra abort must not produce a skip reason, got %q", skipReason)
	}
	if llmCalls > 0 {
		t.Fatalf("LLM verifier must not be called on tooling error, got %d calls", llmCalls)
	}
	if backend.SkippedTask != "" {
		t.Errorf("task must not be skipped on infra abort, got SkippedTask=%q", backend.SkippedTask)
	}
}
