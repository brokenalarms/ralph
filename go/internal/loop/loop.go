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
	Model               string
	ModelCap            string // maximum model tier for all LLM calls; empty means no cap
	OnRebaseConflict    func(err error) git.RebaseRecovery
	Version             string
	VerifyDir             string // project root where tests are run; empty disables verification
	VerifyModel           string // unused; kept for config compatibility
	VerifyEscalationModel string // model for all LLM verification attempts; defaults to sonnet
	OnIterationStart      func() // called at the start of each iteration (e.g. to regenerate resume script)
	// hooks for test injection; nil uses the real implementation
	OnVerify        func(ctx context.Context, dir, headBefore string) (bool, string)
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

	if cfg.IsOnline == nil {
		cfg.IsOnline = isOnline
	}
	if cfg.WaitForInternet == nil {
		cfg.WaitForInternet = waitForInternet
	}
	if cfg.NewRunner == nil {
		cfg.NewRunner = func() claudeRunner { return agent.New(logger) }
	}
	if cfg.QueryFn == nil {
		cfg.QueryFn = agentRunner.Query
	}

	l := &Loop{
		cfg:      cfg,
		state:    st,
		git:      gm,
		limiter:  limiter,
		runner:   agentRunner,
		analyzer: analyzer.New(),
		attempts: attempts.New(cfg.Dirs.RalphDir),
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

// Run executes the full iteration loop. Returns nil on normal completion
// (all tasks done, max iterations reached, or stopped). Returns an error
// for unrecoverable failures.
func (l *Loop) Run(ctx context.Context) error {
	if l.cfg.VerifyDir != "" && verify.DetectTestCommand(l.cfg.VerifyDir, l.cfg.Dirs.ProjectDir) == nil {
		return fmt.Errorf("no ralph:verify script found in %s — add a \"ralph:verify\" script to package.json (or a make ralph-verify target) so the loop can verify task completion", l.cfg.Dirs.ProjectDir)
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

		// ── Run agent and handle outcome ──
		var iterAction analyzer.Action
		var merged bool
		var ct *CompletedTask
		action, iterAction, merged, ct = runAndComplete(ctx, runAndCompleteParams{
			git:                  l.git,
			logger:               l.logger,
			runner:               l.runner,
			verifier:             l.verifier,
			state:                l.state,
			attempts:             l.attempts,
			limiter:              l.limiter,
			signals:              l.signals,
			backend:              l.cfg.TaskBackend,
			analyzer:             l.analyzer,
			quiet:                l.cfg.Quiet,
			verbose:              l.cfg.Verbose,
			model:                l.cfg.Model,
			idleTimeout:          l.cfg.IdleTimeout,
			idleTimeoutProgress:  l.cfg.IdleTimeoutProgress,
			postSignalTimeout:    l.cfg.PostSignalTimeout,
			autoMerge:            l.cfg.AutoMerge,
			evolve:               l.cfg.Evolve,
			notify:               l.cfg.Notify,
			ensureReviewersFn:   func() []git.Reviewer { l.ensureActiveReviewers(); return l.activeReviewers },
			ralphDir:            l.cfg.Dirs.RalphDir,
			promptsDir:          l.cfg.Dirs.PromptsDir,
			projectDir:          l.cfg.Dirs.ProjectDir,
			planFile:            l.cfg.PlanFile,
			callsPerHour:        l.cfg.CallsPerHour,
			runVerifyBuildFn: func(ctx context.Context) string {
				return runVerifyBuild(ctx, runVerifyBuildParams{
					verifyBuild: l.cfg.VerifyBuild,
					projectDir:  l.cfg.Dirs.ProjectDir,
					logger:      l.logger,
				})
			},
			isOnlineFunc:        l.cfg.IsOnline,
			waitForInternetFunc: l.cfg.WaitForInternet,
			verifyFunc:          l.cfg.OnVerify,
			runPostTaskFn: func(ctx context.Context, taskID string, prNumber int, merged bool) {
				runPostTask(ctx, runPostTaskParams{
					postTask:    l.cfg.PostTask,
					worktreeDir: l.cfg.VerifyDir,
					projectDir:  l.cfg.Dirs.ProjectDir,
					logger:      l.logger,
				}, taskID, prNumber, merged)
			},
		}, task, runIteration)
		if ct != nil {
			sessionTasks = append(sessionTasks, *ct)
		}
		if merged {
			lastTaskMerged = true
		}
		lastAction = iterAction
		if action == actionRetry {
			continue
		}
		if action == actionDone {
			break
		}
	}

	l.completedTasks = sessionTasks
	return nil
}
