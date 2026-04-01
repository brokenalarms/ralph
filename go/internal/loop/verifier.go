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

const maxLLMVerifyAttempts = 3
const maxTestFixAttempts = 3

// HeartbeatInterval is how often a heartbeat line is emitted while tests run.
// Exported so tests can override it.
var HeartbeatInterval = 30 * time.Second

// VerifierConfig holds the configuration needed by the Verifier.
type VerifierConfig struct {
	VerifyDir             string
	VerifyModel           string
	VerifyEscalationModel string
	FixModel              string // model used by all fix agents; defaults to ModelOpus
	ModelCap              string // maximum model tier ceiling from --model flag; empty means no cap
	PromptsDir            string
	RalphDir              string
	IdleTimeout           time.Duration
}

// VerifierDeps holds the injected dependencies for the Verifier.
type VerifierDeps struct {
	Logger      *logging.Logger
	Git         git.GitOps
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

	commitResult := verify.CheckCommits(v.deps.Git, p.headBefore)
	if !commitResult.Passed {
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "No new commits — will verify via LLM if work is already on main")
	}

	v.deps.Logger.Emit(logging.Opts{Domain: logging.Test}, "Running post-signal test suite...")
	testResult, elapsed := v.runTestsWithHeartbeat(p.ctx, v.cfg.VerifyDir)
	if testResult.Passed {
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Test}, "Tests passed (%s)", elapsed)
	}

	beadDesc := getBeadDescription(v.deps.TaskBackend, p.taskID)
	beadAcceptance := getBeadAcceptance(v.deps.TaskBackend, p.taskID)

	if !testResult.Passed {
		if !v.testFixLoop(p, beadDesc, beadAcceptance, testResult.Details) {
			return false
		}
	}

	return v.verifyWithFixLoop(p, beadDesc, beadAcceptance)
}

// testFixLoop spawns fix agents to address test failures, re-running tests
// after each fix attempt. Returns true when tests pass, false when attempts
// are exhausted or the fix agent fails to signal.
func (v *Verifier) testFixLoop(p signalParams, beadDesc, beadAcceptance, testDetails string) bool {
	for {
		v.testFixAttempts++
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Tests failed (attempt %d/%d)", v.testFixAttempts, maxTestFixAttempts)

		if v.testFixAttempts > maxTestFixAttempts {
			v.deps.Logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "Tests still failing after %d attempts — giving up", maxTestFixAttempts)
			return false
		}

		if !v.tryFixTests(p, beadDesc, beadAcceptance, testDetails) {
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
func (v *Verifier) tryFixTests(p signalParams, beadDesc, beadAcceptance, testDetails string) bool {
	v.deps.Logger.Emit(logging.Opts{Domain: logging.Test, Model: v.fixModel()}, "Spawning fix agent for test failures (attempt %d/%d)", v.testFixAttempts, maxTestFixAttempts)

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("verify-tests.md", map[string]string{
		"{{TASK_TITLE}}":       p.nextTask,
		"{{TASK_DESCRIPTION}}": fmt.Sprintf("Tests failed after completion. Fix the failures.\n\nAcceptance criteria:\n%s", beadAcceptance),
		"{{TEST_OUTPUT}}":      testDetails,
		"{{SIGNAL_COMPLETE}}":  signalPath,
	})

	fixResult := v.runFixAgent(p.ctx, "test failures", fixPrompt, p.workDir, p.rawLogPath)
	return fixResult.SignalDetected
}

// verifyWithFixLoop runs LLM verification and, on rejection, spawns a fix
// agent within the same iteration. Loops until verified or max attempts
// exhausted. All fix attempts happen in a single OnSignal call — no new
// iteration is created.
func (v *Verifier) verifyWithFixLoop(p signalParams, beadDesc, beadAcceptance string) bool {
	for {
		v.llmVerifyAttempts++
		model := v.verifyModel()
		v.deps.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: model}, "Running LLM verification (attempt %d/%d)...", v.llmVerifyAttempts, maxLLMVerifyAttempts)
		llmResult := v.deps.LLMVerify(verify.VerifyOpts{
			Ctx:             p.ctx,
			Git:             v.deps.Git,
			WorkDir:         p.workDir,
			PromptsDir:      v.cfg.PromptsDir,
			TaskID:          p.taskID,
			HeadBefore:      p.headBefore,
			BeadTitle:       p.nextTask,
			BeadDescription: beadDesc,
			BeadAcceptance:  beadAcceptance,
			PRDiff:          v.deps.Git.PRDiffForTask(p.taskID),
			QueryFn:         v.deps.QueryFn,
			Model:           model,
		})

		if llmResult.Passed {
			v.deps.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success}, "LLM verified: %s", llmResult.Reason)
			v.llmVerifyAttempts = 0
			return true
		}

		v.deps.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Error}, "LLM verification rejected (attempt %d/%d): %s", v.llmVerifyAttempts, maxLLMVerifyAttempts, llmResult.Details)

		if v.llmVerifyAttempts >= maxLLMVerifyAttempts {
			if p.taskID != "" {
				v.deps.SkipTask(p.taskID, fmt.Sprintf("verification_rejected_%d_attempts: %s", maxLLMVerifyAttempts, llmResult.Details))
			}
			return false
		}

		if !v.tryFixVerification(p, beadDesc, beadAcceptance, llmResult.Details) {
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
func (v *Verifier) tryFixVerification(p signalParams, beadDesc, beadAcceptance, rejectionDetails string) bool {
	v.deps.Logger.Emit(logging.Opts{Domain: logging.LLM, Model: v.fixModel()}, "Spawning fix agent for verification rejection (attempt %d/%d)", v.llmVerifyAttempts, maxLLMVerifyAttempts)

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("verify-fix.md", map[string]string{
		"{{TASK_TITLE}}":         p.nextTask,
		"{{TASK_DESCRIPTION}}":   beadDesc,
		"{{ACCEPTANCE_CRITERIA}}": beadAcceptance,
		"{{REJECTION_REASON}}":   rejectionDetails,
		"{{SIGNAL_COMPLETE}}":    signalPath,
	})

	fixResult := v.runFixAgent(p.ctx, "verification rejection", fixPrompt, p.workDir, p.rawLogPath)
	return fixResult.SignalDetected
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

	result := verify.RunTests(ctx, dir)
	return result, time.Since(start).Truncate(time.Millisecond)
}

// verifyModel returns the model for the current LLM verification attempt,
// capped by ModelCap when set.
func (v *Verifier) verifyModel() string {
	var model string
	if v.llmVerifyAttempts <= 1 {
		if v.cfg.VerifyModel != "" {
			model = v.cfg.VerifyModel
		} else {
			model = verify.ModelHaiku
		}
	} else {
		if v.cfg.VerifyEscalationModel != "" {
			model = v.cfg.VerifyEscalationModel
		} else {
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

	commitResult := verify.CheckCommits(v.deps.Git, headBefore)
	if !commitResult.Passed {
		return false, commitResult.Reason
	}

	testResult := verify.RunTests(ctx, v.cfg.VerifyDir)
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

// RunPreIterationTests runs the full test suite before handing off to the
// agent. Returns a human-readable status string for the agent prompt.
func (v *Verifier) RunPreIterationTests(ctx context.Context) string {
	if v.cfg.VerifyDir == "" {
		return ""
	}

	v.deps.Logger.Emit(logging.Opts{Domain: logging.Test}, "Running pre-iteration test suite...")
	result := verify.RunTests(ctx, v.cfg.VerifyDir)
	now := time.Now().Format(time.RFC3339)

	if result.Passed {
		v.deps.State.Write("last_test_result", "pass")
		v.deps.State.Write("last_test_time", now)
		v.deps.Logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Success}, "Pre-iteration tests: all passing")
		return "\n- Test suite status: all tests passing as of start."
	}

	v.deps.State.Write("last_test_result", "fail")
	v.deps.State.Write("last_test_time", now)
	v.deps.Logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Pre-iteration tests: failures detected")
	msg := "\n- Test suite status: some tests are FAILING. Fix them before your task. If the tests pass when you run them, they were fixed externally — proceed with your task."
	if result.Details != "" {
		details := result.Details
		lines := strings.Split(details, "\n")
		if len(lines) > 20 {
			details = strings.Join(lines[len(lines)-20:], "\n")
		}
		msg += "\n  Failure output:\n  " + strings.ReplaceAll(details, "\n", "\n  ")
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
		v.deps.Logger.Emit(logging.Opts{Domain: logging.CI}, "Only optional checks failed on PR #%s — skipping fix agent", ciErr.PRNumber)
		return false
	}

	v.deps.Logger.Emit(logging.Opts{Domain: logging.CI}, "CI failed on PR #%s — spawning fix agent for required checks", ciErr.PRNumber)

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
		v.deps.Logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn}, "Fix agent exited without signal (%s)", description)
	} else if result.Summary != "" {
		v.deps.Logger.Emit(logging.Opts{Domain: logging.LLM}, "Fix agent (%s): %s", description, result.Summary)
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

func getBeadAcceptance(backend tasks.Backend, taskID string) string {
	if taskID == "" || backend == nil {
		return ""
	}
	ac, err := backend.GetAcceptance(taskID)
	if err != nil {
		return ""
	}
	return ac
}
