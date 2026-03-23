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
	"github.com/brokenalarms/ralph/internal/verify"
)

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

// onSignal runs post-signal verification: test suite, LLM review, and fix
// agent spawning. Returns true when the work is verified and ready to merge.
func (l *Loop) onSignal(p signalParams) bool {
	// Step 1: Run tests (commit check is a warning, not a gate)
	commitResult := verify.CheckCommits(l.git.WorkDir, p.headBefore)
	if !commitResult.Passed {
		l.logger.Warn("No new commits — will verify via LLM if work is already on main")
	}

	l.logger.Log("Running post-signal test suite...")
	testResult := verify.RunTests(p.ctx, l.cfg.VerifyDir)
	passed := testResult.Passed
	reason := testResult.Reason
	if passed {
		l.logger.Log("Tests passed")
	}
	if !passed {
		l.logger.Warn("Tests failed: %s", reason)
		testOutput := testResult.Details

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

		// Re-check tests after fix agent (skip commit check — fix agent
		// may not have new commits if it determined work was correct)

		testResult := verify.RunTests(p.ctx, l.cfg.VerifyDir)
		if !testResult.Passed {
			l.logger.Error("Tests still failing after verification agent: %s", testResult.Reason)
			return false
		}
	}

	// LLM verification — does the diff match the bead?
	// Prefer PR diff (covers prior iterations) over iteration diff.
	if passed {
		beadDesc := l.getBeadDescription(p.taskID)

		l.logger.Log("Running LLM verification...")
		llmResult := l.llmVerifyFunc(p.ctx, p.workDir, l.cfg.Dirs.PromptsDir, p.taskID, p.headBefore, p.nextTask, beadDesc, l.git.GitHub)

		if !llmResult.Passed {
			l.logger.Error("LLM verification rejected: %s", llmResult.Details)

			signalPath := filepath.Join(l.cfg.Dirs.RalphDir, ".signal_complete")
			fixPrompt := l.loadVerifyPrompt("verify-llm.md", map[string]string{
				"{{TASK_TITLE}}":       p.nextTask,
				"{{TASK_DESCRIPTION}}": beadDesc,
				"{{LLM_FEEDBACK}}":     llmResult.Details,
				"{{SIGNAL_COMPLETE}}":  signalPath,
			})

			fixResult := l.runFixAgent(p.ctx, "LLM feedback", fixPrompt, p.workDir, p.rawLogPath)
			if !fixResult.SignalDetected {
				return false
			}

			// Re-verify tests after fix (skip commit check)

			testResult := verify.RunTests(p.ctx, l.cfg.VerifyDir)
			if !testResult.Passed {
				l.logger.Error("Tests failed after LLM fix agent: %s", testResult.Reason)
				return false
			}

			// Escalate to Sonnet for re-verification (Haiku already rejected)
			l.logger.Log("Escalating verification to Sonnet...")
			llmResult2 := l.llmVerifyFunc(p.ctx, p.workDir, l.cfg.Dirs.PromptsDir, p.taskID, p.headBefore, p.nextTask, beadDesc, l.git.GitHub, "claude-sonnet-4-6")
			if !llmResult2.Passed {
				l.logger.Error("Sonnet also rejects: %s", llmResult2.Details)
				// Skip this task instead of accepting bad work
				if p.taskID != "" {
					l.cfg.TaskBackend.SkipTask(p.taskID, "verification_rejected: "+llmResult2.Details)
					l.logger.Warn("Skipping task %s — verification rejected twice", p.taskID)
				}
				return false
			}
			l.logger.Success("Sonnet verified after fix: %s", llmResult2.Reason)
		} else {
			l.logger.Success("LLM verified: %s", llmResult.Reason)
		}
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

	commitResult := verify.CheckCommits(l.git.WorkDir, headBefore)
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
			l.logger.Error("Test output:\n%s", testResult.Details)
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
	l.logger.Log("Spawning fix agent: %s", description)

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
		l.logger.Warn("Fix agent exited without signal (%s)", description)
	} else if result.Summary != "" {
		l.logger.Log("Fix agent (%s): %s", description, result.Summary)
	}

	return result
}

// newRunner returns a claudeRunner for spawning sub-agents. Uses newRunnerFunc
// if set (for testing), otherwise creates a default claude.Runner.
func (l *Loop) newRunner() claudeRunner {
	if l.newRunnerFunc != nil {
		return l.newRunnerFunc()
	}
	return &claude.Runner{Logger: l.logger}
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
	l.logger.Log("CI failed on PR #%s — spawning fix agent", ciErr.PRNumber)

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

	if pushErr := l.pushAndCreatePR(ctx, taskID, nextTask); pushErr != nil {
		l.logger.Warn("Push after CI fix failed: %v", pushErr)
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

	l.logger.Log("Running pre-iteration test suite...")
	result := verify.RunTests(ctx, l.cfg.VerifyDir)
	now := time.Now().Format(time.RFC3339)

	if result.Passed {
		l.state.Write("last_test_result", "pass")
		l.state.Write("last_test_output", "")
		l.state.Write("last_test_time", now)
		l.logger.Success("Pre-iteration tests: all passing")
		return "\n- Test suite status: all tests passing as of start."
	}

	l.state.Write("last_test_result", "fail")
	l.state.Write("last_test_output", result.Details)
	l.state.Write("last_test_time", now)
	l.logger.Warn("Pre-iteration tests: failures detected")
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

// forcePush force-pushes the current branch to the remote.
func (l *Loop) forcePush(ctx context.Context) error {
	if l.forcePushFunc != nil {
		return l.forcePushFunc(ctx)
	}
	return l.git.ForcePush(ctx)
}

// findPRInfo looks up the PR number and title for the current branch.
func (l *Loop) findPRInfo(workDir string) (number, title string) {
	if l.findPRInfoFunc != nil {
		return l.findPRInfoFunc(workDir)
	}
	gh := l.git.GitHub
	if gh == nil {
		return "", ""
	}
	num, t, err := gh.FindPR(l.git.WorktreeBranch, workDir)
	if err != nil {
		return "", ""
	}
	return num, t
}

// resolveConflict rebases onto the default branch and force-pushes to
// resolve PR merge conflicts before the next merge attempt.
func (l *Loop) resolveConflict(ctx context.Context) error {
	l.logger.Log("Rebasing onto default branch to resolve merge conflicts...")
	if err := l.git.RebaseOntoDefaultBranch(ctx); err != nil {
		l.logger.Warn("Rebase failed: %v", err)
		return fmt.Errorf("conflict resolution rebase failed: %w", err)
	}

	l.logger.Log("Force-pushing rebased branch...")
	if err := l.forcePush(ctx); err != nil {
		l.logger.Warn("Force-push after rebase failed: %v", err)
		return fmt.Errorf("force-push after conflict rebase failed: %w", err)
	}
	return nil
}
