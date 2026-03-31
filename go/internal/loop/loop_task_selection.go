package loop

import (
	"context"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// loopAction describes what Run() should do after a phase completes.
type loopAction int

const (
	actionProceed loopAction = iota // continue to next phase
	actionRetry                     // re-run the current iteration
	actionDone                      // exit the loop
)

// taskContext holds the selected task and metadata for one iteration.
type taskContext struct {
	info    tasks.TaskInfo
	id      string
	title   string
	changed bool // true when this is a different task than last iteration
}

// selectNextTaskParams bundles the dependencies needed by selectNextTask.
type selectNextTaskParams struct {
	runIteration  int
	maxIterations int
	backend       tasks.Backend
	wait          bool
	state         *state.Store
	logger        *logging.Logger
	completedIDs  map[string]bool

	waitForTasks     func(ctx context.Context) bool
	flushUnpushedWork func(ctx context.Context)
}

// selectNextTask checks all stop conditions and selects the next task.
// Returns actionDone when the loop should exit (max iterations, stop file,
// context cancelled, all tasks complete). Returns actionProceed with a
// taskContext when a task is ready to run.
//
// Handles: max iterations, context cancellation, stop file, no remaining
// tasks (with wait mode), empty backend response, and completed-task dedup.
func selectNextTask(ctx context.Context, p selectNextTaskParams) (taskContext, loopAction) {
	return selectNextTaskInner(ctx, p, 0)
}

const maxSelectionAttempts = 50

func selectNextTaskInner(ctx context.Context, p selectNextTaskParams, attempts int) (taskContext, loopAction) {
	if attempts >= maxSelectionAttempts {
		p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Exhausted %d task selection attempts", maxSelectionAttempts)
		p.state.Write("status", "error")
		return taskContext{}, actionDone
	}
	maxIter := p.state.ReadMaxIterations(p.maxIterations)

	if p.runIteration >= maxIter {
		p.logger.Emit(logging.Opts{Level: logging.Warn}, "Max iterations (%d) reached", maxIter)
		p.state.Write("status", "max_iterations_reached")
		return taskContext{}, actionDone
	}

	if err := ctx.Err(); err != nil {
		p.logger.Emit(logging.Opts{Level: logging.Warn}, "Interrupted — stopping")
		p.state.Write("status", "stopped")
		return taskContext{}, actionDone
	}

	if p.state.CheckStop() {
		p.logger.Emit(logging.Opts{Level: logging.Warn}, "Stop file detected - halting")
		p.state.Write("status", "stopped")
		return taskContext{}, actionDone
	}

	hasRemaining, err := p.backend.HasRemaining()
	if err != nil {
		p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task check error: %v", err)
	}
	if !hasRemaining {
		if p.runIteration == 0 {
			hasTasks, _ := p.backend.HasTasks()
			if !hasTasks && !p.wait {
				p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Error}, "No tasks found — run ralph task to create tasks")
				p.state.Write("status", "error")
				return taskContext{}, actionDone
			}
		}
		if p.runIteration > 0 {
			p.flushUnpushedWork(ctx)
		}
		if !p.wait {
			p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Success}, "All tasks complete!")
			p.state.Write("status", "completed")
			return taskContext{}, actionDone
		}
		if resumed := p.waitForTasks(ctx); !resumed {
			return taskContext{}, actionDone
		}
		// Tasks appeared — re-enter selection to pick one up.
		return selectNextTaskInner(ctx, p, attempts+1)
	}

	if lastID, _ := p.state.Read("last_task_id"); lastID != "" {
		p.backend.SetResumeTaskID(lastID)
	}
	taskInfo, _ := p.backend.GetNextTaskInfo()
	taskID, nextTask := taskInfo.ID, taskInfo.Title

	if taskID == "" && nextTask == "" {
		p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task backend returned empty — no task to run")
		if p.wait {
			if resumed := p.waitForTasks(ctx); !resumed {
				return taskContext{}, actionDone
			}
			return selectNextTaskInner(ctx, p, attempts+1)
		}
		return taskContext{}, actionDone
	}

	if p.completedIDs[taskID] {
		p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task %s already completed this session — skipping", taskID)
		skipTask(p.backend, p.state, p.logger, taskID, "already_completed_this_session")
		// Recurse to try the next task. runIteration stays the same since
		// we didn't actually run anything.
		return selectNextTaskInner(ctx, p, attempts+1)
	}

	changed := isNewTask(p.state, taskID, nextTask)
	return taskContext{
		info:    taskInfo,
		id:      taskID,
		title:   nextTask,
		changed: changed,
	}, actionProceed
}

// selectNextTask delegates to the package-level selectNextTask function.
func (l *Loop) selectNextTask(ctx context.Context, runIteration int) (taskContext, loopAction) {
	completedIDs := make(map[string]bool, len(l.sessionTasks))
	for _, ct := range l.sessionTasks {
		completedIDs[ct.ID] = true
	}
	return selectNextTask(ctx, selectNextTaskParams{
		runIteration:      runIteration,
		maxIterations:     l.cfg.MaxIterations,
		backend:           l.cfg.TaskBackend,
		wait:              l.cfg.Wait,
		state:             l.state,
		logger:            l.logger,
		completedIDs:      completedIDs,
		waitForTasks:      l.waitForTasks,
		flushUnpushedWork: l.flushUnpushedWork,
	})
}
