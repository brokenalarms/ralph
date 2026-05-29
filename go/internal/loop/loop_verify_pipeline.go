package loop

import (
	"context"
	"fmt"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/verifier"
	"github.com/brokenalarms/ralph/internal/verify"
)

// verifyPipelineInput bundles the per-iteration data that runVerifyPipeline
// operates on. All fields are data only — no module references. Fresh HEAD
// and diff data are fetched inside runVerifyPipeline from l.git as
// verification progresses.
type verifyPipelineInput struct {
	ctx             context.Context
	headBefore      string
	workDir         string
	rawLogPath      string
	taskID          string
	nextTask        string // task title
	skipCommitCheck bool   // true for no_code_needed: no commits expected
	noCodeNeeded    bool   // agent claims no code changes required
	agentSummary    string // agent's explanation (for no-code-needed verification)
}

// runVerifyPipeline runs the full post-signal verification sequence: commit
// check → test fix-loop → compile fix-loop → LLM verify fix-loop → final
// compile check. Loop owns the retry counters (local variables) and fetches
// fresh HEAD/diff from l.git between fix-agent calls. Verifier provides the
// individual stateless operations (RunTests, CompileCheck, LLMVerify, and
// the Spawn*FixAgent helpers) and owns the start/result logging for each
// of those operations — Loop only emits orchestration-level information
// (retry counters, attempts exhausted, sequencing decisions).
//
// Returns (verified, skipReason). When skipReason is non-empty the caller
// should call l.skipTask(taskID, skipReason) — that action is Loop's, not
// verifier's.
func (l *Loop) runVerifyPipeline(p verifyPipelineInput) (verified bool, skipReason string) {
	// Zero-commit guard: if the agent signaled completion without committing,
	// the task was not worked. First check iteration-local commits; if none,
	// fall back to checking whether prior iterations left commits ahead of
	// origin/main. This prevents stagnation when iteration N commits but
	// exits without signaling, and iteration N+1 signals without new commits.
	// Skipped for no_code_needed — the agent explicitly confirmed no code
	// changes are required.
	if !p.skipCommitCheck {
		commitResult := verify.CheckCommits(p.headBefore, l.git.HeadRev())
		if !commitResult.Passed {
			baseBranch := l.git.DetectDefaultBranch()
			priorCommits := l.git.LogOneline("origin/"+baseBranch, "HEAD")
			if priorCommits == "" {
				l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error}, "No commits found — task was not worked")
				return false, ""
			}
			l.logger.Emit(logging.Opts{Domain: logging.Git}, "No new commits this iteration, but prior-iteration commits found ahead of origin/%s — proceeding", baseBranch)
		}
	}

	// Task context for LLM verification and fix-agent prompts. Pre-fetched
	// once — neither GetDescription nor GetAcceptance change during the
	// verification flow.
	taskDesc := l.taskDescription(p.taskID)
	taskAccept := l.taskAcceptance(p.taskID)

	maxTestFix := l.maxTestFixAttempts()
	maxLLMVerify := l.maxLLMVerifyAttempts()

	spawn := verifier.FixAgentSpawn{
		Ctx:        p.ctx,
		TaskTitle:  p.nextTask,
		WorkDir:    p.workDir,
		RawLogPath: p.rawLogPath,
	}

	// ── Test fix loop ──
	testResult, _ := l.verifier.RunTests(p.ctx, l.cfg.VerifyDir)
	if testResult.ScriptMissing {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "No ralph:verify script — skipping test verification. Add a verify script for stronger guarantees.")
	} else if !testResult.Passed {
		if !l.runTestFixLoop(spawn, taskAccept, testResult.Details, maxTestFix) {
			return false, ""
		}
	}

	// ── Compile fix loop (post-tests) ──
	if l.cfg.VerifyDir != "" {
		if !l.runCompileFixLoop(spawn, taskAccept, maxTestFix) {
			return false, ""
		}
	}

	// ── LLM verification fix loop ──
	verified, skipReason = l.runLLMVerifyFixLoop(p, spawn, taskDesc, taskAccept, maxLLMVerify, maxTestFix)
	if !verified {
		return false, skipReason
	}

	// ── Final compile check ──
	// Fix agents spawned during LLM verification may have introduced new
	// build errors; re-check before accepting the signal.
	if l.cfg.VerifyDir != "" {
		if !l.runCompileFixLoop(spawn, taskAccept, maxTestFix) {
			return false, ""
		}
	}

	return true, ""
}

// runTestFixLoop handles the "tests failed; spawn fix agent; re-run" retry
// cycle. Returns true when tests eventually pass, false when attempts are
// exhausted or the fix agent fails to signal.
func (l *Loop) runTestFixLoop(spawn verifier.FixAgentSpawn, taskAccept, testDetails string, maxAttempts int) bool {
	attempts := 0
	for {
		attempts++
		l.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Tests failed (attempt %d/%d)", attempts, maxAttempts)

		if attempts > maxAttempts {
			l.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "Tests still failing after %d attempts — giving up", maxAttempts)
			return false
		}

		result := l.verifier.SpawnTestFixAgent(spawn, taskAccept, testDetails, attempts, maxAttempts)
		if !result.SignalDetected {
			return false
		}

		l.logger.Emit(logging.Opts{Domain: logging.Test}, "Re-running test suite after test fix agent...")
		rerun, rerunElapsed := l.verifier.RunTests(spawn.Ctx, l.cfg.VerifyDir)
		if rerun.Passed {
			l.logger.Emit(logging.Opts{Domain: logging.Test}, "Tests passed after fix agent (%s)", rerunElapsed)
			return true
		}
		testDetails = rerun.Details
	}
}

// runCompileFixLoop handles the "compile check failed; spawn fix agent;
// re-check" retry cycle. Uses its own attempt counter independent of test
// fix attempts. Returns true when compilation passes, false when attempts
// are exhausted.
func (l *Loop) runCompileFixLoop(spawn verifier.FixAgentSpawn, taskAccept string, maxAttempts int) bool {
	compileResult := l.verifier.CompileCheck(spawn.Ctx, l.cfg.VerifyDir)
	if compileResult.Passed {
		l.logger.Emit(logging.Opts{Domain: logging.Build}, "Compile check passed")
		return true
	}

	attempts := 0
	details := compileResult.Reason
	if compileResult.Details != "" {
		details += "\n" + compileResult.Details
	}
	for {
		attempts++
		if attempts > maxAttempts {
			l.logger.Emit(logging.Opts{Domain: logging.Build, Level: logging.Error}, "Compile check still failing after %d fix attempts — giving up", maxAttempts)
			return false
		}

		result := l.verifier.SpawnCompileFixAgent(spawn, taskAccept, details, attempts, maxAttempts)
		if !result.SignalDetected {
			return false
		}

		recheck := l.verifier.CompileCheck(spawn.Ctx, l.cfg.VerifyDir)
		if recheck.Passed {
			l.logger.Emit(logging.Opts{Domain: logging.Build}, "Compile check passed after fix agent")
			return true
		}
		details = recheck.Reason
		if recheck.Details != "" {
			details += "\n" + recheck.Details
		}
	}
}

// runLLMVerifyFixLoop handles the "LLM rejected; spawn fix agent; re-run
// tests; re-fetch fresh diff; re-verify" cycle. This is where fresh diffs
// matter — after each fix agent commits, the diff grows and the next LLM
// attempt must see the updated version. Loop fetches fresh diffs from
// l.git between attempts.
//
// Returns (verified, skipReason). A non-empty skipReason means the LLM
// exhausted its attempts and the task should be skipped.
func (l *Loop) runLLMVerifyFixLoop(p verifyPipelineInput, spawn verifier.FixAgentSpawn, taskDesc, taskAccept string, maxLLMAttempts, maxTestFix int) (bool, string) {
	attempts := 0
	for {
		attempts++

		diff, diffSource := l.fetchVerifyDiff(p.ctx, p.taskID, p.headBefore)

		llmResult, _ := l.verifier.LLMVerify(verifier.LLMVerifyOpts{
			Ctx:          p.ctx,
			WorkDir:      p.workDir,
			TaskID:       p.taskID,
			Title:        p.nextTask,
			Description:  taskDesc,
			Acceptance:   taskAccept,
			Diff:         diff,
			DiffSource:   diffSource,
			Attempt:      attempts,
			NoCodeNeeded: p.noCodeNeeded,
			AgentSummary: p.agentSummary,
		})

		if llmResult.Passed {
			return true, ""
		}

		if attempts >= maxLLMAttempts {
			if p.taskID != "" {
				return false, fmt.Sprintf("verification_rejected_%d_attempts: %s", maxLLMAttempts, llmResult.Details)
			}
			return false, ""
		}

		result := l.verifier.SpawnVerifyFixAgent(spawn, taskDesc, taskAccept, llmResult.Details, attempts, maxLLMAttempts)
		if !result.SignalDetected {
			return false, ""
		}

		// Re-run tests after fix agent — if the fix agent broke the tests
		// while addressing LLM feedback, verification fails outright (no
		// retry of the test fix loop here).
		l.logger.Emit(logging.Opts{Domain: logging.Test}, "Re-running test suite after fix agent...")
		testResult, testElapsed := l.verifier.RunTests(p.ctx, l.cfg.VerifyDir)
		if !testResult.Passed {
			l.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "Tests failed after fix agent (%s): %s", testElapsed, testResult.Reason)
			return false, ""
		}
		l.logger.Emit(logging.Opts{Domain: logging.Test}, "Tests passed after fix agent (%s)", testElapsed)
	}
}

// fetchVerifyDiff returns the diff and its label to feed the LLM verifier.
// Prefers the PR diff (which covers prior iterations) and falls back to
// the current iteration diff. Returns empty strings when neither is
// available — LLMVerifyPR treats that as a no-op pass.
func (l *Loop) fetchVerifyDiff(ctx context.Context, taskID, headBefore string) (string, string) {
	if diff := l.git.PRDiffForTask(ctx, taskID); diff != "" {
		return diff, "PR"
	}
	if diff := l.git.DiffFull(headBefore, "HEAD"); diff != "" {
		return diff, "iteration"
	}
	return "", ""
}

// taskAcceptance fetches the task acceptance criteria from the configured
// backend, returning empty string when no acceptance exists or the call
// fails. Parallel to l.taskDescription in loop_verify.go.
func (l *Loop) taskAcceptance(taskID string) string {
	if taskID == "" || l.taskBackend == nil {
		return ""
	}
	ac, err := l.taskBackend.GetAcceptance(taskID)
	if err != nil {
		return ""
	}
	return ac
}

// maxTestFixAttempts returns the configured test-fix retry ceiling, with a
// default of 3 when unset.
func (l *Loop) maxTestFixAttempts() int {
	if l.cfg.MaxTestFixAttempts > 0 {
		return l.cfg.MaxTestFixAttempts
	}
	return 3
}

// maxLLMVerifyAttempts returns the configured LLM verify retry ceiling, with
// a default of 3 when unset.
func (l *Loop) maxLLMVerifyAttempts() int {
	if l.cfg.MaxLLMVerifyAttempts > 0 {
		return l.cfg.MaxLLMVerifyAttempts
	}
	return 3
}

// runPreIterationTests runs pre-iteration test and compile checks via the
// verifier and writes last_test_result/last_test_time to state. Verifier
// reports results; Loop writes state. Returns the human-readable status
// message to append to the agent prompt.
func (l *Loop) runPreIterationTests(ctx context.Context) string {
	if ctx.Err() != nil {
		return ""
	}

	result := l.verifier.RunPreIterationTests(verifier.PreIterationInput{Ctx: ctx})
	if result.TestResult.ScriptMissing {
		return result.Message
	}
	if result.TestResult.Passed || !result.TestResult.ScriptMissing {
		now := time.Now().Format(time.RFC3339)
		if result.TestResult.Passed {
			l.state.Write("last_test_result", "pass")
		} else {
			l.state.Write("last_test_result", "fail")
		}
		l.state.Write("last_test_time", now)
	}
	return result.Message
}

// runSimpleVerifyCompletion is the non-fix-loop verification path used by
// verifyCompletion (loop_iteration.go) when l.cfg.OnVerify is not set. It
// runs a commit check, runs the test suite once, and writes last_test_result
// and last_test_time to state. No fix agents, no LLM verification.
func (l *Loop) runSimpleVerifyCompletion(ctx context.Context, headBefore string) (bool, string) {
	if l.cfg.VerifyDir == "" {
		return true, ""
	}

	commitResult := verify.CheckCommits(headBefore, l.git.HeadRev())
	if !commitResult.Passed {
		baseBranch := l.git.DetectDefaultBranch()
		priorCommits := l.git.LogOneline("origin/"+baseBranch, "HEAD")
		if priorCommits == "" {
			return false, commitResult.Reason
		}
		l.logger.Emit(logging.Opts{Domain: logging.Git}, "No new commits this iteration, but prior-iteration commits found ahead of origin/%s — proceeding", baseBranch)
	}

	testResult, _ := l.verifier.RunTests(ctx, l.cfg.VerifyDir)
	now := time.Now().Format(time.RFC3339)
	if !testResult.Passed {
		l.state.Write("last_test_result", "fail")
		l.state.Write("last_test_time", now)
		if testResult.Details != "" {
			l.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "Test output:\n%s", testResult.Details)
		}
		return false, testResult.Reason
	}

	l.state.Write("last_test_result", "pass")
	l.state.Write("last_test_time", now)
	return true, ""
}
