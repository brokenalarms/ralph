package loop

import (
	"context"

	"github.com/brokenalarms/ralph/internal/logging"
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

// selectNextTaskParams bundles the data needed by selectNextTask.
// All fields are plain data — no module references or callbacks.
type selectNextTaskParams struct {
	runIteration   int
	maxIterations  int
	wait           bool
	completedIDs   map[string]bool
	lastTaskMerged bool // forwarded to flushUnpushedWork when no tasks remain
}

// selectNextTask checks all stop conditions and selects the next task.
// Returns actionDone when the loop should exit (max iterations, stop file,
// context cancelled, all tasks complete). Returns actionProceed with a
// taskContext when a task is ready to run.
//
// Handles: max iterations, context cancellation, stop file, no remaining
// tasks (with wait mode), empty backend response, and completed-task dedup.
func (l *Loop) selectNextTask(ctx context.Context, p selectNextTaskParams) (taskContext, loopAction) {
	return l.selectNextTaskInner(ctx, p, 0)
}

const maxSelectionAttempts = 50

func (l *Loop) selectNextTaskInner(ctx context.Context, p selectNextTaskParams, attempts int) (taskContext, loopAction) {
	if attempts >= maxSelectionAttempts {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Exhausted %d task selection attempts", maxSelectionAttempts)
		l.state.Write("status", "error")
		return taskContext{}, actionDone
	}
	maxIter := l.state.ReadMaxIterations(p.maxIterations)

	if p.runIteration >= maxIter {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "Max iterations (%d) reached", maxIter)
		l.state.Write("status", "max_iterations_reached")
		return taskContext{}, actionDone
	}

	if err := ctx.Err(); err != nil {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "Interrupted — stopping")
		l.state.Write("status", "stopped")
		return taskContext{}, actionDone
	}

	if l.state.CheckStop() {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "Stop file detected - halting")
		l.state.Write("status", "stopped")
		return taskContext{}, actionDone
	}

	avail, err := tasks.CheckAvailability(l.taskBackend)
	if err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task check error: %v", err)
	}
	if !avail.HasRemaining {
		if p.runIteration == 0 && !avail.HasAny && !p.wait {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Error}, "No tasks found — run ralph task to create tasks")
			l.state.Write("status", "error")
			return taskContext{}, actionDone
		}
		if p.runIteration > 0 {
			l.flushUnpushedWork(ctx, p.lastTaskMerged)
		}
		if !p.wait {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Success}, "All tasks complete!")
			l.state.Write("status", "completed")
			return taskContext{}, actionDone
		}
		if allSkipped, _ := tasks.HasOpenButAllSkipped(l.taskBackend); allSkipped {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "All remaining tasks are skipped or blocked — exiting")
			l.state.Write("status", "all_skipped")
			return taskContext{}, actionDone
		}
		if resumed := l.waitForTasks(ctx); !resumed {
			return taskContext{}, actionDone
		}
		// Tasks appeared — re-enter selection to pick one up.
		return l.selectNextTaskInner(ctx, p, attempts+1)
	}

	// Only resume to the last task when starting fresh (first iteration of
	// this session). Mid-session, the agent exited without a completion
	// signal, so forcing a resume to the same task causes back-to-back
	// retries on the same ID. Let the backend pick by priority instead.
	var resumeID string
	if p.runIteration == 0 {
		resumeID, _ = l.state.Read("last_task_id")
	}
	skippedIDs, _ := l.state.GetSkippedTasks()
	taskInfo, _ := tasks.Next(l.taskBackend, resumeID, skippedIDs)
	taskID, nextTask := taskInfo.ID, taskInfo.Title

	if taskID == "" && nextTask == "" {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task backend returned empty — no task to run")
		if p.wait {
			if resumed := l.waitForTasks(ctx); !resumed {
				return taskContext{}, actionDone
			}
			return l.selectNextTaskInner(ctx, p, attempts+1)
		}
		return taskContext{}, actionDone
	}

	if p.completedIDs[taskID] {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task %s already completed this session — skipping", taskID)
		l.skipTask(taskID, "already_completed_this_session")
		// Recurse to try the next task. runIteration stays the same since
		// we didn't actually run anything.
		return l.selectNextTaskInner(ctx, p, attempts+1)
	}

	lastID, _ := l.state.Read("last_task_id")
	lastTask, _ := l.state.Read("last_task")
	changed := isNewTask(lastID, lastTask, taskID, nextTask)
	return taskContext{
		info:    taskInfo,
		id:      taskID,
		title:   nextTask,
		changed: changed,
	}, actionProceed
}
