package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/prompt"
	"github.com/brokenalarms/ralph/internal/ratelimit"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
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
	Quiet               bool
	AutoMerge           bool
	AutoImprove         bool
	CallsPerHour        int
	TaskBackend         tasks.Backend
	IdleTimeout         time.Duration
	IdleTimeoutProgress time.Duration
	OnRebaseConflict    func(err error) git.RebaseRecovery
}

// claudeRunner abstracts the Claude execution interface for testability.
type claudeRunner interface {
	Run(cfg claude.RunConfig) (claude.Result, error)
}

// Loop orchestrates the execution phase: task selection, prompt building,
// rate limiting, branch rotation, Claude invocation, and response analysis.
type Loop struct {
	cfg       Config
	state     *state.Store
	git       *git.Manager
	limiter   *ratelimit.Limiter
	runner    claudeRunner
	analyzer  *analyzer.Analyzer
	logger    *logging.Logger
	signals   claude.SignalPaths
	mergeFunc func() (bool, error)
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
		logger:   logger,
		signals:  signals,
	}
}

// Run executes the full iteration loop. Returns nil on normal completion
// (all tasks done, max iterations reached, or stopped). Returns an error
// for unrecoverable failures.
func (l *Loop) Run(ctx context.Context) error {
	if l.git.WorktreeBranch != "" && l.git.WorkDir != l.git.ProjectDir {
		if err := l.handleRebase(); err != nil {
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
					l.logger.TaskError("No tasks found")
					l.state.Write("status", "error")
					break
				}
			}
			l.logger.TaskSuccess("All tasks complete!")
			l.state.Write("status", "completed")
			break
		}

		runIteration++
		iteration++

		taskID, nextTask, _ := l.cfg.TaskBackend.GetNextTaskInfo()
		taskChanged := l.isNewTask(taskID, nextTask)

		if runIteration > 1 && taskChanged {
			l.git.RotateBranch()
			if l.git.WorktreeBranch != "" && l.git.WorkDir != l.git.ProjectDir {
				if err := l.handleRebase(); err != nil {
					l.state.Write("status", "error")
					break
				}
			}
		}

		if err := l.maybeRefactor(refactorEvery); err != nil {
			l.logger.Warn("Refactor iteration error: %v", err)
		}

		completed, _ := l.cfg.TaskBackend.CountCompleted()
		total, _ := l.cfg.TaskBackend.CountTotal()

		l.logger.Phase("--- Iteration %d/%d (%d total) [%d/%d done] ---",
			runIteration, maxIter, iteration, completed, total)
		l.logger.Task("Next task: %s", nextTask)

		touchFile(filepath.Join(l.cfg.RalphDir, ".plan-refresh"))

		l.state.Write("iteration", strconv.Itoa(iteration))
		l.state.Write("status", "running")
		l.state.Write("last_task", nextTask)
		l.state.Write("last_task_id", taskID)
		if taskChanged {
			l.git.RenameBranchForTask(nextTask)
		}
		l.git.TagTaskStart(taskID)

		l.updateStreamTask(taskID, nextTask)

		taskPrompt := l.buildTaskPrompt(nextTask, taskID)

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

		fullPrompt, err := l.buildPrompt(taskPrompt, feedback)
		if err != nil {
			l.logger.Error("Prompt build failed: %v", err)
			break
		}

		workDir := l.git.WorkDir
		taskStart := time.Now()
		result, runErr := l.runner.Run(claude.RunConfig{
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
		})
		if runErr != nil {
			l.logger.Warn("Claude failed on iteration %d, continuing...", runIteration)
		}
		if result.IdleTimeout {
			l.logger.Warn("Restarting iteration %d after idle timeout", runIteration)
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
		l.logger.Task("Iteration %d complete (%dm%ds). %d/%d tasks done.",
			runIteration, int(elapsed.Minutes()), int(elapsed.Seconds())%60, completed, total)

		headAfter := git.HeadRev(l.git.WorkDir)
		analysisResult := l.analyzeIteration(rawLogPath, logStart, headBefore, headAfter)

		switch analysisResult.Action {
		case analyzer.Halt:
			l.logger.Error("Halting: %s", analysisResult.Reason)
			if analysisResult.Detail != "" {
				l.logger.Error("  %s", analysisResult.Detail)
			}
			l.state.Write("status", "halted_"+analysisResult.Reason)
			l.git.TagTaskEnd(taskID)
			return nil
		case analyzer.Warn:
			l.logger.Warn("Analysis: %s", analysisResult.Reason)
		}

		if l.cfg.AutoMerge && result.SignalDetected {
			merged, err := l.autoMerge()
			if err != nil {
				l.logger.Warn("Auto-merge: %v", err)
			} else if merged && l.cfg.AutoImprove {
				l.git.TagTaskEnd(taskID)
				l.logger.Phase("Auto-improve: restarting with latest main")
				l.state.Write("status", "auto_improve_restart")
				return nil
			}
		}

		l.git.TagTaskEnd(taskID)
		fmt.Println()
	}

	return nil
}

// handleRebase attempts to rebase onto the default branch, and if a conflict
// is detected, consults the OnRebaseConflict handler for recovery.
func (l *Loop) handleRebase() error {
	err := l.git.RebaseOntoDefaultBranch()
	if err == nil {
		return nil
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

func (l *Loop) autoMerge() (bool, error) {
	if l.mergeFunc != nil {
		return l.mergeFunc()
	}
	return l.git.AutoMergeCurrentBranch()
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
	if refactorEvery <= 0 {
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

func (l *Loop) buildPrompt(taskPrompt, feedback string) (string, error) {
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

func (l *Loop) analyzeIteration(rawLogPath string, logStart int, headBefore, headAfter string) analyzer.Result {
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
	})
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

func (l *Loop) clearFeedback() {
	os.Remove(filepath.Join(l.cfg.RalphDir, "feedback"))
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
