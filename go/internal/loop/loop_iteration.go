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
	"github.com/brokenalarms/ralph/internal/verify"
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
	notify   bool
	ralphDir string
}

// completeTaskOut carries the results of completeTask back to Run().
type completeTaskOut struct {
	action   postSignalAction
	ct       *CompletedTask
	merged   bool
	prNumber int
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
	// No iteration-wide deadline is applied here. Post-signal work legitimately
	// chains an agent at every stage (verify → CI fix → conflict → review →
	// merge); a shared wall-clock would guillotine a healthy, progressing agent
	// mid-run when the cumulative time exceeds an arbitrary cap. Each agent is
	// already bounded independently inside claude.go by its idle timeout
	// (IdleTimeout / IdleTimeoutProgress) and a hard per-agent wall-clock
	// (Timeouts.MaxRun). The parent ctx remains cancel-only
	// (Ctrl-C / feedback file).

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

	// Guard: if the task was skipped during verification (e.g. 3 rejected
	// attempts), do not push or merge the rejected work.
	if p.taskID != "" && l.sessionSkippedIDs[p.taskID] {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task %s was skipped during verification — not pushing", p.taskID)
		return completeTaskOut{action: signalSkipped}
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
				l.skipTask(p.taskID, tasks.SkipVerificationRejected, skipReason)
			}
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Verification failed for no-code-needed claim — retrying")
			l.recordAttempt(AttemptEvent{
				Summary:  "Agent claimed no code needed but verification failed",
				DiffStat: p.diffStat,
				Analysis: "no_code_needed_rejected: verifier did not confirm the claim",
			})
			l.releaseClaimForRetry(p.taskID)
			return completeTaskOut{action: signalRetry}
		}
	} else if !p.result.OnSignalUsed {
		// If OnSignal was set, verification already passed in the runner.
		// If not (legacy/test path), run verification here as fallback.
		if passed, reason := l.verifyCompletion(ctx, p.headBefore); !passed {
			l.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Verification failed: %s", reason)
			l.recordAttempt(AttemptEvent{
				Summary:  "Signal received but verification failed: " + reason,
				DiffStat: p.diffStat,
				Analysis: "verification_failed: fix must pass tests and produce commits before closing",
			})
			l.releaseClaimForRetry(p.taskID)
			return completeTaskOut{action: signalRetry}
		}
	}

	l.clearAttempts()
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
				if n, _, _, err := l.git.FindPRForBranch(ctx, l.git.GetWorktreeBranch()); err == nil && n != 0 {
					prNum = n
				}
			}
			if prNum != 0 {
				prState, _ := l.git.GetPRState(ctx, prNum)
				if prState == git.PRStateOpen {
					// Existing open PR with no new commits: route through the
					// single ship/finalize path rather than re-implementing a
					// doShip + close here. The old inline version discarded
					// doShip's ciFailure return and closed as "merge pending"
					// even when required CI was red (the third instance of the
					// close-on-failing-CI bug). shipAndFinalize → doShip handles
					// CI failure (fix agent + retry), merge, and close correctly.
					l.logger.Emit(logging.Opts{Domain: logging.Git}, "Found open PR #%d from prior attempt — routing through shipAndFinalize", prNum)
					return l.shipAndFinalize(ctx, p)
				}
				if prState == git.PRStateMerged {
					l.logger.Emit(logging.Opts{Domain: logging.Git}, "PR #%d already merged — closing bead", prNum)
					prRef := fmt.Sprintf("PR #%d", prNum)
					closeReason := fmt.Sprintf("Fixed in %s (already merged)", prRef)
					_ = l.taskBackend.CloseTask(p.taskID, closeReason)
					l.persistCompleted(p.taskID, true)
					l.state.ClearCurrentTask()
					l.git.TagTaskEnd(p.taskID)
					if p.notify {
						notify.TaskMerged(p.taskID, p.nextTask, time.Now())
					}
					return completeTaskOut{action: signalSkipped, merged: true, prNumber: prNum}
				}
			}
		}

		// No existing PR to merge — close the bead directly.
		l.logger.Emit(logging.Opts{Domain: logging.Git}, "Closing bead (no PR to merge)")
		if p.taskID != "" {
			if ctx.Err() != nil {
				l.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
				l.setPhaseInterrupted(p.taskID)
				return completeTaskOut{action: signalComplete}
			}
			closeReason := "verified complete (no new commits)"
			if err := l.taskBackend.CloseTask(p.taskID, closeReason); err != nil {
				skipReason := tasks.SkipCloseFailed
				detail := ""
				if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
					l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %s blocked by %v", p.taskID, blockers)
					skipReason = tasks.SkipDependencyBlocked
					detail = strings.Join(blockers, ",")
				} else {
					l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %v", err)
				}
				l.skipTask(p.taskID, skipReason, detail)
			} else {
				l.logger.Emit(logging.Opts{Domain: logging.Beads}, "Closed task %s (%s)", p.taskID, closeReason)
				l.persistCompleted(p.taskID, false)
				l.state.ClearCurrentTask()
			}
		}
		l.git.TagTaskEnd(p.taskID)
		if p.notify {
			notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary, time.Now())
		}
		return completeTaskOut{action: signalSkipped}
	}

	return l.shipAndFinalize(ctx, p)
}

// shipAndFinalize is the single source of truth for shipping a task's PR and
// finalizing the bead. It pushes / creates / merges the PR, routes genuine CI
// failures to the fix agent, handles infrastructure failures, conflicts,
// reviews, and stacked PRs, then closes (or leaves open) the bead based on the
// outcome. completeTask (after verification) funnels through here; the resume
// path does too, so the ship / merge / close decision lives in exactly one
// place rather than being re-implemented per entry point.
func (l *Loop) shipAndFinalize(ctx context.Context, p completeTaskParams) completeTaskOut {
	if ctx.Err() != nil {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "Post-signal timeout — aborting before push")
		l.setPhaseInterrupted(p.taskID)
		return completeTaskOut{action: signalComplete}
	}

	prNumber, shipURL, merged, ciFailure, ciInfraFailure, stacked, pushedBranch, shipErr := l.doShip(ctx, p.taskID, p.nextTask, p.result.Summary, p.rawLogPath, p.workDir)

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
			if n, _, _, err := l.git.FindPRForBranch(ctx, l.git.GetWorktreeBranch()); err == nil && n != 0 {
				prNumber = n
			}
		}
	}

	ct := l.buildCompletedTask(ctx, p.taskID, p.nextTask, p.result.Summary, prNumber)
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
		// Three distinct sub-cases when no PR number is available:
		//
		//   (a) shipErr != nil && pushedBranch == "": push was attempted but
		//       failed (e.g. exit 128, network error). The agent's commits are
		//       in the local worktree only — origin was never reached. Skip so
		//       the next iteration can retry; closing would silently discard
		//       work that was never shipped.
		//
		//   (b) pushedBranch != "" && shipErr != nil: the Phase 1 push succeeded
		//       but CreatePR failed (rate limit, 422, network). Commits are on the
		//       remote branch but no PR tracks them. Skip instead of closing so
		//       triage can rediscover the orphaned branch via the skip reason.
		//
		//   (c) shipErr == nil && pushedBranch == "": nothing was pushed. The
		//       agent signalled with no new commits and no prior-iteration commits
		//       to ship. Closing the bead is correct — work is verified complete.
		if shipErr != nil && pushedBranch == "" {
			branch := l.git.GetWorktreeBranch()
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
				"Push failed (%v) — skipping bead %s (work remains in local worktree)",
				shipErr, p.taskID)
			if p.taskID != "" {
				if ctx.Err() != nil {
					l.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
					l.setPhaseInterrupted(p.taskID)
					return completeTaskOut{action: signalComplete}
				}
				l.skipTask(p.taskID, tasks.SkipPushFailed, branch)
			}
			return completeTaskOut{action: signalSkipped}
		}

		if pushedBranch != "" {
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
				"No PR created but branch %s was pushed — skipping bead %s (CreatePR failed; work lives on remote)",
				pushedBranch, p.taskID)
			if p.taskID != "" {
				if ctx.Err() != nil {
					l.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
					l.setPhaseInterrupted(p.taskID)
					return completeTaskOut{action: signalComplete}
				}
				// Downstream triage (and the task-manager prompt) parses this
				// to locate orphaned remote branches whose PRs failed to create.
				l.skipTask(p.taskID, tasks.SkipPRCreationFailed, pushedBranch)
			}
			return completeTaskOut{action: signalSkipped}
		}

		l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "No PR created — closing bead for task %s", p.taskID)
		if p.taskID != "" {
			if ctx.Err() != nil {
				l.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
				l.setPhaseInterrupted(p.taskID)
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
				l.setPhaseInterrupted(p.taskID)
				return completeTaskOut{action: signalComplete}
			}
			prRef := ct.PRURL
			if prRef == "" {
				prRef = fmt.Sprintf("PR #%d", prNumber)
			}
			closeReason := fmt.Sprintf("Verified — %s open, merge pending CI recovery", prRef)
			if err := l.taskBackend.CloseTask(p.taskID, closeReason); err != nil {
				skipReason := tasks.SkipCloseFailed
				detail := ""
				if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
					l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %s blocked by %v", p.taskID, blockers)
					skipReason = tasks.SkipDependencyBlocked
					detail = strings.Join(blockers, ",")
				} else {
					l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask failed: %v", err)
				}
				l.skipTask(p.taskID, skipReason, detail)
			} else {
				l.logger.Emit(logging.Opts{Domain: logging.Beads}, "Closed task %s (%s)", p.taskID, closeReason)
				l.persistCompleted(p.taskID, false)
				l.state.ClearCurrentTask()
			}
			l.git.TagTaskEnd(p.taskID)
			return completeTaskOut{action: signalComplete, ct: &ct, prNumber: prNumber}
		}
		// Actual test failures — leave task open for manual investigation.
		l.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Error}, "CI failing on PR #%d — leaving task %s open.", prNumber, p.taskID)
		l.git.TagTaskEnd(p.taskID)
		return completeTaskOut{action: signalComplete, prNumber: prNumber}
	}

	// Close the task based on merge outcome.
	if p.taskID != "" {
		if ctx.Err() != nil {
			l.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
			l.setPhaseInterrupted(p.taskID)
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
			skipReason := tasks.SkipCloseFailed
			detail := ""
			if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
				l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %s blocked by %v", p.taskID, blockers)
				skipReason = tasks.SkipDependencyBlocked
				detail = strings.Join(blockers, ",")
			} else {
				l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask failed: %v", err)
			}
			l.skipTask(p.taskID, skipReason, detail)
		} else {
			l.logger.Emit(logging.Opts{Domain: logging.Beads}, "Closed task %s (%s)", p.taskID, closeReason)
			l.persistCompleted(p.taskID, merged)
			l.state.ClearCurrentTask()
		}
	}

	if p.notify {
		if merged {
			notify.TaskMerged(p.taskID, p.nextTask, time.Now())
		} else {
			notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary, time.Now())
		}
	}

	return completeTaskOut{action: signalComplete, ct: &ct, merged: merged, prNumber: prNumber}
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
	verify.RunPostTask(ctx, verify.RunPostTaskParams{
		PostTask:    l.cfg.PostTask,
		WorktreeDir: l.git.GetWorkDir(),
		ProjectDir:  l.cfg.Dirs.ProjectDir,
		Logger:      l.logger,
	}, taskID, prNumber, merged)
}

// setPhaseInterrupted marks a task phase=interrupted and releases the loop's
// claim by reverting status to open, so the task manager knows the loop stopped
// mid-work and the bead returns to the ready queue.
//
// Releasing the claim is essential: ClaimTask set status=in_progress at the
// start of the iteration, which removes the task from bd ready. Every caller
// of this function is an abort/interrupt path where the iteration did NOT ship
// or merge, so the task is no longer being worked. Without reopening, the task
// stays in_progress forever — invisible to bd ready (which excludes
// in_progress) and never resumed mid-session — deadlocking the loop and any
// task that depends on it. ReopenTask preserves assignee=ralph-loop, so the
// task stays selectable by bd ready --assignee=ralph-loop on the next pass.
func (l *Loop) setPhaseInterrupted(taskID string) {
	if taskID == "" {
		return
	}
	if err := l.taskBackend.ReopenTask(taskID); err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "ReopenTask (release claim): %v", err)
	} else {
		l.logger.Emit(logging.Opts{Domain: logging.Beads}, "%s → open (claim released)", taskID)
	}
	if err := l.taskBackend.SetState(taskID, "phase", "interrupted", "ralph: interrupted"); err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "SetState phase=interrupted: %v", err)
	} else {
		l.logger.Emit(logging.Opts{Domain: logging.Beads}, "%s → interrupted", taskID)
	}
}

// releaseClaimForRetry reopens the task (status → open, assignee preserved) so
// bd ready can re-select it on the next iteration. Every retry/interrupt exit
// from the task iteration must call this — the liminal phase (selectNextTask,
// waitForTasks, postTaskAndMaybeEvolve) must only be reached with a resolved
// task, never an in_progress claim.
func (l *Loop) releaseClaimForRetry(taskID string) {
	if taskID == "" || l.taskBackend == nil {
		return
	}
	if err := l.taskBackend.ReopenTask(taskID); err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "ReopenTask (release claim for retry) %s: %v", taskID, err)
	} else {
		l.logger.Emit(logging.Opts{Domain: logging.Beads}, "%s → open (released for retry)", taskID)
	}
}

// buildCompletedTask assembles the CompletedTask record for a signal.
func (l *Loop) buildCompletedTask(ctx context.Context, taskID, nextTask, summary string, prNumber int) CompletedTask {
	ct := CompletedTask{
		ID:      taskID,
		Title:   nextTask,
		Summary: summary,
		PRNum:   prNumber,
	}
	if num, t, u, err := l.git.FindPRForBranch(ctx, l.git.GetWorktreeBranch()); err == nil && num != 0 {
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
	rawLogPath string
	logStart   int
	workDir    string
}

// prepareAndBuildPrompt sets the task phase, runs pre-iteration tests, reads
// feedback, assembles attempt context, and builds the full prompt. Returns
// false if Run() should break (internet or rate limit unavailable).
func (l *Loop) prepareAndBuildPrompt(ctx context.Context, taskID, nextTask string) (iterationPrompt, bool) {
	if ctx.Err() != nil {
		return iterationPrompt{}, false
	}

	if taskID != "" {
		if err := l.taskBackend.ClaimTask(taskID); err != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "ClaimTask %s: %v", taskID, err)
		} else {
			l.logger.Emit(logging.Opts{Domain: logging.Beads}, "%s → in_progress (claimed)", taskID)
		}
		if err := l.taskBackend.SetState(taskID, "phase", "implementing", "ralph: starting task"); err != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "SetState phase=implementing: %v", err)
		}
	}

	taskPrompt := l.buildTaskPrompt(nextTask, taskID)
	buildStatus := verify.RunVerifyBuild(ctx, verify.RunVerifyBuildParams{
		VerifyBuild: l.cfg.VerifyBuild,
		WorktreeDir: l.git.GetWorkDir(),
		ProjectDir:  l.cfg.Dirs.ProjectDir,
		TestTimeout: l.cfg.TestTimeout,
		Logger:      l.logger,
	})
	testStatus := buildStatus + l.runPreIterationTests(ctx)

	if !l.connectivity.WaitForInternet(ctx, l.logger) {
		return iterationPrompt{}, false
	}
	if !l.waitForRate(ctx) {
		return iterationPrompt{}, false
	}

	headBefore := l.git.HeadRev()
	rawLogPath := logging.ActiveLogPath(l.cfg.Dirs.RalphDir, "raw")
	logStart := fileLineCount(rawLogPath)

	attemptContext := l.attemptContext(taskID, nextTask)
	if attemptContext != "" {
		var parts []string
		if len(l.taskAttempts) > 0 {
			parts = append(parts, fmt.Sprintf("%d prior attempt(s)", len(l.taskAttempts)))
		}
		if strings.Contains(attemptContext, "## Recent learnings") {
			parts = append(parts, "learnings from other tasks")
		}
		if len(parts) > 0 {
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
		rawLogPath: rawLogPath,
		logStart:   logStart,
		workDir:    l.git.GetWorkDir(),
	}, true
}

// RunIteration re-verifies that the selected task is still ready immediately
// before invoking the agent, then delegates to runAgent. This closes the race
// between task selection (bd ready snapshot) and agent invocation during which
// a dep may be added via bd dep add.
func (l *Loop) RunIteration(ctx context.Context, task taskContext, runIteration int) agentRunResult {
	if task.id != "" && l.taskBackend != nil {
		ready, err := l.taskBackend.IsReady(task.id)
		if err != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "IsReady(%s): %v — proceeding", task.id, err)
		} else if !ready {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task %s no longer ready (deps changed since selection) — skipping iteration", task.id)
			return agentRunResult{action: actionRetry, agentInvoked: false}
		}
	}
	return l.runAgent(ctx, task, runIteration)
}

// handleRunResult processes errors and retryable conditions from a Claude
// run (offline, feedback kill, idle timeout, rate limit). Returns the
// loopAction Run() should take. When actionRetry is returned, the caller
// is responsible for not counting this iteration.
func (l *Loop) handleRunResult(ctx context.Context, result claude.Result, runErr error, taskID, nextTask, headBefore string, runIteration int) loopAction {
	if runErr != nil {
		if ctx.Err() != nil {
			return actionDone
		}
		if !l.connectivity.IsOnline() {
			l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.WorkingModel}, "Claude failed — internet appears down")
			if !l.connectivity.WaitForInternet(ctx, l.logger) {
				return actionDone
			}
			l.releaseClaimForRetry(taskID)
			return actionRetry
		}
		l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.WorkingModel}, "Claude failed on iteration %d, continuing...", runIteration)
	}
	if result.Compacted {
		baseBranch := l.git.DetectDefaultBranch()
		if l.git.LogOneline("origin/"+baseBranch, "HEAD") != "" {
			if passed, _ := l.verifyCompletion(ctx, headBefore); passed {
				l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.WorkingModel}, "Context limit (200K) exceeded but branch has verified commits — routing through ship pipeline")
				return actionCompactionShip
			}
		}
		if l.appWideRecurrence(tasks.SkipCompaction) {
			l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Error, Model: l.cfg.WorkingModel}, "Agent exceeded the context limit (200K) again — compaction already caused a skip this streak, halting instead of parking another task.")
			l.haltAppWide(taskID, tasks.SkipCompaction, "")
			return actionDone
		}
		if count := l.incrementCompactionParkCount(taskID); count >= l.maxCompactionParks() {
			l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Error, Model: l.cfg.WorkingModel}, "Agent exceeded the context limit (200K) %d time(s) — task too big. Skipping task.", count)
			l.skipTask(taskID, tasks.SkipCompaction, "")
			l.recordSkipStreak(tasks.SkipCompaction)
			return actionSkip
		}
		l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.WorkingModel}, "Agent exceeded the context limit (200K) — retrying (below park cap)")
		l.releaseClaimForRetry(taskID)
		return actionRetry
	}
	if result.FeedbackKill {
		l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.WorkingModel}, "Restarting iteration %d — user feedback received", runIteration)
		diffStat := l.git.DiffStatRange(headBefore, l.git.HeadRev())
		l.recordAttempt(AttemptEvent{
			Summary:  "Killed: user feedback received (see bead notes for content)",
			DiffStat: diffStat,
			Analysis: "user_feedback: check bead notes for details",
		})
		l.releaseClaimForRetry(taskID)
		return actionRetry
	}
	if result.IdleTimeout || result.WallClockTimeout {
		kind := "idle timeout"
		analysis := "idle_timeout: consider a lighter approach or make incremental progress rather than deep-thinking without output"
		if result.WallClockTimeout {
			kind = "wall-clock timeout"
			analysis = "wall_clock_timeout: run exceeded max-run-duration hard cap"
		}
		l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.WorkingModel}, "Restarting iteration %d after %s", runIteration, kind)
		diffStat := l.git.DiffStatRange(headBefore, l.git.HeadRev())
		l.recordAttempt(AttemptEvent{
			Summary:  fmt.Sprintf("Killed: %s", kind),
			DiffStat: diffStat,
			Analysis: analysis,
		})
		l.taskIdleTimeouts++
		// Persistent cross-session cap: park after 2 failed starts with no commits.
		// The in-session taskIdleTimeouts guard remains as an additional ceiling.
		hasNewCommits := l.git.LogOneline(headBefore, l.git.HeadRev()) != ""
		if !hasNewCommits {
			if l.appWideRecurrence(tasks.SkipFailedStart) {
				l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.WorkingModel}, "Failed start with no progress for %s again — this reason already caused a skip this streak, halting instead of retrying.", taskID)
				l.haltAppWide(taskID, tasks.SkipFailedStart, "")
				return actionDone
			}
			if count := l.incrementFailedStartCount(taskID); count >= l.maxFailedStarts() {
				l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.WorkingModel}, "Failed start %d times with no progress for %s — skipping task", count, taskID)
				l.skipTask(taskID, tasks.SkipFailedStart, "")
				l.recordSkipStreak(tasks.SkipFailedStart)
				return actionRetry
			}
		}
		if l.taskIdleTimeouts >= l.maxIdleTimeoutFailures() {
			if l.appWideRecurrence(tasks.SkipIdleTimeout) {
				l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.WorkingModel}, "Idle timeout for %s again — this reason already caused a skip this streak, halting instead of retrying.", taskID)
				l.haltAppWide(taskID, tasks.SkipIdleTimeout, "")
				return actionDone
			}
			l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.WorkingModel}, "Idle timeout %d times for %s — skipping task", l.taskIdleTimeouts, taskID)
			l.skipTask(taskID, tasks.SkipIdleTimeout, "")
			l.recordSkipStreak(tasks.SkipIdleTimeout)
			return actionRetry
		}
		l.releaseClaimForRetry(taskID)
		return actionRetry
	}
	if result.RateLimited {
		waitDur := claude.FormatWaitDuration(time.Until(result.ResetAt))
		l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.WorkingModel}, "Claude rate limit — waiting %s until %s", waitDur, result.ResetAt.Format("3:04pm"))
		err := l.limiter.WaitUntil(ctx, result.ResetAt, func(secs int) {
			l.logger.Emit(logging.Opts{Domain: logging.LLM, Model: l.cfg.WorkingModel}, "Rate limit: %ds until reset", secs)
		})
		if err != nil {
			l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.WorkingModel}, "Rate limit wait interrupted: %v", err)
			return actionDone
		}
		l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: l.cfg.WorkingModel}, "Rate limit reset — resuming")
		l.releaseClaimForRetry(taskID)
		return actionRetry
	}

	return actionProceed
}
