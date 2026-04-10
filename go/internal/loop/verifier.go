package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/verify"
)

// HeartbeatInterval is how often a heartbeat line is emitted while tests run.
// Exported so tests can override it.
var HeartbeatInterval = 30 * time.Second

// VerifierConfig holds the configuration needed by the Verifier.
type VerifierConfig struct {
	VerifyDir             string
	ProjectDir            string // project root used as fallback when ralph:verify is absent from VerifyDir
	VerifyModel           string
	VerifyEscalationModel string
	FixModel              string // model used by all fix agents; defaults to ModelOpus
	ModelCap              string // maximum model tier ceiling from --model flag; empty means no cap
	PromptsDir            string
	RalphDir              string
	IdleTimeout           time.Duration
	MaxLLMVerifyAttempts  int
	MaxTestFixAttempts    int
	TestTimeout           time.Duration
	CompileCheckTimeout   time.Duration
}

// VerifierDeps holds the injected dependencies for the Verifier.
type VerifierDeps struct {
	Logger      *logging.Logger
	Git         git.Ops
	State       *state.Store
	TaskBackend tasks.Backend
	Runner      func() claudeRunner // returns the current main runner (for StopStreaming before fix agents)
	Signals     claude.SignalPaths
	NewRunner   func() claudeRunner // creates a new runner for fix agents
	QueryFn     verify.QueryFunc
	LLMVerify   func(opts verify.VerifyOpts) verify.Result
	SkipTask    func(id, reason string)
}

// Verifier owns the full post-signal verification flow: test suite,
// LLM review, fix agent spawning, retry logic, and feature-existence check.
type Verifier struct {
	cfg  VerifierConfig
	deps VerifierDeps

	testFixAttempts   int
	llmVerifyAttempts int
}

// NewVerifier creates a Verifier from config and injected dependencies.
func NewVerifier(cfg VerifierConfig, deps VerifierDeps) *Verifier {
	llmVerify := deps.LLMVerify
	if llmVerify == nil {
		llmVerify = verify.LLMVerifyPR
	}
	deps.LLMVerify = llmVerify
	if cfg.MaxLLMVerifyAttempts == 0 {
		cfg.MaxLLMVerifyAttempts = 3
	}
	if cfg.MaxTestFixAttempts == 0 {
		cfg.MaxTestFixAttempts = 3
	}
	if cfg.TestTimeout == 0 {
		cfg.TestTimeout = 5 * time.Minute
	}
	if cfg.CompileCheckTimeout == 0 {
		cfg.CompileCheckTimeout = 60 * time.Second
	}
	return &Verifier{cfg: cfg, deps: deps}
}

// signalParams holds the context needed by OnSignal.
type signalParams struct {
	ctx        context.Context
	headBefore string
	workDir    string
	rawLogPath string
	taskID     string
	nextTask   string
}

// OnSignal runs post-signal verification: test suite, LLM review, and
// message injection. Returns true when verified, false to clear the signal
// and let the agent continue.
func (v *Verifier) OnSignal(p signalParams) bool {
	v.llmVerifyAttempts = 0
	v.testFixAttempts = 0

	commitResult := verify.CheckCommits(p.headBefore, v.deps.Git.HeadRev())
	if !commitResult.Passed {
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error}, "No commits found — task was not worked")
		return false
	}

	v.deps.Logger.Emit(logging.Opts{Domain: logging.Test}, "Running post-signal test suite...")
	testResult, elapsed := v.runTestsWithHeartbeat(p.ctx, v.cfg.VerifyDir)
	if testResult.Passed {
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Test}, "Tests passed (%s)", elapsed)
	}

	taskDesc := getTaskDescription(v.deps.TaskBackend, p.taskID)
	taskAcceptance := getTaskAcceptance(v.deps.TaskBackend, p.taskID)

	if !testResult.Passed {
		if testResult.ScriptMissing {
			v.deps.Logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "ralph:verify script not found — cannot verify")
			return false
		}
		if !v.testFixLoop(p, taskDesc, taskAcceptance, testResult.Details) {
			return false
		}
	}

	// Run compile check (go build / tsc --noEmit) after tests pass.
	// Pre-existing type errors are the agent's responsibility to fix.
	if v.cfg.VerifyDir != "" {
		if !v.compileFixLoop(p, taskAcceptance) {
			return false
		}
	}

	if !v.verifyWithFixLoop(p, taskDesc, taskAcceptance) {
		return false
	}

	// Re-run compile check after LLM verification — fix agents spawned
	// during verification may have introduced new build errors.
	if v.cfg.VerifyDir != "" {
		if !v.compileFixLoop(p, taskAcceptance) {
			return false
		}
	}

	return true
}

// testFixLoop spawns fix agents to address test failures, re-running tests
// after each fix attempt. Returns true when tests pass, false when attempts
// are exhausted or the fix agent fails to signal.
func (v *Verifier) testFixLoop(p signalParams, taskDesc, taskAcceptance, testDetails string) bool {
	for {
		v.testFixAttempts++
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Tests failed (attempt %d/%d)", v.testFixAttempts, v.cfg.MaxTestFixAttempts)

		if v.testFixAttempts > v.cfg.MaxTestFixAttempts {
			v.deps.Logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "Tests still failing after %d attempts — giving up", v.cfg.MaxTestFixAttempts)
			return false
		}

		if !v.tryFixTests(p, taskDesc, taskAcceptance, testDetails) {
			return false
		}

		v.deps.Logger.Emit(logging.Opts{Domain: logging.Test}, "Re-running test suite after test fix agent...")
		rerun, rerunElapsed := v.runTestsWithHeartbeat(p.ctx, v.cfg.VerifyDir)
		if rerun.Passed {
			v.deps.Logger.Emit(logging.Opts{Domain: logging.Test}, "Tests passed after fix agent (%s)", rerunElapsed)
			v.testFixAttempts = 0
			return true
		}
		testDetails = rerun.Details
	}
}

// tryFixTests spawns a fix agent to address test failures.
func (v *Verifier) tryFixTests(p signalParams, taskDesc, taskAcceptance, testDetails string) bool {
	_ = taskDesc // reserved for future fix-prompt template additions
	v.deps.Logger.Emit(logging.Opts{Domain: logging.Test}, "Spawning fix agent for test failures (attempt %d/%d)", v.testFixAttempts, v.cfg.MaxTestFixAttempts)

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("verify-tests.md", map[string]string{
		"{{TASK_TITLE}}":       p.nextTask,
		"{{TASK_DESCRIPTION}}": fmt.Sprintf("Tests failed after completion. Fix the failures.\n\nAcceptance criteria:\n%s", taskAcceptance),
		"{{TEST_OUTPUT}}":      testDetails,
		"{{SIGNAL_COMPLETE}}":  signalPath,
	})

	fixResult := v.runFixAgent(p.ctx, "test failures", fixPrompt, p.workDir, p.rawLogPath)
	return fixResult.SignalDetected
}

// compileFixLoop runs CompileCheck and, on failure, spawns fix agents to
// resolve build/type errors. Returns true when compilation passes, false
// when attempts are exhausted. Uses its own attempt counter separate from
// test fix attempts.
func (v *Verifier) compileFixLoop(p signalParams, taskAcceptance string) bool {
	compileResult := verify.CompileCheck(p.ctx, v.cfg.CompileCheckTimeout, v.cfg.VerifyDir)
	if compileResult.Passed {
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Build}, "Compile check passed")
		return true
	}

	compileAttempts := 0
	details := compileResult.Reason
	if compileResult.Details != "" {
		details += "\n" + compileResult.Details
	}
	for {
		compileAttempts++
		if compileAttempts > v.cfg.MaxTestFixAttempts {
			v.deps.Logger.Emit(logging.Opts{Domain: logging.Build, Level: logging.Error}, "Compile check still failing after %d fix attempts — giving up", v.cfg.MaxTestFixAttempts)
			return false
		}

		v.deps.Logger.Emit(logging.Opts{Domain: logging.Build, Level: logging.Warn}, "Compile check failed — spawning fix agent (attempt %d/%d)", compileAttempts, v.cfg.MaxTestFixAttempts)

		signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
		fixPrompt := v.loadVerifyPrompt("verify-tests.md", map[string]string{
			"{{TASK_TITLE}}":       p.nextTask,
			"{{TASK_DESCRIPTION}}": fmt.Sprintf("Build/type check failed after completion. Fix the compile errors.\n\nAcceptance criteria:\n%s", taskAcceptance),
			"{{TEST_OUTPUT}}":      details,
			"{{SIGNAL_COMPLETE}}":  signalPath,
		})

		fixResult := v.runFixAgent(p.ctx, "build errors", fixPrompt, p.workDir, p.rawLogPath)
		if !fixResult.SignalDetected {
			return false
		}

		recheck := verify.CompileCheck(p.ctx, v.cfg.CompileCheckTimeout, v.cfg.VerifyDir)
		if recheck.Passed {
			v.deps.Logger.Emit(logging.Opts{Domain: logging.Build}, "Compile check passed after fix agent")
			return true
		}
		details = recheck.Reason
		if recheck.Details != "" {
			details += "\n" + recheck.Details
		}
	}
}

// verifyWithFixLoop runs LLM verification and, on rejection, spawns a fix
// agent within the same iteration. Loops until verified or max attempts
// exhausted. All fix attempts happen in a single OnSignal call — no new
// iteration is created.
func (v *Verifier) verifyWithFixLoop(p signalParams, taskDesc, taskAcceptance string) bool {
	for {
		v.llmVerifyAttempts++
		model := v.verifyModel()
		v.deps.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: model}, "Running LLM verification (attempt %d/%d)...", v.llmVerifyAttempts, v.cfg.MaxLLMVerifyAttempts)
		diff, diffSource := v.fetchVerifyDiff(p.taskID, p.headBefore)
		llmResult := v.deps.LLMVerify(verify.VerifyOpts{
			Ctx:         p.ctx,
			WorkDir:     p.workDir,
			PromptsDir:  v.cfg.PromptsDir,
			TaskID:      p.taskID,
			Title:       p.nextTask,
			Description: taskDesc,
			Acceptance:  taskAcceptance,
			Diff:        diff,
			DiffSource:  diffSource,
			QueryFn:     v.deps.QueryFn,
			Model:       model,
		})

		if llmResult.Passed {
			v.deps.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: model}, "LLM verified: %s", llmResult.Reason)
			v.llmVerifyAttempts = 0
			return true
		}

		v.deps.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Error, Model: model}, "LLM verification rejected (attempt %d/%d): %s", v.llmVerifyAttempts, v.cfg.MaxLLMVerifyAttempts, llmResult.Details)

		if v.llmVerifyAttempts >= v.cfg.MaxLLMVerifyAttempts {
			if p.taskID != "" {
				v.deps.SkipTask(p.taskID, fmt.Sprintf("verification_rejected_%d_attempts: %s", v.cfg.MaxLLMVerifyAttempts, llmResult.Details))
			}
			return false
		}

		if !v.tryFixVerification(p, taskDesc, taskAcceptance, llmResult.Details) {
			return false
		}

		v.deps.Logger.Emit(logging.Opts{Domain: logging.Test}, "Re-running test suite after fix agent...")
		testResult, testElapsed := v.runTestsWithHeartbeat(p.ctx, v.cfg.VerifyDir)
		if !testResult.Passed {
			v.deps.Logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "Tests failed after fix agent (%s): %s", testElapsed, testResult.Reason)
			return false
		}
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Test}, "Tests passed after fix agent (%s)", testElapsed)
	}
}

// tryFixVerification spawns a fix agent to address LLM verification rejection.
func (v *Verifier) tryFixVerification(p signalParams, taskDesc, taskAcceptance, rejectionDetails string) bool {
	v.deps.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: v.fixModel()}, "Spawning fix agent for verification rejection (attempt %d/%d)", v.llmVerifyAttempts, v.cfg.MaxLLMVerifyAttempts)

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("verify-fix.md", map[string]string{
		"{{TASK_TITLE}}":          p.nextTask,
		"{{TASK_DESCRIPTION}}":    taskDesc,
		"{{ACCEPTANCE_CRITERIA}}": taskAcceptance,
		"{{REJECTION_REASON}}":    rejectionDetails,
		"{{SIGNAL_COMPLETE}}":     signalPath,
	})

	fixResult := v.runFixAgent(p.ctx, "verification rejection", fixPrompt, p.workDir, p.rawLogPath)
	return fixResult.SignalDetected
}

// fetchVerifyDiff returns the diff and its label to feed to LLMVerifyPR.
// Prefers the PR diff (which covers prior iterations) and falls back to the
// current iteration diff. Returns empty strings when neither is available —
// LLMVerifyPR treats that as a no-op pass.
func (v *Verifier) fetchVerifyDiff(taskID, headBefore string) (string, string) {
	if diff := v.deps.Git.PRDiffForTask(taskID); diff != "" {
		return diff, "PR"
	}
	if diff := v.deps.Git.DiffFull(headBefore, "HEAD"); diff != "" {
		return diff, "iteration"
	}
	return "", ""
}

// ResetCounters resets the test and LLM verify attempt counters.
func (v *Verifier) ResetCounters() {
	v.testFixAttempts = 0
	v.llmVerifyAttempts = 0
}

// runTestsWithHeartbeat calls verify.RunTests and emits a periodic heartbeat
// log line every HeartbeatInterval while waiting, so loop.log stays alive
// during long test suites. The goroutine is cancelled as soon as RunTests
// returns. Returns the test result and elapsed duration.
func (v *Verifier) runTestsWithHeartbeat(ctx context.Context, dir string) (verify.Result, time.Duration) {
	start := time.Now()
	done := make(chan struct{})
	defer close(done)

	go func() {
		ticker := time.NewTicker(HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				v.deps.Logger.Emit(logging.Opts{Domain: logging.Test},
					"Tests still running... (%s elapsed)", time.Since(start).Truncate(time.Millisecond))
			}
		}
	}()

	result := verify.RunTests(ctx, v.cfg.TestTimeout, dir, v.cfg.ProjectDir)
	return result, time.Since(start).Truncate(time.Millisecond)
}

// verifyModel returns the model for the current LLM verification attempt,
// capped by ModelCap when set. The first attempt uses VerifyModel (haiku);
// subsequent attempts escalate to VerifyEscalationModel (sonnet).
func (v *Verifier) verifyModel() string {
	var model string
	if v.llmVerifyAttempts <= 1 {
		model = v.cfg.VerifyModel
		if model == "" {
			model = verify.ModelHaiku
		}
	} else {
		model = v.cfg.VerifyEscalationModel
		if model == "" {
			model = verify.ModelSonnet
		}
	}
	return verify.CapModel(v.cfg.ModelCap, model)
}

// fixModel returns the model to use for all fix agents, capped by ModelCap
// when set.
func (v *Verifier) fixModel() string {
	base := v.cfg.FixModel
	if base == "" {
		base = verify.ModelOpus
	}
	return verify.CapModel(v.cfg.ModelCap, base)
}

// VerifyCompletion runs post-signal checks: commit presence and test suite.
func (v *Verifier) VerifyCompletion(ctx context.Context, workDir, headBefore string) (bool, string) {
	if v.cfg.VerifyDir == "" {
		return true, ""
	}

	commitResult := verify.CheckCommits(headBefore, v.deps.Git.HeadRev())
	if !commitResult.Passed {
		return false, commitResult.Reason
	}

	testResult := verify.RunTests(ctx, v.cfg.TestTimeout, v.cfg.VerifyDir, v.cfg.ProjectDir)
	now := time.Now().Format(time.RFC3339)
	if !testResult.Passed {
		v.deps.State.Write("last_test_result", "fail")
		v.deps.State.Write("last_test_time", now)
		if testResult.Details != "" {
			v.deps.Logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "Test output:\n%s", testResult.Details)
		}
		return false, testResult.Reason
	}

	v.deps.State.Write("last_test_result", "pass")
	v.deps.State.Write("last_test_time", now)
	return true, ""
}

// RunPreIterationTests runs the full test suite and compile check before
// handing off to the agent. Returns a human-readable status string for the
// agent prompt so the agent knows about pre-existing failures to fix.
func (v *Verifier) RunPreIterationTests(ctx context.Context) string {
	if v.cfg.VerifyDir == "" {
		return ""
	}

	var msg string

	v.deps.Logger.Emit(logging.Opts{Domain: logging.Test}, "Running pre-iteration test suite...")
	testStart := time.Now()
	result := verify.RunTests(ctx, v.cfg.TestTimeout, v.cfg.VerifyDir, v.cfg.ProjectDir)
	testElapsed := time.Since(testStart).Truncate(10 * time.Millisecond)
	now := time.Now().Format(time.RFC3339)

	if result.Command != "" {
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Test}, "Using: %s (in %s)", result.Command, result.Dir)
	}

	if result.Passed {
		v.deps.State.Write("last_test_result", "pass")
		v.deps.State.Write("last_test_time", now)
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Success}, "Pre-iteration tests: all passing (%s, %s)", result.Command, testElapsed)
		msg += "\n- Test suite status: all tests passing as of start."
	} else if result.ScriptMissing {
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "ralph:verify script not found — skipping test suite")
	} else {
		v.deps.State.Write("last_test_result", "fail")
		v.deps.State.Write("last_test_time", now)
		cmdInfo := result.Command
		if cmdInfo == "" {
			cmdInfo = "unknown command"
		}
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Pre-iteration tests: failures detected (%s, %s, %s)", cmdInfo, result.Reason, testElapsed)
		msg += "\n- Test suite status: some tests are FAILING. Fix them before your task. If the tests pass when you run them, they were fixed externally — proceed with your task."
		if result.Details != "" {
			details := result.Details
			lines := strings.Split(details, "\n")
			if len(lines) > 20 {
				details = strings.Join(lines[len(lines)-20:], "\n")
			}
			msg += "\n  Failure output:\n  " + strings.ReplaceAll(details, "\n", "\n  ")
		}
	}

	compileStart := time.Now()
	compileResult := verify.CompileCheck(ctx, v.cfg.CompileCheckTimeout, v.cfg.VerifyDir)
	compileElapsed := time.Since(compileStart).Truncate(10 * time.Millisecond)

	if compileResult.Command != "" {
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Build}, "Using: %s (in %s)", compileResult.Command, compileResult.Dir)
	}

	if compileResult.Passed {
		cmdInfo := compileResult.Command
		if cmdInfo == "" {
			cmdInfo = "skipped"
		}
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Build, Level: logging.Success}, "Pre-iteration compile check: passing (%s, %s)", cmdInfo, compileElapsed)
	} else {
		cmdInfo := compileResult.Command
		if cmdInfo == "" {
			cmdInfo = "unknown command"
		}
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Build, Level: logging.Warn}, "Pre-iteration compile check: failures detected (%s, %s, %s)", cmdInfo, compileResult.Reason, compileElapsed)
		msg += "\n- Build status: compile check is FAILING. Fix the build errors before your task."
		details := compileResult.Details
		if details == "" {
			details = compileResult.Reason
		}
		if details != "" {
			lines := strings.Split(details, "\n")
			if len(lines) > 20 {
				details = strings.Join(lines[len(lines)-20:], "\n")
			}
			msg += "\n  Compile errors:\n  " + strings.ReplaceAll(details, "\n", "\n  ")
		}
	}

	return msg
}

// TryFixCI spawns a fix agent to address CI failures. Only required check
// failures are passed to the agent; optional/deploy checks are logged but
// filtered out. Returns false without spawning if only optional checks failed.
func (v *Verifier) TryFixCI(ctx context.Context, ciLog string, ciErr *git.CIFailureError, nextTask string, workDir, rawLogPath string) bool {
	required := git.RequiredFailedChecks(ciErr.Failures)

	var optionalNames []string
	for _, f := range ciErr.Failures {
		if !f.IsRequired {
			optionalNames = append(optionalNames, f.Name)
		}
	}
	if len(optionalNames) > 0 {
		v.deps.Logger.Emit(logging.Opts{Domain: logging.CI}, "Ignoring optional/deploy check failures: %s", strings.Join(optionalNames, ", "))
	}

	if len(required) == 0 {
		v.deps.Logger.Emit(logging.Opts{Domain: logging.CI}, "Only optional checks failed on PR #%d — skipping fix agent", ciErr.PRNumber)
		return false
	}

	v.deps.Logger.Emit(logging.Opts{Domain: logging.CI}, "CI failed on PR #%d — spawning fix agent for required checks", ciErr.PRNumber)

	var checkNames []string
	for _, f := range required {
		checkNames = append(checkNames, f.Name)
	}

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("verify-ci.md", map[string]string{
		"{{TASK_TITLE}}":      nextTask,
		"{{FAILED_CHECKS}}":   strings.Join(checkNames, ", "),
		"{{CI_LOG}}":          ciLog,
		"{{SIGNAL_COMPLETE}}": signalPath,
	})

	fixResult := v.runFixAgent(ctx, "CI failures", fixPrompt, workDir, rawLogPath)
	return fixResult.SignalDetected
}

// TryCopilotFix spawns a fix agent to address actionable Copilot review
// comments. reviewContext is the pre-formatted comment block including file
// paths and line numbers. Returns false without spawning if the fix agent
// exits without signaling.
func (v *Verifier) TryCopilotFix(ctx context.Context, reviewContext, nextTask, workDir, rawLogPath string) bool {
	v.deps.Logger.Emit(logging.Opts{Domain: logging.Git}, "Spawning Copilot review fix agent")

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("verify-copilot-review.md", map[string]string{
		"{{TASK_TITLE}}":      nextTask,
		"{{REVIEW_FEEDBACK}}": reviewContext,
		"{{SIGNAL_COMPLETE}}": signalPath,
	})

	fixResult := v.runFixAgent(ctx, "Copilot review feedback", fixPrompt, workDir, rawLogPath)
	return fixResult.SignalDetected
}

// TryFixConflict spawns a conflict resolution agent when automatic rebase
// could not resolve merge conflicts.
func (v *Verifier) TryFixConflict(ctx context.Context, conflictDiff, beadDesc, nextTask, workDir, rawLogPath string) bool {
	v.deps.Logger.Emit(logging.Opts{Domain: logging.Git}, "Unresolvable merge conflict — spawning conflict resolution agent")

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("resolve-conflict.md", map[string]string{
		"{{TASK_TITLE}}":       nextTask,
		"{{TASK_DESCRIPTION}}": beadDesc,
		"{{CONFLICT_DIFF}}":    conflictDiff,
		"{{SIGNAL_COMPLETE}}":  signalPath,
	})

	fixResult := v.runFixAgent(ctx, "conflict resolution", fixPrompt, workDir, rawLogPath)
	return fixResult.SignalDetected
}


func (v *Verifier) runFixAgent(ctx context.Context, description, prompt, workDir, rawLogPath string) claude.Result {
	v.deps.Runner().StopStreaming()
	v.deps.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: v.fixModel()}, "Spawning fix agent: %s", description)

	runner := v.deps.NewRunner()
	result, _ := runner.Run(claude.RunConfig{
		Ctx:          ctx,
		WorkDir:      workDir,
		RalphDir:     v.cfg.RalphDir,
		Prompt:       prompt,
		RawLog:       rawLogPath,
		Quiet:        true,
		Signals:      v.deps.Signals,
		PollInterval: 2 * time.Second,
		IdleTimeout:  v.cfg.IdleTimeout,
		Model:        v.fixModel(),
	})

	if !result.SignalDetected {
		v.deps.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: v.fixModel()}, "Fix agent exited without signal (%s)", description)
	} else if result.Summary != "" {
		v.deps.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: v.fixModel()}, "Fix agent (%s): %s", description, result.Summary)
	}

	return result
}

func (v *Verifier) loadVerifyPrompt(filename string, vars map[string]string) string {
	path := filepath.Join(v.cfg.PromptsDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		result := ""
		for k, val := range vars {
			result += k + ": " + val + "\n"
		}
		return result
	}
	s := string(data)
	for k, val := range vars {
		s = strings.ReplaceAll(s, k, val)
	}
	return s
}

func getTaskAcceptance(backend tasks.Backend, taskID string) string {
	if taskID == "" || backend == nil {
		return ""
	}
	ac, err := backend.GetAcceptance(taskID)
	if err != nil {
		return ""
	}
	return ac
}
