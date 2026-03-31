package loop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/attempts"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// runAndComplete builds the prompt, runs the agent, handles retryable
// failures, analyzes the outcome, and processes the signal. Returns the
// loopAction Run() should take.
func (l *Loop) runAndComplete(ctx context.Context, task taskContext, runIteration int) loopAction {
	prep, ok := l.prepareAndBuildPrompt(ctx, task.id, task.title)
	if !ok {
		return actionDone
	}

	taskStart := time.Now()
	result, runErr := l.runner.Run(claude.RunConfig{
		Ctx:                 ctx,
		WorkDir:             prep.workDir,
		RalphDir:            l.cfg.Dirs.RalphDir,
		Prompt:              prep.fullPrompt,
		TaskID:              task.id,
		RawLog:              prep.rawLogPath,
		LogFile:             filepath.Join(l.cfg.Dirs.RalphDir, "loop.log"),
		Quiet:               l.cfg.Quiet,
		Verbose:             l.cfg.Verbose,
		Signals:             l.signals,
		PollInterval:        2 * time.Second,
		IdleTimeout:         l.cfg.IdleTimeout,
		IdleTimeoutProgress: l.cfg.IdleTimeoutProgress,
		HasProgress: func() bool {
			return l.git.HasDiff() || l.git.HeadRev() != prep.headBefore
		},
		OnSignal: func(summary string) bool {
			return l.onSignal(signalParams{
				ctx:        ctx,
				headBefore: prep.headBefore,
				workDir:    prep.workDir,
				rawLogPath: prep.rawLogPath,
				taskID:     task.id,
				nextTask:   task.title,
			})
		},
		FeedbackFile: filepath.Join(l.cfg.Dirs.RalphDir, "feedback"),
	})

	runAction := l.handleRunResult(ctx, result, runErr, task.id, task.title, prep.headBefore, runIteration)
	if runAction != actionProceed {
		return runAction
	}
	elapsed := time.Since(taskStart)
	l.limiter.Increment()

	diffStat, halt := l.processRunOutcome(result, elapsed, runIteration, prep, task.id, task.title)
	if halt {
		return actionDone
	}

	if result.SignalDetected {
		signalAction := l.handlePostSignal(postSignalParams{
			ctx:        ctx,
			result:     result,
			headBefore: prep.headBefore,
			workDir:    prep.workDir,
			rawLogPath: prep.rawLogPath,
			taskID:     task.id,
			nextTask:   task.title,
			diffStat:   diffStat,
		})
		switch signalAction {
		case signalRetry, signalSkipped:
			return actionRetry
		case signalEvolve:
			return actionDone
		}
	}

	l.git.TagTaskEnd(task.id)
	return actionProceed
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

// handlePostSignal runs after the agent signals completion: verifies the
// work, pushes a PR, merges if configured, and closes the bead. Returns
// the action Run() should take. Callers pass runIteration and iteration
// pointers so the no-commits path can increment them.
func (l *Loop) handlePostSignal(p postSignalParams) postSignalAction {
	if timeout := l.cfg.PostSignalTimeout; timeout > 0 {
		ctx, cancel := context.WithTimeout(p.ctx, timeout)
		defer cancel()
		p.ctx = ctx
	}

	// Preflight: check bead wasn't prematurely closed by the agent.
	if p.taskID != "" {
		phase, _ := l.cfg.TaskBackend.GetState(p.taskID, "phase")
		if phase != "implementing" {
			l.logger.Warn("beads", "Task %s phase is %q (expected implementing) — agent may have tampered with task state", p.taskID, phase)
		}
	}

	// If OnSignal was set, verification already passed in the runner.
	// If not (legacy/test path), run verification here as fallback.
	if !p.result.OnSignalUsed {
		if passed, reason := l.verifyCompletion(p.ctx, p.headBefore); !passed {
			l.logger.Warn("test", "Verification failed: %s", reason)
			l.attempts.Record(p.taskID, p.nextTask,
				"Signal received but verification failed: "+reason,
				p.diffStat,
				"verification_failed: fix must pass tests and produce commits before closing")
			return signalRetry
		}
	}

	if p.taskID != "" {
		if err := l.cfg.TaskBackend.SetState(p.taskID, "phase", "verified", "ralph: tests passed, commits present"); err != nil {
			l.logger.Warn("beads", "SetState phase=verified: %v", err)
		} else {
			l.logger.Log("beads", "%s → verified", p.taskID)
		}
	}

	l.attempts.Clear(p.taskID, p.nextTask)
	recordCompletedTask(l.cfg.Dirs.RalphDir, p.taskID, p.nextTask)
	touchFile(filepath.Join(l.cfg.Dirs.RalphDir, ".plan-flash"))

	headAfterSignal := l.git.HeadRev()
	if p.headBefore != "" && headAfterSignal == p.headBefore {
		// No new commits but verification passed (agent + LLM + tests agree).
		// That's sufficient proof the work is on main — close the bead.
		l.logger.Log("git", "No new commits — verified complete, closing bead")
		if p.taskID != "" {
			closeReason := "verified complete (no new commits)"
			ref, _ := l.cfg.TaskBackend.GetExternalRef(p.taskID)
			if prNum := parsePRNumber(ref); prNum != "" {
				if prState, _ := l.git.GetPRState(prNum); strings.ToUpper(prState) == "MERGED" {
					closeReason = fmt.Sprintf("PR #%s already merged", prNum)
				}
			}
			_ = l.cfg.TaskBackend.SetState(p.taskID, "phase", "verified", closeReason)
			if err := l.cfg.TaskBackend.CloseTask(p.taskID, closeReason); err != nil {
				skipReason := "close_failed"
				if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
					l.logger.Warn("beads", "CloseTask: %s blocked by %v", p.taskID, blockers)
					skipReason = fmt.Sprintf("dependency_blocked_by:%s", strings.Join(blockers, ","))
				} else {
					l.logger.Warn("beads", "CloseTask: %v", err)
				}
				skipTask(l.cfg.TaskBackend, l.state, l.logger, p.taskID, skipReason)
			} else {
				l.logger.Log("beads", "Closed task %s (%s)", p.taskID, closeReason)
				persistCompletedTask(l.state, l.logger, p.taskID)
			}
		}
		l.git.TagTaskEnd(p.taskID)
		l.runPostTask(p.ctx, p.taskID, "", false)
		if l.cfg.Notify {
			notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary)
		}
		return signalSkipped
	}

	if p.ctx.Err() != nil {
		l.logger.Warn("", "Post-signal timeout — aborting before push")
		return signalComplete
	}

	prNumber, shipURL := l.pushSignalPR(p)
	prState := "OPEN"

	// Recovery: if push failed but a PR already exists, use it.
	if prNumber == "" && p.taskID != "" {
		ref, _ := l.cfg.TaskBackend.GetExternalRef(p.taskID)
		if existing := parsePRNumber(ref); existing != "" {
			prNumber = existing
			prState = "" // unknown — let finalizePR look it up
		}
	}

	ct := l.buildCompletedTask(p.taskID, p.nextTask, p.result.Summary, prNumber, p.workDir)
	if shipURL != "" {
		ct.PRURL = shipURL
	}

	// buildCompletedTask may discover a PR via findPRInfo that push missed.
	// findPRInfo queries open PRs for the current branch, so OPEN is safe.
	if prNumber == "" && ct.PRNum != "" {
		prNumber = ct.PRNum
		prState = "OPEN"
	}

	if p.ctx.Err() != nil {
		l.logger.Warn("", "Post-signal timeout — aborting before merge")
		l.sessionTasks = append(l.sessionTasks, ct)
		return signalComplete
	}

	if prNumber == "" {
		l.logger.Warn("git", "No PR created — task %s stays open", p.taskID)
		l.sessionTasks = append(l.sessionTasks, ct)
		return signalComplete
	}

	result := l.finalizePR(finalizePRParams{
		ctx:        p.ctx,
		taskID:     p.taskID,
		nextTask:   p.nextTask,
		prNumber:   prNumber,
		prState:    prState,
		prURL:      ct.PRURL,
		workDir:    p.workDir,
		rawLogPath: p.rawLogPath,
	})
	l.sessionTasks = append(l.sessionTasks, ct)

	l.runPostTask(p.ctx, p.taskID, prNumber, result.merged)

	if l.cfg.Notify {
		notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary)
	}

	if result.merged {
		l.lastTaskMerged = true
		notify.TaskMerged(p.taskID, p.nextTask)
		if l.cfg.Evolve {
			l.git.TagTaskEnd(p.taskID)
			l.logger.Phase("Evolve: restarting with latest main")
			l.state.Write("status", "evolve_restart")
			return signalEvolve
		}
	}

	return signalComplete
}

// pushSignalPR pushes the branch and creates a PR after a successful signal.
func (l *Loop) pushSignalPR(p postSignalParams) (string, string) {
	prBody := buildPRBody(l.cfg.TaskBackend, p.taskID, p.result.Summary)
	shipOpts := git.ShipOpts{TaskID: p.taskID, TaskTitle: p.nextTask, Body: prBody}

	result, shipErr := l.shipWork(p.ctx, shipOpts)
	if shipErr != nil {
		if !l.isOnlineFunc() {
			l.logger.Warn("git", "Ship failed — internet appears down")
			l.waitForInternetFunc(p.ctx, l.logger)
			result, shipErr = l.shipWork(p.ctx, shipOpts)
		}
		if shipErr != nil {
			l.logger.Warn("git", "Ship: %v", shipErr)
		}
	}

	if result.PRNumber != "" && p.taskID != "" {
		ref := result.PRURL
		if ref == "" {
			ref = prURL(l.git.RemoteURL(), result.PRNumber)
		}
		if ref != "" {
			l.logger.Log("git", "Linking task %s to %s (branch: %s)", p.taskID, ref, l.git.GetWorktreeBranch())
			if refErr := l.cfg.TaskBackend.SetExternalRef(p.taskID, ref); refErr != nil {
				l.logger.Warn("beads", "SetExternalRef: %v", refErr)
			}
		}
	}
	return result.PRNumber, result.PRURL
}

// buildCompletedTask assembles the CompletedTask record for a signal.
func (l *Loop) buildCompletedTask(taskID, nextTask, summary, prNumber, workDir string) CompletedTask {
	ct := CompletedTask{
		ID:      taskID,
		Title:   nextTask,
		Summary: summary,
		PRNum:   prNumber,
	}
	if prNum, prTitle, prURL := l.findPRInfo(workDir); prNum != "" {
		ct.PRNum = prNum
		ct.PRTitle = prTitle
		ct.PRURL = prURL
	} else if prNumber != "" {
		ct.PRNum = prNumber
	}
	return ct
}

// finalizePRParams bundles the context needed to finalize a PR: merge if
// applicable, then close the bead. Used by both the post-signal flow and
// the resume-via-PR flow so neither duplicates merge+close logic.
type finalizePRParams struct {
	ctx        context.Context
	taskID     string
	nextTask   string
	prNumber   string
	prState    string // "OPEN" or "MERGED"; looked up from GH if empty
	prURL      string
	workDir    string
	rawLogPath string
}

type finalizePRResult struct {
	merged bool
	closed bool
}

// finalizePR handles an existing PR: merges if applicable, closes the bead.
// Returns the merge/close outcome so callers can act on it (e.g. evolve).
func (l *Loop) finalizePR(p finalizePRParams) finalizePRResult {
	if p.prNumber == "" {
		l.logger.Warn("git", "No PR — task %s stays open", p.taskID)
		return finalizePRResult{}
	}

	prState := p.prState
	if prState == "" {
		looked, err := l.git.GetPRState(p.prNumber)
		if err != nil || looked == "" {
			l.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: l.prLink(p.prNumber)}, "Failed to get state: %v", err)
			return finalizePRResult{}
		}
		prState = strings.ToUpper(looked)
	}

	merged := prState == "MERGED"

	if prState == "OPEN" && l.cfg.AutoMerge {
		l.git.SetLocalTestsPassed(true)
		prBase := l.git.GetPRBase(p.prNumber)
		defaultBranch := l.git.DetectDefaultBranch()
		if prBase != "" && prBase != defaultBranch {
			l.logger.Emit(logging.Opts{Domain: "git", Link: l.prLink(p.prNumber)}, "targets %s — stacked, closing bead", prBase)
		} else {
			l.logger.Emit(logging.Opts{Domain: "git", Link: l.prLink(p.prNumber)}, "targets %s — merging", defaultBranch)
			var mergeErr error
			merged, mergeErr = l.mergeWithRetry(p.ctx, p.taskID, p.nextTask, p.workDir, p.rawLogPath)
			if errors.Is(mergeErr, git.ErrMergeBlockedByInfra) {
				l.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: l.prLink(p.prNumber)}, "Merge blocked by CI infra — PR stays open, closing bead")
			} else if mergeErr != nil {
				l.logger.Warn("git", "Auto-merge: %v", mergeErr)
			}
			if !merged && !errors.Is(mergeErr, git.ErrMergeBlockedByInfra) {
				l.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: l.prLink(p.prNumber)}, "Merge failed — skipping task")
				skipTask(l.cfg.TaskBackend, l.state, l.logger, p.taskID, "merge_failed")
				return finalizePRResult{}
			}
		}
	}

	if merged {
		l.git.PostMergeUpdateMain()
	}

	if p.taskID == "" {
		return finalizePRResult{merged: merged, closed: true}
	}

	closeReason := fmt.Sprintf("Fixed in PR #%s", p.prNumber)
	if p.prURL != "" {
		closeReason = fmt.Sprintf("Fixed in %s", p.prURL)
	}
	l.attempts.ClearMergeFailures(p.taskID)
	stateReason := "ralph: PR open or stacked"
	if merged {
		stateReason = "ralph: PR merged"
	}
	_ = l.cfg.TaskBackend.SetState(p.taskID, "phase", "verified", stateReason)
	if err := l.cfg.TaskBackend.CloseTask(p.taskID, closeReason); err != nil {
		skipReason := "close_failed"
		if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
			l.logger.Warn("beads", "CloseTask: %s blocked by %v", p.taskID, blockers)
			skipReason = fmt.Sprintf("dependency_blocked_by:%s", strings.Join(blockers, ","))
		} else {
			l.logger.Warn("beads", "CloseTask failed: %v", err)
		}
		skipTask(l.cfg.TaskBackend, l.state, l.logger, p.taskID, skipReason)
	} else {
		l.logger.Log("beads", "Closed task %s (%s)", p.taskID, closeReason)
		persistCompletedTask(l.state, l.logger, p.taskID)
	}

	return finalizePRResult{merged: merged, closed: true}
}

// checkoutExistingBranch checks metadata for a branch from a previous
// iteration. If the remote has that branch with work, it checks it out.
// Otherwise, it renames the current branch for the task and stores the
// new name in metadata. Returns true if an existing remote branch was
// checked out.
func (l *Loop) checkoutExistingBranch(taskID, nextTask string) bool {
	storedBranch := ""
	if taskID != "" {
		storedBranch, _ = l.cfg.TaskBackend.GetMetadata(taskID, "branch")
	}
	if storedBranch != "" {
		_ = l.git.FetchBranch(storedBranch)
		if l.git.RemoteBranchHasCommits(storedBranch) {
			if l.git.RemoteBranchIsOnMain(storedBranch) {
				l.git.CheckoutRemoteBranch(storedBranch)
				return true
			}
			// Stale branch diverged from main. If no open PR, clean it up.
			l.logger.Warn("git", "Remote branch %s diverged from main — cleaning up", storedBranch)
			ref, _ := l.cfg.TaskBackend.GetExternalRef(taskID)
			if parsePRNumber(ref) == "" {
				if err := l.git.DeleteRemoteBranchByName(storedBranch); err != nil {
					l.logger.Warn("git", "Failed to delete stale remote branch: %v", err)
				}
			}
		}
		// Reuse the branch name locally, starting from main.
		l.git.RenameBranchTo(storedBranch)
		return false
	}
	l.git.RenameBranchForTask(nextTask, taskID)
	if taskID != "" && l.git.GetWorktreeBranch() != "" && strings.Contains(l.git.GetWorktreeBranch(), taskID) {
		_ = l.cfg.TaskBackend.SetMetadata(taskID, "branch", l.git.GetWorktreeBranch())
	}
	return false
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
	if taskID != "" {
		if err := l.cfg.TaskBackend.SetState(taskID, "phase", "implementing", "ralph: starting task"); err != nil {
			l.logger.Warn("beads", "SetState phase=implementing: %v", err)
		}
	}

	taskPrompt := l.buildTaskPrompt(nextTask, taskID)
	testStatus := l.runPreIterationTests(ctx)

	if !l.waitForInternetFunc(ctx, l.logger) {
		return iterationPrompt{}, false
	}
	if !l.waitForRate(ctx) {
		return iterationPrompt{}, false
	}

	headBefore := l.git.HeadRev()
	rawLogPath := filepath.Join(l.cfg.Dirs.RalphDir, "raw.log")
	logStart := fileLineCount(rawLogPath)

	attemptContext := l.buildAttemptContext(taskID, nextTask)
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
			l.logger.Log("", "Including %s", strings.Join(parts, " + "))
		}
	}

	fullPrompt, err := l.buildPrompt(taskPrompt, attemptContext, testStatus)
	if err != nil {
		l.logger.Error("", "Prompt build failed: %v", err)
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

// runResultAction describes what Run() should do after handling the Claude result.
// handleRunResult processes errors and retryable conditions from a Claude
// run (offline, feedback kill, idle timeout, rate limit). Returns the
// loopAction Run() should take. When actionRetry is returned, the caller
// is responsible for not counting this iteration.
func (l *Loop) handleRunResult(ctx context.Context, result claude.Result, runErr error,
	taskID, nextTask, headBefore string, runIteration int) loopAction {

	if runErr != nil {
		if !l.isOnlineFunc() {
			l.logger.Warn("llm", "Claude failed — internet appears down")
			if !l.waitForInternetFunc(ctx, l.logger) {
				return actionDone
			}
			return actionRetry
		}
		l.logger.Warn("llm", "Claude failed on iteration %d, continuing...", runIteration)
	}
	if result.FeedbackKill {
		l.logger.Warn("llm", "Restarting iteration %d — user feedback received", runIteration)
		diffStat := l.git.DiffStatRange(headBefore, l.git.HeadRev())
		l.attempts.Record(taskID, nextTask,
			"Killed: user feedback received (see bead notes for content)",
			diffStat,
			"user_feedback: check bead notes for details")
		return actionRetry
	}
	if result.IdleTimeout {
		l.logger.Warn("llm", "Restarting iteration %d after idle timeout", runIteration)
		diffStat := l.git.DiffStatRange(headBefore, l.git.HeadRev())
		l.attempts.Record(taskID, nextTask,
			"Killed: idle timeout (no output for configured duration)",
			diffStat,
			"idle_timeout: consider a lighter approach or make incremental progress rather than deep-thinking without output")
		count, _ := l.attempts.RecordIdleTimeoutFailure(taskID)
		if count >= attempts.MaxIdleTimeoutFailures {
			l.logger.Warn("llm", "Idle timeout %d times for %s — skipping task", count, taskID)
			skipTask(l.cfg.TaskBackend, l.state, l.logger, taskID, "idle_timeout_max_failures")
			return actionRetry
		}
		return actionRetry
	}
	if result.RateLimited {
		waitDur := claude.FormatWaitDuration(time.Until(result.ResetAt))
		l.logger.Warn("llm", "Claude rate limit — waiting %s until %s", waitDur, result.ResetAt.Format("3:04pm"))
		err := l.limiter.WaitUntil(ctx, result.ResetAt, func(secs int) {
			l.logger.Log("llm", "Rate limit: %ds until reset", secs)
		})
		if err != nil {
			l.logger.Warn("llm", "Rate limit wait interrupted: %v", err)
			return actionDone
		}
		l.logger.Success("llm", "Rate limit reset — resuming")
		return actionRetry
	}

	return actionProceed
}

