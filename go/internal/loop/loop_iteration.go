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
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// completeTaskParams bundles the signal data and config for the post-signal
// pipeline. All fields are data only — no module references, no interfaces,
// no func types.
type completeTaskParams struct {
	// signal data
	result     claude.Result
	headBefore string
	workDir    string
	rawLogPath string
	diffStat   string
	taskID     string
	nextTask   string
	// config
	postSignalTimeout time.Duration
	evolve            bool
	notify            bool
	ralphDir          string
}

// completeTaskOut carries the results of completeTask back to Run().
type completeTaskOut struct {
	action postSignalAction
	ct     *CompletedTask
	merged bool
}

// postSignalAction describes the outcome of post-signal processing.
type postSignalAction int

const (
	signalComplete postSignalAction = iota // task done, fall through to tagTaskEnd
	signalRetry                            // verification failed, skip tagTaskEnd
	signalSkipped                          // no new commits, already handled
	signalEvolve                           // evolve restart, caller returns nil
)

// postSignalParams bundles the context needed for post-signal processing.
type postSignalParams struct {
	ctx        context.Context
	result     claude.Result
	headBefore string
	workDir    string
	rawLogPath string
	taskID     string
	nextTask   string
	diffStat   string
}

// completeTask runs after the agent signals completion: verifies the work,
// ships a PR, merges if configured, and closes the bead.
func (l *Loop) completeTask(ctx context.Context, p completeTaskParams) completeTaskOut {
	if p.postSignalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.postSignalTimeout)
		defer cancel()
	}

	// Watch for feedback file and cancel context when it appears.
	if p.ralphDir != "" {
		feedbackFile := filepath.Join(p.ralphDir, "feedback")
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		ticker := time.NewTicker(500 * time.Millisecond)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := os.Stat(feedbackFile); err == nil {
						os.Remove(feedbackFile)
						l.logger.Emit(logging.Opts{Domain: logging.Git}, "Feedback signal detected during post-signal pipeline — cancelling")
						cancel()
						return
					}
				}
			}
		}()
		defer func() {
			cancel()
			ticker.Stop()
			<-done
		}()
	}

	// Guard: if the task was already skipped during verification (e.g. 3
	// rejected attempts), do not push or merge the rejected work.
	if p.taskID != "" {
		skipped, err := l.state.GetSkippedTasks()
		if err != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Failed to load skipped tasks for %s: %v — conservatively not pushing", p.taskID, err)
			return completeTaskOut{action: signalSkipped}
		}
		for _, id := range skipped {
			if id == p.taskID {
				l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task %s was skipped during verification — not pushing", p.taskID)
				return completeTaskOut{action: signalSkipped}
			}
		}
	}

	// Preflight: check bead wasn't prematurely closed by the agent.
	if p.taskID != "" {
		phase, _ := l.taskBackend.GetState(p.taskID, "phase")
		if phase != "implementing" {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task %s phase is %q (expected implementing) — agent may have tampered with task state", p.taskID, phase)
		}
	}

	// NoCodeNeeded: agent confirmed investigation found no code changes
	// required (already fixed, not a bug, etc.). The no_code_needed signal
	// bypasses OnSignal (detected post-exit), so run the verify pipeline
	// here with the commit check skipped — tests and LLM verification
	// still gate the close.
	if p.result.NoCodeNeeded {
		l.logger.Emit(logging.Opts{Domain: logging.Beads}, "No code changes needed — verifying before close")
		verified, skipReason := l.runVerifyPipeline(verifyPipelineInput{
			ctx:             ctx,
			headBefore:      p.headBefore,
			workDir:         p.workDir,
			rawLogPath:      p.rawLogPath,
			taskID:          p.taskID,
			nextTask:        p.nextTask,
			skipCommitCheck: true,
			noCodeNeeded:    true,
			agentSummary:    p.result.Summary,
		})
		if !verified {
			if skipReason != "" {
				l.skipTask(p.taskID, skipReason)
			}
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Verification failed for no-code-needed claim — retrying")
			l.attempts.Record(p.taskID, p.nextTask,
				"Agent claimed no code needed but verification failed",
				p.diffStat,
				"no_code_needed_rejected: verifier did not confirm the claim")
			return completeTaskOut{action: signalRetry}
		}
	} else if !p.result.OnSignalUsed {
		// If OnSignal was set, verification already passed in the runner.
		// If not (legacy/test path), run verification here as fallback.
		if passed, reason := l.verifyCompletion(ctx, p.headBefore); !passed {
			l.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Verification failed: %s", reason)
			l.attempts.Record(p.taskID, p.nextTask,
				"Signal received but verification failed: "+reason,
				p.diffStat,
				"verification_failed: fix must pass tests and produce commits before closing")
			return completeTaskOut{action: signalRetry}
		}
	}

	if p.taskID != "" {
		stateReason := "ralph: tests passed, commits present"
		if p.result.NoCodeNeeded {
			stateReason = "ralph: no code needed — verifier confirmed"
		}
		if err := l.taskBackend.SetState(p.taskID, "phase", "verified", stateReason); err != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "SetState phase=verified: %v", err)
		} else {
			l.logger.Emit(logging.Opts{Domain: logging.Beads}, "%s → verified", p.taskID)
		}
	}

	l.attempts.Clear(p.taskID, p.nextTask)
	l.state.RecordCompletedTask(p.taskID, p.nextTask)
	l.state.TouchPlanFlash()

	headAfterSignal := l.git.HeadRev()
	hasPriorIterationCommits := false
	if p.headBefore != "" && headAfterSignal == p.headBefore {
		// No new commits this iteration. Check if prior iterations left
		// commits ahead of origin/main — if so, fall through to Ship
		// instead of closing without a PR.
		baseBranch := l.git.DetectDefaultBranch()
		if priorLog := l.git.LogOneline("origin/"+baseBranch, "HEAD"); priorLog != "" {
			l.logger.Emit(logging.Opts{Domain: logging.Git}, "No new commits this iteration, but prior-iteration commits ahead of origin/%s — routing through Ship", baseBranch)
			hasPriorIterationCommits = true
		}
	}
	if p.headBefore != "" && headAfterSignal == p.headBefore && !hasPriorIterationCommits {
		// No new commits and no prior-iteration commits — verification passed
		// but there's nothing to push.
		l.logger.Emit(logging.Opts{Domain: logging.Git}, "No new commits — verified complete")

		// Check for an existing PR from a prior attempt that still needs merging.
		// Try external ref first, then fall back to branch-based PR discovery.
		if p.taskID != "" {
			var prNum int
			if ref, _ := l.taskBackend.GetExternalRef(p.taskID); ref != "" {
				prNum = parsePRNumber(ref)
			}
			if prNum == 0 {
				if n, _, _, err := l.git.FindPRForBranch(l.git.GetWorktreeBranch()); err == nil && n != 0 {
					prNum = n
				}
			}
			if prNum != 0 {
				prState, _ := l.git.GetPRState(prNum)
				if prState == git.PRStateOpen {
					l.logger.Emit(logging.Opts{Domain: logging.Git}, "Found open PR #%d from prior attempt — routing through merge", prNum)
					_, _, merged, _, _, _, _ := l.doShip(ctx, p.taskID, p.nextTask, p.result.Summary, p.rawLogPath, p.workDir)
					prRef := fmt.Sprintf("PR #%d", prNum)
					closeReason := fmt.Sprintf("Verified — %s open, merge pending", prRef)
					if merged {
						closeReason = fmt.Sprintf("Fixed in %s", prRef)
					}
					_ = l.taskBackend.CloseTask(p.taskID, closeReason)
					l.git.TagTaskEnd(p.taskID)
					l.execRunPostTask(ctx, p.taskID, prNum, merged)
					if p.notify {
						notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary)
						if merged {
							notify.TaskMerged(p.taskID, p.nextTask)
						}
					}
					if p.evolve {
						return completeTaskOut{action: l.maybeEvolve(signalSkipped), merged: merged}
					}
					return completeTaskOut{action: signalSkipped, merged: merged}
				}
				if prState == git.PRStateMerged {
					l.logger.Emit(logging.Opts{Domain: logging.Git}, "PR #%d already merged — closing bead", prNum)
					prRef := fmt.Sprintf("PR #%d", prNum)
					closeReason := fmt.Sprintf("Fixed in %s (already merged)", prRef)
					_ = l.taskBackend.CloseTask(p.taskID, closeReason)
					l.persistCompleted(p.taskID, true)
					l.git.TagTaskEnd(p.taskID)
					l.execRunPostTask(ctx, p.taskID, prNum, true)
					if p.notify {
						notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary)
						notify.TaskMerged(p.taskID, p.nextTask)
					}
					if p.evolve {
						return completeTaskOut{action: l.maybeEvolve(signalSkipped), merged: true}
					}
					return completeTaskOut{action: signalSkipped, merged: true}
				}
			}
		}

		// No existing PR to merge — close the bead directly.
		l.logger.Emit(logging.Opts{Domain: logging.Git}, "Closing bead (no PR to merge)")
		if p.taskID != "" {
			if ctx.Err() != nil {
				l.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
				return completeTaskOut{action: signalComplete}
			}
			closeReason := "verified complete (no new commits)"
			if err := l.taskBackend.CloseTask(p.taskID, closeReason); err != nil {
				skipReason := "close_failed"
				if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
					l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %s blocked by %v", p.taskID, blockers)
					skipReason = fmt.Sprintf("dependency_blocked_by:%s", strings.Join(blockers, ","))
				} else {
					l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %v", err)
				}
				l.skipTask(p.taskID, skipReason)
			} else {
				l.logger.Emit(logging.Opts{Domain: logging.Beads}, "Closed task %s (%s)", p.taskID, closeReason)
				l.persistCompleted(p.taskID, false)
			}
		}
		l.git.TagTaskEnd(p.taskID)
		l.execRunPostTask(ctx, p.taskID, 0, false)
		if p.notify {
			notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary)
		}
		if p.evolve {
			return completeTaskOut{action: l.maybeEvolve(signalSkipped)}
		}
		return completeTaskOut{action: signalSkipped}
	}

	if ctx.Err() != nil {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "Post-signal timeout — aborting before push")
		return completeTaskOut{action: signalComplete}
	}

	prNumber, shipURL, merged, ciFailure, ciInfraFailure, stacked, pushedBranch := l.doShip(ctx, p.taskID, p.nextTask, p.result.Summary, p.rawLogPath, p.workDir)

	// Record every successful push in chronological order so completedBranches()
	// can build the correct stack for the next iteration. This captures both
	// the normal ship path (PR created) and the pr_creation_failed path (push
	// succeeded but PR creation errored). Ordering matters: setStackHead walks
	// newest-first so the most recently pushed branch becomes the stack parent.
	if pushedBranch != "" {
		if err := l.state.AddPushedBranch(pushedBranch); err != nil {
			l.logger.Emit(logging.Opts{Domain: "state", Level: logging.Warn}, "AddPushedBranch: %v", err)
		}
	}

	// Recovery: if ship didn't produce a PR, find any existing PR in any state.
	if prNumber == 0 && p.taskID != "" {
		if ref, _ := l.taskBackend.GetExternalRef(p.taskID); ref != "" {
			if n := parsePRNumber(ref); n != 0 {
				prNumber = n
			}
		}
		if prNumber == 0 {
			if n, _, _, err := l.git.FindPRForBranch(l.git.GetWorktreeBranch()); err == nil && n != 0 {
				prNumber = n
			}
		}
	}

	ct := l.buildCompletedTask(p.taskID, p.nextTask, p.result.Summary, prNumber)
	if shipURL != "" {
		ct.PRURL = shipURL
	}

	// buildCompletedTask may discover a PR that recovery missed.
	if prNumber == 0 && ct.PRNum != 0 {
		prNumber = ct.PRNum
	}

	if ctx.Err() != nil {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "Post-signal timeout — aborting before merge")
		return completeTaskOut{action: signalComplete}
	}

	if prNumber == 0 {
		// Two distinct sub-cases are collapsed into prNumber==0:
		//
		//   (a) pushedBranch == "": nothing was pushed to the remote. The
		//       agent signalled with no new commits and no prior-iteration
		//       commits to ship. Closing the bead is correct — work is
		//       verified complete.
		//
		//   (b) pushedBranch != "": the Phase 1 push succeeded but CreatePR
		//       failed (rate limit, 422, network). Commits are now on the
		//       remote branch. Closing the bead would orphan that branch —
		//       bead tracking loses the link to the work that exists on
		//       origin. Skip instead, encoding the branch in the skip reason
		//       so triage can rediscover the orphaned branch.
		if pushedBranch != "" {
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
				"No PR created but branch %s was pushed — skipping bead %s (CreatePR failed; work lives on remote)",
				pushedBranch, p.taskID)
			if p.taskID != "" {
				if ctx.Err() != nil {
					l.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
					return completeTaskOut{action: signalComplete}
				}
				// Machine-parseable reason: "pr_creation_failed:<branch>".
				// Downstream triage (and the task-manager prompt) parses this
				// to locate orphaned remote branches whose PRs failed to create.
				l.skipTask(p.taskID, fmt.Sprintf("pr_creation_failed:%s", pushedBranch))
			}
			return completeTaskOut{action: signalSkipped}
		}

		l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "No PR created — closing bead for task %s", p.taskID)
		if p.taskID != "" {
			if ctx.Err() != nil {
				l.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
				return completeTaskOut{action: signalComplete}
			}
			branch := l.git.GetWorktreeBranch()
			closeReason := "Verified — no PR created"
			if branch != "" {
				closeReason = fmt.Sprintf("Verified — branch %s, no PR", branch)
			}
			if err := l.taskBackend.CloseTask(p.taskID, closeReason); err != nil {
				l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %v", err)
			}
		}
		return completeTaskOut{action: signalComplete, ct: &ct}
	}

	// CI is failing — decide whether to close or leave open based on failure type.
	if ciFailure {
		if ciInfraFailure && p.taskID != "" {
			// Infrastructure failure (zero job steps): work is verified locally,
			// CI never ran due to billing/runner issues. Close the bead and leave
			// the PR open — it will merge when CI infrastructure recovers.
			if ctx.Err() != nil {
				l.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
				return completeTaskOut{action: signalComplete}
			}
			prRef := ct.PRURL
			if prRef == "" {
				prRef = fmt.Sprintf("PR #%d", prNumber)
			}
			closeReason := fmt.Sprintf("Verified — %s open, merge pending CI recovery", prRef)
			if err := l.taskBackend.CloseTask(p.taskID, closeReason); err != nil {
				skipReason := "close_failed"
				if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
					l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %s blocked by %v", p.taskID, blockers)
					skipReason = fmt.Sprintf("dependency_blocked_by:%s", strings.Join(blockers, ","))
				} else {
					l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask failed: %v", err)
				}
				l.skipTask(p.taskID, skipReason)
			} else {
				l.logger.Emit(logging.Opts{Domain: logging.Beads}, "Closed task %s (%s)", p.taskID, closeReason)
				l.persistCompleted(p.taskID, false)
			}
			l.git.TagTaskEnd(p.taskID)
			l.execRunPostTask(ctx, p.taskID, prNumber, false)
			return completeTaskOut{action: signalComplete, ct: &ct}
		}
		// Actual test failures — leave task open for manual investigation.
		l.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Error}, "CI failing on PR #%d — leaving task %s open.", prNumber, p.taskID)
		l.git.TagTaskEnd(p.taskID)
		l.execRunPostTask(ctx, p.taskID, prNumber, false)
		return completeTaskOut{action: signalComplete}
	}

	// Close the task based on merge outcome.
	if p.taskID != "" {
		if ctx.Err() != nil {
			l.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
			return completeTaskOut{action: signalComplete}
		}
		prRef := ct.PRURL
		if prRef == "" {
			prRef = fmt.Sprintf("PR #%d", prNumber)
		}
		var closeReason string
		if stacked {
			closeReason = fmt.Sprintf("Verified — %s open, merge pending", prRef)
		} else if !merged {
			closeReason = fmt.Sprintf("Verified — %s open, merge pending", prRef)
		} else {
			closeReason = fmt.Sprintf("Fixed in %s", prRef)
		}
		if err := l.taskBackend.CloseTask(p.taskID, closeReason); err != nil {
			skipReason := "close_failed"
			if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
				l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %s blocked by %v", p.taskID, blockers)
				skipReason = fmt.Sprintf("dependency_blocked_by:%s", strings.Join(blockers, ","))
			} else {
				l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask failed: %v", err)
			}
			l.skipTask(p.taskID, skipReason)
		} else {
			l.logger.Emit(logging.Opts{Domain: logging.Beads}, "Closed task %s (%s)", p.taskID, closeReason)
			l.persistCompleted(p.taskID, merged)
		}
	}

	l.execRunPostTask(ctx, p.taskID, prNumber, merged)

	if p.notify {
		notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary)
	}

	if merged {
		notify.TaskMerged(p.taskID, p.nextTask)
	}

	if p.evolve {
		if l.maybeEvolve(signalComplete) == signalEvolve {
			l.git.TagTaskEnd(p.taskID)
			return completeTaskOut{action: signalEvolve, ct: &ct, merged: merged}
		}
	}

	return completeTaskOut{action: signalComplete, ct: &ct, merged: merged}
}

// verifyCompletion delegates to the VerifyHook when set, otherwise runs
// the standard simple-path verification (commit check + single test run +
// state write). This is the non-fix-loop variant used outside the
// post-signal pipeline.
func (l *Loop) verifyCompletion(ctx context.Context, headBefore string) (bool, string) {
	if l.verifyHook != nil {
		return l.verifyHook.Verify(ctx, l.git.GetWorkDir(), headBefore)
	}
	return l.runSimpleVerifyCompletion(ctx, headBefore)
}

// persistCompleted records a completed task in the persistent state store.
func (l *Loop) persistCompleted(taskID string, merged bool) {
	if taskID == "" {
		return
	}
	if err := l.state.AddCompletedTask(taskID, merged); err != nil {
		l.logger.Emit(logging.Opts{Domain: "state", Level: logging.Warn}, "AddCompletedTask: %v", err)
	}
}

// execRunPostTask runs the configured post-task hook after a task completes.
// When PostTaskHook is set (test path), it fires; otherwise the production
// runPostTask script path runs.
func (l *Loop) execRunPostTask(ctx context.Context, taskID string, prNumber int, merged bool) {
	if l.postTaskHook != nil {
		l.postTaskHook.OnPostTask(ctx, taskID, prNumber, merged)
		return
	}
	runPostTask(ctx, runPostTaskParams{
		postTask:    l.cfg.PostTask,
		worktreeDir: l.cfg.VerifyDir,
		projectDir:  l.cfg.Dirs.ProjectDir,
		logger:      l.logger,
	}, taskID, prNumber, merged)
}

// buildCompletedTask assembles the CompletedTask record for a signal.
func (l *Loop) buildCompletedTask(taskID, nextTask, summary string, prNumber int) CompletedTask {
	ct := CompletedTask{
		ID:      taskID,
		Title:   nextTask,
		Summary: summary,
		PRNum:   prNumber,
	}
	if num, t, u, err := l.git.FindPRForBranch(l.git.GetWorktreeBranch()); err == nil && num != 0 {
		ct.PRNum = num
		ct.PRTitle = t
		ct.PRURL = u
	} else if prNumber != 0 {
		ct.PRNum = prNumber
	}
	return ct
}

// iterationPrompt holds the prepared prompt and context needed to invoke Claude.
type iterationPrompt struct {
	fullPrompt string
	headBefore string
	diffBefore bool // true if a diff already existed when this iteration started
	rawLogPath string
	logStart   int
	workDir    string
}

// prepareAndBuildPrompt sets the task phase, runs pre-iteration tests, reads
// feedback, assembles attempt context, and builds the full prompt. Returns
// false if Run() should break (internet or rate limit unavailable).
func (l *Loop) prepareAndBuildPrompt(ctx context.Context, taskID, nextTask string) (iterationPrompt, bool) {
	if taskID != "" {
		if err := l.taskBackend.SetState(taskID, "phase", "implementing", "ralph: starting task"); err != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "SetState phase=implementing: %v", err)
		}
	}

	ralphDir := l.cfg.Dirs.RalphDir

	taskPrompt := l.buildTaskPrompt(nextTask, taskID)
	buildStatus := runVerifyBuild(ctx, runVerifyBuildParams{
		verifyBuild: l.cfg.VerifyBuild,
		projectDir:  l.cfg.Dirs.ProjectDir,
		testTimeout: l.cfg.TestTimeout,
		logger:      l.logger,
	})
	testStatus := buildStatus + l.runPreIterationTests(ctx)

	if !l.connectivity.WaitForInternet(ctx, l.logger) {
		return iterationPrompt{}, false
	}
	if !l.waitForRate(ctx) {
		return iterationPrompt{}, false
	}

	headBefore := l.git.HeadRev()
	diffBefore := l.git.HasDiff()
	rawLogPath := filepath.Join(ralphDir, "raw.log")
	logStart := fileLineCount(rawLogPath)

	attemptContext := l.attemptContext(taskID, nextTask)
	if attemptContext != "" {
		attemptCount := strings.Count(attemptContext, "### Attempt ")
		reflectionCount := strings.Count(attemptContext, "## Recent learnings")
		if attemptCount > 0 || reflectionCount > 0 {
			var parts []string
			if attemptCount > 0 {
				parts = append(parts, fmt.Sprintf("%d prior attempt(s)", attemptCount))
			}
			if reflectionCount > 0 {
				parts = append(parts, "learnings from other tasks")
			}
			l.logger.Emit(logging.Opts{}, "Including %s", strings.Join(parts, " + "))
		}
	}

	fullPrompt, err := l.buildPrompt(taskPrompt, attemptContext, testStatus)
	if err != nil {
		l.logger.Emit(logging.Opts{Level: logging.Error}, "Prompt build failed: %v", err)
		return iterationPrompt{}, false
	}

	return iterationPrompt{
		fullPrompt: fullPrompt,
		headBefore: headBefore,
		diffBefore: diffBefore,
		rawLogPath: rawLogPath,
		logStart:   logStart,
		workDir:    l.git.GetWorkDir(),
	}, true
}

// handleRunResult processes errors and retryable conditions from a Claude
// run (offline, feedback kill, idle timeout, rate limit). Returns the
// loopAction Run() should take. When actionRetry is returned, the caller
// is responsible for not counting this iteration.
func (l *Loop) handleRunResult(ctx context.Context, result claude.Result, runErr error, taskID, nextTask, headBefore string, runIteration int) loopAction {
	if runErr != nil {
		if !l.connectivity.IsOnline() {
			l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.Model}, "Claude failed — internet appears down")
			if !l.connectivity.WaitForInternet(ctx, l.logger) {
				return actionDone
			}
			return actionRetry
		}
		l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.Model}, "Claude failed on iteration %d, continuing...", runIteration)
	}
	if result.FeedbackKill {
		l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.Model}, "Restarting iteration %d — user feedback received", runIteration)
		diffStat := l.git.DiffStatRange(headBefore, l.git.HeadRev())
		l.attempts.Record(taskID, nextTask,
			"Killed: user feedback received (see bead notes for content)",
			diffStat,
			"user_feedback: check bead notes for details")
		return actionRetry
	}
	if result.IdleTimeout {
		l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.Model}, "Restarting iteration %d after idle timeout", runIteration)
		diffStat := l.git.DiffStatRange(headBefore, l.git.HeadRev())
		l.attempts.Record(taskID, nextTask,
			"Killed: idle timeout (no output for configured duration)",
			diffStat,
			"idle_timeout: consider a lighter approach or make incremental progress rather than deep-thinking without output")
		count, _ := l.attempts.RecordIdleTimeoutFailure(taskID)
		if count >= l.attempts.MaxIdleTimeoutFailures() {
			l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.Model}, "Idle timeout %d times for %s — skipping task", count, taskID)
			l.skipTask(taskID, "idle_timeout_max_failures")
			return actionRetry
		}
		return actionRetry
	}
	if result.RateLimited {
		waitDur := claude.FormatWaitDuration(time.Until(result.ResetAt))
		l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.Model}, "Claude rate limit — waiting %s until %s", waitDur, result.ResetAt.Format("3:04pm"))
		err := l.limiter.WaitUntil(ctx, result.ResetAt, func(secs int) {
			l.logger.Emit(logging.Opts{Domain: logging.LLM, Model: l.cfg.Model}, "Rate limit: %ds until reset", secs)
		})
		if err != nil {
			l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.Model}, "Rate limit wait interrupted: %v", err)
			return actionDone
		}
		l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: l.cfg.Model}, "Rate limit reset — resuming")
		return actionRetry
	}

	return actionProceed
}
