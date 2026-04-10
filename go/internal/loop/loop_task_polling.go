package loop

import (
	"context"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// pollForTasks checks once for new tasks. Returns (found=true, _) if tasks
// are available, (false, done=true) if a stop condition was hit.
func (l *Loop) pollForTasks() (found, done bool) {
	if l.state.CheckStop() {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "Stop file detected - halting")
		l.state.Write("status", "stopped")
		return false, true
	}
	skipped, _ := l.state.GetSkippedTasks()
	hasRemaining, err := tasks.Poll(l.cfg.TaskBackend, skipped)
	if err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task check error during wait: %v", err)
		return false, false
	}
	if hasRemaining {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Success}, "New tasks detected!")
		l.state.TouchPlanRefresh()
		return true, false
	}
	return false, false
}

func (l *Loop) waitForTasks(ctx context.Context) bool {
	const waitPollInterval = 5 * time.Second
	l.logger.Emit(logging.Opts{Domain: logging.Beads}, "Waiting for new tasks (polling every %s)...", waitPollInterval)
	l.state.Write("status", "waiting")
	l.state.UpdateStreamTask("", "Waiting for tasks...", nil)
	l.state.TouchPlanRefresh()
	if l.cfg.OnWait != nil {
		l.cfg.OnWait()
	}

	// Check immediately before waiting for the first tick.
	if found, done := l.pollForTasks(); found || done {
		return found
	}

	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			l.state.Write("status", "stopped")
			return false
		case <-ticker.C:
			if found, done := l.pollForTasks(); found || done {
				return found
			}
		}
	}
}

// beginIteration records that a task iteration is starting.
func (l *Loop) beginIteration(task taskContext, iteration int) {
	l.state.TouchPlanRefresh()
	l.state.BeginIteration(task.id, task.title, iteration)
	l.git.TagTaskStart(task.id)
	l.state.UpdateStreamTask(task.id, task.title, task.info.Priority)
}

// logIterationBannerParams carries the data needed for logIterationBanner
// that is not available on the Loop receiver.
type logIterationBannerParams struct {
	version string
}

// logIterationBanner gathers context and delegates to the logger.
func (l *Loop) logIterationBanner(p logIterationBannerParams, runIteration, maxIter, iteration int, task taskContext, lastAction analyzer.Action) {
	completed, total := tasks.Progress(l.cfg.TaskBackend)

	if runIteration > 1 {
		l.logger.DashedSeparator(logging.Yellow)
	}

	l.logger.IterationBanner(logging.BannerOpts{
		RunIteration: runIteration,
		MaxIteration: maxIter,
		Lifetime:     iteration,
		Completed:    completed,
		Total:        total,
		TaskID:       task.id,
		TaskTitle:    task.title,
		TaskChanged:  task.changed,
		Priority:     task.info.Priority,
		Version:      p.version,
		WarnPhase:    lastAction == analyzer.Warn,
		Description:  l.taskDescription(task.id),
	})
}
