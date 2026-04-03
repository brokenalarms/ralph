package loop

import (
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
	"github.com/brokenalarms/ralph/internal/verify"
)

// promptCapturingFixRunner captures the prompt and model passed to Run so
// tests can verify that fix agents receive the correct context and model.
type promptCapturingFixRunner struct {
	onPrompt func(string)
	onModel  func(string)
	result   claude.Result
}

func (r *promptCapturingFixRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	if r.onPrompt != nil {
		r.onPrompt(cfg.Prompt)
	}
	if r.onModel != nil {
		r.onModel(cfg.Model)
	}
	return r.result, nil
}

func (r *promptCapturingFixRunner) StopStreaming() {}

func (r *promptCapturingFixRunner) InjectMessage(_ string) error { return nil }


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
		Git:         &testutil.StubGit{HeadRevValue: "def456"},
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

// Verifier exhausts LLM retries within a single OnSignal call and calls
// SkipTask, proving retry logic and skip behavior are owned by the Verifier type.
func TestVerifier_OnSignal_LLMExhaustsRetries_SkipsTask(t *testing.T) {
	var skippedID string
	v := newTestVerifier(t, func(v *Verifier) {
		v.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
			return verify.Result{Passed: false, Details: "diff doesn't match bead"}
		}
		v.deps.SkipTask = func(id, reason string) { skippedID = id }
		v.deps.NewRunner = func() claudeRunner {
			return &stubRunner{result: stubResult(true, "attempted fix")}
		}
	})

	result := v.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: t.TempDir(), rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID: "test-skip", nextTask: "Skip test",
	})

	if result {
		t.Fatal("expected OnSignal to return false after exhausting retries")
	}
	if skippedID != "test-skip" {
		t.Fatalf("expected task test-skip to be skipped, got %q", skippedID)
	}
}

// Model escalation within a single OnSignal call: first attempt uses Haiku,
// subsequent attempts within the same iteration use Sonnet.
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
		v.deps.NewRunner = func() claudeRunner {
			return &stubRunner{result: stubResult(true, "attempted fix")}
		}
	})

	params := signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: t.TempDir(), rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID: "test-escalation", nextTask: "Escalation test",
	}

	v.OnSignal(params)

	if len(modelsUsed) != 3 {
		t.Fatalf("expected 3 LLM calls within one iteration, got %d", len(modelsUsed))
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

// TryFixCI passes failed check names and CI log output as separate fields
// in the fix agent prompt, so the agent can read the error literally
// instead of guessing from check names alone.
func TestVerifier_TryFixCI_PromptContainsCILogAndCheckNames(t *testing.T) {
	var capturedPrompt string
	v := newTestVerifier(t, func(v *Verifier) {
		// Write the verify-ci.md template so loadVerifyPrompt uses it.
		ciTemplate := "CHECKS: {{FAILED_CHECKS}}\nLOG: {{CI_LOG}}\nSIGNAL: {{SIGNAL_COMPLETE}}\nTASK: {{TASK_TITLE}}"
		os.WriteFile(filepath.Join(v.cfg.PromptsDir, "verify-ci.md"), []byte(ciTemplate), 0o644)

		v.deps.NewRunner = func() claudeRunner {
			return &promptCapturingFixRunner{
				onPrompt: func(p string) { capturedPrompt = p },
				result:   stubResult(true, "fixed import"),
			}
		}
	})

	ciErr := &git.CIFailureError{
		PRNumber: "42",
		Failures: []git.CICheckResult{
			{Name: "typecheck", State: "FAILURE", Bucket: "fail", IsRequired: true},
			{Name: "test", State: "FAILURE", Bucket: "fail", IsRequired: true},
		},
	}
	ciLog := "src/app.tsx(3,1): error TS2307: Cannot find module './MissingSVG'"

	result := v.TryFixCI(context.Background(), ciLog, ciErr, "Build app", t.TempDir(), filepath.Join(t.TempDir(), "raw.log"))

	if !result {
		t.Fatal("expected TryFixCI to return true when fix agent signals")
	}
	if !strings.Contains(capturedPrompt, "typecheck, test") {
		t.Errorf("prompt should contain failed check names, got: %s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "Cannot find module './MissingSVG'") {
		t.Errorf("prompt should contain CI error log, got: %s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "Build app") {
		t.Errorf("prompt should contain task title, got: %s", capturedPrompt)
	}
}

// TryFixCI returns false when the fix agent exits without signaling,
// proving the orchestrator won't force-push stale code.
func TestVerifier_TryFixCI_NoSignal_ReturnsFalse(t *testing.T) {
	v := newTestVerifier(t, func(v *Verifier) {
		v.deps.NewRunner = func() claudeRunner {
			return &stubRunner{result: stubResult(false, "")}
		}
	})

	ciErr := &git.CIFailureError{
		PRNumber: "42",
		Failures: []git.CICheckResult{{Name: "build", State: "FAILURE", Bucket: "fail", IsRequired: true}},
	}

	result := v.TryFixCI(context.Background(), "build failed", ciErr, "Build app", t.TempDir(), filepath.Join(t.TempDir(), "raw.log"))

	if result {
		t.Fatal("expected TryFixCI to return false when fix agent doesn't signal")
	}
}

// TryFixCI filters out optional/deploy checks and only passes required
// check names to the fix agent prompt.
// TryFixCI passes all failed checks to the fix agent since gh pr checks
// does not expose isRequired.
func TestVerifier_TryFixCI_PassesAllFailedChecks(t *testing.T) {
	var capturedPrompt string
	v := newTestVerifier(t, func(v *Verifier) {
		ciTemplate := "CHECKS: {{FAILED_CHECKS}}\nLOG: {{CI_LOG}}\nSIGNAL: {{SIGNAL_COMPLETE}}\nTASK: {{TASK_TITLE}}"
		os.WriteFile(filepath.Join(v.cfg.PromptsDir, "verify-ci.md"), []byte(ciTemplate), 0o644)

		v.deps.NewRunner = func() claudeRunner {
			return &promptCapturingFixRunner{
				onPrompt: func(p string) { capturedPrompt = p },
				result:   stubResult(true, "fixed"),
			}
		}
	})

	ciErr := &git.CIFailureError{
		PRNumber: "42",
		Failures: []git.CICheckResult{
			{Name: "typecheck", State: "FAILURE", Bucket: "fail"},
			{Name: "deploy/netlify", State: "FAILURE", Bucket: "fail"},
		},
	}

	result := v.TryFixCI(context.Background(), "error log", ciErr, "Build app", t.TempDir(), filepath.Join(t.TempDir(), "raw.log"))

	if !result {
		t.Fatal("expected TryFixCI to return true when fix agent signals")
	}
	if !strings.Contains(capturedPrompt, "typecheck") {
		t.Errorf("prompt should contain typecheck, got: %s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "deploy/netlify") {
		t.Errorf("prompt should contain deploy/netlify, got: %s", capturedPrompt)
	}
}

// Fix agents (verification and CI) always use Opus so they can understand
// abstract feedback that Sonnet repeatedly fails to resolve.
func TestVerifier_FixAgents_UseOpusModel(t *testing.T) {
	var verifyFixModel, ciFixModel, testFixModel string

	// Verification fix agent: LLM rejects once, fix agent spawns.
	v := newTestVerifier(t, func(v *Verifier) {
		llmCalls := 0
		v.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
			llmCalls++
			if llmCalls == 1 {
				return verify.Result{Passed: false, Details: "Push wasn't extracted"}
			}
			return verify.Result{Passed: true, Reason: "approved"}
		}
		v.deps.NewRunner = func() claudeRunner {
			return &promptCapturingFixRunner{
				onModel: func(m string) { verifyFixModel = m },
				result:  stubResult(true, "fixed"),
			}
		}
	})
	v.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: t.TempDir(), rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID: "test-opus", nextTask: "Extract Push",
	})
	if verifyFixModel != verify.ModelOpus {
		t.Errorf("verification fix agent: expected %s, got %s", verify.ModelOpus, verifyFixModel)
	}

	// CI fix agent.
	vCI := newTestVerifier(t, func(v *Verifier) {
		v.deps.NewRunner = func() claudeRunner {
			return &promptCapturingFixRunner{
				onModel: func(m string) { ciFixModel = m },
				result:  stubResult(true, "fixed"),
			}
		}
	})
	ciErr := &git.CIFailureError{
		PRNumber: "42",
		Failures: []git.CICheckResult{{Name: "build", State: "FAILURE", Bucket: "fail", IsRequired: true}},
	}
	vCI.TryFixCI(context.Background(), "build failed", ciErr, "Build app", t.TempDir(), filepath.Join(t.TempDir(), "raw.log"))
	if ciFixModel != verify.ModelOpus {
		t.Errorf("CI fix agent: expected %s, got %s", verify.ModelOpus, ciFixModel)
	}

	// Test fix agent.
	vTest := newTestVerifier(t, func(v *Verifier) {
		verifyDir := v.cfg.VerifyDir
		os.WriteFile(filepath.Join(verifyDir, "Makefile"), []byte("test:\n\t@echo 'FAIL' && exit 1\n"), 0o644)
		v.deps.NewRunner = func() claudeRunner {
			os.Remove(filepath.Join(verifyDir, "Makefile"))
			return &promptCapturingFixRunner{
				onModel: func(m string) { testFixModel = m },
				result:  stubResult(true, "fixed"),
			}
		}
	})
	vTest.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: t.TempDir(), rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID: "test-opus-test", nextTask: "Fix tests",
	})
	if testFixModel != verify.ModelOpus {
		t.Errorf("test fix agent: expected %s, got %s", verify.ModelOpus, testFixModel)
	}
}

// A verifier with prior llmVerifyAttempts from a previous iteration starts
// from attempt 1 on the next OnSignal call, giving it a full quota of retries.
func TestVerifier_OnSignal_ResetsAttemptsEachIteration(t *testing.T) {
	var modelsUsed []string

	v := newTestVerifier(t, func(v *Verifier) {
		v.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
			modelsUsed = append(modelsUsed, opts.Model)
			return verify.Result{Passed: true, Reason: "approved"}
		}
	})

	// Simulate prior iteration that left llmVerifyAttempts at 2.
	v.llmVerifyAttempts = 2

	v.OnSignal(signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    t.TempDir(),
		rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID:     "test-reset",
		nextTask:   "Fresh iteration",
	})

	if len(modelsUsed) == 0 {
		t.Fatal("expected at least one LLM call")
	}
	if modelsUsed[0] != verify.ModelHaiku {
		t.Errorf("fresh iteration should start with %s (attempt 1), got %s — llmVerifyAttempts was not reset", verify.ModelHaiku, modelsUsed[0])
	}
}

// Verification log lines emit the model as a colored [model] sub-tag (not in the
// message body), so operators can visually scan which model is running.
func TestVerifier_VerificationLog_ShowsModelSubTag(t *testing.T) {
	var logOut strings.Builder
	logger := logging.NewWithWriter(&logOut)

	v := newTestVerifier(t, func(v *Verifier) {
		v.deps.Logger = logger
		v.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
			return verify.Result{Passed: true, Reason: "looks good"}
		}
	})

	v.OnSignal(signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    t.TempDir(),
		rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID:     "test-model-tag",
		nextTask:   "Model tag test",
	})

	got := logOut.String()
	if !strings.Contains(got, "[haiku]") {
		t.Errorf("verification log should contain [haiku] model sub-tag, got:\n%s", got)
	}
	// Model name must NOT appear parenthesized in message body.
	if strings.Contains(got, "(haiku)") {
		t.Errorf("model name must not appear as '(haiku)' in message body, got:\n%s", got)
	}
}

// Fix agent spawn log lines emit the model as a [model] sub-tag, not in the
// message body.
func TestVerifier_FixAgentSpawnLog_ShowsModelSubTag(t *testing.T) {
	var logOut strings.Builder
	logger := logging.NewWithWriter(&logOut)

	llmCalls := 0
	v := newTestVerifier(t, func(v *Verifier) {
		v.deps.Logger = logger
		v.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
			llmCalls++
			if llmCalls == 1 {
				return verify.Result{Passed: false, Details: "needs work"}
			}
			return verify.Result{Passed: true, Reason: "approved"}
		}
		v.deps.NewRunner = func() claudeRunner {
			return &stubRunner{result: stubResult(true, "fixed")}
		}
	})

	v.OnSignal(signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    t.TempDir(),
		rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID:     "test-fix-tag",
		nextTask:   "Fix agent tag test",
	})

	got := logOut.String()
	// The fix agent spawn line should show the opus model sub-tag.
	if !strings.Contains(got, "[opus]") {
		t.Errorf("fix agent spawn log should contain [opus] model sub-tag, got:\n%s", got)
	}
}

// TryFixCI returns false when no checks have bucket=fail — nothing to fix.
func TestVerifier_TryFixCI_NoneFailedSkipsAgent(t *testing.T) {
	agentSpawned := false
	v := newTestVerifier(t, func(v *Verifier) {
		v.deps.NewRunner = func() claudeRunner {
			agentSpawned = true
			return &stubRunner{result: stubResult(true, "fixed")}
		}
	})

	ciErr := &git.CIFailureError{
		PRNumber: "42",
		Failures: []git.CICheckResult{
			{Name: "deploy/netlify", State: "SUCCESS", Bucket: "pass"},
			{Name: "Pages changed", State: "SUCCESS", Bucket: "pass"},
		},
	}

	result := v.TryFixCI(context.Background(), "deploy log", ciErr, "Build app", t.TempDir(), filepath.Join(t.TempDir(), "raw.log"))

	if result {
		t.Fatal("expected TryFixCI to return false when no checks failed")
	}
	if agentSpawned {
		t.Fatal("fix agent should not be spawned when no checks failed")
	}
}

// TestVerifier_ModelCap_FixAgent verifies that ModelCap clamps fix agents
// below Opus when --model is set to a lower tier.
func TestVerifier_ModelCap_FixAgent(t *testing.T) {
	var capturedModel string

	v := newTestVerifier(t, func(v *Verifier) {
		v.cfg.ModelCap = verify.ModelSonnet
		llmCalls := 0
		v.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
			llmCalls++
			if llmCalls == 1 {
				return verify.Result{Passed: false, Details: "incomplete"}
			}
			return verify.Result{Passed: true, Reason: "approved"}
		}
		v.deps.NewRunner = func() claudeRunner {
			return &promptCapturingFixRunner{
				onModel: func(m string) { capturedModel = m },
				result:  stubResult(true, "fixed"),
			}
		}
	})
	v.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: t.TempDir(), rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID: "cap-test", nextTask: "Extract Push",
	})
	if capturedModel != verify.ModelSonnet {
		t.Errorf("fix agent with ModelCap=sonnet: expected %s, got %s", verify.ModelSonnet, capturedModel)
	}
}

// runTestsWithHeartbeat emits periodic [test] lines while RunTests is in
// progress, so operators can confirm the loop is alive during long test suites.
func TestVerifier_RunTestsWithHeartbeat_EmitsHeartbeat(t *testing.T) {
	var logOut strings.Builder
	logger := logging.NewWithWriter(&logOut)

	orig := HeartbeatInterval
	HeartbeatInterval = 10 * time.Millisecond
	defer func() { HeartbeatInterval = orig }()

	v := newTestVerifier(t, func(v *Verifier) {
		v.deps.Logger = logger
		os.WriteFile(filepath.Join(v.cfg.VerifyDir, "Makefile"), []byte("test:\n\t@sleep 0.1\n"), 0o644)
	})

	result, elapsed := v.runTestsWithHeartbeat(context.Background(), v.cfg.VerifyDir)

	if !result.Passed {
		t.Fatalf("expected tests to pass, got: %s", result.Reason)
	}
	if !strings.Contains(logOut.String(), "Tests still running") {
		t.Errorf("expected heartbeat lines in log output, got:\n%s", logOut.String())
	}
	if elapsed <= 0 {
		t.Errorf("expected positive elapsed duration, got %s", elapsed)
	}
}

// OnSignal includes elapsed duration in the final Tests passed/failed log line
// so operators can see how long the test suite took without digging in logs.
func TestVerifier_OnSignal_ElapsedInFinalLine(t *testing.T) {
	var logOut strings.Builder
	logger := logging.NewWithWriter(&logOut)

	v := newTestVerifier(t, func(v *Verifier) {
		v.deps.Logger = logger
		v.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
			return verify.Result{Passed: true, Reason: "looks good"}
		}
	})

	v.OnSignal(signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    t.TempDir(),
		rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID:     "test-elapsed",
		nextTask:   "Elapsed test",
	})

	got := logOut.String()
	if !strings.Contains(got, "Tests passed") {
		t.Fatalf("expected 'Tests passed' in log, got:\n%s", got)
	}
	// The final Tests passed line must include an elapsed duration in parentheses.
	if !strings.Contains(got, "(") || !strings.Contains(got, "s)") {
		t.Errorf("expected elapsed duration in 'Tests passed' line, got:\n%s", got)
	}
}

// TestVerifier_ModelCap_OpusUnchanged verifies that ModelCap=opus does not
// restrict fix agents (opus is the ceiling, so the full escalation ladder
// remains intact).
func TestVerifier_ModelCap_OpusUnchanged(t *testing.T) {
	var capturedModel string

	v := newTestVerifier(t, func(v *Verifier) {
		v.cfg.ModelCap = verify.ModelOpus
		llmCalls := 0
		v.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
			llmCalls++
			if llmCalls == 1 {
				return verify.Result{Passed: false, Details: "incomplete"}
			}
			return verify.Result{Passed: true, Reason: "approved"}
		}
		v.deps.NewRunner = func() claudeRunner {
			return &promptCapturingFixRunner{
				onModel: func(m string) { capturedModel = m },
				result:  stubResult(true, "fixed"),
			}
		}
	})
	v.OnSignal(signalParams{
		ctx: context.Background(), headBefore: "abc123",
		workDir: t.TempDir(), rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID: "cap-opus-test", nextTask: "Extract Push",
	})
	if capturedModel != verify.ModelOpus {
		t.Errorf("fix agent with ModelCap=opus: expected %s, got %s", verify.ModelOpus, capturedModel)
	}
}

// Verifies that "Spawning fix agent for test failures" log line does NOT
// include a model tag — it uses Domain: logging.Test, not logging.LLM.
func TestVerifier_TestDomainSpawnLine_NoModelTag(t *testing.T) {
	var buf strings.Builder

	v := newTestVerifier(t, func(v *Verifier) {
		v.cfg.FixModel = verify.ModelOpus
		v.deps.Logger = logging.NewWithWriter(&buf)
		v.deps.Runner = func() claudeRunner { return &stubRunner{} }
		v.deps.NewRunner = func() claudeRunner {
			return &stubRunner{result: stubResult(true, "fixed")}
		}
		v.cfg.VerifyDir = t.TempDir()
	})

	v.tryFixTests(signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    t.TempDir(),
		rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID:     "test-123",
		nextTask:   "Fix something",
	}, "Fix something", "", "tests failed output")

	// Find the specific "Spawning fix agent for test failures" line and verify
	// it does not contain a model sub-tag like [opus], [sonnet], or [haiku].
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "Spawning fix agent for test failures") {
			if strings.Contains(line, "[opus]") || strings.Contains(line, "[sonnet]") || strings.Contains(line, "[haiku]") {
				t.Errorf("test-domain spawn line should not include model tag, got: %q", line)
			}
			return
		}
	}
	t.Error("expected to find 'Spawning fix agent for test failures' line in output")
}

// Verifies that "Running LLM verification" log line includes the model tag,
// proving Domain: logging.LLM lines pass Model to the logger.
func TestVerifier_LLMDomainVerifyLine_HasModelTag(t *testing.T) {
	var buf strings.Builder

	v := newTestVerifier(t, func(v *Verifier) {
		v.cfg.VerifyModel = verify.ModelHaiku
		v.deps.Logger = logging.NewWithWriter(&buf)
		v.deps.LLMVerify = func(opts verify.VerifyOpts) verify.Result {
			return verify.Result{Passed: true, Reason: "looks good"}
		}
	})

	v.verifyWithFixLoop(signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    t.TempDir(),
		rawLogPath: filepath.Join(t.TempDir(), "raw.log"),
		taskID:     "test-123",
		nextTask:   "Fix something",
	}, "task description", "acceptance criteria")

	output := buf.String()
	// "Running LLM verification" uses Domain: logging.LLM, Model: verifyModel.
	// Since VerifyModel is haiku, the output must contain "[haiku]".
	if !strings.Contains(output, "[haiku]") {
		t.Errorf("LLM-domain 'Running LLM verification' line should include model tag [haiku], got output: %q", output)
	}
}
