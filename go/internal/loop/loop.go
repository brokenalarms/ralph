package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/brokenalarms/ralph/internal/agent"
	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/attempts"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/ratelimit"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/verify"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Config holds all parameters needed by the execution loop.
type Config struct {
	Dirs                workctx.WorkContext
	PlanFile            string
	MaxIterations       int
	Refactor            bool
	Quiet               bool
	AutoMerge           bool
	Evolve              bool
	CallsPerHour        int
	TaskBackend         tasks.Backend
	IdleTimeout         time.Duration
	IdleTimeoutProgress time.Duration
	PostSignalTimeout   time.Duration
	PostTask            string
	VerifyBuild         string
	Notify              bool
	Wait                bool
	Verbose             bool
	Model                  string
	AgentEscalationModel   string // model for agent on retry attempts; defaults to opus
	ModelCap               string // maximum model tier for all LLM calls; empty means no cap
	OnRebaseConflict    func(err error) git.RebaseRecovery
	Version             string
	VerifyDir             string // project root where tests are run; empty disables verification
	VerifyModel           string // model for the first LLM verification attempt; defaults to haiku
	VerifyEscalationModel string // model for subsequent LLM verification attempts; defaults to sonnet
	OnIterationStart      func() // called at the start of each iteration (e.g. to regenerate resume script)

	// Attempt limits — overrides package defaults when set.
	MaxPromptAttempts      int
	MaxMergeFailures       int
	MaxIdleTimeoutFailures int
	MaxLLMVerifyAttempts   int
	MaxTestFixAttempts     int

	// Test/compile timeouts.
	TestTimeout         time.Duration
	CompileCheckTimeout time.Duration

	// Network timeouts.
	ConnectivityCheckTimeout time.Duration
	InternetRestoreInterval  time.Duration

	// hooks for test injection; nil uses the real implementation
	CheckGitHub     func(ctx context.Context) error // startup GitHub reachability check; nil uses real implementation
	OnVerify        func(ctx context.Context, dir, headBefore string) (bool, string)
	OnPostTask      func(ctx context.Context, taskID string, prNumber int, merged bool) // called instead of runPostTask when set
	IsOnline        func() bool
	WaitForInternet func(ctx context.Context, logger *logging.Logger) bool
	OnWait          func()
	NewRunner       func() claudeRunner
	QueryFn         func(ctx context.Context, workDir, prompt, model string) (string, error)
}

// claudeRunner abstracts the Claude execution interface for testability.
type claudeRunner interface {
	Run(cfg claude.RunConfig) (claude.Result, error)
	StopStreaming()
	InjectMessage(msg string) error
}

// CompletedTask holds summary info for a task completed during this session.
type CompletedTask struct {
	ID      string
	Title   string
	Summary string
	PRNum   int
	PRTitle string
	PRURL   string
}

// Loop orchestrates the execution phase: task selection, prompt building,
// rate limiting, branch rotation, Claude invocation, and response analysis.
type Loop struct {
	cfg                  Config
	state                *state.Store
	git                  git.GitOps
	limiter              *ratelimit.Limiter
	runner               claudeRunner
	verifier             *Verifier
	analyzer             *analyzer.Analyzer
	attempts             *attempts.Tracker
	logger               *logging.Logger
	signals              claude.SignalPaths
	completedTasks      []CompletedTask
	activeReviewers     []git.Reviewer
	reviewersDetected   bool
}

// New creates an execution loop from the given configuration. All agent
// invocations go through the centralized agent module.
func New(cfg Config, st *state.Store, gm git.GitOps, logger *logging.Logger) *Loop {
	signals := claude.DefaultSignalPaths(cfg.Dirs.RalphDir)

	limiter := ratelimit.New(cfg.Dirs.RalphDir, cfg.CallsPerHour)
	limiter.StopFile = filepath.Join(cfg.Dirs.RalphDir, "stop")

	agentRunner := agent.New(logger)

	if cfg.CheckGitHub == nil {
		cfg.CheckGitHub = checkGitHubConnectivity
	}
	if cfg.ConnectivityCheckTimeout == 0 {
		cfg.ConnectivityCheckTimeout = 3 * time.Second
	}
	if cfg.InternetRestoreInterval == 0 {
		cfg.InternetRestoreInterval = 30 * time.Second
	}
	if cfg.TestTimeout == 0 {
		cfg.TestTimeout = 5 * time.Minute
	}
	if cfg.CompileCheckTimeout == 0 {
		cfg.CompileCheckTimeout = 60 * time.Second
	}
	if cfg.IsOnline == nil {
		checkTimeout := cfg.ConnectivityCheckTimeout
		cfg.IsOnline = func() bool { return isOnline(checkTimeout) }
	}
	if cfg.WaitForInternet == nil {
		interval := cfg.InternetRestoreInterval
		checkTimeout := cfg.ConnectivityCheckTimeout
		cfg.WaitForInternet = func(ctx context.Context, logger *logging.Logger) bool {
			return waitForInternet(ctx, logger, interval, checkTimeout)
		}
	}
	if cfg.NewRunner == nil {
		cfg.NewRunner = func() claudeRunner { return agent.New(logger) }
	}
	if cfg.QueryFn == nil {
		cfg.QueryFn = agentRunner.Query
	}

	at := attempts.New(cfg.Dirs.RalphDir)
	if cfg.MaxPromptAttempts > 0 {
		at.MaxPromptAttempts = cfg.MaxPromptAttempts
	}
	if cfg.MaxMergeFailures > 0 {
		at.MaxMergeFailures = cfg.MaxMergeFailures
	}
	if cfg.MaxIdleTimeoutFailures > 0 {
		at.MaxIdleTimeoutFailures = cfg.MaxIdleTimeoutFailures
	}
	l := &Loop{
		cfg:      cfg,
		state:    st,
		git:      gm,
		limiter:  limiter,
		runner:   agentRunner,
		analyzer: analyzer.New(),
		attempts: at,
		logger:   logger,
		signals:  signals,
	}
	l.verifier = NewVerifier(VerifierConfig{
		VerifyDir:             cfg.VerifyDir,
		ProjectDir:            cfg.Dirs.ProjectDir,
		VerifyModel:           cfg.VerifyModel,
		VerifyEscalationModel: cfg.VerifyEscalationModel,
		ModelCap:              cfg.ModelCap,
		PromptsDir:            cfg.Dirs.PromptsDir,
		RalphDir:              cfg.Dirs.RalphDir,
		IdleTimeout:           cfg.IdleTimeout,
		MaxLLMVerifyAttempts:  cfg.MaxLLMVerifyAttempts,
		MaxTestFixAttempts:    cfg.MaxTestFixAttempts,
		TestTimeout:           cfg.TestTimeout,
		CompileCheckTimeout:   cfg.CompileCheckTimeout,
	}, VerifierDeps{
		Logger:      logger,
		Git:         gm,
		State:       st,
		TaskBackend: cfg.TaskBackend,
		Runner:      func() claudeRunner { return l.runner },
		Signals:     signals,
		NewRunner:   func() claudeRunner { return l.cfg.NewRunner() },
		QueryFn:     cfg.QueryFn,
		LLMVerify:   verify.LLMVerifyPR,
		SkipTask: func(id, reason string) {
			skipTask(l.cfg.TaskBackend, l.state, l.logger, id, reason)
		},
	})
	return l
}

// SessionTasks returns the tasks completed during this session.
func (l *Loop) SessionTasks() []CompletedTask {
	return l.completedTasks
}

// ensureActiveReviewers populates l.activeReviewers on first call. Subsequent
// calls are no-ops. The loop is single-threaded so no synchronization is needed.
func (l *Loop) ensureActiveReviewers() {
	if l.reviewersDetected {
		return
	}
	l.reviewersDetected = true
	reviewers, err := l.git.DetectActiveReviewers()
	if err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Could not detect active reviewers: %v", err)
		return
	}
	l.activeReviewers = reviewers
	for _, r := range reviewers {
		if r.AppSlug == "copilot-code-review" {
			if r.ReviewOnPush {
				l.logger.Emit(logging.Opts{Domain: logging.Git}, "Copilot code review is enabled (review_on_push=true)")
			} else {
				l.logger.Emit(logging.Opts{Domain: logging.Git}, "Copilot code review is enabled (review_on_push=false, opportunistic)")
			}
		} else {
			l.logger.Emit(logging.Opts{Domain: logging.Git}, "%s code review is enabled", r.AppSlug)
		}
	}
}

// agentRunResult carries the outcome of a single agent invocation.
type agentRunResult struct {
	prep       iterationPrompt
	result     claude.Result
	iterAction analyzer.Action
	diffStat   string
	action     loopAction // actionDone or actionRetry if short-circuited; actionProceed otherwise
}

// runAgent prepares the prompt, invokes Claude, handles run errors, and analyzes
// the outcome. If action != actionProceed, Run() should use that action directly.
func (l *Loop) runAgent(ctx context.Context, task taskContext, runIteration int) agentRunResult {
	prep, ok := prepareAndBuildPrompt(ctx, prepareAndBuildPromptParams{
		backend:             l.cfg.TaskBackend,
		git:                 l.git,
		logger:              l.logger,
		verifier:            l.verifier,
		limiter:             l.limiter,
		attempts:            l.attempts,
		signals:             l.signals,
		promptsDir:          l.cfg.Dirs.PromptsDir,
		ralphDir:            l.cfg.Dirs.RalphDir,
		projectDir:          l.cfg.Dirs.ProjectDir,
		planFile:            l.cfg.PlanFile,
		callsPerHour:        l.cfg.CallsPerHour,
		runVerifyBuildFn: func(ctx context.Context) string {
			return runVerifyBuild(ctx, runVerifyBuildParams{
				verifyBuild: l.cfg.VerifyBuild,
				projectDir:  l.cfg.Dirs.ProjectDir,
				testTimeout: l.cfg.TestTimeout,
				logger:      l.logger,
			})
		},
		waitForInternetFunc: l.cfg.WaitForInternet,
	}, task.id, task.title)
	if !ok {
		return agentRunResult{action: actionDone}
	}

	taskStart := time.Now()
	agentModel := l.cfg.Model
	if l.attempts.Count(task.id, task.title) > 0 {
		agentModel = l.cfg.AgentEscalationModel
	}
	agentModel = verify.CapModel(l.cfg.ModelCap, agentModel)
	l.logger.Emit(logging.Opts{Domain: logging.LLM, Model: agentModel}, "Agent model: %s", agentModel)
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
		Model:               agentModel,
		Signals:             l.signals,
		PollInterval:        2 * time.Second,
		IdleTimeout:         l.cfg.IdleTimeout,
		IdleTimeoutProgress: l.cfg.IdleTimeoutProgress,
		HasProgress: func() bool {
			if l.git.HeadRev() != prep.headBefore {
				return true
			}
			if prep.diffBefore {
				return false
			}
			return l.git.HasDiff()
		},
		OnSignal: func(summary string) bool {
			return l.verifier.OnSignal(signalParams{
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

	runAction := handleRunResult(ctx, handleRunResultParams{
		result:              result,
		runErr:              runErr,
		taskID:              task.id,
		nextTask:            task.title,
		headBefore:          prep.headBefore,
		runIteration:        runIteration,
		model:               l.cfg.Model,
		isOnlineFunc:        l.cfg.IsOnline,
		waitForInternetFunc: l.cfg.WaitForInternet,
		logger:              l.logger,
		git:                 l.git,
		attempts:            l.attempts,
		limiter:             l.limiter,
		backend:             l.cfg.TaskBackend,
		state:               l.state,
		skipTask:            skipTask,
	})
	if runAction != actionProceed {
		return agentRunResult{action: runAction}
	}
	elapsed := time.Since(taskStart)
	l.limiter.Increment()

	diffStat, halt, iterAction := processRunOutcome(processRunOutcomeParams{
		backend:  l.cfg.TaskBackend,
		git:      l.git,
		logger:   l.logger,
		state:    l.state,
		attempts: l.attempts,
		analyzer: l.analyzer,
		signals:  l.signals,
		model:    l.cfg.Model,
	}, result, elapsed, runIteration, prep, task.id, task.title)
	if halt {
		return agentRunResult{action: actionDone, iterAction: iterAction}
	}

	return agentRunResult{
		prep:       prep,
		result:     result,
		iterAction: iterAction,
		diffStat:   diffStat,
		action:     actionProceed,
	}
}

// Run executes the full iteration loop. Returns nil on normal completion
// (all tasks done, max iterations reached, or stopped). Returns an error
// for unrecoverable failures.
func (l *Loop) Run(ctx context.Context) error {
	if l.cfg.VerifyDir != "" && verify.DetectTestCommand(l.cfg.VerifyDir, l.cfg.Dirs.ProjectDir) == nil {
		return fmt.Errorf("no ralph:verify script found in %s — add a \"ralph:verify\" script to package.json (or a make ralph-verify target) so the loop can verify task completion", l.cfg.Dirs.ProjectDir)
	}

	if err := l.cfg.CheckGitHub(ctx); err != nil {
		if ctx.Err() != nil {
			l.state.Write("status", "stopped")
			return nil
		}
		return fmt.Errorf("Cannot reach GitHub (%v).\nPossible causes: VPN blocking GitHub, no internet, gh auth expired.\nFixes: disconnect VPN, check internet, or run \"gh auth login\" to refresh credentials.", err)
	}

	if err := initialize(ctx, initParams{
		limiter: l.limiter,
		maxIter: l.cfg.MaxIterations,
		state:   l.state,
		backend: l.cfg.TaskBackend,
		logger:  l.logger,
		git:     l.git,
	}); err != nil {
		return err
	}

	var runIteration int
	var lastAction analyzer.Action
	var lastTaskMerged bool
	var sessionTasks []CompletedTask
	st, _ := l.state.Load()
	iteration := st.Iteration

	wtParams := waitForTasksParams{
		logger:     l.logger,
		state:      l.state,
		backend:    l.cfg.TaskBackend,
		onWaitFunc: l.cfg.OnWait,
	}

iterLoop:
	for {
		// ── Task selection ──
		completedIDs := make(map[string]bool, len(sessionTasks))
		for _, ct := range sessionTasks {
			completedIDs[ct.ID] = true
		}
		task, action := selectNextTask(ctx, selectNextTaskParams{
			runIteration:  runIteration,
			maxIterations: l.cfg.MaxIterations,
			backend:       l.cfg.TaskBackend,
			wait:          l.cfg.Wait,
			state:         l.state,
			logger:        l.logger,
			completedIDs:  completedIDs,
			waitForTasks:  func(ctx context.Context) bool { return waitForTasks(ctx, wtParams) },
			flushUnpushedWork: func(ctx context.Context) {
				flushUnpushedWork(ctx, flushUnpushedWorkParams{
					autoMerge:      l.cfg.AutoMerge,
					lastTaskMerged: lastTaskMerged,
					state:          l.state,
					git:            l.git,
					logger:         l.logger,
				})
			},
		})
		if action == actionDone {
			break
		}

		runIteration++
		iteration++
		lastTaskMerged = false

		if task.changed {
			l.verifier.ResetCounters()
		}

		// ── Branch setup ──
		if task.changed || !l.git.IsBranchRenamed() {
			if err := prepareBranch(ctx, branchParams{
				git:     l.git,
				backend: l.cfg.TaskBackend,
				state:   l.state,
				logger:  l.logger,
			}, task.id, task.title); err != nil {
				if ctx.Err() != nil {
					l.state.Write("status", "stopped")
				} else {
					l.state.Write("status", "error")
				}
				break
			}
		}

		if err := maybeRefactor(ctx, maybeRefactorParams{
			cfg:          l.cfg,
			git:          l.git,
			limiter:      l.limiter,
			runner:       l.runner,
			logger:       l.logger,
			signals:      l.signals,
			queryFn:      l.cfg.QueryFn,
			sessionCount: len(sessionTasks),
		}); err != nil {
			l.logger.Emit(logging.Opts{Level: logging.Warn}, "Refactor iteration error: %v", err)
		}

		if l.cfg.OnIterationStart != nil {
			l.cfg.OnIterationStart()
		}

		logIterationBanner(logIterationBannerParams{
			backend: l.cfg.TaskBackend,
			state:   l.state,
			logger:  l.logger,
			version: l.cfg.Version,
		}, runIteration, l.state.ReadMaxIterations(l.cfg.MaxIterations), iteration, task, lastAction)
		beginIteration(beginIterationParams{
			state: l.state,
			git:   l.git,
		}, task, iteration)

		// ── Resume check: does a PR already exist for this task? ──
		if resumeViaPR(ctx, resumeViaPRParams{
			taskID:    task.id,
			nextTask:  task.title,
			backend:   l.cfg.TaskBackend,
			git:       l.git,
			logger:    l.logger,
			attempts:  l.attempts,
			state:     l.state,
			autoMerge: l.cfg.AutoMerge,
			notify:    l.cfg.Notify,
			ralphDir:  l.cfg.Dirs.RalphDir,
			verifier:  l.verifier,
		}) {
			l.git.TagTaskEnd(task.id)
			continue
		}

		// ── Run agent ──
		agentRun := l.runAgent(ctx, task, runIteration)
		lastAction = agentRun.iterAction
		if agentRun.action != actionProceed {
			if agentRun.action == actionRetry {
				continue
			}
			break
		}

		// ── Complete task (post-signal pipeline) ──
		if agentRun.result.SignalDetected {
			out := completeTask(ctx, completeTaskParams{
				result:            agentRun.result,
				headBefore:        agentRun.prep.headBefore,
				workDir:           agentRun.prep.workDir,
				rawLogPath:        agentRun.prep.rawLogPath,
				diffStat:          agentRun.diffStat,
				taskID:            task.id,
				nextTask:          task.title,
				postSignalTimeout: l.cfg.PostSignalTimeout,
				autoMerge:         l.cfg.AutoMerge,
				evolve:            l.cfg.Evolve,
				notify:            l.cfg.Notify,
				ralphDir:          l.cfg.Dirs.RalphDir,
				logger:            l.logger,
				verifyFn: func(ctx context.Context, headBefore string) (bool, string) {
					if l.cfg.OnVerify != nil {
						return l.cfg.OnVerify(ctx, l.git.GetWorkDir(), headBefore)
					}
					return l.verifier.VerifyCompletion(ctx, l.git.GetWorkDir(), headBefore)
				},
				headRevFn:        l.git.HeadRev,
				worktreeBranchFn: l.git.GetWorktreeBranch,
				tagTaskEndFn:     l.git.TagTaskEnd,
				getPRStateFn:     l.git.GetPRState,
				findExistingPRFn: func(taskID, branch string) (int, bool) {
					return findExistingPRForTask(taskID, branch, l.cfg.TaskBackend, l.git)
				},
				getStateFn: func(taskID, key string) (string, error) {
					return l.cfg.TaskBackend.GetState(taskID, key)
				},
				setStateFn: func(taskID, key, value, reason string) error {
					return l.cfg.TaskBackend.SetState(taskID, key, value, reason)
				},
				closeTaskFn: l.cfg.TaskBackend.CloseTask,
				getExternalRefFn: func(taskID string) (string, error) {
					return l.cfg.TaskBackend.GetExternalRef(taskID)
				},
				getSkippedTasksFn: l.state.GetSkippedTasks,
				recordCompletedFn: func(taskID, nextTask string) {
					l.state.RecordCompletedTask(taskID, nextTask)
				},
				persistCompletedFn: func(taskID string, merged bool) {
					persistCompletedTask(l.state, l.logger, taskID, merged)
				},
				touchPlanFlashFn: l.state.TouchPlanFlash,
				writeStateFn: func(key, value string) {
					l.state.Write(key, value) //nolint:errcheck
				},
				recordAttemptFn: func(taskID, nextTask, reason, diffStat, note string) {
					l.attempts.Record(taskID, nextTask, reason, diffStat, note)
				},
				clearAttemptsFn: l.attempts.Clear,
				skipTaskFn: func(taskID, reason string) {
					skipTask(l.cfg.TaskBackend, l.state, l.logger, taskID, reason)
				},
				shipFn: func(ctx context.Context, taskID, title, summary string) (int, string) {
					prBody := buildPRBody(l.cfg.TaskBackend, taskID, summary)
					shipOpts := git.ShipOpts{TaskID: taskID, TaskTitle: title, Body: prBody}
					result, err := l.git.Ship(ctx, shipOpts)
					if err != nil {
						if !l.cfg.IsOnline() {
							l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Ship failed — internet appears down")
							l.cfg.WaitForInternet(ctx, l.logger)
							result, err = l.git.Ship(ctx, shipOpts)
						}
						if err != nil {
							l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Ship: %v", err)
						}
					}
					if result.PRNumber != 0 && taskID != "" {
						ref := result.PRURL
						if ref == "" {
							ref = prURL(l.git.RemoteURL(), result.PRNumber)
						}
						if ref != "" {
							l.logger.Emit(logging.Opts{Domain: logging.Git}, "Linking task %s to %s (branch: %s)", taskID, ref, l.git.GetWorktreeBranch())
							if refErr := l.cfg.TaskBackend.SetExternalRef(taskID, ref); refErr != nil {
								l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "SetExternalRef: %v", refErr)
							}
						}
					}
					return result.PRNumber, result.PRURL
				},
				finalizePRFn: func(ctx context.Context, taskID, nextTask string, prNumber int, prState git.PRState, prURL, workDir, rawLogPath string) finalizePRResult {
					l.ensureActiveReviewers()
					return finalizePR(finalizePRParams{
						ctx:             ctx,
						taskID:          taskID,
						nextTask:        nextTask,
						prNumber:        prNumber,
						prState:         prState,
						prURL:           prURL,
						workDir:         workDir,
						rawLogPath:      rawLogPath,
						autoMerge:       l.cfg.AutoMerge,
						activeReviewers: l.activeReviewers,
						git:             l.git,
						logger:          l.logger,
						backend:         l.cfg.TaskBackend,
						state:           l.state,
						attempts:        l.attempts,
						verifier:        l.verifier,
					})
				},
				buildCTFn: func(taskID, nextTask, summary string, prNumber int) CompletedTask {
					return buildCompletedTask(taskID, nextTask, summary, prNumber, l.git)
				},
				runPostTaskFn: func(ctx context.Context, taskID string, prNumber int, merged bool) {
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
				},
			})
			if out.ct != nil {
				sessionTasks = append(sessionTasks, *out.ct)
			}
			if out.merged {
				lastTaskMerged = true
			}
			switch out.action {
			case signalRetry, signalSkipped:
				continue
			case signalEvolve:
				break iterLoop
			}
			// signalComplete: fall through to tagTaskEnd
		}
		l.git.TagTaskEnd(task.id)
	}

	l.completedTasks = sessionTasks
	return nil
}
