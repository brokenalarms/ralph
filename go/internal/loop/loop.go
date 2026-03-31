package loop

import (
	"context"
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
	Notify              bool
	Wait                bool
	Verbose             bool
	Model               string
	OnRebaseConflict    func(err error) git.RebaseRecovery
	Version             string
	VerifyDir             string // project root where tests are run; empty disables verification
	VerifyModel           string // model for first LLM verification attempt
	VerifyEscalationModel string // model for subsequent LLM verification attempts
	OnIterationStart      func() // called at the start of each iteration (e.g. to regenerate resume script)
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
	PRNum   string
	PRTitle string
	PRURL   string
}

// Loop orchestrates the execution phase: task selection, prompt building,
// rate limiting, branch rotation, Claude invocation, and response analysis.
type Loop struct {
	cfg        Config
	state      *state.Store
	git        git.GitOps
	limiter    *ratelimit.Limiter
	runner     claudeRunner
	verifier   *Verifier
	analyzer   *analyzer.Analyzer
	attempts   *attempts.Tracker
	logger     *logging.Logger
	signals    claude.SignalPaths
	mergeFunc          func(ctx context.Context) (bool, error)
	pushPRFunc         func(ctx context.Context, taskID, taskDesc, body string) (string, error)
	verifyFunc      func(ctx context.Context, dir, headBefore string) (passed bool, reason string)
	newRunnerFunc      func() claudeRunner
	findPRInfoFunc     func(workDir string) (number, title string)
	agentRunner        *agent.Runner
	refactorQueryFunc  func(ctx context.Context, workDir, prompt, model string) (string, error)
	isOnlineFunc       func() bool
	waitForInternetFunc func(ctx context.Context, logger *logging.Logger) bool
	onWaitFunc         func() // called when the loop enters waitForTasks (test hook)
	lastAction         analyzer.Action
	lastTaskMerged     bool
	sessionTasks       []CompletedTask
}

// New creates an execution loop from the given configuration. All agent
// invocations go through the centralized agent module.
func New(cfg Config, st *state.Store, gm git.GitOps, logger *logging.Logger) *Loop {
	signals := claude.DefaultSignalPaths(cfg.Dirs.RalphDir)

	limiter := ratelimit.New(cfg.Dirs.RalphDir, cfg.CallsPerHour)
	limiter.StopFile = filepath.Join(cfg.Dirs.RalphDir, "stop")

	agentRunner := agent.New(logger)

	l := &Loop{
		cfg:                 cfg,
		state:               st,
		git:                 gm,
		limiter:             limiter,
		runner:              agentRunner,
		analyzer:            analyzer.New(),
		attempts:            attempts.New(cfg.Dirs.RalphDir),
		logger:              logger,
		signals:             signals,
		agentRunner:         agentRunner,
		isOnlineFunc:        isOnline,
		waitForInternetFunc: waitForInternet,
	}
	l.verifier = NewVerifier(VerifierConfig{
		VerifyDir:             cfg.VerifyDir,
		VerifyModel:           cfg.VerifyModel,
		VerifyEscalationModel: cfg.VerifyEscalationModel,
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
		NewRunner:   l.newRunner,
		QueryFn:     l.queryFunc(),
		LLMVerify:   verify.LLMVerifyPR,
		SkipTask: func(id, reason string) {
			skipTask(l.cfg.TaskBackend, l.state, l.logger, id, reason)
		},
	})
	return l
}

// SessionTasks returns the tasks completed during this session.
func (l *Loop) SessionTasks() []CompletedTask {
	return l.sessionTasks
}

// wasCompletedThisSession returns true if the given task ID was already
// completed in this session. Prevents the loop from re-selecting a task
// that the backend keeps returning after close.
func (l *Loop) wasCompletedThisSession(taskID string) bool {
	if taskID == "" {
		return false
	}
	for _, ct := range l.sessionTasks {
		if ct.ID == taskID {
			return true
		}
	}
	return false
}

// Run executes the full iteration loop. Returns nil on normal completion
// (all tasks done, max iterations reached, or stopped). Returns an error
// for unrecoverable failures.
func (l *Loop) Run(ctx context.Context) error {
	if err := l.initialize(ctx); err != nil {
		return err
	}

	var runIteration int
	st, _ := l.state.Load()
	iteration := st.Iteration

	for {
		// ── Task selection ──
		task, action := l.selectNextTask(ctx, runIteration)
		if action == actionDone {
			break
		}

		runIteration++
		iteration++
		l.lastTaskMerged = false

		if task.changed {
			l.verifier.ResetCounters()
		}

		// ── Branch setup ──
		if task.changed || !l.git.IsBranchRenamed() {
			if err := l.prepareBranch(ctx, task.id, task.title); err != nil {
				if ctx.Err() != nil {
					l.state.Write("status", "stopped")
				} else {
					l.state.Write("status", "error")
				}
				break
			}
		}

		if err := l.maybeRefactor(); err != nil {
			l.logger.Warn("", "Refactor iteration error: %v", err)
		}

		if l.cfg.OnIterationStart != nil {
			l.cfg.OnIterationStart()
		}

		l.logIterationBanner(runIteration, l.state.ReadMaxIterations(l.cfg.MaxIterations), iteration, task)
		l.beginIteration(task, iteration)

		// ── Resume check: does a PR already exist for this task? ──
		if l.resumeViaPR(ctx, task.id, task.title) {
			l.git.TagTaskEnd(task.id)
			continue
		}

		// ── Run agent and handle outcome ──
		action = l.runAndComplete(ctx, task, runIteration)
		if action == actionRetry {
			continue
		}
		if action == actionDone {
			break
		}
	}

	return nil
}
