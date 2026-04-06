package loop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/attempts"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/ratelimit"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/verify"
)

type runAndCompleteParams struct {
	git                 git.GitOps
	logger              *logging.Logger
	runner              claudeRunner
	verifier            *Verifier
	state               *state.Store
	attempts            *attempts.Tracker
	limiter             *ratelimit.Limiter
	signals             claude.SignalPaths
	backend             tasks.Backend
	analyzer            *analyzer.Analyzer
	// destructured cfg fields
	quiet                bool
	verbose              bool
	model                string
	agentEscalationModel string
	modelCap             string
	idleTimeout         time.Duration
	idleTimeoutProgress time.Duration
	postSignalTimeout   time.Duration
	autoMerge           bool
	evolve              bool
	notify              bool
	ralphDir            string
	promptsDir          string
	projectDir          string
	planFile            string
	callsPerHour        int
	ensureReviewersFn   func() []git.Reviewer
	// func deps
	runVerifyBuildFn    func(ctx context.Context) string
	isOnlineFunc        func() bool
	waitForInternetFunc func(context.Context, *logging.Logger) bool
	verifyFunc          func(ctx context.Context, dir, headBefore string) (bool, string)
	runPostTaskFn       func(ctx context.Context, taskID string, prNumber int, merged bool)
}

// runAndComplete builds the prompt, runs the agent, handles retryable
// failures, analyzes the outcome, and processes the signal. Returns the
// loopAction Run() should take, the analyzer action, whether the task was
// merged, and any CompletedTask produced (so Run() can append to sessionTasks).
func runAndComplete(ctx context.Context, p runAndCompleteParams, task taskContext, runIteration int) (loopAction, analyzer.Action, bool, *CompletedTask) {
	prep, ok := prepareAndBuildPrompt(ctx, prepareAndBuildPromptParams{
		backend:             p.backend,
		git:                 p.git,
		logger:              p.logger,
		verifier:            p.verifier,
		limiter:             p.limiter,
		attempts:            p.attempts,
		signals:             p.signals,
		promptsDir:          p.promptsDir,
		ralphDir:            p.ralphDir,
		projectDir:          p.projectDir,
		planFile:            p.planFile,
		callsPerHour:        p.callsPerHour,
		runVerifyBuildFn:    p.runVerifyBuildFn,
		waitForInternetFunc: p.waitForInternetFunc,
	}, task.id, task.title)
	if !ok {
		return actionDone, analyzer.Continue, false, nil
	}

	taskStart := time.Now()
	agentModel := p.model
	if p.attempts.Count(task.id, task.title) > 0 {
		agentModel = p.agentEscalationModel
	}
	agentModel = verify.CapModel(p.modelCap, agentModel)
	p.logger.Emit(logging.Opts{Domain: logging.LLM, Model: agentModel}, "Agent model: %s", agentModel)
	result, runErr := p.runner.Run(claude.RunConfig{
		Ctx:                 ctx,
		WorkDir:             prep.workDir,
		RalphDir:            p.ralphDir,
		Prompt:              prep.fullPrompt,
		TaskID:              task.id,
		RawLog:              prep.rawLogPath,
		LogFile:             filepath.Join(p.ralphDir, "loop.log"),
		Quiet:               p.quiet,
		Verbose:             p.verbose,
		Model:               agentModel,
		Signals:             p.signals,
		PollInterval:        2 * time.Second,
		IdleTimeout:         p.idleTimeout,
		IdleTimeoutProgress: p.idleTimeoutProgress,
		HasProgress: func() bool {
			if p.git.HeadRev() != prep.headBefore {
				return true
			}
			if prep.diffBefore {
				return false
			}
			return p.git.HasDiff()
		},
		OnSignal: func(summary string) bool {
			return p.verifier.OnSignal(signalParams{
				ctx:        ctx,
				headBefore: prep.headBefore,
				workDir:    prep.workDir,
				rawLogPath: prep.rawLogPath,
				taskID:     task.id,
				nextTask:   task.title,
			})
		},
		FeedbackFile: filepath.Join(p.ralphDir, "feedback"),
	})

	runAction := handleRunResult(ctx, handleRunResultParams{
		result:              result,
		runErr:              runErr,
		taskID:              task.id,
		nextTask:            task.title,
		headBefore:          prep.headBefore,
		runIteration:        runIteration,
		model:               p.model,
		isOnlineFunc:        p.isOnlineFunc,
		waitForInternetFunc: p.waitForInternetFunc,
		logger:              p.logger,
		git:                 p.git,
		attempts:            p.attempts,
		limiter:             p.limiter,
		backend:             p.backend,
		state:               p.state,
		skipTask:            skipTask,
	})
	if runAction != actionProceed {
		return runAction, analyzer.Continue, false, nil
	}
	elapsed := time.Since(taskStart)
	p.limiter.Increment()

	diffStat, halt, iterAction := processRunOutcome(processRunOutcomeParams{
		backend:  p.backend,
		git:      p.git,
		logger:   p.logger,
		state:    p.state,
		attempts: p.attempts,
		analyzer: p.analyzer,
		signals:  p.signals,
		model:    p.model,
	}, result, elapsed, runIteration, prep, task.id, task.title)
	if halt {
		return actionDone, iterAction, false, nil
	}

	if result.SignalDetected {
		sp := postSignalParams{
			ctx:        ctx,
			result:     result,
			headBefore: prep.headBefore,
			workDir:    prep.workDir,
			rawLogPath: prep.rawLogPath,
			taskID:     task.id,
			nextTask:   task.title,
			diffStat:   diffStat,
		}
		out := handlePostSignal(sp, handlePostSignalOpts{
			postSignalTimeout:    p.postSignalTimeout,
			autoMerge:            p.autoMerge,
			evolve:               p.evolve,
			notify:               p.notify,
			git:             p.git,
			backend:           p.backend,
			state:             p.state,
			logger:            p.logger,
			attempts:          p.attempts,
			verifyFn: func(ctx context.Context, headBefore string) (bool, string) {
				if p.verifyFunc != nil {
					return p.verifyFunc(ctx, p.git.GetWorkDir(), headBefore)
				}
				return p.verifier.VerifyCompletion(ctx, p.git.GetWorkDir(), headBefore)
			},
			pushSignalPRFn: func(sp postSignalParams) (int, string) {
				return pushSignalPR(sp.ctx, sp, pushSignalPROpts{
					git:                 p.git,
					backend:             p.backend,
					logger:              p.logger,
					isOnlineFunc:        p.isOnlineFunc,
					waitForInternetFunc: p.waitForInternetFunc,
					shipFn: func(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
						return p.git.Ship(ctx, opts)
					},
				})
			},
			finalizePRFn: func(fp finalizePRParams) finalizePRResult {
				fp.autoMerge = p.autoMerge
				fp.activeReviewers = p.ensureReviewersFn()
				fp.git = p.git
				fp.logger = p.logger
				fp.backend = p.backend
				fp.state = p.state
				fp.attempts = p.attempts
				fp.verifier = p.verifier
				return finalizePR(fp)
			},
			buildCTFn: func(taskID, nextTask, summary string, prNumber int, _ string) CompletedTask {
				return buildCompletedTask(taskID, nextTask, summary, prNumber, p.git)
			},
			runPostTaskFn: p.runPostTaskFn,
		})
		switch out.action {
		case signalRetry, signalSkipped:
			return actionRetry, iterAction, out.merged, out.ct
		case signalEvolve:
			return actionDone, iterAction, out.merged, out.ct
		}
		p.git.TagTaskEnd(task.id)
		return actionProceed, iterAction, out.merged, out.ct
	}

	p.git.TagTaskEnd(task.id)
	return actionProceed, iterAction, false, nil
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
	postSignalTimeout    time.Duration
	autoMerge            bool
	evolve               bool
	notify               bool
	git             git.GitOps
	backend         tasks.Backend
	state           *state.Store
	logger          *logging.Logger
	attempts        *attempts.Tracker
	verifyFn        func(ctx context.Context, headBefore string) (bool, string)
	pushSignalPRFn    func(p postSignalParams) (int, string)
	finalizePRFn      func(p finalizePRParams) finalizePRResult
	buildCTFn         func(taskID, nextTask, summary string, prNumber int, workDir string) CompletedTask
	runPostTaskFn     func(ctx context.Context, taskID string, prNumber int, merged bool)
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

	// Guard: if the task was already skipped during verification (e.g. 3
	// rejected attempts), do not push or merge the rejected work.
	if p.taskID != "" {
		skipped, err := opts.state.GetSkippedTasks()
		if err != nil {
			opts.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Failed to load skipped tasks for %s: %v — conservatively not pushing", p.taskID, err)
			return handlePostSignalOut{action: signalSkipped}
		}
		for _, id := range skipped {
			if id == p.taskID {
				opts.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task %s was skipped during verification — not pushing", p.taskID)
				return handlePostSignalOut{action: signalSkipped}
			}
		}
	}

	// Preflight: check bead wasn't prematurely closed by the agent.
	if p.taskID != "" {
		phase, _ := opts.backend.GetState(p.taskID, "phase")
		if phase != "implementing" {
			opts.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task %s phase is %q (expected implementing) — agent may have tampered with task state", p.taskID, phase)
		}
	}

	// If OnSignal was set, verification already passed in the runner.
	// If not (legacy/test path), run verification here as fallback.
	if !p.result.OnSignalUsed {
		if passed, reason := opts.verifyFn(p.ctx, p.headBefore); !passed {
			opts.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Verification failed: %s", reason)
			opts.attempts.Record(p.taskID, p.nextTask,
				"Signal received but verification failed: "+reason,
				p.diffStat,
				"verification_failed: fix must pass tests and produce commits before closing")
			return handlePostSignalOut{action: signalRetry}
		}
	}

	if p.taskID != "" {
		if err := opts.backend.SetState(p.taskID, "phase", "verified", "ralph: tests passed, commits present"); err != nil {
			opts.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "SetState phase=verified: %v", err)
		} else {
			opts.logger.Emit(logging.Opts{Domain: logging.Beads}, "%s → verified", p.taskID)
		}
	}

	opts.attempts.Clear(p.taskID, p.nextTask)
	opts.state.RecordCompletedTask(p.taskID, p.nextTask)
	opts.state.TouchPlanFlash()

	headAfterSignal := opts.git.HeadRev()
	if p.headBefore != "" && headAfterSignal == p.headBefore {
		// No new commits but verification passed (agent + LLM + tests agree).
		// That's sufficient proof the work is on main — close the bead.
		opts.logger.Emit(logging.Opts{Domain: logging.Git}, "No new commits — verified complete, closing bead")
		if p.taskID != "" {
			closeReason := "verified complete (no new commits)"
			ref, _ := opts.backend.GetExternalRef(p.taskID)
			if prNum := parsePRNumber(ref); prNum != 0 {
				if prState, _ := opts.git.GetPRState(prNum); prState == git.PRStateMerged {
					closeReason = fmt.Sprintf("PR #%d already merged", prNum)
				}
			}
			_ = opts.backend.SetState(p.taskID, "phase", "verified", closeReason)
			if err := opts.backend.CloseTask(p.taskID, closeReason); err != nil {
				skipReason := "close_failed"
				if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
					opts.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %s blocked by %v", p.taskID, blockers)
					skipReason = fmt.Sprintf("dependency_blocked_by:%s", strings.Join(blockers, ","))
				} else {
					opts.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %v", err)
				}
				skipTask(opts.backend, opts.state, opts.logger, p.taskID, skipReason)
			} else {
				opts.logger.Emit(logging.Opts{Domain: logging.Beads}, "Closed task %s (%s)", p.taskID, closeReason)
				persistCompletedTask(opts.state, opts.logger, p.taskID, false)
			}
		}
		opts.git.TagTaskEnd(p.taskID)
		opts.runPostTaskFn(p.ctx, p.taskID, 0, false)
		if opts.notify {
			notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary)
		}
		return handlePostSignalOut{action: signalSkipped}
	}

	if p.ctx.Err() != nil {
		opts.logger.Emit(logging.Opts{Level: logging.Warn}, "Post-signal timeout — aborting before push")
		return handlePostSignalOut{action: signalComplete}
	}

	prNumber, shipURL := opts.pushSignalPRFn(p)
	prState := git.PRStateOpen

	// Recovery: if push/Ship didn't produce a PR, find any existing PR in any state.
	if prNumber == 0 && p.taskID != "" {
		if num, found := findExistingPRForTask(p.taskID, opts.git.GetWorktreeBranch(), opts.backend, opts.git); found {
			prNumber = num
			prState = "" // let finalizePR look up the actual state
		}
	}

	ct := opts.buildCTFn(p.taskID, p.nextTask, p.result.Summary, prNumber, p.workDir)
	if shipURL != "" {
		ct.PRURL = shipURL
	}

	// buildCTFn may discover a PR that recovery missed. A PR found in the
	// post-push context was just created, so OPEN is a safe assumption.
	if prNumber == 0 && ct.PRNum != 0 {
		prNumber = ct.PRNum
		prState = git.PRStateOpen
	}

	if p.ctx.Err() != nil {
		opts.logger.Emit(logging.Opts{Level: logging.Warn}, "Post-signal timeout — aborting before merge")
		return handlePostSignalOut{action: signalComplete, ct: &ct}
	}

	if prNumber == 0 {
		opts.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "No PR created — closing bead for task %s", p.taskID)
		if p.taskID != "" {
			branch := opts.git.GetWorktreeBranch()
			closeReason := "Verified — no PR created"
			if branch != "" {
				closeReason = fmt.Sprintf("Verified — branch %s, no PR", branch)
			}
			if err := opts.backend.CloseTask(p.taskID, closeReason); err != nil {
				opts.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %v", err)
			}
		}
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
func pushSignalPR(ctx context.Context, p postSignalParams, opts pushSignalPROpts) (int, string) {
	prBody := buildPRBody(opts.backend, p.taskID, p.result.Summary)
	shipOpts := git.ShipOpts{TaskID: p.taskID, TaskTitle: p.nextTask, Body: prBody}

	result, shipErr := opts.shipFn(ctx, shipOpts)
	if shipErr != nil {
		if !opts.isOnlineFunc() {
			opts.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Ship failed — internet appears down")
			opts.waitForInternetFunc(ctx, opts.logger)
			result, shipErr = opts.shipFn(ctx, shipOpts)
		}
		if shipErr != nil {
			opts.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Ship: %v", shipErr)
		}
	}

	if result.PRNumber != 0 && p.taskID != "" {
		ref := result.PRURL
		if ref == "" {
			ref = prURL(opts.git.RemoteURL(), result.PRNumber)
		}
		if ref != "" {
			opts.logger.Emit(logging.Opts{Domain: logging.Git}, "Linking task %s to %s (branch: %s)", p.taskID, ref, opts.git.GetWorktreeBranch())
			if refErr := opts.backend.SetExternalRef(p.taskID, ref); refErr != nil {
				opts.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "SetExternalRef: %v", refErr)
			}
		}
	}
	return result.PRNumber, result.PRURL
}

// buildCompletedTask assembles the CompletedTask record for a signal.
func buildCompletedTask(taskID, nextTask, summary string, prNumber int, g git.GitOps) CompletedTask {
	ct := CompletedTask{
		ID:      taskID,
		Title:   nextTask,
		Summary: summary,
		PRNum:   prNumber,
	}
	if num, t, u, err := g.FindPRForBranch(g.GetWorktreeBranch()); err == nil && num != 0 {
		ct.PRNum = num
		ct.PRTitle = t
		ct.PRURL = u
	} else if prNumber != 0 {
		ct.PRNum = prNumber
	}
	return ct
}

// finalizePRParams bundles the context and dependencies needed to finalize a
// PR: merge if applicable, then close the bead. Used by both the post-signal
// flow and the resume-via-PR flow so neither duplicates merge+close logic.
type finalizePRParams struct {
	ctx        context.Context
	taskID     string
	nextTask   string
	prNumber   int
	prState    git.PRState // looked up from GH if empty
	prURL      string
	workDir    string
	rawLogPath string
	// dependency fields
	autoMerge       bool
	activeReviewers []git.Reviewer
	git             git.GitOps
	logger               *logging.Logger
	backend              tasks.Backend
	state                *state.Store
	attempts             *attempts.Tracker
	verifier             *Verifier
	// mergeFunc overrides git.MergeWithRetry for tests; nil uses the real path.
	mergeFunc func(ctx context.Context) (bool, error)
}

type finalizePRResult struct {
	merged bool
	closed bool
}

// reviewAddressed returns true when the given reviewer's feedback was already
// addressed for this task in a previous finalizePR call.
func (p *finalizePRParams) reviewAddressed(botUsername string) bool {
	if p.state == nil || p.taskID == "" {
		return false
	}
	v, _ := p.state.Read("review_addressed:" + botUsername + ":" + p.taskID)
	return v == "true"
}

// markReviewAddressed records that the given reviewer's feedback was addressed
// so subsequent finalizePR calls for the same task skip re-polling.
func (p *finalizePRParams) markReviewAddressed(botUsername string) {
	if p.state == nil || p.taskID == "" {
		return
	}
	p.state.Write("review_addressed:"+botUsername+":"+p.taskID, "true")
}

// finalizePR handles an existing PR: merges if applicable, closes the bead.
// Returns the merge/close outcome so callers can act on it (e.g. evolve).
func finalizePR(p finalizePRParams) finalizePRResult {
	if p.prNumber == 0 {
		p.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "No PR — task %s stays open", p.taskID)
		return finalizePRResult{}
	}

	prState := p.prState
	if prState == "" {
		looked, err := p.git.GetPRState(p.prNumber)
		if err != nil || looked == "" {
			p.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: prLink(p.git, p.prNumber)}, "Failed to get state: %v", err)
			return finalizePRResult{}
		}
		prState = looked
	}

	merged := prState == git.PRStateMerged
	mergeFailed := false

	if prState == git.PRStateOpen && p.autoMerge {
		p.git.SetLocalTestsPassed(true)
		prBase := p.git.GetPRBase(p.prNumber)
		defaultBranch := p.git.DetectDefaultBranch()
		if prBase != "" && prBase != defaultBranch {
			p.logger.Emit(logging.Opts{Domain: "git", Link: prLink(p.git, p.prNumber)}, "targets %s — stacked, closing bead", prBase)
		} else {
			for _, reviewer := range p.activeReviewers {
				if p.reviewAddressed(reviewer.BotUsername) {
					continue
				}
				review, err := p.git.PollReview(reviewer.BotUsername, p.prNumber, reviewer.DefaultTimeout)
				if err != nil {
					p.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink(p.git, p.prNumber)}, "%s review poll: %v", reviewer.BotUsername, err)
					continue
				}
				if review != nil {
					p.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink(p.git, p.prNumber)}, "%s review received (%d comments)", reviewer.BotUsername, len(review.Comments))
					if p.verifier != nil {
						tryFixReviewComments(p.ctx, p.git, p.verifier, p.logger, reviewer.BotUsername, review, p.prNumber, p.nextTask, p.workDir, p.rawLogPath)
					}
					p.markReviewAddressed(reviewer.BotUsername)
				} else {
					p.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink(p.git, p.prNumber)}, "No %s review arrived within timeout — proceeding to merge", reviewer.BotUsername)
				}
			}
			p.logger.Emit(logging.Opts{Domain: "git", Link: prLink(p.git, p.prNumber)}, "targets %s — merging", defaultBranch)
			p.git.SetKnownPRNumber(p.prNumber)
			defer p.git.SetKnownPRNumber(0)
			var mergeErr error
			merged, mergeErr = mergeWithRetry(p.ctx, mergeWithRetryParams{
				taskID:     p.taskID,
				nextTask:   p.nextTask,
				workDir:    p.workDir,
				rawLogPath: p.rawLogPath,
				mergeFunc:  p.mergeFunc,
				git:        p.git,
				verifier:   p.verifier,
				logger:     p.logger,
				backend:    p.backend,
			})
			if mergeErr != nil {
				p.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Auto-merge: %v", mergeErr)
				var ciExhausted *git.CIFixExhaustedError
				if errors.As(mergeErr, &ciExhausted) {
					p.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Error}, "CI fix agents gave up after %d attempts — tests still failing. Leaving task %s open for manual investigation.", ciExhausted.Attempts, p.taskID)
					return finalizePRResult{}
				}
			}
			if !merged {
				mergeFailed = true
				p.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: prLink(p.git, p.prNumber)}, "Merge pending — closing bead")
			}
		}
	}

	if merged {
		p.git.PostMergeUpdateMain()
	}

	if p.taskID == "" {
		return finalizePRResult{merged: merged, closed: true}
	}

	var closeReason string
	if mergeFailed {
		closeReason = fmt.Sprintf("Verified — PR #%d open, merge pending", p.prNumber)
		if p.prURL != "" {
			closeReason = fmt.Sprintf("Verified — %s open, merge pending", p.prURL)
		}
	} else {
		closeReason = fmt.Sprintf("Fixed in PR #%d", p.prNumber)
		if p.prURL != "" {
			closeReason = fmt.Sprintf("Fixed in %s", p.prURL)
		}
	}
	p.attempts.ClearMergeFailures(p.taskID)
	stateReason := "ralph: PR open or stacked"
	if merged {
		stateReason = "ralph: PR merged"
	}
	_ = p.backend.SetState(p.taskID, "phase", "verified", stateReason)
	if err := p.backend.CloseTask(p.taskID, closeReason); err != nil {
		skipReason := "close_failed"
		if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
			p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %s blocked by %v", p.taskID, blockers)
			skipReason = fmt.Sprintf("dependency_blocked_by:%s", strings.Join(blockers, ","))
		} else {
			p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask failed: %v", err)
		}
		skipTask(p.backend, p.state, p.logger, p.taskID, skipReason)
	} else {
		p.logger.Emit(logging.Opts{Domain: logging.Beads}, "Closed task %s (%s)", p.taskID, closeReason)
		persistCompletedTask(p.state, p.logger, p.taskID, merged)
	}

	return finalizePRResult{merged: merged, closed: true}
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

type prepareAndBuildPromptParams struct {
	backend             tasks.Backend
	git                 git.GitOps
	logger              *logging.Logger
	verifier            *Verifier
	limiter             *ratelimit.Limiter
	attempts            *attempts.Tracker
	signals             claude.SignalPaths
	promptsDir          string
	ralphDir            string
	projectDir          string
	planFile            string
	callsPerHour        int
	runVerifyBuildFn    func(ctx context.Context) string
	waitForInternetFunc func(context.Context, *logging.Logger) bool
}

// prepareAndBuildPrompt sets the task phase, runs pre-iteration tests, reads
// feedback, assembles attempt context, and builds the full prompt. Returns
// false if Run() should break (internet or rate limit unavailable).
func prepareAndBuildPrompt(ctx context.Context, p prepareAndBuildPromptParams, taskID, nextTask string) (iterationPrompt, bool) {
	if taskID != "" {
		if err := p.backend.SetState(taskID, "phase", "implementing", "ralph: starting task"); err != nil {
			p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "SetState phase=implementing: %v", err)
		}
	}

	taskPrompt := buildTaskPrompt(nextTask, taskID, p.backend, p.promptsDir, p.ralphDir)
	buildStatus := p.runVerifyBuildFn(ctx)
	testStatus := buildStatus + p.verifier.RunPreIterationTests(ctx)

	if !p.waitForInternetFunc(ctx, p.logger) {
		return iterationPrompt{}, false
	}
	if !waitForRate(ctx, waitForRateParams{limiter: p.limiter, callsPerHour: p.callsPerHour, logger: p.logger}) {
		return iterationPrompt{}, false
	}

	headBefore := p.git.HeadRev()
	diffBefore := p.git.HasDiff()
	rawLogPath := filepath.Join(p.ralphDir, "raw.log")
	logStart := fileLineCount(rawLogPath)

	attemptContext := buildAttemptContext(taskID, nextTask, p.attempts, p.ralphDir)
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
			p.logger.Emit(logging.Opts{}, "Including %s", strings.Join(parts, " + "))
		}
	}

	fullPrompt, err := buildPrompt(taskPrompt, attemptContext, testStatus, p.backend, p.promptsDir, p.projectDir, p.git.GetWorkDir(), p.ralphDir, p.planFile, p.signals, p.logger)
	if err != nil {
		p.logger.Emit(logging.Opts{Level: logging.Error}, "Prompt build failed: %v", err)
		return iterationPrompt{}, false
	}

	return iterationPrompt{
		fullPrompt: fullPrompt,
		headBefore: headBefore,
		diffBefore: diffBefore,
		rawLogPath: rawLogPath,
		logStart:   logStart,
		workDir:    p.git.GetWorkDir(),
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
	model        string

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
			p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Claude failed — internet appears down")
			if !p.waitForInternetFunc(ctx, p.logger) {
				return actionDone
			}
			return actionRetry
		}
		p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Claude failed on iteration %d, continuing...", p.runIteration)
	}
	if p.result.FeedbackKill {
		p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Restarting iteration %d — user feedback received", p.runIteration)
		diffStat := p.git.DiffStatRange(p.headBefore, p.git.HeadRev())
		p.attempts.Record(p.taskID, p.nextTask,
			"Killed: user feedback received (see bead notes for content)",
			diffStat,
			"user_feedback: check bead notes for details")
		return actionRetry
	}
	if p.result.IdleTimeout {
		p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Restarting iteration %d after idle timeout", p.runIteration)
		diffStat := p.git.DiffStatRange(p.headBefore, p.git.HeadRev())
		p.attempts.Record(p.taskID, p.nextTask,
			"Killed: idle timeout (no output for configured duration)",
			diffStat,
			"idle_timeout: consider a lighter approach or make incremental progress rather than deep-thinking without output")
		count, _ := p.attempts.RecordIdleTimeoutFailure(p.taskID)
		if count >= p.attempts.MaxIdleTimeoutFailures {
			p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Idle timeout %d times for %s — skipping task", count, p.taskID)
			p.skipTask(p.backend, p.state, p.logger, p.taskID, "idle_timeout_max_failures")
			return actionRetry
		}
		return actionRetry
	}
	if p.result.RateLimited {
		waitDur := claude.FormatWaitDuration(time.Until(p.result.ResetAt))
		p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Claude rate limit — waiting %s until %s", waitDur, p.result.ResetAt.Format("3:04pm"))
		err := p.limiter.WaitUntil(ctx, p.result.ResetAt, func(secs int) {
			p.logger.Emit(logging.Opts{Domain: logging.LLM, Model: p.model}, "Rate limit: %ds until reset", secs)
		})
		if err != nil {
			p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Rate limit wait interrupted: %v", err)
			return actionDone
		}
		p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: p.model}, "Rate limit reset — resuming")
		return actionRetry
	}

	return actionProceed
}



