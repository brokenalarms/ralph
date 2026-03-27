package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	Wait                bool
	WaitInterval        time.Duration
	Verbose             bool
	OnRebaseConflict    func(err error) git.RebaseRecovery
	Version             string
	VerifyDir             string // project root where tests are run; empty disables verification
	VerifyLevel           string // "fire" (default) or "hog" — controls no-diff verification depth
	VerifyModel           string // model for first LLM verification attempt
	VerifyEscalationModel string // model for subsequent LLM verification attempts
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
	git        *git.Manager
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
	lastAction         analyzer.Action
	lastTaskMerged     bool
	sessionTasks       []CompletedTask
}

// New creates an execution loop from the given configuration. All agent
// invocations go through the centralized agent module.
func New(cfg Config, st *state.Store, gm *git.Manager, logger *logging.Logger) *Loop {
	signals := claude.DefaultSignalPaths(cfg.Dirs.RalphDir)

	limiter := ratelimit.New(cfg.Dirs.RalphDir, cfg.CallsPerHour)
	limiter.StopFile = filepath.Join(cfg.Dirs.RalphDir, "stop")

	agentRunner := agent.New(logger)

	l := &Loop{
		cfg:           cfg,
		state:         st,
		git:           gm,
		limiter:       limiter,
		runner:        agentRunner,
		analyzer:      analyzer.New(),
		attempts:      attempts.New(cfg.Dirs.RalphDir),
		logger:        logger,
		signals:       signals,
		agentRunner:   agentRunner,
	}
	l.verifier = NewVerifier(VerifierConfig{
		VerifyDir:             cfg.VerifyDir,
		VerifyLevel:           cfg.VerifyLevel,
		VerifyModel:           cfg.VerifyModel,
		VerifyEscalationModel: cfg.VerifyEscalationModel,
		PromptsDir:            cfg.Dirs.PromptsDir,
		RalphDir:              cfg.Dirs.RalphDir,
		IdleTimeout:           cfg.IdleTimeout,
	}, VerifierDeps{
		Logger:      logger,
		Git:         gm,
		GitHub:      gm.GitHub,
		State:       st,
		TaskBackend: cfg.TaskBackend,
		Runner:      func() claudeRunner { return l.runner },
		Signals:     signals,
		NewRunner:   l.newRunner,
		QueryFn:     l.queryFunc(),
		LLMVerify:   verify.LLMVerifyPR,
		SkipTask: func(id, reason string) {
			skipTask(l.state, l.cfg.TaskBackend, l.logger, id, reason)
		},
	})
	return l
}

// SessionTasks returns the tasks completed during this session.
func (l *Loop) SessionTasks() []CompletedTask {
	return l.sessionTasks
}

// Run executes the full iteration loop. Returns nil on normal completion
// (all tasks done, max iterations reached, or stopped). Returns an error
// for unrecoverable failures.
func (l *Loop) Run(ctx context.Context) error {
	if err := l.initRun(ctx); err != nil {
		return err
	}
	if err := l.limiter.Init(); err != nil {
		return fmt.Errorf("rate limiter init: %w", err)
	}
	l.state.WriteConfig(l.cfg.MaxIterations)

	var runIteration int
	st, _ := l.state.Load()
	iteration := st.Iteration
	if len(st.SkippedTasks) > 0 {
		l.logger.Warn("beads", "Skipped tasks: %s", strings.Join(st.SkippedTasks, ", "))
		l.cfg.TaskBackend.SetSkippedIDs(st.SkippedTasks)
	}
	os.Remove(filepath.Join(l.cfg.Dirs.RalphDir, ".completed-tasks"))

	for {
		maxIter := l.state.ReadMaxIterations(l.cfg.MaxIterations)

		if runIteration >= maxIter {
			l.logger.Warn("", "Max iterations (%d) reached", maxIter)
			l.state.Write("status", "max_iterations_reached")
			break
		}

		if err := ctx.Err(); err != nil {
			l.logger.Warn("", "Interrupted — stopping")
			l.state.Write("status", "stopped")
			return nil
		}

		if checkStopFile(l.cfg.Dirs.RalphDir) {
			l.logger.Warn("", "Stop file detected - halting")
			l.state.Write("status", "stopped")
			break
		}

		hasRemaining, err := l.cfg.TaskBackend.HasRemaining()
		if err != nil {
			l.logger.Warn("beads", "Task check error: %v", err)
		}
		if !hasRemaining {
			if runIteration == 0 {
				hasTasks, _ := l.cfg.TaskBackend.HasTasks()
				if !hasTasks && !l.cfg.Wait {
					l.logger.Error("beads", "No tasks found — run ralph task to create tasks")
					l.state.Write("status", "error")
					break
				}
			}
			if runIteration > 0 {
				l.flushUnpushedWork(ctx)
			}
			if !l.cfg.Wait {
				l.logger.Success("beads", "All tasks complete!")
				l.state.Write("status", "completed")
				break
			}
			if resumed := l.waitForTasks(ctx); !resumed {
				break
			}
			continue
		}

		runIteration++
		iteration++
		l.lastTaskMerged = false

		taskInfo, _ := l.cfg.TaskBackend.GetNextTaskInfo()
		taskID, nextTask := taskInfo.ID, taskInfo.Title
		taskChanged := isNewTask(l.state, taskID, nextTask)
		if taskChanged {
			l.verifier.ResetCounters()
		}

		if runIteration > 1 && taskChanged {
			l.git.PrepareForNextTask()
			if l.git.WorktreeBranch != "" && l.git.WorkDir != l.git.ProjectDir {
				l.setStackHead()
				if l.git.PrevBranch == "" {
					l.git.ResetToDefaultBranch()
				}
				if err := l.handleRebase(ctx); err != nil {
					if ctx.Err() != nil {
						l.state.Write("status", "stopped")
					} else {
						l.state.Write("status", "error")
					}
					break
				}
			}
		}

		if err := l.maybeRefactor(); err != nil {
			l.logger.Warn("", "Refactor iteration error: %v", err)
		}

		l.logIterationBanner(runIteration, maxIter, iteration, taskID, nextTask, taskChanged, taskInfo)
		touchFile(filepath.Join(l.cfg.Dirs.RalphDir, ".plan-refresh"))

		l.state.Write("iteration", strconv.Itoa(iteration))
		l.state.Write("status", "running")
		l.state.Write("last_task", nextTask)
		l.state.Write("last_task_id", taskID)

		if taskChanged || !l.git.BranchRenamed {
			l.setStackHead()
			l.checkoutExistingBranch(taskID, nextTask)
		}
		writeRunBranch(l.cfg.Dirs.RalphDir, l.git.WorktreeBranch)
		l.git.TagTaskStart(taskID)
		updateStreamTask(l.cfg.Dirs.RalphDir, taskID, nextTask, taskInfo.Priority)

		if resumed := l.resumeViaPR(ctx, taskID, nextTask); resumed {
			l.git.TagTaskEnd(taskID)
			runIteration++
			iteration++
			continue
		}

		prep, ok := l.prepareAndBuildPrompt(ctx, taskID, nextTask)
		if !ok {
			break
		}

		taskStart := time.Now()
		result, runErr := l.runner.Run(claude.RunConfig{
			Ctx:                 ctx,
			WorkDir:             prep.workDir,
			RalphDir:            l.cfg.Dirs.RalphDir,
			Prompt:              prep.fullPrompt,
			TaskID:              taskID,
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
					taskID:     taskID,
					nextTask:   nextTask,
				})
			},
			FeedbackFile: filepath.Join(l.cfg.Dirs.RalphDir, "feedback"),
		})

		action := l.handleRunResult(ctx, result, runErr, taskID, nextTask, prep.headBefore, &runIteration, &iteration)
		if action == resultRetry {
			continue
		}
		if action == resultBreak {
			break
		}
		elapsed := time.Since(taskStart)
		l.limiter.Increment()

		diffStat, halt := l.processRunOutcome(result, elapsed, runIteration, prep, taskID, nextTask)
		if halt {
			return nil
		}

		if result.SignalDetected {
			signalAction := l.handlePostSignal(postSignalParams{
				ctx:        ctx,
				result:     result,
				headBefore: prep.headBefore,
				workDir:    prep.workDir,
				rawLogPath: prep.rawLogPath,
				taskID:     taskID,
				nextTask:   nextTask,
				diffStat:   diffStat,
			}, &runIteration, &iteration)

			switch signalAction {
			case signalRetry, signalSkipped:
				continue
			case signalEvolve:
				return nil
			}
		}

		l.git.TagTaskEnd(taskID)
	}

	return nil
}
