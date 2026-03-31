package loop

import (
	"context"

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

// selectNextTask checks all stop conditions and selects the next task.
// Returns actionDone when the loop should exit (max iterations, stop file,
// context cancelled, all tasks complete). Returns actionProceed with a
// taskContext when a task is ready to run.
//
// Handles: max iterations, context cancellation, stop file, no remaining
// tasks (with wait mode), empty backend response, and completed-task dedup.
func (l *Loop) selectNextTask(ctx context.Context, runIteration int) (taskContext, loopAction) {
	return l.selectNextTaskInner(ctx, runIteration, 0)
}

const maxSelectionAttempts = 50

func (l *Loop) selectNextTaskInner(ctx context.Context, runIteration, attempts int) (taskContext, loopAction) {
	if attempts >= maxSelectionAttempts {
		l.logger.Warn("beads", "Exhausted %d task selection attempts", maxSelectionAttempts)
		l.state.Write("status", "error")
		return taskContext{}, actionDone
	}
	maxIter := l.state.ReadMaxIterations(l.cfg.MaxIterations)

	if runIteration >= maxIter {
		l.logger.Warn("", "Max iterations (%d) reached", maxIter)
		l.state.Write("status", "max_iterations_reached")
		return taskContext{}, actionDone
	}

	if err := ctx.Err(); err != nil {
		l.logger.Warn("", "Interrupted — stopping")
		l.state.Write("status", "stopped")
		return taskContext{}, actionDone
	}

	if checkStopFile(l.cfg.Dirs.RalphDir) {
		l.logger.Warn("", "Stop file detected - halting")
		l.state.Write("status", "stopped")
		return taskContext{}, actionDone
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
				return taskContext{}, actionDone
			}
		}
		if runIteration > 0 {
			l.flushUnpushedWork(ctx)
		}
		if !l.cfg.Wait {
			l.logger.Success("beads", "All tasks complete!")
			l.state.Write("status", "completed")
			return taskContext{}, actionDone
		}
		if resumed := l.waitForTasks(ctx); !resumed {
			return taskContext{}, actionDone
		}
		// Tasks appeared — re-enter selection to pick one up.
		return l.selectNextTaskInner(ctx, runIteration, attempts+1)
	}

	if lastID, _ := l.state.Read("last_task_id"); lastID != "" {
		l.cfg.TaskBackend.SetResumeTaskID(lastID)
	}
	taskInfo, _ := l.cfg.TaskBackend.GetNextTaskInfo()
	taskID, nextTask := taskInfo.ID, taskInfo.Title

	if taskID == "" && nextTask == "" {
		l.logger.Warn("beads", "Task backend returned empty — no task to run")
		if l.cfg.Wait {
			if resumed := l.waitForTasks(ctx); !resumed {
				return taskContext{}, actionDone
			}
			return l.selectNextTaskInner(ctx, runIteration, attempts+1)
		}
		return taskContext{}, actionDone
	}

	if l.wasCompletedThisSession(taskID) {
		l.logger.Warn("beads", "Task %s already completed this session — skipping", taskID)
		skipTask(l.cfg.TaskBackend, l.state, l.logger, taskID, "already_completed_this_session")
		// Recurse to try the next task. runIteration stays the same since
		// we didn't actually run anything.
		return l.selectNextTaskInner(ctx, runIteration, attempts+1)
	}

	changed := isNewTask(l.state, taskID, nextTask)
	return taskContext{
		info:    taskInfo,
		id:      taskID,
		title:   nextTask,
		changed: changed,
	}, actionProceed
}
