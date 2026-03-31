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
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verify"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifier.runFixAgent stops the main runner's streaming, creates a new
// runner via NewRunner, passes the standard RunConfig, and returns the result.
func TestVerifier_runFixAgent(t *testing.T) {
	dir, _ := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	mainRunner := &stubRunner{}
	var capturedCfg claude.RunConfig
	fixRunner := &stubRunner{result: claude.Result{SignalDetected: true, Summary: "fixed"}}

	signals := claude.DefaultSignalPaths(ralphDir)
	v := &Verifier{
		cfg: VerifierConfig{
			RalphDir:    ralphDir,
			IdleTimeout: 30 * time.Second,
		},
		deps: VerifierDeps{
			Logger:  logging.New(nil),
			Runner:  func() claudeRunner { return mainRunner },
			Signals: signals,
			NewRunner: func() claudeRunner {
				return &configCapturingRunner{inner: fixRunner, captured: &capturedCfg}
			},
		},
	}

	ctx := context.Background()
	result := v.runFixAgent(ctx, "test failures", "fix the tests", "/work", "/logs/raw.log")

	if !result.SignalDetected {
		t.Error("expected SignalDetected from fix agent result")
	}
	if capturedCfg.Prompt != "fix the tests" {
		t.Errorf("expected prompt %q, got %q", "fix the tests", capturedCfg.Prompt)
	}
	if capturedCfg.WorkDir != "/work" {
		t.Errorf("expected WorkDir %q, got %q", "/work", capturedCfg.WorkDir)
	}
	if capturedCfg.RawLog != "/logs/raw.log" {
		t.Errorf("expected RawLog %q, got %q", "/logs/raw.log", capturedCfg.RawLog)
	}
	if capturedCfg.RalphDir != ralphDir {
		t.Errorf("expected RalphDir %q, got %q", ralphDir, capturedCfg.RalphDir)
	}
	if !capturedCfg.Quiet {
		t.Error("expected Quiet=true for fix agent")
	}
	if capturedCfg.IdleTimeout != 30*time.Second {
		t.Errorf("expected IdleTimeout 30s, got %v", capturedCfg.IdleTimeout)
	}
}

// Verifies that a fix agent's summary is logged when the signal includes
// a descriptive message (not just "done").
func TestVerifier_runFixAgent_logsSummary(t *testing.T) {
	dir, _ := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	var logBuf bytes.Buffer
	fixRunner := &stubRunner{result: claude.Result{
		SignalDetected: true,
		Summary:        "added missing nil check in parseConfig",
	}}

	signals := claude.DefaultSignalPaths(ralphDir)
	v := &Verifier{
		cfg: VerifierConfig{
			RalphDir:    ralphDir,
			IdleTimeout: 30 * time.Second,
		},
		deps: VerifierDeps{
			Logger:  logging.NewWithWriter(&logBuf),
			Runner:  func() claudeRunner { return &stubRunner{} },
			Signals: signals,
			NewRunner: func() claudeRunner {
				return fixRunner
			},
		},
	}

	ctx := context.Background()
	v.runFixAgent(ctx, "test failures", "fix the tests", "/work", "/logs/raw.log")

	output := logBuf.String()
	if !strings.Contains(output, "Fix agent (test failures): added missing nil check in parseConfig") {
		t.Errorf("expected fix agent summary in log output, got: %s", output)
	}
}

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
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\t@echo 'FAIL: broken test' && exit 1\n"), 0o644)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Fix something",
		NextID:    "ralph-test1",
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

	fixAgentCalled := false

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		VerifyDir:     dir,
	}, st, gm, logger)

	l.newRunnerFunc = func() claudeRunner {
		fixAgentCalled = true
		// Fix agent "fixes" by removing the failing Makefile
		os.Remove(filepath.Join(dir, "Makefile"))
		return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "fixed tests"}}
	}

	p := signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-test1",
		nextTask:   "Fix something",
	}

	accepted := l.verifier.OnSignal(p)

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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		VerifyDir:     dir,
	}, st, gm, logger)

	l.verifier.deps.LLMVerify = func(verify.VerifyOpts) verify.Result {
		return verify.Result{Passed: false, Details: "missing error handling in parseConfig"}
	}

	fixAgentCalled := false
	l.newRunnerFunc = func() claudeRunner {
		fixAgentCalled = true
		return &stubRunner{result: claude.Result{}}
	}

	p := signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-llm1",
		nextTask:   "Add feature",
	}

	accepted := l.verifier.OnSignal(p)

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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		VerifyDir:     dir,
	}, st, gm, logger)

	llmCalls := 0
	l.verifier.deps.LLMVerify = func(verify.VerifyOpts) verify.Result {
		llmCalls++
		if llmCalls == 1 {
			return verify.Result{Passed: false, Details: "incomplete implementation"}
		}
		return verify.Result{Passed: true, Reason: "approved"}
	}

	fixAgentCalled := false
	l.newRunnerFunc = func() claudeRunner {
		fixAgentCalled = true
		return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "fixed"}}
	}

	p := signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-fb1",
		nextTask:   "Fix bug",
	}

	accepted := l.verifier.OnSignal(p)

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
// (fix agents loop internally) and onSignal returns false when max is reached.
func TestLoop_onSignal_TestFixAttemptsExhausted(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	// Create a Makefile with a failing test command.
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\t@echo 'FAIL: broken' && exit 1\n"), 0o644)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Fix tests",
		NextID:    "ralph-tr1",
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	fixAttempts := 0
	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		VerifyDir:     dir,
	}, st, gm, logger)

	l.newRunnerFunc = func() claudeRunner {
		fixAttempts++
		// Fix agent signals success but tests keep failing
		return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "attempted fix"}}
	}

	p := signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-tr1",
		nextTask:   "Fix tests",
	}

	// Single onSignal call exhausts all fix attempts internally.
	result := l.verifier.OnSignal(p)

	if result {
		t.Error("expected onSignal to return false after exhausting test fix attempts")
	}
	if fixAttempts != maxTestFixAttempts {
		t.Errorf("expected %d fix agent spawns, got %d", maxTestFixAttempts, fixAttempts)
	}
}
