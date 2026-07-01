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
	signalTimeHead  string // HEAD at the moment the agent's complete signal was handled
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
	// Worktree invariant. Every diff/HEAD read in this pipeline (CheckCommits,
	// fetchVerifyDiff's DiffFull/DiffFromBase, the signal-time ancestor guard)
	// resolves against l.git's workDir. When the run is configured to use a
	// per-task worktree (cfg.Dirs.WorkDir is a separate dir, not projectDir)
	// but git operations have fallen back to the project checkout, all of those
	// reads silently reflect the project checkout — whose HEAD is the PREVIOUS
	// task's merged commit, not this task's branch tip — and the verifier is
	// handed the prior task's diff. No internal consistency check can catch
	// this: signalTimeHead and HEAD are both read from the same contaminated
	// workDir, so the ancestor guard below passes trivially. Compare against
	// the configured worktree instead. The inverse (Dirs.WorkDir == projectDir)
	// is legitimate in-place operation and must not trip this guard. Abort as
	// an infrastructure error — no skip, no attempt consumed — rather than
	// false-reject correct work. Observed on ralph-732q: the worktree held the
	// deliverable commit but verification resolved to projectDir and rejected
	// the prior task's diff three times.
	if l.cfg.Dirs.WorkDir != "" && l.cfg.Dirs.WorkDir != l.git.GetProjectDir() && l.git.GetWorkDir() == l.git.GetProjectDir() {
		l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error},
			"Infrastructure error: run is configured for worktree %q but git operations resolve to projectDir %q — aborting verification to avoid verifying a stale project-checkout diff",
			l.cfg.Dirs.WorkDir, l.git.GetProjectDir())
		return false, ""
	}

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

	// ── Test fix loop ──
	if passed, _ := l.runFixLoop(p.ctx, l.testFixPlan(p, taskAccept, maxTestFix)); !passed {
		return false, ""
	}

	// ── Compile fix loop (post-tests) ──
	if p.workDir != "" {
		if passed, _ := l.runFixLoop(p.ctx, l.compileFixPlan(p, taskAccept, maxTestFix)); !passed {
			return false, ""
		}
	}

	// ── Empty-diff guard ──
	// A branch that is ahead of origin/<base> with an empty verify diff signals
	// a tooling fault — unfetched origin, wrong workdir, or base misresolution —
	// not absent work. Passing an empty diff to the LLM verifier would silently
	// accept unverified work; an AC-unmet rejection would false-reject correct
	// work. Both are wrong: abort as an infrastructure error before consuming an
	// attempt. Observed when push/fetch is incomplete and DiffFromBase returns
	// empty even though commits exist on the branch.
	{
		diff, _ := l.fetchVerifyDiff(p.ctx, p.taskID, p.headBefore, p.signalTimeHead)
		if diff == "" {
			baseBranch := l.git.DetectDefaultBranch()
			if l.git.LogOneline("origin/"+baseBranch, "HEAD") != "" {
				l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error},
					"Infrastructure error: verify diff is empty but branch is ahead of origin/%s — unfetched origin, wrong workdir, or base misresolution; aborting verification without consuming an attempt",
					baseBranch)
				return false, ""
			}
		}
	}

	// ── LLM verification fix loop ──
	verified, skipReason = l.runLLMVerifyFixLoop(p, taskDesc, taskAccept, maxLLMVerify, maxTestFix)
	if !verified {
		return false, skipReason
	}

	// ── Final compile check ──
	// Fix agents spawned during LLM verification may have introduced new
	// build errors; re-check before accepting the signal.
	if p.workDir != "" {
		if passed, _ := l.runFixLoop(p.ctx, l.compileFixPlan(p, taskAccept, maxTestFix)); !passed {
			return false, ""
		}
	}

	return true, ""
}

// testCheck is a fixCheck backed by the verifier's test suite. Constructed
// with the Loop it reads (l.verifier is the only module allowed to run
// tests) and the workDir to test in — data-only fields, no callbacks.
type testCheck struct {
	l       *Loop
	workDir string
}

func (c testCheck) name() string { return "tests" }

func (c testCheck) evaluate(ctx context.Context) checkOutcome {
	result, _ := c.l.verifier.RunTests(ctx, c.workDir)
	if result.ScriptMissing {
		c.l.logger.Emit(logging.Opts{Level: logging.Warn}, "No ralph:verify script — skipping test verification. Add a verify script for stronger guarantees.")
		return checkOutcome{Passed: true}
	}
	return checkOutcome{Passed: result.Passed, Failure: result.Details}
}

// compileCheck is a fixCheck backed by the verifier's compile/build check.
type compileCheck struct {
	l       *Loop
	workDir string
}

func (c compileCheck) name() string { return "compile" }

func (c compileCheck) evaluate(ctx context.Context) checkOutcome {
	result := c.l.verifier.CompileCheck(ctx, c.workDir)
	if result.Passed {
		return checkOutcome{Passed: true}
	}
	failure := result.Reason
	if result.Details != "" {
		failure += "\n" + result.Details
	}
	return checkOutcome{Passed: false, Failure: failure}
}

// testFixPlan builds the fixPlan for the "tests failed; spawn fix agent;
// re-run" retry cycle, run through runFixLoop.
func (l *Loop) testFixPlan(p verifyPipelineInput, taskAccept string, maxAttempts int) fixPlan {
	return fixPlan{
		checks: []fixCheck{testCheck{l: l, workDir: p.workDir}},
		spawnVars: map[string]string{
			"{{TASK_TITLE}}":       p.nextTask,
			"{{TASK_DESCRIPTION}}": fmt.Sprintf("Tests failed after completion. Fix the failures.\n\nAcceptance criteria:\n%s", taskAccept),
		},
		spawnTemplate:    "verify-tests.md",
		spawnDescription: "test failures",
		maxAttempts:      maxAttempts,
		exhaustedFormat:  "Tests still failing after %d attempts",
		workDir:          p.workDir,
		rawLogPath:       p.rawLogPath,
		signalTimeHead:   p.signalTimeHead,
		logDomain:        logging.Test,
	}
}

// compileFixPlan builds the fixPlan for the "compile check failed; spawn
// fix agent; re-check" retry cycle, run through runFixLoop. Uses the same
// maxAttempts ceiling as testFixPlan but its own independent counter,
// since runFixLoop is called separately for each plan.
func (l *Loop) compileFixPlan(p verifyPipelineInput, taskAccept string, maxAttempts int) fixPlan {
	return fixPlan{
		checks: []fixCheck{compileCheck{l: l, workDir: p.workDir}},
		spawnVars: map[string]string{
			"{{TASK_TITLE}}":       p.nextTask,
			"{{TASK_DESCRIPTION}}": fmt.Sprintf("Build/type check failed after completion. Fix the compile errors.\n\nAcceptance criteria:\n%s", taskAccept),
		},
		spawnTemplate:    "verify-tests.md",
		spawnDescription: "build errors",
		maxAttempts:      maxAttempts,
		exhaustedFormat:  "Compile check still failing after %d fix attempts",
		workDir:          p.workDir,
		rawLogPath:       p.rawLogPath,
		signalTimeHead:   p.signalTimeHead,
		logDomain:        logging.Build,
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
func (l *Loop) runLLMVerifyFixLoop(p verifyPipelineInput, taskDesc, taskAccept string, maxLLMAttempts, maxTestFix int) (bool, string) {
	attempts := 0
	for {
		// Assert signal-time HEAD is still reachable before consuming an attempt.
		// A worktree reset drops the agent's commit without incrementing the
		// rejection counter — treat it as an infrastructure failure, not a
		// verification rejection.
		if p.signalTimeHead != "" && !l.git.IsCommitAncestorOf(p.signalTimeHead, "HEAD") {
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error},
				"Infrastructure error: signal-time commit %s is not an ancestor of HEAD — worktree may have been reset; aborting verification without consuming an attempt",
				p.signalTimeHead)
			return false, ""
		}

		attempts++

		diff, diffSource := l.fetchVerifyDiff(p.ctx, p.taskID, p.headBefore, p.signalTimeHead)

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

		result := l.verifier.SpawnFixAgent(verifier.FixAgentInput{
			Ctx:      p.ctx,
			Template: "verify-fix.md",
			Vars: map[string]string{
				"{{TASK_TITLE}}":          p.nextTask,
				"{{TASK_DESCRIPTION}}":    taskDesc,
				"{{ACCEPTANCE_CRITERIA}}": taskAccept,
				"{{REJECTION_REASON}}":    llmResult.Details,
			},
			Attempt:     attempts,
			WorkDir:     p.workDir,
			RawLogPath:  p.rawLogPath,
			Description: "verification rejection",
		})
		if !result.SignalDetected {
			return false, ""
		}

		// Re-run tests after fix agent — if the fix agent broke the tests
		// while addressing LLM feedback, verification fails outright (no
		// retry of the test fix loop here).
		l.logger.Emit(logging.Opts{Domain: logging.Test}, "Re-running test suite after fix agent...")
		testResult, testElapsed := l.verifier.RunTests(p.ctx, p.workDir)
		if !testResult.Passed {
			l.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Error}, "Tests failed after fix agent (%s): %s", testElapsed, testResult.Reason)
			return false, ""
		}
		l.logger.Emit(logging.Opts{Domain: logging.Test}, "Tests passed after fix agent (%s)", testElapsed)
	}
}

// fetchVerifyDiff returns the diff and its label to feed the LLM verifier.
//
// The diff is read from LOCAL git, not GitHub. The loop just produced this
// task's commits, so the authoritative record of the work is already in the
// worktree — there is no reason to re-fetch it over the network, and (crucially)
// no reason to identify a PR by search. An earlier design preferred a PR diff
// located by full-text searching GitHub for the task ID; that search could
// return an unrelated PR (e.g. a dependency PR that merely mentions the task),
// feeding the verifier the wrong diff and stalling correct work until the loop
// halted on stagnation. PR identity now comes only from the exact external-ref
// stored on the task — a 1:1 pointer, never a search.
//
// Order:
//  1. DiffFromBase — three-dot origin/<base>...HEAD. Diffs against the
//     merge-base, so it covers all of this branch's iterations and excludes
//     commits that landed on the base after the branch diverged. This is the
//     primary, exact source.
//  2. PR diff via external-ref — resume recovery only. If the local branch has
//     no commits ahead of the base (e.g. a freshly recreated worktree that has
//     not re-fetched the task's commits) and a prior run already pushed a PR,
//     fetch that PR's diff using the exact ref stored on the task.
//  3. Iteration-local headBefore..HEAD — last resort.
//
// Returns empty strings when none are available — the verifier treats that as a
// no-op pass.
func (l *Loop) fetchVerifyDiff(ctx context.Context, taskID, headBefore, signalTimeHead string) (string, string) {
	if diff := l.git.DiffFromBase(); diff != "" {
		l.logger.Emit(logging.Opts{Domain: logging.Test}, "Verify diff: origin/%s...HEAD in %s (branch)", l.git.DetectDefaultBranch(), l.git.GetWorkDir())
		return diff, "branch"
	}
	// Resume recovery: the local branch is not ahead of the base, but a prior
	// run may have pushed the work as a PR. Resolve that PR only from the exact
	// external-ref — no search, so only this task's own PR can match.
	if l.taskBackend != nil && taskID != "" {
		if ref, _ := l.taskBackend.GetExternalRef(taskID); ref != "" {
			if diff := l.git.PRDiffForRef(ctx, ref); diff != "" {
				l.logger.Emit(logging.Opts{Domain: logging.Test}, "Verify diff: PR from external-ref %s (resume recovery)", ref)
				return diff, "PR"
			}
		}
	}
	// Last resort: iteration-local headBefore..HEAD.
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

	result := l.verifier.RunPreIterationTests(verifier.PreIterationInput{Ctx: ctx, WorkDir: l.git.GetWorkDir()})
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
	workDir := l.git.GetWorkDir()
	if workDir == "" {
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

	testResult, _ := l.verifier.RunTests(ctx, workDir)
	if testResult.ScriptMissing {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "No ralph:verify script — skipping test verification. Add a verify script for stronger guarantees.")
		return true, ""
	}
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
