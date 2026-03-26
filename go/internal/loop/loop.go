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
	"github.com/brokenalarms/ralph/internal/health"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
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
	Wait                bool
	WaitInterval        time.Duration
	OnRebaseConflict    func(err error) git.RebaseRecovery
	Version             string
	VerifyDir           string // project root where tests are run; empty disables verification
	VerifyLevel         string // "fire" (default) or "hog" — controls no-diff verification depth
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
	analyzer   *analyzer.Analyzer
	attempts   *attempts.Tracker
	logger     *logging.Logger
	signals    claude.SignalPaths
	mergeFunc          func(ctx context.Context) (bool, error)
	pushPRFunc         func(ctx context.Context, taskID, taskDesc string) (string, error)
	verifyFunc      func(ctx context.Context, dir, headBefore string) (passed bool, reason string)
	llmVerifyFunc   func(ctx context.Context, gq verify.GitQuerier, workDir, promptsDir, taskID, headBefore, beadTitle, beadDescription, beadAcceptance string, gh git.GitHub, queryFn verify.QueryFunc, model ...string) verify.Result
	newRunnerFunc      func() claudeRunner
	findPRInfoFunc     func(workDir string) (number, title string)
	agentRunner        *agent.Runner
	refactorQueryFunc  func(ctx context.Context, workDir, prompt, model string) (string, error)
	lastAction         analyzer.Action
	lastTaskMerged     bool
	sessionTasks       []CompletedTask
	testFixAttempts    int
	llmVerifyAttempts  int
}

// New creates an execution loop from the given configuration. All agent
// invocations go through the centralized agent module, which applies
// container isolation when sandbox-exec is available on the host.
func New(cfg Config, st *state.Store, gm *git.Manager, logger *logging.Logger) *Loop {
	signals := claude.DefaultSignalPaths(cfg.Dirs.RalphDir)

	limiter := ratelimit.New(cfg.Dirs.RalphDir, cfg.CallsPerHour)
	limiter.StopFile = filepath.Join(cfg.Dirs.RalphDir, "stop")

	var sandbox *agent.Sandbox
	if agent.Available() {
		sandbox = agent.DefaultSandbox()
	}
	agentRunner := agent.New(logger, sandbox)

	return &Loop{
		cfg:           cfg,
		state:         st,
		git:           gm,
		limiter:       limiter,
		runner:        agentRunner,
		analyzer:      analyzer.New(),
		attempts:      attempts.New(cfg.Dirs.RalphDir),
		logger:        logger,
		signals:       signals,
		llmVerifyFunc: verify.LLMVerifyPR,
		agentRunner:   agentRunner,
	}
}

// SessionTasks returns the tasks completed during this session.
func (l *Loop) SessionTasks() []CompletedTask {
	return l.sessionTasks
}

// Run executes the full iteration loop. Returns nil on normal completion
// (all tasks done, max iterations reached, or stopped). Returns an error
// for unrecoverable failures.
func (l *Loop) Run(ctx context.Context) error {
	if l.git.WorktreeBranch != "" && l.git.WorkDir != l.git.ProjectDir {
		if err := l.handleRebase(ctx); err != nil {
			if ctx.Err() != nil {
				l.state.Write("status", "stopped")
				return nil
			}
			l.state.Write("status", "error")
			return fmt.Errorf("initial rebase failed: %w", err)
		}

		// On resume, if the next task is the same, mark branch as renamed
		// so we continue on the current branch.
		nextInfo, _ := l.cfg.TaskBackend.GetNextTaskInfo()
		if !l.isNewTask(nextInfo.ID, nextInfo.Title) {
			l.git.BranchRenamed = true
		} else {
			l.git.PrepareForNextTask()
		}

		l.logger.Log("git", "Branch: %s", l.git.WorktreeBranch)
		l.writeRunBranch()
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

	// Clear completed-tasks tracker for this run so the plan pane only
	// shows tasks completed in the current run, not historical closures.
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

		if l.checkStopFile() {
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
				if !hasTasks {
					if !l.cfg.Wait {
						l.logger.Error("beads", "No tasks found — run ralph task to create tasks")
						l.state.Write("status", "error")
						break
					}
				}
			}
			// Safety net: flush any unpushed work from the last task before
			// exiting or entering wait mode. PushAndCreatePR is idempotent
			// (returns early when no commits ahead), so this is harmless if
			// the signal handler already pushed successfully.
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
		taskChanged := l.isNewTask(taskID, nextTask)
		if taskChanged {
			l.testFixAttempts = 0
			l.llmVerifyAttempts = 0
		}

		if runIteration > 1 && taskChanged {
			l.git.PrepareForNextTask()
			if l.git.WorktreeBranch != "" && l.git.WorkDir != l.git.ProjectDir {
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

		completed, _ := l.cfg.TaskBackend.CountCompleted()
		total, _ := l.cfg.TaskBackend.CountTotal()

		if runIteration > 1 {
			health.Log(l.logger, health.Collect(l.cfg.Dirs.RalphDir, l.git.WorkDir))
			l.logger.DashedSeparator(logging.Yellow)
		}

		if taskID != "" && taskChanged {
			l.logger.TaskBanner(taskID, nextTask, taskInfo.Priority)
		}

		phaseColor := logging.Green
		if l.lastAction == analyzer.Warn {
			phaseColor = logging.Yellow
		}
		versionTag := ""
		if l.cfg.Version != "" {
			versionTag = fmt.Sprintf(" | Ralph v%s", l.cfg.Version)
		}
		l.logger.PhaseColor(phaseColor, "--- Run iteration %d/%d | %d lifetime [%d/%d done]%s ---",
			runIteration, maxIter, iteration, completed, total, versionTag)
		if desc := l.getBeadDescription(taskID); desc != "" {
			l.logger.Log("beads", "  %s", desc)
		}

		touchFile(filepath.Join(l.cfg.Dirs.RalphDir, ".plan-refresh"))

		l.state.Write("iteration", strconv.Itoa(iteration))
		l.state.Write("status", "running")
		l.state.Write("last_task", nextTask)
		l.state.Write("last_task_id", taskID)
		if taskChanged || !l.git.BranchRenamed {
			l.git.RenameBranchForTask(nextTask, taskID)
		}
		l.writeRunBranch()
		l.git.TagTaskStart(taskID)

		l.updateStreamTask(taskID, nextTask, taskInfo.Priority)

		// PR-based resume: check the bead's external-ref for a linked PR.
		if resumed := l.resumeViaPR(ctx, taskID, nextTask); resumed {
			l.git.TagTaskEnd(taskID)
			runIteration++
			iteration++
			continue
		}

		if taskID != "" {
			if err := l.cfg.TaskBackend.SetState(taskID, "phase", "implementing", "ralph: starting task"); err != nil {
				l.logger.Warn("beads", "SetState phase=implementing: %v", err)
			}
		}

		taskPrompt := l.buildTaskPrompt(nextTask, taskID)

		// Run full test suite before handing off to agent. The agent only
		// runs scoped tests during development; the orchestrator owns the
		// full suite both here (pre-iteration) and after signal (gate).
		testStatus := l.runPreIterationTests(ctx)

		if !l.waitForInternet(ctx) {
			break
		}
		if !l.waitForRate(ctx) {
			break
		}

		headBefore := l.git.HeadRev()
		rawLogPath := filepath.Join(l.cfg.Dirs.RalphDir, "raw.log")
		logStart := fileLineCount(rawLogPath)

		feedback := l.readFeedback()
		if feedback != "" {
			l.logger.Warn("", "[feedback] %s", feedback)
			l.clearFeedback()
			l.attempts.Record(taskID, nextTask,
				"User feedback (pre-iteration): "+feedback,
				"",
				"user_feedback: "+feedback)
		}

		attemptContext := l.buildAttemptContext(taskID, nextTask)
		if attemptContext != "" {
			attemptCount := strings.Count(attemptContext, "### Attempt ")
			reflectionCount := strings.Count(attemptContext, "## Recent learnings")
			if attemptCount > 0 || reflectionCount > 0 {
				l.logger.Log("", "Including prior context: %d attempt(s), cross-task learnings: %v", attemptCount, reflectionCount > 0)
			}
		}

		fullPrompt, err := l.buildPrompt(taskPrompt, attemptContext, testStatus)
		if err != nil {
			l.logger.Error("", "Prompt build failed: %v", err)
			break
		}

		workDir := l.git.WorkDir
		taskStart := time.Now()
		result, runErr := l.runner.Run(claude.RunConfig{
			Ctx:                 ctx,
			WorkDir:             workDir,
			RalphDir:            l.cfg.Dirs.RalphDir,
			Prompt:              fullPrompt,
			TaskID:              taskID,
			RawLog:              rawLogPath,
			LogFile:             filepath.Join(l.cfg.Dirs.RalphDir, "loop.log"),
			Quiet:               l.cfg.Quiet,
			Signals:             l.signals,
			PollInterval:        2 * time.Second,
			IdleTimeout:         l.cfg.IdleTimeout,
			IdleTimeoutProgress: l.cfg.IdleTimeoutProgress,
			HasProgress: func() bool {
				return l.git.HasDiff() || l.git.HeadRev() != headBefore
			},
			OnSignal: func(summary string) bool {
				return l.onSignal(signalParams{
					ctx:        ctx,
					headBefore: headBefore,
					workDir:    workDir,
					rawLogPath: rawLogPath,
					taskID:     taskID,
					nextTask:   nextTask,
				})
			},
			FeedbackFile: filepath.Join(l.cfg.Dirs.RalphDir, "feedback"),
		})
		if runErr != nil {
			if !isOnline() {
				l.logger.Warn("llm", "Claude failed — internet appears down")
				if !l.waitForInternet(ctx) {
					break
				}
				runIteration--
				iteration--
				continue
			}
			l.logger.Warn("llm", "Claude failed on iteration %d, continuing...", runIteration)
		}
		if result.FeedbackKill {
			// Fallback path: stdin injection failed, agent was killed.
			l.logger.Warn("llm", "Restarting iteration %d — feedback injection failed, agent killed", runIteration)
			diffStat := l.git.DiffStatRange(headBefore, l.git.HeadRev())
			l.attempts.Record(taskID, nextTask,
				"Killed: feedback injection failed. Feedback: "+result.FeedbackContent,
				diffStat,
				"user_feedback: "+result.FeedbackContent)
			runIteration--
			iteration--
			continue
		}
		if result.IdleTimeout {
			l.logger.Warn("llm", "Restarting iteration %d after idle timeout", runIteration)
			diffStat := l.git.DiffStatRange(headBefore, l.git.HeadRev())
			l.attempts.Record(taskID, nextTask,
				"Killed: idle timeout (no output for configured duration)",
				diffStat,
				"idle_timeout: consider a lighter approach or make incremental progress rather than deep-thinking without output")
			runIteration--
			iteration--
			continue
		}
		if result.RateLimited {
			waitDur := claude.FormatWaitDuration(time.Until(result.ResetAt))
			l.logger.Warn("llm", "Claude rate limit — waiting %s until %s", waitDur, result.ResetAt.Format("3:04pm"))
			err := l.limiter.WaitUntil(ctx, result.ResetAt, func(secs int) {
				l.logger.Log("llm", "Rate limit: %ds until reset", secs)
			})
			if err != nil {
				l.logger.Warn("llm", "Rate limit wait interrupted: %v", err)
				break
			}
			l.logger.Success("llm", "Rate limit reset — resuming")
			runIteration--
			iteration--
			continue
		}
		elapsed := time.Since(taskStart)
		l.limiter.Increment()


		if result.Summary != "" {
			l.logger.Log("llm", "Summary: %s", result.Summary)
		}

		completed, _ = l.cfg.TaskBackend.CountCompleted()
		total, _ = l.cfg.TaskBackend.CountTotal()
		l.logger.Log("", "Run iteration %d complete (%dm%ds). %d/%d tasks done.",
			runIteration, int(elapsed.Minutes()), int(elapsed.Seconds())%60, completed, total)

		headAfter := l.git.HeadRev()
		diffStat := l.git.DiffStatRange(headBefore, headAfter)
		analysisResult := l.analyzeIteration(rawLogPath, logStart, headBefore, headAfter, taskID)

		summary := result.Summary
		if summary == "" {
			summary = "no completion summary"
		}
		analysisDesc := analysisResult.Reason
		if analysisDesc == "" {
			analysisDesc = "continue"
		}

		l.lastAction = analysisResult.Action

		switch analysisResult.Action {
		case analyzer.Halt:
			l.logger.Error("", "Halting: %s", analysisResult.Reason)
			if analysisResult.Detail != "" {
				l.logger.Error("", "  %s", analysisResult.Detail)
			}
			l.attempts.Record(taskID, nextTask, "Halted: "+analysisResult.Reason, diffStat, analysisResult.Detail)
			l.state.Write("status", "halted_"+analysisResult.Reason)
			l.git.TagTaskEnd(taskID)
			return nil
		case analyzer.Warn:
			l.logger.Warn("", "Analysis: %s", analysisResult.Reason)
			l.attempts.Record(taskID, nextTask, summary, diffStat, "warn: "+analysisDesc)
		default:
			if !result.SignalDetected {
				l.attempts.Record(taskID, nextTask, summary, diffStat, analysisDesc)
			}
		}

		if result.SignalDetected {
			// Preflight: check bead wasn't prematurely closed by the agent.
			if taskID != "" {
				phase, _ := l.cfg.TaskBackend.GetState(taskID, "phase")
				if phase != "implementing" {
					l.logger.Warn("beads", "Task %s phase is %q (expected implementing) — agent may have tampered with task state", taskID, phase)
				}
			}

			// If OnSignal was set, verification already passed in the runner.
			// If not (legacy/test path), run verification here as fallback.
			if result.OnSignalUsed == false {
				if passed, reason := l.verifyCompletion(ctx, headBefore); !passed {
					l.logger.Warn("test", "Verification failed: %s", reason)
					l.attempts.Record(taskID, nextTask,
						"Signal received but verification failed: "+reason,
						diffStat,
						"verification_failed: fix must pass tests and produce commits before closing")
					continue
				}
			}

			if taskID != "" {
				if err := l.cfg.TaskBackend.SetState(taskID, "phase", "verified", "ralph: tests passed, commits present"); err != nil {
					l.logger.Warn("beads", "SetState phase=verified: %v", err)
				} else {
					l.logger.Log("beads", "%s → verified", taskID)
				}
			}

			l.attempts.Clear(taskID, nextTask)
			l.recordCompletedTask(taskID, nextTask)
			touchFile(filepath.Join(l.cfg.Dirs.RalphDir, ".plan-flash"))

			// PushAndCreatePR calls EnsureUpToDate internally, which
			// rebases onto the latest base branch before pushing.
			prNumber, pushErr := l.pushAndCreatePR(ctx, taskID, nextTask)
			if pushErr != nil {
				if !isOnline() {
					l.logger.Warn("git", "Push failed — internet appears down")
					l.waitForInternet(ctx)
					prNumber, pushErr = l.pushAndCreatePR(ctx, taskID, nextTask)
				}
				if pushErr != nil {
					l.logger.Warn("git", "Push/PR: %v", pushErr)
				}
			}
			if prNumber != "" && taskID != "" {
				_, _, prURL := l.findPRInfo(workDir)
				ref := prURL
				if ref == "" {
					ref = "gh-" + prNumber
				}
				if refErr := l.cfg.TaskBackend.SetExternalRef(taskID, ref); refErr != nil {
					l.logger.Warn("beads", "SetExternalRef: %v", refErr)
				}
			}

			ct := CompletedTask{
				ID:      taskID,
				Title:   nextTask,
				Summary: result.Summary,
				PRNum:   prNumber,
			}
			if prNum, prTitle, prURL := l.findPRInfo(workDir); prNum != "" {
				ct.PRNum = prNum
				ct.PRTitle = prTitle
				ct.PRURL = prURL
			} else if prNumber != "" {
				ct.PRNum = prNumber
			}

			merged := false
			if l.cfg.AutoMerge && prNumber != "" {
				var mergeErr error
				merged, mergeErr = l.mergeWithRetry(ctx, taskID, nextTask, workDir, rawLogPath)
				if mergeErr != nil {
					l.logger.Warn("git", "Auto-merge: %v", mergeErr)
				}
			}

			// No new PR created — check the bead's existing PR.
			if prNumber == "" && taskID != "" && l.cfg.AutoMerge {
				ref, _ := l.cfg.TaskBackend.GetExternalRef(taskID)
				if existingPR := parsePRNumber(ref); existingPR != "" {
					gh := l.git.GH()
					if gh != nil {
						prState, _ := gh.GetPRState(workDir, existingPR)
						switch strings.ToUpper(prState) {
						case "MERGED":
							l.logger.Log("git", "PR #%s already merged — work is on main", existingPR)
							merged = true
						case "OPEN":
							l.logger.Log("git", "Existing PR #%s still open — attempting merge", existingPR)
							var mergeErr error
							merged, mergeErr = l.mergeWithRetry(ctx, taskID, nextTask, workDir, rawLogPath)
							if mergeErr != nil {
								l.logger.Warn("git", "Auto-merge existing PR: %v", mergeErr)
							}
						}
					}
				}
			}

			// Close bead only after successful merge (or if auto-merge is off).
			if taskID != "" && (merged || !l.cfg.AutoMerge) {
				l.attempts.ClearMergeFailures(taskID)
				closeReason := "completed by ralph"
				if ct.PRURL != "" {
					closeReason = fmt.Sprintf("Fixed in %s", ct.PRURL)
				} else if ct.PRNum != "" {
					closeReason = fmt.Sprintf("Fixed in PR #%s", ct.PRNum)
				}
				if err := l.cfg.TaskBackend.CloseTask(taskID, closeReason); err != nil {
					l.logger.Warn("beads", "CloseTask failed: %v", err)
				} else {
					l.logger.Log("beads", "Closed task %s (%s)", taskID, closeReason)
				}
			} else if taskID != "" && l.cfg.AutoMerge && !merged {
				count, _ := l.attempts.RecordMergeFailure(taskID)
				if count >= attempts.MaxMergeFailures {
					l.logger.Warn("git", "Merge failed %d times — skipping task %s for manual review", count, taskID)
					l.skipTask(taskID, fmt.Sprintf("merge_failed_%d_times", count))
				} else {
					l.logger.Warn("git", "Merge failed (%d/%d) — task %s left open for retry", count, attempts.MaxMergeFailures, taskID)
				}
			}

			l.sessionTasks = append(l.sessionTasks, ct)

			if merged {
				l.lastTaskMerged = true
				notify.TaskMerged(taskID, nextTask)
				l.git.PostMergeUpdateMain()
				if l.cfg.Evolve {
					l.git.TagTaskEnd(taskID)
					l.logger.Phase("Evolve: restarting with latest main")
					l.state.Write("status", "evolve_restart")
					return nil
				}
			}
		}

		l.git.TagTaskEnd(taskID)
	}

	return nil
}
