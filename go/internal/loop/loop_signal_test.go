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
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Note: TestVerifier_runFixAgent and TestVerifier_runFixAgent_logsSummary
// were removed during the Verifier-extraction migration. runFixAgent is now
// a private method of package verifier; the equivalent coverage belongs in
// package verifier's own test suite.

// Verifies that when post-signal tests fail, a fix agent is spawned with the
// failure output in its prompt — no stdin injection. The fix agent runs within
// the same onSignal call.
func TestLoop_onSignal_TestFailure_SpawnsFixAgent(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	// Create a Makefile with a failing test command so verify.RunTests
	// detects a test runner and returns a failure.
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\t@echo 'FAIL: broken test' && exit 1\n"), 0o644)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Fix something",
		NextID:    "ralph-test1",
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

	fixAgentCalled := false
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		VerifyDir:     dir,
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				fixAgentCalled = true
				// Fix agent "fixes" by replacing the failing Makefile with a passing one
				os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\ttrue\n"), 0o644)
				return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "fixed tests"}}
			},
		}),
	})

	p := verifyPipelineInput{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-test1",
		nextTask:   "Fix something",
	}

	accepted, skipReason := l.runVerifyPipeline(p)
	if skipReason != "" {
		l.skipTask("ralph-test1", skipReason)
	}

	if !accepted {
		t.Error("onSignal should return true after fix agent fixes tests")
	}
	if !fixAgentCalled {
		t.Fatal("expected test failure to spawn a fix agent, not use stdin injection")
	}

	output := logBuf.String()
	if strings.Contains(output, "injected to agent via stdin") {
		t.Error("should not use stdin injection — must spawn fix agent instead")
	}
	if !strings.Contains(output, "Spawning fix agent") {
		t.Errorf("expected fix agent spawn log message, got:\n%s", output)
	}
}

// LLM rejection spawns a fix agent with rejection context — no stdin
// injection. When the fix agent fails (no signal), onSignal returns false.
func TestLoop_onSignal_LLMReject_FixAgentNoSignal_ReturnsFalse(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Add feature",
		NextID:    "ralph-llm1",
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, DiffFullValue: "+ stub diff"}
	logger := logging.New(nil)
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		VerifyDir:     dir,
	}
	fixAgentCalled := false
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				return "NO: missing error handling in parseConfig", nil
			}},
			newRunner: func() verifier.Runner {
				fixAgentCalled = true
				return &stubRunner{result: claude.Result{}}
			},
		}),
	})

	p := verifyPipelineInput{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-llm1",
		nextTask:   "Add feature",
	}

	accepted, skipReason := l.runVerifyPipeline(p)
	if skipReason != "" {
		l.skipTask("ralph-llm1", skipReason)
	}

	if accepted {
		t.Error("onSignal should return false when fix agent exits without signal")
	}
	if !fixAgentCalled {
		t.Error("fix agent should be spawned on LLM rejection")
	}
}

// LLM rejection spawns a fix agent. When the fix agent signals completion,
// the verifier re-runs LLM verification within the same onSignal call.
func TestLoop_onSignal_LLMReject_SpawnsFixAgent(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Fix bug",
		NextID:    "ralph-fb1",
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, DiffFullValue: "+ stub diff"}

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		VerifyDir:     dir,
	}
	llmCalls := 0
	fixAgentCalled := false
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				llmCalls++
				if llmCalls == 1 {
					return "NO: incomplete implementation", nil
				}
				return "YES: approved", nil
			}},
			newRunner: func() verifier.Runner {
				fixAgentCalled = true
				return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "fixed"}}
			},
		}),
	})

	p := verifyPipelineInput{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-fb1",
		nextTask:   "Fix bug",
	}

	accepted, skipReason := l.runVerifyPipeline(p)
	if skipReason != "" {
		l.skipTask("ralph-fb1", skipReason)
	}

	if !accepted {
		t.Error("onSignal should return true after fix agent + re-verify succeeds")
	}
	if !fixAgentCalled {
		t.Error("fix agent should be spawned on LLM rejection")
	}
	if llmCalls != 2 {
		t.Errorf("expected 2 LLM calls (reject + approve), got %d", llmCalls)
	}

	output := logBuf.String()
	if !strings.Contains(output, "Spawning fix agent for verification rejection") {
		t.Errorf("expected fix agent spawn log, got:\n%s", output)
	}
}

// Verifies that test fix attempts are exhausted within a single onSignal call
// (fix agents loop internally), onSignal returns false when max is reached,
// and the task is NOT closed — it stays open for manual investigation.
func TestLoop_onSignal_TestFixAttemptsExhausted(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	// Create a Makefile with a failing test command.
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\t@echo 'FAIL: broken' && exit 1\n"), 0o644)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix tests",
				NextID:    "ralph-tr1",
			},
		},
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	fixAttempts := 0
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		VerifyDir:     dir,
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				fixAttempts++
				// Fix agent signals success but tests keep failing
				return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "attempted fix"}}
			},
		}),
	})

	p := verifyPipelineInput{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-tr1",
		nextTask:   "Fix tests",
	}

	// Single onSignal call exhausts all fix attempts internally.
	result, skipReason := l.runVerifyPipeline(p)
	if skipReason != "" {
		l.skipTask("ralph-tr1", skipReason)
	}

	if result {
		t.Error("expected onSignal to return false after exhausting test fix attempts")
	}
	if fixAttempts != l.maxTestFixAttempts() {
		t.Errorf("expected %d fix agent spawns, got %d", l.maxTestFixAttempts(), fixAttempts)
	}

	// Task must NOT be closed — tests are still failing, leave open for investigation.
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("task must not be closed when test fix agents exhausted, got %v", backend.ClosedIDs)
	}
}

// Ctrl-C (pre-cancelled context) on the no-commits path leaves the bead open.
// Proves that SIGINT before CloseTask on the no-commits branch does not close the bead.
func TestCompleteTask_CancelledCtx_NoCommits_BeadStaysOpen(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-ctrlc1"}},
	}

	// HeadRevValue == headBefore triggers the no-commits branch.
	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, HeadRevValue: "same-sha"}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
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
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel to simulate Ctrl-C

	l.completeTask(ctx, completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "same-sha",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-ctrlc1",
		nextTask:   "Fix bug",
		ralphDir:   ralphDir,
	})

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("Ctrl-C: bead must not be closed on no-commits path, got %v", backend.ClosedIDs)
	}
}

// Ctrl-C (pre-cancelled context) on the no-PR path leaves the bead open.
// Proves that SIGINT before CloseTask when Ship produces no PR does not close the bead.
func TestCompleteTask_CancelledCtx_NoPR_BeadStaysOpen(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-ctrlc2"}},
	}

	// HeadRevValue != headBefore so the no-commits branch is skipped.
	// ShipResult with PRNumber=0 means no PR was created.
	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	gm.ShipResult = git.ShipResult{PRNumber: 0}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
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
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel to simulate Ctrl-C

	l.completeTask(ctx, completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "before",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-ctrlc2",
		nextTask:   "Fix bug",
		ralphDir:   ralphDir,
	})

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("Ctrl-C: bead must not be closed on no-PR path, got %v", backend.ClosedIDs)
	}
}

// When headBefore == headAfterSignal (no new commits) but an open PR exists
// from a prior attempt, the no-commits path must route through finalizePR to
// merge the PR instead of orphaning it.
func TestCompleteTask_NoNewCommits_ExistingOpenPR_MergesViaFinalize(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-retry1"},
			ExternalRefs: map[string]string{
				"ralph-retry1": "gh-42",
			},
		},
	}

	// HeadRevValue == headBefore triggers the no-commits branch.
	// PRState is open — simulates a PR from a prior attempt that wasn't merged.
	gm := &git.StubRepo{
		ProjectDir:       dir,
		WorkDir:          dir,
		HeadRevValue:     "same-sha",
		PRState:          git.PRStateOpen,
		PRBase:           "main",
		DefaultBranch:    "main",
		ShipResult:       git.ShipResult{PRNumber: 42},
		MergeRetryResult: true,
	}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	var postTaskPR int
	var postTaskMerged bool
	l.cfg.OnPostTask = func(_ context.Context, _ string, prNumber int, merged bool) {
		postTaskPR = prNumber
		postTaskMerged = merged
	}

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "same-sha",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-retry1",
		nextTask:   "Fix bug",
		ralphDir:   ralphDir,
	})

	// The task should be closed (finalizePR handles close).
	backend.CloseMu.Lock()
	closedIDs := append([]string{}, backend.ClosedIDs...)
	backend.CloseMu.Unlock()
	if len(closedIDs) != 1 || closedIDs[0] != "ralph-retry1" {
		t.Errorf("expected bead ralph-retry1 to be closed, got %v", closedIDs)
	}

	// MergeWithRetry should have been called (via finalizePR).
	if gm.MergeRetryCalls != 1 {
		t.Errorf("expected MergeWithRetry to be called once, got %d", gm.MergeRetryCalls)
	}

	// Post-task should receive the real PR number and merged=true.
	if postTaskPR != 42 {
		t.Errorf("post-task PR number: got %d, want 42", postTaskPR)
	}
	if !postTaskMerged {
		t.Errorf("post-task merged: got false, want true")
	}

	if out.action != signalSkipped {
		t.Errorf("expected signalSkipped, got %v", out.action)
	}
}

// When headBefore == headAfterSignal and the existing PR is already merged,
// the no-commits path should close the bead without trying to merge again.
func TestCompleteTask_NoNewCommits_ExistingMergedPR_ClosesDirectly(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-retry2"},
			ExternalRefs: map[string]string{
				"ralph-retry2": "gh-55",
			},
		},
	}

	gm := &git.StubRepo{
		ProjectDir:   dir,
		WorkDir:      dir,
		HeadRevValue: "same-sha",
		PRState:      git.PRStateMerged,
	}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "same-sha",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-retry2",
		nextTask:   "Fix bug",
		ralphDir:   ralphDir,
	})

	// Bead closed via the direct path (not finalizePR).
	backend.CloseMu.Lock()
	closedIDs := append([]string{}, backend.ClosedIDs...)
	backend.CloseMu.Unlock()
	if len(closedIDs) != 1 || closedIDs[0] != "ralph-retry2" {
		t.Errorf("expected bead ralph-retry2 to be closed, got %v", closedIDs)
	}

	// MergeWithRetry should NOT have been called — PR is already merged.
	if gm.MergeRetryCalls != 0 {
		t.Errorf("expected no MergeWithRetry calls for already-merged PR, got %d", gm.MergeRetryCalls)
	}

	if out.action != signalSkipped {
		t.Errorf("expected signalSkipped, got %v", out.action)
	}
}

// Ctrl-C (pre-cancelled context) inside finalizePR leaves the bead open.
// Proves that SIGINT before CloseTask in finalizePR does not close the bead
// even when a PR was successfully created.
func TestCompleteTask_CancelledCtx_FinalizePR_BeadStaysOpen(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-ctrlc3"}},
	}

	// HeadRevValue != headBefore so no-commits branch is skipped.
	// Ship returns a PR so finalizePR is called.
	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	gm.ShipResult = git.ShipResult{PRNumber: 99, PRURL: "https://github.com/example/repo/pull/99"}
	gm.PRState = git.PRStateOpen
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     false, // skip merge attempt, go straight to close
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel to simulate Ctrl-C

	l.completeTask(ctx, completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "before",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-ctrlc3",
		nextTask:   "Fix bug",
		ralphDir:   ralphDir,
	})

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("Ctrl-C: bead must not be closed in finalizePR path, got %v", backend.ClosedIDs)
	}
}
