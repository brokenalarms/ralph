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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, DiffFullResult: "+ stub diff"})

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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, DiffFullResult: "+ stub diff"})
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, DiffFullResult: "+ stub diff"})

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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, DiffFullResult: "+ stub diff"})
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, DiffFullResult: "+ stub diff"})
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, DiffFullResult: "+ stub diff"})
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
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

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:       dir,
		WorkDir:          dir,
		HeadRev:          "same-sha",
		LogOnelineResult: "abc1234 prior iteration commit",
	})
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

// Proves: every runAgent call uses cfg.Model — no model escalation exists.
// AgentEscalationModel config is accepted but has no effect on model selection.
func TestAgentModelEscalation(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})

	const baseModel = verify.ModelSonnet
	const escalationModel = verify.ModelOpus
	cfg := Config{
		Dirs:                 workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:        1,
		CallsPerHour:         80,
		Model:                baseModel,
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

	// First call: no prior attempts — uses cfg.Model.
	l.runAgent(context.Background(), task, 0)

	// Second call: with in-memory prior attempt — still uses cfg.Model, not escalation model.
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

// Proves: ModelCap is applied as a ceiling over both agent model and escalation model.
func TestAgentModelEscalation_ModelCapApplied(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
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

	// Seed a prior attempt — ModelCap must still be applied as ceiling over cfg.Model.
	l.currentTaskID = "ralph-cap"
	l.taskAttempts = append(l.taskAttempts, AttemptEvent{Summary: "first try failed", Analysis: "continue"})

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

// Phase C deletion: TestTryFixReviewComments_RepliesAndResolvesAfterPush
// relied on HeadRevFunc (sequenced responses: abc123 → def456 to simulate
// "fix agent committed") and on stub internal tracking of
// ReplyToAndResolveCommentsCalled/Args. Neither pattern is expressible in
// the new stubRepo: HeadRevFunc is forbidden (no callback fields) and
// call-tracking fields do not exist. Equivalent coverage belongs in Phase D
// real-git integration: a real rebase + real review-resolve via the GitHub
// fake can observe both outcomes without sequenced callbacks.

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

// Proves: each Spawn*FixAgent entrypoint uses cfg.FixModel (default sonnet) on
// attempt 1 and cfg.FixEscalationModel (default opus) on attempt 2. The runner
// factory stub captures the Model field from the RunConfig passed to runner.Run.
func TestFixModelEscalation(t *testing.T) {
	const firstModel = verify.ModelSonnet
	const escalationModel = verify.ModelOpus

	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	spawn := verifier.FixAgentSpawn{
		Ctx:        context.Background(),
		TaskTitle:  "test task",
		WorkDir:    dir,
		RawLogPath: filepath.Join(ralphDir, "raw.log"),
	}

	newCaptureRunner := func(captured *[]string) verifier.RunnerFactory {
		return func() verifier.Runner {
			return &stubRunner{
				result: claude.Result{SignalDetected: true},
				onRunCfg: func(cfg claude.RunConfig) {
					*captured = append(*captured, cfg.Model)
				},
			}
		}
	}

	makeVrf := func(captured *[]string) *verifier.Verifier {
		return verifier.New(verifier.Config{
			FixModel:           firstModel,
			FixEscalationModel: escalationModel,
			RalphDir:           ralphDir,
			PromptsDir:         promptsDir,
		}, logging.New(nil), newCaptureRunner(captured), nil)
	}

	tests := []struct {
		name string
		run  func(vrf *verifier.Verifier, captured *[]string)
	}{
		{
			name: "SpawnTestFixAgent",
			run: func(vrf *verifier.Verifier, captured *[]string) {
				vrf.SpawnTestFixAgent(spawn, "ac", "output", 1, 3)
				vrf.SpawnTestFixAgent(spawn, "ac", "output", 2, 3)
			},
		},
		{
			name: "SpawnCompileFixAgent",
			run: func(vrf *verifier.Verifier, captured *[]string) {
				vrf.SpawnCompileFixAgent(spawn, "ac", "errors", 1, 3)
				vrf.SpawnCompileFixAgent(spawn, "ac", "errors", 2, 3)
			},
		},
		{
			name: "SpawnVerifyFixAgent",
			run: func(vrf *verifier.Verifier, captured *[]string) {
				vrf.SpawnVerifyFixAgent(spawn, "desc", "ac", "rejected", 1, 3)
				vrf.SpawnVerifyFixAgent(spawn, "desc", "ac", "rejected", 2, 3)
			},
		},
		{
			name: "SpawnCIFixAgent",
			run: func(vrf *verifier.Verifier, captured *[]string) {
				vrf.SpawnCIFixAgent(verifier.CIFixInput{Spawn: spawn, PRNumber: 1, RequiredFailures: []string{"ci"}})
			},
		},
		{
			name: "SpawnCopilotFixAgent",
			run: func(vrf *verifier.Verifier, captured *[]string) {
				vrf.SpawnCopilotFixAgent(spawn, "review context")
			},
		},
		{
			name: "SpawnConflictFixAgent",
			run: func(vrf *verifier.Verifier, captured *[]string) {
				vrf.SpawnConflictFixAgent(spawn, "diff", "bead desc")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var captured []string
			vrf := makeVrf(&captured)
			tc.run(vrf, &captured)

			if len(captured) == 0 {
				t.Fatal("no runner.Run calls recorded")
			}
			// Attempt 1 must use first model (sonnet).
			if captured[0] != firstModel {
				t.Errorf("attempt 1: expected %s, got %s", firstModel, captured[0])
			}
			// Attempt 2 must use escalation model (opus) — only for methods that accept attempt.
			if len(captured) >= 2 {
				if captured[1] != escalationModel {
					t.Errorf("attempt 2: expected %s, got %s", escalationModel, captured[1])
				}
			}
		})
	}
}

// Proves: ModelCap clamps the fix-agent escalation model. With ModelCap=sonnet
// and FixEscalationModel=opus, attempt 2 still uses sonnet.
func TestFixModelEscalation_ModelCapApplied(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	var capturedModels []string
	vrf := verifier.New(verifier.Config{
		FixModel:           verify.ModelSonnet,
		FixEscalationModel: verify.ModelOpus,
		ModelCap:           verify.ModelSonnet,
		RalphDir:           ralphDir,
	}, logging.New(nil), func() verifier.Runner {
		return &stubRunner{
			result: claude.Result{SignalDetected: true},
			onRunCfg: func(cfg claude.RunConfig) {
				capturedModels = append(capturedModels, cfg.Model)
			},
		}
	}, nil)

	spawn := verifier.FixAgentSpawn{
		Ctx:     context.Background(),
		WorkDir: dir,
	}
	// Attempt 2 would normally escalate to opus, but ModelCap=sonnet clamps it.
	vrf.SpawnTestFixAgent(spawn, "ac", "output", 2, 3)

	if len(capturedModels) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(capturedModels))
	}
	if capturedModels[0] != verify.ModelSonnet {
		t.Errorf("ModelCap not applied: expected %s (sonnet), got %s", verify.ModelSonnet, capturedModels[0])
	}
}

// Proves: when FixModel and FixEscalationModel are unset in Config, defaults
// are ModelSonnet for attempt 1 and ModelOpus for attempt 2+.
func TestFixModelEscalation_DefaultModels(t *testing.T) {
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

	spawn := verifier.FixAgentSpawn{
		Ctx:     context.Background(),
		WorkDir: dir,
	}
	vrf.SpawnTestFixAgent(spawn, "ac", "output", 1, 3)
	vrf.SpawnTestFixAgent(spawn, "ac", "output", 2, 3)

	if len(capturedModels) != 2 {
		t.Fatalf("expected 2 runner calls, got %d", len(capturedModels))
	}
	if capturedModels[0] != verify.ModelSonnet {
		t.Errorf("default attempt 1: expected %s (sonnet), got %s", verify.ModelSonnet, capturedModels[0])
	}
	if capturedModels[1] != verify.ModelOpus {
		t.Errorf("default attempt 2: expected %s (opus), got %s", verify.ModelOpus, capturedModels[1])
	}
}

// Phase C deletion: TestTryFixCI_RevertsOutOfScopeFiles and
// TestTryFixCI_DoesNotRevertInScopeFiles both rely on HeadRevFunc
// (sequenced "before-sha"/"after-sha") and DiffFilesBetweenFunc
// (different return per (from, to) pair). The new stubRepo exposes neither
// callback-style hook: HeadRev is static, DiffFilesBetween returns
// cfg.DiffFilesBetweenResult regardless of args, and RevertFilesToRef has
// no call-tracking. Out-of-scope revert logic is a real-git behavior and
// belongs in Phase D integration with a real rebase+checkout cycle.
