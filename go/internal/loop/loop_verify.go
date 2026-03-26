package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/agent"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/verify"
)

const maxLLMVerifyAttempts = 3
const maxTestFixAttempts = 3

// signalParams holds the context needed by onSignal, avoiding a long
// parameter list of values captured from the iteration scope.
type signalParams struct {
	ctx        context.Context
	headBefore string
	workDir    string
	rawLogPath string
	taskID     string
	nextTask   string
}

// onSignal runs post-signal verification: test suite, LLM review, and
// message injection. Instead of spawning separate fix agents, test failures
// and LLM rejections are injected to the running agent via stdin — the agent
// has full context of what it built and is better positioned to fix its own
// work. Returns true when verified, false to clear the signal and let the
// agent continue.
func (l *Loop) onSignal(p signalParams) bool {
	// Step 1: Run tests (commit check is a warning, not a gate)
	commitResult := verify.CheckCommits(l.git, p.headBefore)
	if !commitResult.Passed {
		l.logger.Warn("git", "No new commits — will verify via LLM if work is already on main")
	}

	l.logger.Log("test", "Running post-signal test suite...")
	testResult := verify.RunTests(p.ctx, l.cfg.VerifyDir)
	if testResult.Passed {
		l.logger.Log("test", "Tests passed")
	}

	if !testResult.Passed {
		l.testFixAttempts++
		l.logger.Warn("test", "Tests failed (attempt %d/%d): %s", l.testFixAttempts, maxTestFixAttempts, testResult.Reason)

		if l.testFixAttempts > maxTestFixAttempts {
			l.logger.Error("test", "Tests still failing after %d attempts — giving up", maxTestFixAttempts)
			return false
		}

		msg := fmt.Sprintf("Tests failed after your completion signal. Fix these failures and signal completion again.\n\nTest output:\n%s", testResult.Details)
		if err := l.runner.InjectMessage(msg); err != nil {
			l.logger.Warn("test", "Stdin injection failed (%v) — falling back to fix agent", err)
			return l.fallbackFixTestFailures(p, testResult.Details)
		}
		l.logger.Log("test", "Test failure output injected to agent via stdin")
		return false
	}

	// Tests passed — reset test fix counter.
	l.testFixAttempts = 0

	// Step 2: LLM verification — does the diff match the bead?
	beadDesc := l.getBeadDescription(p.taskID)
	beadAcceptance := l.getBeadAcceptance(p.taskID)

	l.llmVerifyAttempts++
	l.logger.Log("llm", "Running LLM verification (attempt %d/%d)...", l.llmVerifyAttempts, maxLLMVerifyAttempts)
	llmResult := l.llmVerifyFunc(p.ctx, l.git, p.workDir, l.cfg.Dirs.PromptsDir, p.taskID, p.headBefore, p.nextTask, beadDesc, beadAcceptance, l.git.GitHub, l.queryFunc())

	if llmResult.Passed && llmResult.NoDiff && l.cfg.VerifyLevel == "hog" {
		l.logger.Log("llm", "No diff detected — spawning codebase verification agent (hog mode)")
		if !l.verifyFeatureExists(p, beadDesc) {
			return false
		}
	}

	if llmResult.Passed {
		l.logger.Success("llm", "LLM verified: %s", llmResult.Reason)
		l.llmVerifyAttempts = 0
		return true
	}

	l.logger.Error("llm", "LLM verification rejected (attempt %d/%d): %s", l.llmVerifyAttempts, maxLLMVerifyAttempts, llmResult.Details)

	if l.llmVerifyAttempts >= maxLLMVerifyAttempts {
		if p.taskID != "" {
			l.skipTask(p.taskID, fmt.Sprintf("verification_rejected_%d_attempts: %s", maxLLMVerifyAttempts, llmResult.Details))
		}
		return false
	}

	msg := fmt.Sprintf("LLM verification rejected your work. Fix the issues and signal completion again.\n\nFeedback:\n%s", llmResult.Details)
	if err := l.runner.InjectMessage(msg); err != nil {
		l.logger.Warn("llm", "Stdin injection failed (%v) — falling back to fix agent", err)
		return l.fallbackFixLLMRejection(p, beadDesc, llmResult.Details)
	}
	l.logger.Log("llm", "LLM feedback injected to agent via stdin")
	return false
}

// fallbackFixTestFailures spawns a fix agent when stdin injection fails.
func (l *Loop) fallbackFixTestFailures(p signalParams, testOutput string) bool {
	beadDesc := l.getBeadDescription(p.taskID)
	signalPath := filepath.Join(l.cfg.Dirs.RalphDir, ".signal_complete")
	verifyPrompt := l.loadVerifyPrompt("verify-tests.md", map[string]string{
		"{{TASK_TITLE}}":       p.nextTask,
		"{{TASK_DESCRIPTION}}": beadDesc,
		"{{TEST_OUTPUT}}":      testOutput,
		"{{SIGNAL_COMPLETE}}":  signalPath,
	})

	verifyResult := l.runFixAgent(p.ctx, "test failures", verifyPrompt, p.workDir, p.rawLogPath)
	if !verifyResult.SignalDetected {
		return false
	}

	retest := verify.RunTests(p.ctx, l.cfg.VerifyDir)
	if !retest.Passed {
		l.logger.Error("test", "Tests still failing after fix agent: %s", retest.Reason)
		return false
	}
	return true
}

// fallbackFixLLMRejection spawns a fix agent when stdin injection fails.
func (l *Loop) fallbackFixLLMRejection(p signalParams, beadDesc, llmFeedback string) bool {
	signalPath := filepath.Join(l.cfg.Dirs.RalphDir, ".signal_complete")
	fixPrompt := l.loadVerifyPrompt("verify-llm.md", map[string]string{
		"{{TASK_TITLE}}":       p.nextTask,
		"{{TASK_DESCRIPTION}}": beadDesc,
		"{{LLM_FEEDBACK}}":     llmFeedback,
		"{{SIGNAL_COMPLETE}}":  signalPath,
	})

	fixResult := l.runFixAgent(p.ctx, "LLM feedback", fixPrompt, p.workDir, p.rawLogPath)
	if !fixResult.SignalDetected {
		return false
	}

	testResult := verify.RunTests(p.ctx, l.cfg.VerifyDir)
	if !testResult.Passed {
		l.logger.Error("test", "Tests failed after LLM fix agent: %s", testResult.Reason)
		return false
	}
	return true
}

// verifyCompletion runs post-signal checks: commit presence and test suite.
// Returns (true, "") on success or (false, reason) on failure.
func (l *Loop) verifyCompletion(ctx context.Context, headBefore string) (bool, string) {
	if l.verifyFunc != nil {
		return l.verifyFunc(ctx, l.git.WorkDir, headBefore)
	}

	if l.cfg.VerifyDir == "" {
		return true, ""
	}

	commitResult := verify.CheckCommits(l.git, headBefore)
	if !commitResult.Passed {
		return false, commitResult.Reason
	}

	testResult := verify.RunTests(ctx, l.cfg.VerifyDir)
	now := time.Now().Format(time.RFC3339)
	if !testResult.Passed {
		l.state.Write("last_test_result", "fail")
		l.state.Write("last_test_output", testResult.Details)
		l.state.Write("last_test_time", now)
		if testResult.Details != "" {
			l.logger.Error("test", "Test output:\n%s", testResult.Details)
		}
		return false, testResult.Reason
	}

	l.state.Write("last_test_result", "pass")
	l.state.Write("last_test_output", "")
	l.state.Write("last_test_time", now)
	return true, ""
}

// runFixAgent stops the main agent's streaming, spawns a fix agent with the
// given prompt, and returns the result. All fix agent invocations share the
// same RunConfig shape — this method is the single place that wires it up.
func (l *Loop) runFixAgent(ctx context.Context, description, prompt, workDir, rawLogPath string) claude.Result {
	l.runner.StopStreaming()
	l.logger.Log("llm", "Spawning fix agent: %s", description)

	runner := l.newRunner()
	result, _ := runner.Run(claude.RunConfig{
		Ctx:          ctx,
		WorkDir:      workDir,
		RalphDir:     l.cfg.Dirs.RalphDir,
		Prompt:       prompt,
		RawLog:       rawLogPath,
		Quiet:        true,
		Signals:      l.signals,
		PollInterval: 2 * time.Second,
		IdleTimeout:  l.cfg.IdleTimeout,
	})

	if !result.SignalDetected {
		l.logger.Warn("llm", "Fix agent exited without signal (%s)", description)
	} else if result.Summary != "" {
		l.logger.Log("llm", "Fix agent (%s): %s", description, result.Summary)
	}

	return result
}

// newRunner returns a claudeRunner for spawning sub-agents. Uses newRunnerFunc
// if set (for testing), otherwise creates a new agent.Runner through the
// centralized agent module (with container isolation if available).
func (l *Loop) newRunner() claudeRunner {
	if l.newRunnerFunc != nil {
		return l.newRunnerFunc()
	}
	var sandbox *agent.Sandbox
	if l.agentRunner != nil && l.agentRunner.Sandbox != nil {
		sandbox = l.agentRunner.Sandbox
	}
	return agent.New(l.logger, sandbox)
}

// queryFunc returns the Query method from the centralized agent runner,
// routing LLM verification through the agent module (with container isolation).
func (l *Loop) queryFunc() verify.QueryFunc {
	if l.agentRunner != nil {
		return l.agentRunner.Query
	}
	return nil
}

// loadVerifyPrompt reads a prompt template from the prompts directory and
// replaces placeholders with the given values.
func (l *Loop) loadVerifyPrompt(filename string, vars map[string]string) string {
	path := filepath.Join(l.cfg.Dirs.PromptsDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		// Fallback: return a minimal prompt
		result := ""
		for k, v := range vars {
			result += k + ": " + v + "\n"
		}
		return result
	}
	s := string(data)
	for k, v := range vars {
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}

// verifyFeatureExists spawns a verification agent with tool access to
// grep/read the codebase and confirm the described feature actually exists.
// Used in "hog" mode when the agent signals completion with no diff.
func (l *Loop) verifyFeatureExists(p signalParams, beadDesc string) bool {
	signalPath := filepath.Join(l.cfg.Dirs.RalphDir, ".signal_complete")
	prompt := l.loadVerifyPrompt("verify-exists.md", map[string]string{
		"{{TASK_TITLE}}":       p.nextTask,
		"{{TASK_DESCRIPTION}}": beadDesc,
		"{{WORK_DIR}}":         p.workDir,
		"{{SIGNAL_COMPLETE}}":  signalPath,
	})

	result := l.runFixAgent(p.ctx, "feature existence check", prompt, p.workDir, p.rawLogPath)
	if !result.SignalDetected {
		l.logger.Warn("llm", "Verification agent exited without signal — treating as rejection")
		if p.taskID != "" {
			l.skipTask(p.taskID, "verification_no_signal: agent could not confirm feature exists")
		}
		return false
	}

	l.logger.Success("llm", "Verification agent confirmed feature exists")
	return true
}

// getBeadAcceptance retrieves the acceptance criteria for verification.
func (l *Loop) getBeadAcceptance(taskID string) string {
	if taskID == "" || l.cfg.TaskBackend == nil {
		return ""
	}
	ac, err := l.cfg.TaskBackend.GetAcceptance(taskID)
	if err != nil {
		return ""
	}
	return ac
}

// getBeadDescription retrieves the bead description for LLM verification.
func (l *Loop) getBeadDescription(taskID string) string {
	if taskID == "" || l.cfg.TaskBackend == nil {
		return ""
	}
	desc, err := l.cfg.TaskBackend.GetDescription(taskID)
	if err != nil {
		return ""
	}
	return desc
}

// getCIFailureLog retrieves the failed CI run's log output for the given PR.
func (l *Loop) getCIFailureLog(prNumber string) string {
	return l.git.GetCIFailureLog(prNumber)
}

// tryFixCI spawns a fix agent to address CI failures, pushes the fix,
// and returns true if the fix was applied (ready for merge retry).
func (l *Loop) tryFixCI(ctx context.Context, ciErr *git.CIFailureError, taskID, nextTask, workDir, rawLogPath string) bool {
	l.logger.Log("ci", "CI failed on PR #%s — spawning fix agent", ciErr.PRNumber)

	ciLog := l.getCIFailureLog(ciErr.PRNumber)
	signalPath := filepath.Join(l.cfg.Dirs.RalphDir, ".signal_complete")
	fixPrompt := l.loadVerifyPrompt("verify-tests.md", map[string]string{
		"{{TASK_TITLE}}":       nextTask,
		"{{TASK_DESCRIPTION}}": "CI checks failed after push. Fix the failures so CI passes.",
		"{{TEST_OUTPUT}}":      ciErr.Error() + "\n\n" + ciLog,
		"{{SIGNAL_COMPLETE}}":  signalPath,
	})

	fixResult := l.runFixAgent(ctx, "CI failures", fixPrompt, workDir, rawLogPath)
	if !fixResult.SignalDetected {
		return false
	}

	if _, pushErr := l.pushAndCreatePR(ctx, taskID, nextTask); pushErr != nil {
		l.logger.Warn("git", "Push after CI fix failed: %v", pushErr)
		return false
	}

	return true
}

// runPreIterationTests runs the full test suite before handing off to the
// agent. Stores results in state.json so they persist across restarts.
// Returns a human-readable status string for the agent prompt.
func (l *Loop) runPreIterationTests(ctx context.Context) string {
	if l.cfg.VerifyDir == "" {
		return ""
	}

	l.logger.Log("test", "Running pre-iteration test suite...")
	result := verify.RunTests(ctx, l.cfg.VerifyDir)
	now := time.Now().Format(time.RFC3339)

	if result.Passed {
		l.state.Write("last_test_result", "pass")
		l.state.Write("last_test_output", "")
		l.state.Write("last_test_time", now)
		l.logger.Success("test", "Pre-iteration tests: all passing")
		return "\n- Test suite status: all tests passing as of start."
	}

	l.state.Write("last_test_result", "fail")
	l.state.Write("last_test_output", result.Details)
	l.state.Write("last_test_time", now)
	l.logger.Warn("test", "Pre-iteration tests: failures detected")
	msg := "\n- Test suite status: some tests are FAILING. Fix them before your task. If the tests pass when you run them, they were fixed externally — proceed with your task."
	if result.Details != "" {
		// Truncate to avoid bloating the prompt.
		details := result.Details
		lines := strings.Split(details, "\n")
		if len(lines) > 20 {
			details = strings.Join(lines[len(lines)-20:], "\n")
		}
		msg += "\n  Failure output:\n  " + strings.ReplaceAll(details, "\n", "\n  ")
	}
	return msg
}

// findPRInfo looks up the PR number, title, and URL for the current branch.
func (l *Loop) findPRInfo(workDir string) (number, title, url string) {
	if l.findPRInfoFunc != nil {
		n, t := l.findPRInfoFunc(workDir)
		return n, t, ""
	}
	gh := l.git.GitHub
	if gh == nil {
		return "", "", ""
	}
	num, t, u, err := gh.FindPR(l.git.WorktreeBranch, workDir)
	if err != nil {
		return "", "", ""
	}
	return num, t, u
}

