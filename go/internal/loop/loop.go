package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/attempts"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/prompt"
	"github.com/brokenalarms/ralph/internal/ratelimit"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/verify"
)

// Config holds all parameters needed by the execution loop.
type Config struct {
	ProjectDir          string
	WorkDir             string
	RalphDir            string
	PromptsDir          string
	PlanFile            string
	MaxIterations       int
	RefactorEvery       int
	NoRefactor          bool
	RefactorThreshold   int
	DisabledChecks      []string
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
	VerifyDir           string // project root where tests are run; empty disables verification
}

// claudeRunner abstracts the Claude execution interface for testability.
type claudeRunner interface {
	Run(cfg claude.RunConfig) (claude.Result, error)
	StopStreaming()
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
	mergeFunc      func() (bool, error)
	pushPRFunc     func(taskDesc string) error
	forcePushFunc  func() error
	verifyFunc     func(dir, headBefore string) (passed bool, reason string)
	newRunnerFunc  func() claudeRunner
	lastAction     analyzer.Action
}

// New creates an execution loop from the given configuration.
func New(cfg Config, st *state.Store, gm *git.Manager, logger *logging.Logger) *Loop {
	signals := claude.DefaultSignalPaths(cfg.RalphDir)

	limiter := ratelimit.New(cfg.RalphDir, cfg.CallsPerHour)
	limiter.StopFile = filepath.Join(cfg.RalphDir, "stop")

	runner := &claude.Runner{Logger: logger}

	return &Loop{
		cfg:      cfg,
		state:    st,
		git:      gm,
		limiter:  limiter,
		runner:   runner,
		analyzer: analyzer.New(),
		attempts: attempts.New(cfg.RalphDir),
		logger:   logger,
		signals:  signals,
	}
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

		// On resume, only rotate to a fresh branch if the next task
		// differs from the last one. If it's the same task, stay on
		// the existing task branch so additional commits land there.
		if !strings.HasSuffix(l.git.WorktreeBranch, "/next") {
			nextTaskID, nextTask, _ := l.cfg.TaskBackend.GetNextTaskInfo()
			if l.isNewTask(nextTaskID, nextTask) {
				l.git.RotateBranch()
			} else {
				l.git.BranchRenamed = true
			}
		}

		l.logger.Log("Branch: %s", l.git.WorktreeBranch)
		l.writeRunBranch()
	}

	if err := l.limiter.Init(); err != nil {
		return fmt.Errorf("rate limiter init: %w", err)
	}

	l.state.WriteConfig(l.cfg.MaxIterations, l.cfg.RefactorEvery)
	l.state.Write("iterations_since_refactor", "0")

	var runIteration int
	st, _ := l.state.Load()
	iteration := st.Iteration

	l.logger.Phase("=== PHASE 2: EXECUTION ===")

	// Clear completed-tasks tracker for this run so the plan pane only
	// shows tasks completed in the current run, not historical closures.
	os.Remove(filepath.Join(l.cfg.RalphDir, ".completed-tasks"))

	for {
		maxIter := l.state.ReadMaxIterations(l.cfg.MaxIterations)
		refactorEvery := l.state.ReadRefactorEvery()

		if runIteration >= maxIter {
			l.logger.Warn("Max iterations (%d) reached", maxIter)
			l.state.Write("status", "max_iterations_reached")
			break
		}

		if err := ctx.Err(); err != nil {
			l.state.Write("status", "stopped")
			return nil
		}

		if l.checkStopFile() {
			l.logger.Warn("Stop file detected - halting")
			l.state.Write("status", "stopped")
			break
		}

		hasRemaining, err := l.cfg.TaskBackend.HasRemaining()
		if err != nil {
			l.logger.Warn("Task check error: %v", err)
		}
		if !hasRemaining {
			if runIteration == 0 {
				hasTasks, _ := l.cfg.TaskBackend.HasTasks()
				if !hasTasks {
					if !l.cfg.Wait {
						l.logger.TaskError("No tasks found")
						l.state.Write("status", "error")
						break
					}
				}
			}
			if !l.cfg.Wait {
				l.logger.TaskSuccess("All tasks complete!")
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

		taskID, nextTask, _ := l.cfg.TaskBackend.GetNextTaskInfo()
		taskChanged := l.isNewTask(taskID, nextTask)

		if runIteration > 1 && taskChanged {
			l.git.RotateBranch()
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

		if err := l.maybeRefactor(refactorEvery); err != nil {
			l.logger.Warn("Refactor iteration error: %v", err)
		}

		completed, _ := l.cfg.TaskBackend.CountCompleted()
		total, _ := l.cfg.TaskBackend.CountTotal()

		phaseColor := logging.Green
		if l.lastAction == analyzer.Warn {
			phaseColor = logging.Yellow
		}
		l.logger.PhaseColor(phaseColor, "--- Run iteration %d/%d | %d lifetime [%d/%d done] ---",
			runIteration, maxIter, iteration, completed, total)
		l.logger.Task("Next task: %s", nextTask)

		touchFile(filepath.Join(l.cfg.RalphDir, ".plan-refresh"))

		l.state.Write("iteration", strconv.Itoa(iteration))
		l.state.Write("status", "running")
		l.state.Write("last_task", nextTask)
		l.state.Write("last_task_id", taskID)
		if taskChanged {
			l.git.RenameBranchForTask(nextTask, taskID)
		}
		l.writeRunBranch()
		l.git.TagTaskStart(taskID)

		l.updateStreamTask(taskID, nextTask)

		if taskID != "" {
			if err := l.cfg.TaskBackend.SetState(taskID, "phase", "implementing", "ralph: starting task"); err != nil {
				l.logger.Warn("SetState phase=implementing: %v", err)
			}
		}

		taskPrompt := l.buildTaskPrompt(nextTask, taskID)

		// Run full test suite before handing off to agent. The agent only
		// runs scoped tests during development; the orchestrator owns the
		// full suite both here (pre-iteration) and after signal (gate).
		testStatus := l.runPreIterationTests()

		if !l.waitForRate(ctx) {
			break
		}

		headBefore := git.HeadRev(l.git.WorkDir)
		rawLogPath := filepath.Join(l.cfg.RalphDir, "raw.log")
		logStart := fileLineCount(rawLogPath)

		feedback := l.readFeedback()
		if feedback != "" {
			l.logger.Warn("[feedback] %s", feedback)
		}

		attemptContext := l.buildAttemptContext(taskID, nextTask)
		if attemptContext != "" {
			l.logger.Log("Including %d previous attempt(s) in prompt", strings.Count(attemptContext, "### Attempt "))
		}

		fullPrompt, err := l.buildPrompt(taskPrompt, feedback, attemptContext, testStatus)
		if err != nil {
			l.logger.Error("Prompt build failed: %v", err)
			break
		}

		workDir := l.git.WorkDir
		taskStart := time.Now()
		result, runErr := l.runner.Run(claude.RunConfig{
			Ctx:                 ctx,
			WorkDir:             workDir,
			RalphDir:            l.cfg.RalphDir,
			Prompt:              fullPrompt,
			RawLog:              rawLogPath,
			LogFile:             filepath.Join(l.cfg.RalphDir, "loop.log"),
			Quiet:               l.cfg.Quiet,
			Signals:             l.signals,
			PollInterval:        2 * time.Second,
			IdleTimeout:         l.cfg.IdleTimeout,
			IdleTimeoutProgress: l.cfg.IdleTimeoutProgress,
			HasProgress: func() bool {
				return git.HasDiff(workDir) || git.HeadRev(workDir) != headBefore
			},
			OnSignal: func(summary string) bool {
				// Orchestrator verification: sync prompts and run tests.
				srcPrompts := filepath.Join(workDir, "prompts")
				dstPrompts := filepath.Join(workDir, "go", "cmd", "ralph", "prompts")
				syncPrompts := func() {
					if _, err := os.Stat(srcPrompts); err == nil {
						if _, err := os.Stat(filepath.Dir(dstPrompts)); err == nil {
							exec.Command("cp", "-r", srcPrompts+"/", dstPrompts+"/").Run()
						}
					}
				}
				syncPrompts()

				// Step 1: Run tests (commit check is a warning, not a gate)
				commitResult := verify.CheckCommits(l.git.WorkDir, headBefore)
				if !commitResult.Passed {
					l.logger.Warn("No new commits — will verify via LLM if work is already on main")
				}

				testResult := verify.RunTests(l.cfg.VerifyDir)
				passed := testResult.Passed
				reason := testResult.Reason
				if !passed {
					l.logger.Warn("Tests failed: %s", reason)
					testOutput := testResult.Details

					beadDesc := l.getBeadDescription(taskID)

					signalPath := filepath.Join(l.cfg.RalphDir, ".signal_complete")
					verifyPrompt := l.loadVerifyPrompt("verify-tests.md", map[string]string{
						"{{TASK_TITLE}}":       nextTask,
						"{{TASK_DESCRIPTION}}": beadDesc,
						"{{TEST_OUTPUT}}":      testOutput,
						"{{SIGNAL_COMPLETE}}":  signalPath,
					})

					verifyResult := l.runFixAgent(ctx, "test failures", verifyPrompt, workDir, rawLogPath)
					if !verifyResult.SignalDetected {
						return false
					}

					// Re-check tests after fix agent (skip commit check — fix agent
					// may not have new commits if it determined work was correct)
					syncPrompts()
					testResult := verify.RunTests(l.cfg.VerifyDir)
					if !testResult.Passed {
						l.logger.Error("Tests still failing after verification agent: %s", testResult.Reason)
						return false
					}
				}

				// LLM verification — does the diff match the bead?
				// Prefer PR diff (covers prior iterations) over iteration diff.
				if passed {
					beadDesc := l.getBeadDescription(taskID)

					l.logger.Log("Running LLM verification...")
					llmResult := verify.LLMVerifyPR(workDir, l.cfg.PromptsDir, taskID, headBefore, nextTask, beadDesc)

					if !llmResult.Passed {
						l.logger.Warn("LLM verification rejected: %s", llmResult.Details)

						signalPath := filepath.Join(l.cfg.RalphDir, ".signal_complete")
						fixPrompt := l.loadVerifyPrompt("verify-llm.md", map[string]string{
							"{{TASK_TITLE}}":       nextTask,
							"{{TASK_DESCRIPTION}}": beadDesc,
							"{{LLM_FEEDBACK}}":     llmResult.Details,
							"{{SIGNAL_COMPLETE}}":  signalPath,
						})

						fixResult := l.runFixAgent(ctx, "LLM feedback", fixPrompt, workDir, rawLogPath)
						if !fixResult.SignalDetected {
							return false
						}

						// Re-verify tests after fix (skip commit check)
						syncPrompts()
						testResult := verify.RunTests(l.cfg.VerifyDir)
						if !testResult.Passed {
							l.logger.Error("Tests failed after LLM fix agent: %s", testResult.Reason)
							return false
						}

						// Re-run LLM check using PR diff
						llmResult2 := verify.LLMVerifyPR(workDir, l.cfg.PromptsDir, taskID, headBefore, nextTask, beadDesc)
						if !llmResult2.Passed {
							l.logger.Warn("LLM still rejects after fix agent: %s — accepting anyway (tests passed)", llmResult2.Details)
						} else {
							l.logger.Log("LLM verified after fix: %s", llmResult2.Reason)
						}
					} else {
						l.logger.Log("LLM verified: %s", llmResult.Reason)
					}
				}

				return true
			},
			FeedbackFile: filepath.Join(l.cfg.RalphDir, "feedback"),
		})
		if runErr != nil {
			l.logger.Warn("Claude failed on iteration %d, continuing...", runIteration)
		}
		if result.IdleTimeout {
			l.logger.Warn("Restarting iteration %d after idle timeout", runIteration)
			diffStat := git.DiffStatRange(l.git.WorkDir, headBefore, git.HeadRev(l.git.WorkDir))
			l.attempts.Record(taskID, nextTask,
				"Killed: idle timeout (no output for configured duration)",
				diffStat,
				"idle_timeout: consider a lighter approach or make incremental progress rather than deep-thinking without output")
			runIteration--
			iteration--
			continue
		}
		elapsed := time.Since(taskStart)
		l.limiter.Increment()

		if feedback != "" {
			l.clearFeedback()
		}

		if result.Summary != "" {
			l.logger.Log("Summary: %s", result.Summary)
		}

		completed, _ = l.cfg.TaskBackend.CountCompleted()
		total, _ = l.cfg.TaskBackend.CountTotal()
		l.logger.Task("Run iteration %d complete (%dm%ds). %d/%d tasks done.",
			runIteration, int(elapsed.Minutes()), int(elapsed.Seconds())%60, completed, total)

		headAfter := git.HeadRev(l.git.WorkDir)
		diffStat := git.DiffStatRange(l.git.WorkDir, headBefore, headAfter)
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
			l.logger.Error("Halting: %s", analysisResult.Reason)
			if analysisResult.Detail != "" {
				l.logger.Error("  %s", analysisResult.Detail)
			}
			l.attempts.Record(taskID, nextTask, "Halted: "+analysisResult.Reason, diffStat, analysisResult.Detail)
			l.state.Write("status", "halted_"+analysisResult.Reason)
			l.git.TagTaskEnd(taskID)
			return nil
		case analyzer.Warn:
			l.logger.Warn("Analysis: %s", analysisResult.Reason)
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
					l.logger.Warn("Task %s phase is %q (expected implementing) — agent may have tampered with task state", taskID, phase)
				}
			}

			// If OnSignal was set, verification already passed in the runner.
			// If not (legacy/test path), run verification here as fallback.
			if result.OnSignalUsed == false {
				if passed, reason := l.verifyCompletion(headBefore); !passed {
					l.logger.Warn("Verification failed: %s", reason)
					l.attempts.Record(taskID, nextTask,
						"Signal received but verification failed: "+reason,
						diffStat,
						"verification_failed: fix must pass tests and produce commits before closing")
					continue
				}
			}

			if taskID != "" {
				if err := l.cfg.TaskBackend.SetState(taskID, "phase", "verified", "ralph: tests passed, commits present"); err != nil {
					l.logger.Warn("SetState phase=verified: %v", err)
				}
			}

			l.attempts.Clear(taskID, nextTask)
			l.recordCompletedTask(taskID, nextTask)
			touchFile(filepath.Join(l.cfg.RalphDir, ".plan-flash"))

			if taskID != "" {
				closeReason := "completed by ralph"
				if prNum := l.findPRNumber(workDir); prNum != "" {
					closeReason = fmt.Sprintf("completed by ralph — PR #%s", prNum)
				}
				if err := l.cfg.TaskBackend.CloseTask(taskID, closeReason); err != nil {
					l.logger.Warn("CloseTask failed: %v", err)
				} else {
					l.logger.Log("Closed task %s (%s)", taskID, closeReason)
				}
			}

			if err := l.pushAndCreatePR(nextTask); err != nil {
				l.logger.Warn("Push/PR: %v", err)
			}

			if l.cfg.AutoMerge {
				merged, err := l.autoMerge()
				if err != nil {
					l.logger.Warn("Auto-merge: %v", err)
					merged, err = l.handleAutoMergeError(ctx, err, nextTask, workDir, rawLogPath)
				}
				if merged {
					if err := l.git.PostMergeReset(); err != nil {
						l.logger.Warn("Post-merge reset: %v", err)
					}
					if l.cfg.Evolve {
						l.git.TagTaskEnd(taskID)
						l.logger.Phase("Evolve: restarting with latest main")
						l.state.Write("status", "evolve_restart")
						return nil
					}
				}
			}
		}

		l.git.TagTaskEnd(taskID)
		fmt.Println()
	}

	return nil
}

// handleRebase attempts to rebase onto the default branch, and if a conflict
// is detected, consults the OnRebaseConflict handler for recovery.
func (l *Loop) handleRebase(ctx context.Context) error {
	err := l.git.RebaseOntoDefaultBranch(ctx)
	if err == nil {
		return nil
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	var conflictErr *git.RebaseConflictError
	if !errors.As(err, &conflictErr) {
		return err
	}

	if l.cfg.OnRebaseConflict == nil {
		return err
	}

	switch l.cfg.OnRebaseConflict(err) {
	case git.RebaseFreshWorktree:
		l.logger.Log("Recreating worktree from main...")
		if recreateErr := l.git.RecreateFromMain(); recreateErr != nil {
			return fmt.Errorf("worktree recreation failed: %w", recreateErr)
		}
		return nil
	case git.RebaseManualResolve:
		l.logger.Warn("Pausing for manual conflict resolution. Re-run ralph to resume.")
		return fmt.Errorf("paused for manual resolution: %w", err)
	default:
		return err
	}
}

// handleAutoMergeError handles recoverable auto-merge errors: CI failures
// and merge conflicts. Returns the final merge state.
func (l *Loop) handleAutoMergeError(ctx context.Context, err error, nextTask, workDir, rawLogPath string) (bool, error) {
	var conflictErr *git.MergeConflictError
	if errors.As(err, &conflictErr) {
		return l.handleMergeConflict(ctx, nextTask)
	}

	var ciErr *git.CIFailureError
	if errors.As(err, &ciErr) {
		return l.handleCIFailure(ctx, ciErr, nextTask, workDir, rawLogPath)
	}

	return false, err
}

// handleMergeConflict rebases the working branch onto main and force-pushes
// to resolve PR merge conflicts, then retries the merge.
func (l *Loop) handleMergeConflict(ctx context.Context, nextTask string) (bool, error) {
	l.logger.Log("Rebasing onto main to resolve merge conflicts...")

	if err := l.git.RebaseOntoDefaultBranch(ctx); err != nil {
		l.logger.Warn("Rebase failed: %v", err)
		return false, fmt.Errorf("conflict resolution rebase failed: %w", err)
	}

	l.logger.Log("Force-pushing rebased branch...")
	if err := l.forcePush(); err != nil {
		l.logger.Warn("Force-push after rebase failed: %v", err)
		return false, fmt.Errorf("force-push after conflict rebase failed: %w", err)
	}

	l.logger.Log("Retrying merge after conflict resolution...")
	return l.autoMerge()
}

// handleCIFailure spawns fix agents to address CI failures and retries
// the merge after each fix attempt.
func (l *Loop) handleCIFailure(ctx context.Context, ciErr *git.CIFailureError, nextTask, workDir, rawLogPath string) (bool, error) {
	l.logger.Log("CI failed on PR #%s", ciErr.PRNumber)

	ciDetails := ciErr.Error()
	ciLog := l.getCIFailureLog(ciErr.PRNumber)

	signalPath := filepath.Join(l.cfg.RalphDir, ".signal_complete")
	fixPrompt := l.loadVerifyPrompt("verify-tests.md", map[string]string{
		"{{TASK_TITLE}}":       nextTask,
		"{{TASK_DESCRIPTION}}": "CI checks failed after push. Fix the failures so CI passes.",
		"{{TEST_OUTPUT}}":      ciDetails + "\n\n" + ciLog,
		"{{SIGNAL_COMPLETE}}":  signalPath,
	})

	var merged bool
	for ciAttempt := 0; ciAttempt < 2; ciAttempt++ {
		fixResult := l.runFixAgent(ctx, "CI failures", fixPrompt, workDir, rawLogPath)
		if !fixResult.SignalDetected {
			break
		}

		if pushErr := l.pushAndCreatePR(nextTask); pushErr != nil {
			l.logger.Warn("Push after CI fix failed: %v", pushErr)
			break
		}

		var mergeErr error
		merged, mergeErr = l.autoMerge()
		if mergeErr == nil && merged {
			return true, nil
		}
		if mergeErr != nil {
			if errors.As(mergeErr, &ciErr) {
				l.logger.Warn("CI still failing after fix attempt %d: %s", ciAttempt+1, ciErr.Error())
				ciDetails = ciErr.Error()
				ciLog = l.getCIFailureLog(ciErr.PRNumber)
				fixPrompt = l.loadVerifyPrompt("verify-tests.md", map[string]string{
					"{{TASK_TITLE}}":       nextTask,
					"{{TASK_DESCRIPTION}}": "CI checks still failing. Fix the remaining failures.",
					"{{TEST_OUTPUT}}":      ciDetails + "\n\n" + ciLog,
					"{{SIGNAL_COMPLETE}}":  signalPath,
				})
			} else {
				l.logger.Warn("Auto-merge retry failed: %v", mergeErr)
				return false, mergeErr
			}
		}
	}

	return merged, nil
}

// runFixAgent stops the main agent's streaming, spawns a fix agent with the
// given prompt, and returns the result. All fix agent invocations share the
// same RunConfig shape — this method is the single place that wires it up.
func (l *Loop) runFixAgent(ctx context.Context, description, prompt, workDir, rawLogPath string) claude.Result {
	l.runner.StopStreaming()
	l.logger.Log("Spawning fix agent: %s", description)

	runner := l.newRunner()
	result, _ := runner.Run(claude.RunConfig{
		Ctx:          ctx,
		WorkDir:      workDir,
		RalphDir:     l.cfg.RalphDir,
		Prompt:       prompt,
		RawLog:       rawLogPath,
		Quiet:        true,
		Signals:      l.signals,
		PollInterval: 2 * time.Second,
		IdleTimeout:  l.cfg.IdleTimeout,
	})

	if !result.SignalDetected {
		l.logger.Warn("Fix agent exited without signal (%s)", description)
	}

	return result
}

// newRunner returns a claudeRunner for spawning sub-agents. Uses newRunnerFunc
// if set (for testing), otherwise creates a default claude.Runner.
func (l *Loop) newRunner() claudeRunner {
	if l.newRunnerFunc != nil {
		return l.newRunnerFunc()
	}
	return &claude.Runner{Logger: l.logger}
}

// forcePush force-pushes the current branch to the remote.
func (l *Loop) forcePush() error {
	if l.forcePushFunc != nil {
		return l.forcePushFunc()
	}
	return l.git.ForcePush()
}

// getCIFailureLog retrieves the failed CI run's log output for the given PR.
func (l *Loop) getCIFailureLog(prNumber string) string {
	// Get the latest failed run ID
	cmd := exec.Command("gh", "pr", "checks", prNumber, "--json", "name,state,link", "--jq",
		`.[] | select(.state == "FAILURE") | .link`)
	cmd.Dir = l.git.WorkDir
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return ""
	}

	// Get the run ID from the first failed check URL
	link := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	parts := strings.Split(link, "/")
	if len(parts) < 2 {
		return ""
	}
	runID := ""
	for i, p := range parts {
		if p == "runs" && i+1 < len(parts) {
			runID = parts[i+1]
			break
		}
	}
	if runID == "" {
		return ""
	}

	logCmd := exec.Command("gh", "run", "view", runID, "--log-failed")
	logCmd.Dir = l.git.WorkDir
	logOut, err := logCmd.Output()
	if err != nil {
		return ""
	}

	// Truncate to last 50 lines
	lines := strings.Split(string(logOut), "\n")
	if len(lines) > 50 {
		lines = lines[len(lines)-50:]
	}
	return strings.Join(lines, "\n")
}

// loadVerifyPrompt reads a prompt template from the prompts directory and
// replaces placeholders with the given values.
func (l *Loop) loadVerifyPrompt(filename string, vars map[string]string) string {
	path := filepath.Join(l.cfg.PromptsDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		// Fallback: return a minimal prompt
		result := ""
		for k, v := range vars {
			result += k + ": " + v + "\n"
		}
		return result
	}
	s := string(data)
	for k, v := range vars {
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}

// getBeadDescription retrieves the bead description for LLM verification.
func (l *Loop) getBeadDescription(taskID string) string {
	if taskID == "" {
		return ""
	}
	desc, err := l.cfg.TaskBackend.GetDescription(taskID)
	if err != nil {
		return ""
	}
	return desc
}

// isNewTask returns true when the next task differs from the last one stored
// in state. Prefers task ID comparison (stable across description edits);
// falls back to description when no ID is available.
func (l *Loop) isNewTask(taskID, taskDesc string) bool {
	if taskID != "" {
		lastID, _ := l.state.Read("last_task_id")
		return lastID != taskID
	}
	lastTask, _ := l.state.Read("last_task")
	return lastTask != taskDesc
}

// verifyCompletion runs post-signal checks: commit presence and test suite.
// Returns (true, "") on success or (false, reason) on failure.
func (l *Loop) verifyCompletion(headBefore string) (bool, string) {
	if l.verifyFunc != nil {
		return l.verifyFunc(l.git.WorkDir, headBefore)
	}

	if l.cfg.VerifyDir == "" {
		return true, ""
	}

	commitResult := verify.CheckCommits(l.git.WorkDir, headBefore)
	if !commitResult.Passed {
		return false, commitResult.Reason
	}

	testResult := verify.RunTests(l.cfg.VerifyDir)
	now := time.Now().Format(time.RFC3339)
	if !testResult.Passed {
		l.state.Write("last_test_result", "fail")
		l.state.Write("last_test_output", testResult.Details)
		l.state.Write("last_test_time", now)
		if testResult.Details != "" {
			l.logger.Error("Test output:\n%s", testResult.Details)
		}
		return false, testResult.Reason
	}

	l.state.Write("last_test_result", "pass")
	l.state.Write("last_test_output", "")
	l.state.Write("last_test_time", now)
	return true, ""
}

func (l *Loop) pushAndCreatePR(taskDesc string) error {
	if l.pushPRFunc != nil {
		return l.pushPRFunc(taskDesc)
	}
	return l.git.PushAndCreatePR(taskDesc)
}

func (l *Loop) autoMerge() (bool, error) {
	if l.mergeFunc != nil {
		return l.mergeFunc()
	}
	return l.git.AutoMergeCurrentBranch()
}

func (l *Loop) findPRNumber(workDir string) string {
	cmd := exec.Command("gh", "pr", "list",
		"--head", l.git.WorktreeBranch,
		"--state", "all", "--json", "number", "--jq", ".[0].number")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (l *Loop) waitForTasks(ctx context.Context) bool {
	l.logger.Log("Waiting for new tasks (polling every %s)...", l.cfg.WaitInterval)
	l.state.Write("status", "waiting")
	l.updateStreamTask("", "Waiting for tasks...")
	touchFile(filepath.Join(l.cfg.RalphDir, ".plan-refresh"))

	ticker := time.NewTicker(l.cfg.WaitInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			l.state.Write("status", "stopped")
			return false
		case <-ticker.C:
			if l.checkStopFile() {
				l.logger.Warn("Stop file detected - halting")
				l.state.Write("status", "stopped")
				return false
			}
			hasRemaining, err := l.cfg.TaskBackend.HasRemaining()
			if err != nil {
				l.logger.Warn("Task check error during wait: %v", err)
				continue
			}
			if hasRemaining {
				l.logger.TaskSuccess("New tasks detected!")
				touchFile(filepath.Join(l.cfg.RalphDir, ".plan-refresh"))
				return true
			}
		}
	}
}

func (l *Loop) checkStopFile() bool {
	stopFile := filepath.Join(l.cfg.RalphDir, "stop")
	if _, err := os.Stat(stopFile); err == nil {
		os.Remove(stopFile)
		return true
	}
	return false
}

func (l *Loop) maybeRefactor(refactorEvery int) error {
	if l.cfg.NoRefactor || refactorEvery <= 0 {
		return nil
	}

	sinceRefactorStr, _ := l.state.Read("iterations_since_refactor")
	sinceRefactor, _ := strconv.Atoi(sinceRefactorStr)

	if sinceRefactor < refactorEvery {
		l.state.Write("iterations_since_refactor", strconv.Itoa(sinceRefactor+1))
		return nil
	}

	l.logger.Phase("--- Refactor iteration (every %d iterations) ---", refactorEvery)

	recentFiles := git.RecentChangedFiles(l.git.WorkDir, refactorEvery)
	if recentFiles == "" {
		l.logger.Log("No recently changed files — skipping refactor")
		l.state.Write("iterations_since_refactor", "0")
		return nil
	}

	refactorPrompt, err := prompt.BuildRefactorPrompt(prompt.Vars{
		PromptsDir:       l.cfg.PromptsDir,
		WorkDir:          l.git.WorkDir,
		SignalToken:      l.signals.Complete,
		CurrentTaskToken: l.signals.CurrentTask,
		AllCompleteToken: l.signals.AllComplete,
	}, recentFiles)
	if err != nil {
		l.state.Write("iterations_since_refactor", "0")
		return fmt.Errorf("building refactor prompt: %w", err)
	}

	if !l.limiter.Allowed() {
		l.logger.Warn("Rate limit hit before refactor — waiting for reset")
		if err := l.limiter.WaitForReset(context.Background(), func(secs int) {
			l.logger.Log("Rate limit: %ds until reset", secs)
		}); err != nil {
			return err
		}
	}

	rawLogPath := filepath.Join(l.cfg.RalphDir, "raw.log")
	_, err = l.runner.Run(claude.RunConfig{
		WorkDir:      l.git.WorkDir,
		RalphDir:     l.cfg.RalphDir,
		Prompt:       refactorPrompt,
		RawLog:       rawLogPath,
		LogFile:      filepath.Join(l.cfg.RalphDir, "loop.log"),
		Quiet:        l.cfg.Quiet,
		Signals:      l.signals,
		PollInterval: 2 * time.Second,
	})
	l.limiter.Increment()

	l.logger.TaskSuccess("Refactor iteration complete")
	l.state.Write("iterations_since_refactor", "0")

	return err
}

func (l *Loop) buildTaskPrompt(nextTask, taskID string) string {
	if taskID != "" {
		return fmt.Sprintf("Complete this task (bd id: %s): %s", taskID, nextTask)
	}
	return fmt.Sprintf("Complete this task: %s", nextTask)
}

func (l *Loop) buildPrompt(taskPrompt, feedback, attemptHistory, testStatus string) (string, error) {
	backend := prompt.BackendChecklist
	if l.cfg.TaskBackend.Label() == "beads" {
		backend = prompt.BackendBD
	}

	return prompt.BuildPrompt(prompt.Vars{
		PromptsDir:       l.cfg.PromptsDir,
		ProjectDir:       l.cfg.ProjectDir,
		WorkDir:          l.git.WorkDir,
		RalphDir:         l.cfg.RalphDir,
		PlanFile:         l.cfg.PlanFile,
		SignalToken:      l.signals.Complete,
		CurrentTaskToken: l.signals.CurrentTask,
		AllCompleteToken: l.signals.AllComplete,
		TaskPrompt:       taskPrompt,
		Feedback:         feedback,
		AttemptHistory:   attemptHistory,
		TestStatus:       testStatus,
		TaskBackend:      backend,
	})
}

func (l *Loop) waitForRate(ctx context.Context) bool {
	if l.limiter.Allowed() {
		return true
	}

	l.logger.Warn("Rate limit reached (%d/%d calls this hour)", l.limiter.Count(), l.cfg.CallsPerHour)

	err := l.limiter.WaitForReset(ctx, func(secs int) {
		l.logger.Log("Rate limit: %ds until reset", secs)
	})
	return err == nil
}

func (l *Loop) analyzeIteration(rawLogPath string, logStart int, headBefore, headAfter, taskKey string) analyzer.Result {
	iterLog := readLogFrom(rawLogPath, logStart)
	hasDiff := git.HasDiff(l.git.WorkDir)
	newCommits := headBefore != "" && headAfter != "" && headBefore != headAfter
	changedFiles := git.ChangedFiles(l.git.WorkDir, headBefore, headAfter)

	hasSignal := false
	if _, err := os.Stat(l.signals.Complete); err == nil {
		hasSignal = true
	}
	if _, err := os.Stat(l.signals.AllComplete); err == nil {
		hasSignal = true
	}

	return l.analyzer.Analyze(analyzer.IterationState{
		HasDiff:      hasDiff,
		NewCommits:   newCommits,
		HasSignal:    hasSignal,
		ChangedFiles: changedFiles,
		IterationLog: iterLog,
		TaskKey:      taskKey,
	})
}

func (l *Loop) writeRunBranch() {
	branch := l.git.WorktreeBranch
	if branch == "" {
		branch = "ralph"
	}
	os.WriteFile(filepath.Join(l.cfg.RalphDir, ".run-branch"), []byte(branch), 0o644)
}

func (l *Loop) updateStreamTask(taskID, nextTask string) {
	streamTaskFile := filepath.Join(l.cfg.RalphDir, ".stream-task")
	content := nextTask
	if taskID != "" {
		content = taskID + ": " + nextTask
	}
	os.WriteFile(streamTaskFile, []byte(content), 0o644)
}

func (l *Loop) readFeedback() string {
	feedbackFile := filepath.Join(l.cfg.RalphDir, "feedback")
	data, err := os.ReadFile(feedbackFile)
	if err != nil || len(data) == 0 {
		return ""
	}
	return string(data)
}

func (l *Loop) writeFeedback(msg string) {
	feedbackFile := filepath.Join(l.cfg.RalphDir, "feedback")
	f, err := os.OpenFile(feedbackFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		l.logger.Warn("Failed to write feedback: %v", err)
		return
	}
	defer f.Close()
	f.WriteString(msg + "\n")
}

func (l *Loop) clearFeedback() {
	os.Remove(filepath.Join(l.cfg.RalphDir, "feedback"))
}

// readReflection returns the content of a previous reflection file for a task.
// Uses task ID if available, falls back to slugified task name.
func (l *Loop) readReflection(taskID, taskName string) string {
	key := taskID
	if key == "" {
		key = git.Slugify(taskName)
	}
	path := filepath.Join(l.cfg.RalphDir, "reflections", key+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// buildAttemptContext assembles attempt history and reflections into a single
// block for the prompt. Returns empty string if no prior context exists.
func (l *Loop) buildAttemptContext(taskID, taskName string) string {
	var parts []string

	if history := l.attempts.Read(taskID, taskName); history != "" {
		parts = append(parts, history)
	}

	if reflection := l.readReflection(taskID, taskName); reflection != "" {
		parts = append(parts, "### Previous reflection\n"+reflection)
	}

	return strings.Join(parts, "\n")
}

// recordCompletedTask appends a completed task label to .completed-tasks
// so the plan pane can show which tasks were finished in this run.
func (l *Loop) recordCompletedTask(taskID, taskTitle string) {
	label := taskID
	if label == "" {
		label = taskTitle
	}
	if label == "" {
		return
	}
	path := filepath.Join(l.cfg.RalphDir, ".completed-tasks")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(label + "\n")
}

// runPreIterationTests runs the full test suite before handing off to the
// agent. Stores results in state.json so they persist across restarts.
// Returns a human-readable status string for the agent prompt.
func (l *Loop) runPreIterationTests() string {
	if l.cfg.VerifyDir == "" {
		return ""
	}

	l.logger.Log("Running pre-iteration test suite...")
	result := verify.RunTests(l.cfg.VerifyDir)
	now := time.Now().Format(time.RFC3339)

	if result.Passed {
		l.state.Write("last_test_result", "pass")
		l.state.Write("last_test_output", "")
		l.state.Write("last_test_time", now)
		l.logger.TaskSuccess("Pre-iteration tests: all passing")
		return "\n- Test suite status: all tests passing as of start."
	}

	l.state.Write("last_test_result", "fail")
	l.state.Write("last_test_output", result.Details)
	l.state.Write("last_test_time", now)
	l.logger.Warn("Pre-iteration tests: failures detected")
	msg := "\n- Test suite status: some tests are FAILING. Fix them before your task."
	if result.Details != "" {
		// Truncate to avoid bloating the prompt.
		details := result.Details
		lines := strings.Split(details, "\n")
		if len(lines) > 20 {
			details = strings.Join(lines[len(lines)-20:], "\n")
		}
		msg += "\n  Failure output:\n  " + strings.ReplaceAll(details, "\n", "\n  ")
	}
	return msg
}


func touchFile(path string) {
	f, err := os.Create(path)
	if err == nil {
		f.Close()
	}
}

func fileLineCount(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	count := 0
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}
	return count
}

func readLogFrom(path string, startLine int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := 0
	offset := 0
	for i, b := range data {
		if b == '\n' {
			lines++
			if lines >= startLine {
				offset = i + 1
				break
			}
		}
	}
	if offset >= len(data) {
		return ""
	}
	return string(data[offset:])
}
