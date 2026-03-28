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

// VerifierConfig holds the configuration needed by the Verifier.
type VerifierConfig struct {
	VerifyDir             string
	VerifyLevel           string
	VerifyModel           string
	VerifyEscalationModel string
	PromptsDir            string
	RalphDir              string
	IdleTimeout           time.Duration
}

// VerifierDeps holds the injected dependencies for the Verifier.
type VerifierDeps struct {
	Logger      *logging.Logger
	Git         verify.GitQuerier
	GitHub      git.GitHub
	State       *state.Store
	TaskBackend tasks.Backend
	Runner      func() claudeRunner // returns the current main runner (for InjectMessage)
	Signals     claude.SignalPaths
	NewRunner   func() claudeRunner // creates a new runner for fix agents
	QueryFn     verify.QueryFunc
	LLMVerify   func(opts verify.VerifyOpts) verify.Result
	SkipTask    func(id, reason string)
}

// Verifier owns the full post-signal verification flow: test suite,
// LLM review, stdin injection, retry logic, and feature-existence check.
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
	commitResult := verify.CheckCommits(v.deps.Git, p.headBefore)
	if !commitResult.Passed {
		v.deps.Logger.Warn("git", "No new commits — will verify via LLM if work is already on main")
	}

	v.deps.Logger.Log("test", "Running post-signal test suite...")
	testResult := verify.RunTests(p.ctx, v.cfg.VerifyDir)
	if testResult.Passed {
		v.deps.Logger.Log("test", "Tests passed")
	}

	if !testResult.Passed {
		v.testFixAttempts++
		v.deps.Logger.Warn("test", "Tests failed (attempt %d/%d): %s", v.testFixAttempts, maxTestFixAttempts, testResult.Reason)

		if v.testFixAttempts > maxTestFixAttempts {
			v.deps.Logger.Error("test", "Tests still failing after %d attempts — giving up", maxTestFixAttempts)
			return false
		}

		msg := fmt.Sprintf("Tests failed after your completion signal. Fix these failures and signal completion again.\n\nTest output:\n%s", testResult.Details)
		if err := v.deps.Runner().InjectMessage(msg); err != nil {
			v.deps.Logger.Warn("test", "Stdin injection failed (%v) — agent will be restarted", err)
			return false
		}
		v.deps.Logger.Log("test", "Test failure output injected to agent via stdin")
		return false
	}

	v.testFixAttempts = 0

	beadDesc := getBeadDescription(v.deps.TaskBackend, p.taskID)
	beadAcceptance := getBeadAcceptance(v.deps.TaskBackend, p.taskID)

	v.llmVerifyAttempts++
	model := v.verifyModel()
	v.deps.Logger.Log("llm", "Running LLM verification (attempt %d/%d, %s)...", v.llmVerifyAttempts, maxLLMVerifyAttempts, verify.ModelShortName(model))
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
		GitHub:          v.deps.GitHub,
		QueryFn:         v.deps.QueryFn,
		Model:           model,
	})

	if llmResult.Passed && llmResult.NoDiff && v.cfg.VerifyLevel == "hog" {
		v.deps.Logger.Log("llm", "No diff detected — spawning codebase verification agent (hog mode)")
		if !v.verifyFeatureExists(p, beadDesc) {
			return false
		}
	}

	if llmResult.Passed {
		v.deps.Logger.Success("llm", "LLM verified: %s", llmResult.Reason)
		v.llmVerifyAttempts = 0
		return true
	}

	v.deps.Logger.Error("llm", "LLM verification rejected (attempt %d/%d): %s", v.llmVerifyAttempts, maxLLMVerifyAttempts, llmResult.Details)

	if v.llmVerifyAttempts >= maxLLMVerifyAttempts {
		if p.taskID != "" {
			v.deps.SkipTask(p.taskID, fmt.Sprintf("verification_rejected_%d_attempts: %s", maxLLMVerifyAttempts, llmResult.Details))
		}
		return false
	}

	msg := fmt.Sprintf("LLM verification rejected your work. Fix the issues and signal completion again.\n\nFeedback:\n%s", llmResult.Details)
	if err := v.deps.Runner().InjectMessage(msg); err != nil {
		v.deps.Logger.Warn("llm", "Stdin injection failed (%v) — agent will be restarted", err)
		return false
	}
	v.deps.Logger.Log("llm", "LLM feedback injected to agent via stdin")
	return false
}

// ResetCounters resets the test and LLM verify attempt counters.
func (v *Verifier) ResetCounters() {
	v.testFixAttempts = 0
	v.llmVerifyAttempts = 0
}

// verifyModel returns the model for the current LLM verification attempt.
func (v *Verifier) verifyModel() string {
	if v.llmVerifyAttempts <= 1 {
		if v.cfg.VerifyModel != "" {
			return v.cfg.VerifyModel
		}
		return verify.ModelHaiku
	}
	if v.cfg.VerifyEscalationModel != "" {
		return v.cfg.VerifyEscalationModel
	}
	return verify.ModelSonnet
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
		v.deps.State.Write("last_test_output", testResult.Details)
		v.deps.State.Write("last_test_time", now)
		if testResult.Details != "" {
			v.deps.Logger.Error("test", "Test output:\n%s", testResult.Details)
		}
		return false, testResult.Reason
	}

	v.deps.State.Write("last_test_result", "pass")
	v.deps.State.Write("last_test_output", "")
	v.deps.State.Write("last_test_time", now)
	return true, ""
}

// RunPreIterationTests runs the full test suite before handing off to the
// agent. Returns a human-readable status string for the agent prompt.
func (v *Verifier) RunPreIterationTests(ctx context.Context) string {
	if v.cfg.VerifyDir == "" {
		return ""
	}

	v.deps.Logger.Log("test", "Running pre-iteration test suite...")
	result := verify.RunTests(ctx, v.cfg.VerifyDir)
	now := time.Now().Format(time.RFC3339)

	if result.Passed {
		v.deps.State.Write("last_test_result", "pass")
		v.deps.State.Write("last_test_output", "")
		v.deps.State.Write("last_test_time", now)
		v.deps.Logger.Success("test", "Pre-iteration tests: all passing")
		return "\n- Test suite status: all tests passing as of start."
	}

	v.deps.State.Write("last_test_result", "fail")
	v.deps.State.Write("last_test_output", result.Details)
	v.deps.State.Write("last_test_time", now)
	v.deps.Logger.Warn("test", "Pre-iteration tests: failures detected")
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

// TryFixCI spawns a fix agent to address CI failures.
func (v *Verifier) TryFixCI(ctx context.Context, ciLog string, ciErr *git.CIFailureError, nextTask string, workDir, rawLogPath string) bool {
	v.deps.Logger.Log("ci", "CI failed on PR #%s — spawning fix agent", ciErr.PRNumber)

	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	fixPrompt := v.loadVerifyPrompt("verify-tests.md", map[string]string{
		"{{TASK_TITLE}}":       nextTask,
		"{{TASK_DESCRIPTION}}": "CI checks failed after push. Fix the failures so CI passes.",
		"{{TEST_OUTPUT}}":      ciErr.Error() + "\n\n" + ciLog,
		"{{SIGNAL_COMPLETE}}":  signalPath,
	})

	fixResult := v.runFixAgent(ctx, "CI failures", fixPrompt, workDir, rawLogPath)
	return fixResult.SignalDetected
}

// TryFixConflict spawns a conflict resolution agent when automatic rebase
// could not resolve merge conflicts.
func (v *Verifier) TryFixConflict(ctx context.Context, conflictDiff, beadDesc, nextTask, workDir, rawLogPath string) bool {
	v.deps.Logger.Log("git", "Unresolvable merge conflict — spawning conflict resolution agent")

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

func (v *Verifier) verifyFeatureExists(p signalParams, beadDesc string) bool {
	signalPath := filepath.Join(v.cfg.RalphDir, ".signal_complete")
	prompt := v.loadVerifyPrompt("verify-exists.md", map[string]string{
		"{{TASK_TITLE}}":       p.nextTask,
		"{{TASK_DESCRIPTION}}": beadDesc,
		"{{WORK_DIR}}":         p.workDir,
		"{{SIGNAL_COMPLETE}}":  signalPath,
	})

	result := v.runFixAgent(p.ctx, "feature existence check", prompt, p.workDir, p.rawLogPath)
	if !result.SignalDetected {
		v.deps.Logger.Warn("llm", "Verification agent exited without signal — treating as rejection")
		if p.taskID != "" {
			v.deps.SkipTask(p.taskID, "verification_no_signal: agent could not confirm feature exists")
		}
		return false
	}

	v.deps.Logger.Success("llm", "Verification agent confirmed feature exists")
	return true
}

func (v *Verifier) runFixAgent(ctx context.Context, description, prompt, workDir, rawLogPath string) claude.Result {
	v.deps.Runner().StopStreaming()
	v.deps.Logger.Log("llm", "Spawning fix agent: %s", description)

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
	})

	if !result.SignalDetected {
		v.deps.Logger.Warn("llm", "Fix agent exited without signal (%s)", description)
	} else if result.Summary != "" {
		v.deps.Logger.Log("llm", "Fix agent (%s): %s", description, result.Summary)
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
