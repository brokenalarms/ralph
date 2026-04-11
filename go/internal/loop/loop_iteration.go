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

	// If OnSignal was set, verification already passed in the runner.
	// If not (legacy/test path), run verification here as fallback.
	if !p.result.OnSignalUsed {
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
		if err := l.taskBackend.SetState(p.taskID, "phase", "verified", "ralph: tests passed, commits present"); err != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "SetState phase=verified: %v", err)
		} else {
			l.logger.Emit(logging.Opts{Domain: logging.Beads}, "%s → verified", p.taskID)
		}
	}

	l.attempts.Clear(p.taskID, p.nextTask)
	l.state.RecordCompletedTask(p.taskID, p.nextTask)
	l.state.TouchPlanFlash()

	headAfterSignal := l.git.HeadRev()
	if p.headBefore != "" && headAfterSignal == p.headBefore {
		// No new commits but verification passed (agent + LLM + tests agree).
		l.logger.Emit(logging.Opts{Domain: logging.Git}, "No new commits — verified complete")

		// Check for an existing PR from a prior attempt that still needs merging.
		if p.taskID != "" {
			ref, _ := l.taskBackend.GetExternalRef(p.taskID)
			if prNum := parsePRNumber(ref); prNum != 0 {
				prState, _ := l.git.GetPRState(prNum)
				if prState == git.PRStateOpen {
					l.logger.Emit(logging.Opts{Domain: logging.Git}, "Found open PR #%d from prior attempt — routing through merge", prNum)
					_, _, merged, _, _ := l.doShip(ctx, p.taskID, p.nextTask, p.result.Summary, p.rawLogPath, p.workDir)
					l.attempts.ClearMergeFailures(p.taskID)
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
					if merged && p.evolve {
						l.logger.Phase("Evolve: restarting with latest main")
						l.state.Write("status", "evolve_restart") //nolint:errcheck
						return completeTaskOut{action: signalEvolve, merged: true}
					}
					return completeTaskOut{action: signalSkipped, merged: merged}
				}
				if prState == git.PRStateMerged {
					l.logger.Emit(logging.Opts{Domain: logging.Git}, "PR #%d already merged", prNum)
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
			_ = l.taskBackend.SetState(p.taskID, "phase", "verified", closeReason)
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
		return completeTaskOut{action: signalSkipped}
	}

	if ctx.Err() != nil {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "Post-signal timeout — aborting before push")
		return completeTaskOut{action: signalComplete}
	}

	prNumber, shipURL, merged, ciFailure, stacked := l.doShip(ctx, p.taskID, p.nextTask, p.result.Summary, p.rawLogPath, p.workDir)

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
		return completeTaskOut{action: signalComplete, ct: &ct}
	}

	if prNumber == 0 {
		l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "No PR created — closing bead for task %s", p.taskID)
		if p.taskID != "" {
			if ctx.Err() != nil {
				l.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
				return completeTaskOut{action: signalComplete, ct: &ct}
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

	// CI is failing — leave task open for manual investigation or next loop.
	if ciFailure {
		l.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Error}, "CI failing on PR #%d — leaving task %s open.", prNumber, p.taskID)
		l.git.TagTaskEnd(p.taskID)
		l.execRunPostTask(ctx, p.taskID, prNumber, false)
		return completeTaskOut{action: signalComplete, ct: &ct}
	}

	// Close the task based on merge outcome.
	if p.taskID != "" {
		if ctx.Err() != nil {
			l.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
			return completeTaskOut{action: signalComplete, ct: &ct}
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
		l.attempts.ClearMergeFailures(p.taskID)
		_ = l.taskBackend.SetState(p.taskID, "phase", "verified", "ralph: PR open or stacked")
		if merged {
			_ = l.taskBackend.SetState(p.taskID, "phase", "verified", "ralph: PR merged")
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
		if p.evolve {
			l.git.TagTaskEnd(p.taskID)
			l.logger.Phase("Evolve: restarting with latest main")
			l.state.Write("status", "evolve_restart") //nolint:errcheck
			return completeTaskOut{action: signalEvolve, ct: &ct, merged: true}
		}
	}

	return completeTaskOut{action: signalComplete, ct: &ct, merged: merged}
}

// verifyCompletion delegates to OnVerify when set, otherwise runs the standard
// simple-path verification (commit check + single test run + state write).
// This is the non-fix-loop variant used outside the post-signal pipeline.
func (l *Loop) verifyCompletion(ctx context.Context, headBefore string) (bool, string) {
	if l.cfg.OnVerify != nil {
		return l.cfg.OnVerify(ctx, l.git.GetWorkDir(), headBefore)
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
func (l *Loop) execRunPostTask(ctx context.Context, taskID string, prNumber int, merged bool) {
	if l.cfg.OnPostTask != nil {
		l.cfg.OnPostTask(ctx, taskID, prNumber, merged)
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

	if !l.cfg.WaitForInternet(ctx, l.logger) {
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
		if !l.cfg.IsOnline() {
			l.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: l.cfg.Model}, "Claude failed — internet appears down")
			if !l.cfg.WaitForInternet(ctx, l.logger) {
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
