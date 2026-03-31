package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/attempts"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/state"
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
		Model:               l.cfg.Model,
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

// handlePostSignalOpts bundles all dependencies for the handlePostSignal package function.
type handlePostSignalOpts struct {
	postSignalTimeout time.Duration
	autoMerge         bool
	evolve            bool
	notify            bool
	git               git.GitOps
	backend           tasks.Backend
	state             *state.Store
	logger            *logging.Logger
	attempts          *attempts.Tracker
	verifyFn          func(ctx context.Context, headBefore string) (bool, string)
	pushSignalPRFn    func(p postSignalParams) (string, string)
	finalizePRFn      func(p finalizePRParams) finalizePRResult
	buildCTFn         func(taskID, nextTask, summary, prNumber, workDir string) CompletedTask
	runPostTaskFn     func(ctx context.Context, taskID, prNumber string, merged bool)
}

// handlePostSignalOut carries the results of handlePostSignal back to the method wrapper.
type handlePostSignalOut struct {
	action postSignalAction
	ct     *CompletedTask // non-nil if a CompletedTask was produced and should be appended
	merged bool           // true if the task was merged (caller should set lastTaskMerged)
}

// handlePostSignal runs after the agent signals completion: verifies the
// work, pushes a PR, merges if configured, and closes the bead.
func handlePostSignal(p postSignalParams, opts handlePostSignalOpts) handlePostSignalOut {
	if opts.postSignalTimeout > 0 {
		ctx, cancel := context.WithTimeout(p.ctx, opts.postSignalTimeout)
		defer cancel()
		p.ctx = ctx
	}

	// Preflight: check bead wasn't prematurely closed by the agent.
	if p.taskID != "" {
		phase, _ := opts.backend.GetState(p.taskID, "phase")
		if phase != "implementing" {
			opts.logger.Warn("beads", "Task %s phase is %q (expected implementing) — agent may have tampered with task state", p.taskID, phase)
		}
	}

	// If OnSignal was set, verification already passed in the runner.
	// If not (legacy/test path), run verification here as fallback.
	if !p.result.OnSignalUsed {
		if passed, reason := opts.verifyFn(p.ctx, p.headBefore); !passed {
			opts.logger.Warn("test", "Verification failed: %s", reason)
			opts.attempts.Record(p.taskID, p.nextTask,
				"Signal received but verification failed: "+reason,
				p.diffStat,
				"verification_failed: fix must pass tests and produce commits before closing")
			return handlePostSignalOut{action: signalRetry}
		}
	}

	if p.taskID != "" {
		if err := opts.backend.SetState(p.taskID, "phase", "verified", "ralph: tests passed, commits present"); err != nil {
			opts.logger.Warn("beads", "SetState phase=verified: %v", err)
		} else {
			opts.logger.Log("beads", "%s → verified", p.taskID)
		}
	}

	opts.attempts.Clear(p.taskID, p.nextTask)
	opts.state.RecordCompletedTask(p.taskID, p.nextTask)
	opts.state.TouchPlanFlash()

	headAfterSignal := opts.git.HeadRev()
	if p.headBefore != "" && headAfterSignal == p.headBefore {
		// No new commits but verification passed (agent + LLM + tests agree).
		// That's sufficient proof the work is on main — close the bead.
		opts.logger.Log("git", "No new commits — verified complete, closing bead")
		if p.taskID != "" {
			closeReason := "verified complete (no new commits)"
			ref, _ := opts.backend.GetExternalRef(p.taskID)
			if prNum := parsePRNumber(ref); prNum != "" {
				if prState, _ := opts.git.GetPRState(prNum); strings.ToUpper(prState) == "MERGED" {
					closeReason = fmt.Sprintf("PR #%s already merged", prNum)
				}
			}
			_ = opts.backend.SetState(p.taskID, "phase", "verified", closeReason)
			if err := opts.backend.CloseTask(p.taskID, closeReason); err != nil {
				skipReason := "close_failed"
				if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
					opts.logger.Warn("beads", "CloseTask: %s blocked by %v", p.taskID, blockers)
					skipReason = fmt.Sprintf("dependency_blocked_by:%s", strings.Join(blockers, ","))
				} else {
					opts.logger.Warn("beads", "CloseTask: %v", err)
				}
				skipTask(opts.backend, opts.state, opts.logger, p.taskID, skipReason)
			} else {
				opts.logger.Log("beads", "Closed task %s (%s)", p.taskID, closeReason)
				persistCompletedTask(opts.state, opts.logger, p.taskID, false)
			}
		}
		opts.git.TagTaskEnd(p.taskID)
		opts.runPostTaskFn(p.ctx, p.taskID, "", false)
		if opts.notify {
			notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary)
		}
		return handlePostSignalOut{action: signalSkipped}
	}

	if p.ctx.Err() != nil {
		opts.logger.Warn("", "Post-signal timeout — aborting before push")
		return handlePostSignalOut{action: signalComplete}
	}

	prNumber, shipURL := opts.pushSignalPRFn(p)
	prState := "OPEN"

	// Recovery: if push failed but a PR already exists, use it.
	if prNumber == "" && p.taskID != "" {
		ref, _ := opts.backend.GetExternalRef(p.taskID)
		if existing := parsePRNumber(ref); existing != "" {
			prNumber = existing
			prState = "" // unknown — let finalizePR look it up
		}
	}

	ct := opts.buildCTFn(p.taskID, p.nextTask, p.result.Summary, prNumber, p.workDir)
	if shipURL != "" {
		ct.PRURL = shipURL
	}

	// buildCTFn may discover a PR via findPRInfo that push missed.
	// findPRInfo queries open PRs for the current branch, so OPEN is safe.
	if prNumber == "" && ct.PRNum != "" {
		prNumber = ct.PRNum
		prState = "OPEN"
	}

	if p.ctx.Err() != nil {
		opts.logger.Warn("", "Post-signal timeout — aborting before merge")
		return handlePostSignalOut{action: signalComplete, ct: &ct}
	}

	if prNumber == "" {
		opts.logger.Warn("git", "No PR created — task %s stays open", p.taskID)
		return handlePostSignalOut{action: signalComplete, ct: &ct}
	}

	finalResult := opts.finalizePRFn(finalizePRParams{
		ctx:        p.ctx,
		taskID:     p.taskID,
		nextTask:   p.nextTask,
		prNumber:   prNumber,
		prState:    prState,
		prURL:      ct.PRURL,
		workDir:    p.workDir,
		rawLogPath: p.rawLogPath,
	})

	opts.runPostTaskFn(p.ctx, p.taskID, prNumber, finalResult.merged)

	if opts.notify {
		notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary)
	}

	if finalResult.merged {
		notify.TaskMerged(p.taskID, p.nextTask)
		if opts.evolve {
			opts.git.TagTaskEnd(p.taskID)
			opts.logger.Phase("Evolve: restarting with latest main")
			opts.state.Write("status", "evolve_restart")
			return handlePostSignalOut{action: signalEvolve, ct: &ct, merged: true}
		}
	}

	return handlePostSignalOut{action: signalComplete, ct: &ct, merged: finalResult.merged}
}

// handlePostSignal delegates to the package-level handlePostSignal function,
// collecting side-effect mutations to sessionTasks and lastTaskMerged.
func (l *Loop) handlePostSignal(p postSignalParams) postSignalAction {
	opts := handlePostSignalOpts{
		postSignalTimeout: l.cfg.PostSignalTimeout,
		autoMerge:         l.cfg.AutoMerge,
		evolve:            l.cfg.Evolve,
		notify:            l.cfg.Notify,
		git:               l.git,
		backend:           l.cfg.TaskBackend,
		state:             l.state,
		logger:            l.logger,
		attempts:          l.attempts,
		verifyFn: func(ctx context.Context, headBefore string) (bool, string) {
			return l.verifyCompletion(ctx, headBefore)
		},
		pushSignalPRFn: l.pushSignalPR,
		finalizePRFn:   l.finalizePR,
		buildCTFn:      l.buildCompletedTask,
		runPostTaskFn:  l.runPostTask,
	}
	out := handlePostSignal(p, opts)
	if out.ct != nil {
		l.sessionTasks = append(l.sessionTasks, *out.ct)
	}
	if out.merged {
		l.lastTaskMerged = true
	}
	return out.action
}

// pushSignalPROpts bundles the dependencies for the pushSignalPR package function.
type pushSignalPROpts struct {
	git                 git.GitOps
	backend             tasks.Backend
	logger              *logging.Logger
	isOnlineFunc        func() bool
	waitForInternetFunc func(context.Context, *logging.Logger) bool
	shipFn              func(context.Context, git.ShipOpts) (git.ShipResult, error)
}

// pushSignalPR pushes the branch and creates a PR after a successful signal.
func pushSignalPR(ctx context.Context, p postSignalParams, opts pushSignalPROpts) (string, string) {
	prBody := buildPRBody(opts.backend, p.taskID, p.result.Summary)
	shipOpts := git.ShipOpts{TaskID: p.taskID, TaskTitle: p.nextTask, Body: prBody}

	result, shipErr := opts.shipFn(ctx, shipOpts)
	if shipErr != nil {
		if !opts.isOnlineFunc() {
			opts.logger.Warn("git", "Ship failed — internet appears down")
			opts.waitForInternetFunc(ctx, opts.logger)
			result, shipErr = opts.shipFn(ctx, shipOpts)
		}
		if shipErr != nil {
			opts.logger.Warn("git", "Ship: %v", shipErr)
		}
	}

	if result.PRNumber != "" && p.taskID != "" {
		ref := result.PRURL
		if ref == "" {
			ref = prURL(opts.git.RemoteURL(), result.PRNumber)
		}
		if ref != "" {
			opts.logger.Log("git", "Linking task %s to %s (branch: %s)", p.taskID, ref, opts.git.GetWorktreeBranch())
			if refErr := opts.backend.SetExternalRef(p.taskID, ref); refErr != nil {
				opts.logger.Warn("beads", "SetExternalRef: %v", refErr)
			}
		}
	}
	return result.PRNumber, result.PRURL
}

// pushSignalPR delegates to the package-level pushSignalPR function.
func (l *Loop) pushSignalPR(p postSignalParams) (string, string) {
	return pushSignalPR(p.ctx, p, pushSignalPROpts{
		git:                 l.git,
		backend:             l.cfg.TaskBackend,
		logger:              l.logger,
		isOnlineFunc:        l.isOnlineFunc,
		waitForInternetFunc: l.waitForInternetFunc,
		shipFn:              l.shipWork,
	})
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
	mergeFailed := false

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
			if mergeErr != nil {
				l.logger.Warn("git", "Auto-merge: %v", mergeErr)
			}
			if !merged {
				mergeFailed = true
				l.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: l.prLink(p.prNumber)}, "Merge pending — closing bead")
			}
		}
	}

	if merged {
		l.git.PostMergeUpdateMain()
	}

	if p.taskID == "" {
		return finalizePRResult{merged: merged, closed: true}
	}

	var closeReason string
	if mergeFailed {
		closeReason = fmt.Sprintf("Verified — PR #%s open, merge pending", p.prNumber)
		if p.prURL != "" {
			closeReason = fmt.Sprintf("Verified — %s open, merge pending", p.prURL)
		}
	} else {
		closeReason = fmt.Sprintf("Fixed in PR #%s", p.prNumber)
		if p.prURL != "" {
			closeReason = fmt.Sprintf("Fixed in %s", p.prURL)
		}
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
		persistCompletedTask(l.state, l.logger, p.taskID, merged)
	}

	return finalizePRResult{merged: merged, closed: true}
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

// handleRunResultParams bundles the inputs and dependencies for handleRunResult.
type handleRunResultParams struct {
	result       claude.Result
	runErr       error
	taskID       string
	nextTask     string
	headBefore   string
	runIteration int

	isOnlineFunc        func() bool
	waitForInternetFunc func(context.Context, *logging.Logger) bool
	logger              *logging.Logger
	git                 git.GitOps
	attempts            *attempts.Tracker
	limiter             interface {
		WaitUntil(ctx context.Context, target time.Time, onTick func(int)) error
	}
	backend  tasks.Backend
	state    *state.Store
	skipTask func(backend tasks.Backend, st *state.Store, logger *logging.Logger, id, reason string)
}

// handleRunResult processes errors and retryable conditions from a Claude
// run (offline, feedback kill, idle timeout, rate limit). Returns the
// loopAction Run() should take. When actionRetry is returned, the caller
// is responsible for not counting this iteration.
func handleRunResult(ctx context.Context, p handleRunResultParams) loopAction {
	if p.runErr != nil {
		if !p.isOnlineFunc() {
			p.logger.Warn("llm", "Claude failed — internet appears down")
			if !p.waitForInternetFunc(ctx, p.logger) {
				return actionDone
			}
			return actionRetry
		}
		p.logger.Warn("llm", "Claude failed on iteration %d, continuing...", p.runIteration)
	}
	if p.result.FeedbackKill {
		p.logger.Warn("llm", "Restarting iteration %d — user feedback received", p.runIteration)
		diffStat := p.git.DiffStatRange(p.headBefore, p.git.HeadRev())
		p.attempts.Record(p.taskID, p.nextTask,
			"Killed: user feedback received (see bead notes for content)",
			diffStat,
			"user_feedback: check bead notes for details")
		return actionRetry
	}
	if p.result.IdleTimeout {
		p.logger.Warn("llm", "Restarting iteration %d after idle timeout", p.runIteration)
		diffStat := p.git.DiffStatRange(p.headBefore, p.git.HeadRev())
		p.attempts.Record(p.taskID, p.nextTask,
			"Killed: idle timeout (no output for configured duration)",
			diffStat,
			"idle_timeout: consider a lighter approach or make incremental progress rather than deep-thinking without output")
		count, _ := p.attempts.RecordIdleTimeoutFailure(p.taskID)
		if count >= attempts.MaxIdleTimeoutFailures {
			p.logger.Warn("llm", "Idle timeout %d times for %s — skipping task", count, p.taskID)
			p.skipTask(p.backend, p.state, p.logger, p.taskID, "idle_timeout_max_failures")
			return actionRetry
		}
		return actionRetry
	}
	if p.result.RateLimited {
		waitDur := claude.FormatWaitDuration(time.Until(p.result.ResetAt))
		p.logger.Warn("llm", "Claude rate limit — waiting %s until %s", waitDur, p.result.ResetAt.Format("3:04pm"))
		err := p.limiter.WaitUntil(ctx, p.result.ResetAt, func(secs int) {
			p.logger.Log("llm", "Rate limit: %ds until reset", secs)
		})
		if err != nil {
			p.logger.Warn("llm", "Rate limit wait interrupted: %v", err)
			return actionDone
		}
		p.logger.Success("llm", "Rate limit reset — resuming")
		return actionRetry
	}

	return actionProceed
}

// handleRunResult delegates to the package-level handleRunResult function.
func (l *Loop) handleRunResult(ctx context.Context, result claude.Result, runErr error,
	taskID, nextTask, headBefore string, runIteration int) loopAction {
	return handleRunResult(ctx, handleRunResultParams{
		result:              result,
		runErr:              runErr,
		taskID:              taskID,
		nextTask:            nextTask,
		headBefore:          headBefore,
		runIteration:        runIteration,
		isOnlineFunc:        l.isOnlineFunc,
		waitForInternetFunc: l.waitForInternetFunc,
		logger:              l.logger,
		git:                 l.git,
		attempts:            l.attempts,
		limiter:             l.limiter,
		backend:             l.cfg.TaskBackend,
		state:               l.state,
		skipTask:            skipTask,
	})
}

