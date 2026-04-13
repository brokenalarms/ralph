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

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		VerifyDir:     dir,
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

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, DiffFullValue: "+ stub diff"}

	backend := &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		VerifyDir:     dir,
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
	if llmCalls != l.maxLLMVerifyAttempts() {
		t.Fatalf("expected %d LLM verify calls, got %d", l.maxLLMVerifyAttempts(), llmCalls)
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

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, DiffFullValue: "+ stub diff"}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		VerifyDir:     dir,
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

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, DiffFullValue: "+ stub diff"}

	customFirst := "claude-haiku-custom"
	customEscalation := "claude-sonnet-custom"
	cfg := Config{
		Dirs:                  workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:         5,
		CallsPerHour:          80,
		VerifyDir:             dir,
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

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, DiffFullValue: "+ stub diff"}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		VerifyDir:     dir,
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

// Fix agent receives the rejection reason in its prompt so it knows what
// to fix. Captured via the prompt string passed to the stub runner.
func TestOnSignal_LLMReject_FixAgent_ReceivesRejectionReason(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, DiffFullValue: "+ stub diff"}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		VerifyDir:     dir,
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

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, DiffFullValue: "+ stub diff"}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		VerifyDir:     dir,
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

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		VerifyDir:     dir,
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

	gm := &git.StubRepo{
		ProjectDir:      dir,
		WorkDir:         dir,
		HeadRevValue:    "same-sha",
		LogOnelineValue: "abc1234 prior iteration commit",
	}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		VerifyDir:     dir,
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

	gm := &git.StubRepo{
		ProjectDir:      dir,
		WorkDir:         dir,
		HeadRevValue:    "same-sha",
		LogOnelineValue: "", // no prior-iteration commits
	}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		VerifyDir:     dir,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
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

// Proves: first agent attempt uses cfg.Model; subsequent attempts (when prior
// attempts exist on disk) use cfg.AgentEscalationModel, with ModelCap applied
// as a ceiling over both.
func TestAgentModelEscalation(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	const firstModel = verify.ModelSonnet
	const escalationModel = verify.ModelOpus
	cfg := Config{
		Dirs:                 workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:        1,
		CallsPerHour:         80,
		Model:                firstModel,
		AgentEscalationModel: escalationModel,
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

	// First run: no prior attempts on disk — should use Model (sonnet).
	l.runAgent(context.Background(), task, 0)

	// Record a prior attempt to simulate a retry.
	l.attempts.Record("ralph-abc", "Fix login", "first try failed", "", "continue")

	// Second run: one prior attempt on disk — should use AgentEscalationModel (opus).
	l.runAgent(context.Background(), task, 1)

	if len(capturedModels) != 2 {
		t.Fatalf("expected 2 runner calls, got %d", len(capturedModels))
	}
	if capturedModels[0] != firstModel {
		t.Errorf("attempt 1: expected %s (sonnet), got %s", firstModel, capturedModels[0])
	}
	if capturedModels[1] != escalationModel {
		t.Errorf("attempt 2: expected %s (opus escalation), got %s", escalationModel, capturedModels[1])
	}
}

// Proves: ModelCap is applied as a ceiling over both agent model and escalation model.
func TestAgentModelEscalation_ModelCapApplied(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}
	cfg := Config{
		Dirs:                 workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:        1,
		CallsPerHour:         80,
		Model:                verify.ModelSonnet,
		AgentEscalationModel: verify.ModelOpus,
		ModelCap:             verify.ModelHaiku, // cap everything to haiku
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix login", NextID: "ralph-cap"},
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	var capturedModels []string
	l.runner = &stubRunner{
		onRunCfg: func(cfg claude.RunConfig) {
			capturedModels = append(capturedModels, cfg.Model)
		},
	}

	// Record a prior attempt so escalation model would normally be used.
	l.attempts.Record("ralph-cap", "Fix login", "first try failed", "", "continue")

	l.runAgent(context.Background(), taskContext{id: "ralph-cap", title: "Fix login"}, 1)

	if len(capturedModels) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(capturedModels))
	}
	if capturedModels[0] != verify.ModelHaiku {
		t.Errorf("ModelCap not applied: expected %s (haiku), got %s", verify.ModelHaiku, capturedModels[0])
	}
}

func stubResult(signal bool, summary string) claude.Result {
	return claude.Result{
		SignalDetected: signal,
		Summary:        summary,
	}
}

// After a successful fix push, tryFixReviewComments calls ReplyToAndResolveComments
// with the actionable comments so review threads are automatically closed on GitHub.
func TestTryFixReviewComments_RepliesAndResolvesAfterPush(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	// HeadRev returns a different value after the fix agent "commits",
	// so tryFixReviewComments proceeds past the no-commits guard to push.
	headCallCount := 0
	gm := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		RemoteURLValue: "https://github.com/owner/repo.git",
		HeadRevFunc: func() string {
			headCallCount++
			if headCallCount == 1 {
				return "abc123"
			}
			return "def456"
		},
	}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		VerifyDir:     dir,
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		Logger:      logging.New(nil),
		Verifier: newTestVerifier(t, cfg, logging.New(nil), verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: stubResult(true, "fixed")}
			},
		}),
	})

	review := &git.AutoReview{
		Comments: []git.ReviewComment{
			{ID: 5001, Path: "src/foo.go", Line: 42, Body: "Missing nil check before dereferencing ptr"},
			{ID: 5002, Path: "pkg/bar.go", Line: 7, Body: "Should use constants here"},
		},
	}

	result := l.tryFixReviewComments(context.Background(), "copilot-pull-request-reviewer", review, 99, "task", t.TempDir(), t.TempDir()+"/raw.log")

	if !result {
		t.Fatal("expected tryFixReviewComments to return true after successful push")
	}
	if !gm.ReplyToAndResolveCommentsCalled {
		t.Fatal("expected ReplyToAndResolveComments to be called after push")
	}
	if gm.ReplyToAndResolveCommentsPRNumber != 99 {
		t.Errorf("expected PR number 99, got %d", gm.ReplyToAndResolveCommentsPRNumber)
	}
	if len(gm.ReplyToAndResolveCommentsArgs) != 2 {
		t.Fatalf("expected 2 comments passed to ReplyToAndResolveComments, got %d", len(gm.ReplyToAndResolveCommentsArgs))
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

	gm := &git.StubRepo{HeadRevValue: "abc123", ProjectDir: dir, WorkDir: dir}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		VerifyDir:     dir,
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

// tryFixCI reverts out-of-scope files when the CI fix agent modifies files
// that weren't in the task's original diff against origin/main.
func TestTryFixCI_RevertsOutOfScopeFiles(t *testing.T) {
	dir, _ := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)
	os.WriteFile(filepath.Join(promptsDir, "verify-ci.md"), []byte("fix CI: {{TASK_TITLE}} {{FAILED_CHECKS}} {{CI_LOG}} {{SIGNAL_COMPLETE}}"), 0o644)

	headCalls := 0
	gm := &git.StubRepo{
		ProjectDir:   dir,
		WorkDir:      dir,
		DefaultBranch: "main",
		HeadRevFunc: func() string {
			headCalls++
			if headCalls <= 1 {
				return "before-sha"
			}
			return "after-sha"
		},
		DiffFilesBetweenFunc: func(from, to string) []string {
			if from == "origin/main" && to == "before-sha" {
				// Task's original diff: only src/app.ts
				return []string{"src/app.ts"}
			}
			if from == "before-sha" && to == "after-sha" {
				// Fix agent changed src/app.ts AND .github/workflows/test.yml
				return []string{"src/app.ts", ".github/workflows/test.yml"}
			}
			return nil
		},
	}

	var logBuf bytes.Buffer
	logger := logging.New(&logBuf)
	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
	}
	_, st := setupTestDir(t)
	l := New(cfg, Modules{
		State:  st,
		Git:    gm,
		Logger: logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: stubResult(true, "fixed CI")}
			},
		}),
		Connectivity: onlineStubConnectivity(),
	})

	ciErr := &git.CIFailureError{
		PRNumber: 42,
		Failures: []git.CICheckResult{{Name: "test", Bucket: "fail"}},
	}
	result := l.tryFixCI(context.Background(), ciErr, "test task", dir, filepath.Join(ralphDir, "raw.log"))

	if result != git.CIFixApplied {
		t.Fatalf("expected CIFixApplied, got %v", result)
	}
	if len(gm.RevertedFiles) != 1 || gm.RevertedFiles[0] != ".github/workflows/test.yml" {
		t.Fatalf("expected RevertFilesToRef called with [.github/workflows/test.yml], got %v", gm.RevertedFiles)
	}
	if gm.RevertedRef != "origin/main" {
		t.Fatalf("expected revert ref origin/main, got %s", gm.RevertedRef)
	}
	output := logBuf.String()
	if !strings.Contains(output, "outside task scope") {
		t.Errorf("expected log to mention out-of-scope files, got: %s", output)
	}
}

// tryFixCI does NOT revert files that are already in the task's diff.
func TestTryFixCI_DoesNotRevertInScopeFiles(t *testing.T) {
	dir, _ := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)
	os.WriteFile(filepath.Join(promptsDir, "verify-ci.md"), []byte("fix CI: {{TASK_TITLE}} {{FAILED_CHECKS}} {{CI_LOG}} {{SIGNAL_COMPLETE}}"), 0o644)

	headCalls := 0
	gm := &git.StubRepo{
		ProjectDir:   dir,
		WorkDir:      dir,
		DefaultBranch: "main",
		HeadRevFunc: func() string {
			headCalls++
			if headCalls <= 1 {
				return "before-sha"
			}
			return "after-sha"
		},
		DiffFilesBetweenFunc: func(from, to string) []string {
			if from == "origin/main" && to == "before-sha" {
				return []string{"src/app.ts", "src/utils.ts"}
			}
			if from == "before-sha" && to == "after-sha" {
				// Fix agent only modified files already in the task diff
				return []string{"src/app.ts"}
			}
			return nil
		},
	}

	logger := logging.New(nil)
	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
	}
	l := New(cfg, Modules{
		Git:    gm,
		Logger: logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: stubResult(true, "fixed CI")}
			},
		}),
		Connectivity: onlineStubConnectivity(),
	})

	ciErr := &git.CIFailureError{
		PRNumber: 42,
		Failures: []git.CICheckResult{{Name: "test", Bucket: "fail"}},
	}
	result := l.tryFixCI(context.Background(), ciErr, "test task", dir, filepath.Join(ralphDir, "raw.log"))

	if result != git.CIFixApplied {
		t.Fatalf("expected CIFixApplied, got %v", result)
	}
	if len(gm.RevertedFiles) != 0 {
		t.Fatalf("expected no files reverted when all changes are in scope, got %v", gm.RevertedFiles)
	}
}
