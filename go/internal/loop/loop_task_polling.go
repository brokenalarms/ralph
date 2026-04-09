package loop

import (
	"context"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
)

type pollForTasksParams struct {
	state   *state.Store
	backend tasks.Backend
	logger  *logging.Logger
}

// pollForTasks checks once for new tasks. Returns (found=true, _) if tasks
// are available, (false, done=true) if a stop condition was hit.
func pollForTasks(p pollForTasksParams) (found, done bool) {
	if p.state.CheckStop() {
		p.logger.Emit(logging.Opts{Level: logging.Warn}, "Stop file detected - halting")
		p.state.Write("status", "stopped")
		return false, true
	}
	if skipped, err := p.state.GetSkippedTasks(); err == nil {
		p.backend.SetSkippedIDs(skipped)
	}
	hasRemaining, err := p.backend.HasRemaining()
	if err != nil {
		p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task check error during wait: %v", err)
		return false, false
	}
	if hasRemaining {
		p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Success}, "New tasks detected!")
		p.state.TouchPlanRefresh()
		return true, false
	}
	return false, false
}

type waitForTasksParams struct {
	logger     *logging.Logger
	state      *state.Store
	backend    tasks.Backend
	onWaitFunc func()
}

func waitForTasks(ctx context.Context, p waitForTasksParams) bool {
	const waitPollInterval = 5 * time.Second
	p.logger.Emit(logging.Opts{Domain: logging.Beads}, "Waiting for new tasks (polling every %s)...", waitPollInterval)
	p.state.Write("status", "waiting")
	p.state.UpdateStreamTask("", "Waiting for tasks...", nil)
	p.state.TouchPlanRefresh()
	if p.onWaitFunc != nil {
		p.onWaitFunc()
	}

	poll := pollForTasksParams{state: p.state, backend: p.backend, logger: p.logger}

	// Check immediately before waiting for the first tick.
	if found, done := pollForTasks(poll); found || done {
		return found
	}

	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.state.Write("status", "stopped")
			return false
		case <-ticker.C:
			if found, done := pollForTasks(poll); found || done {
				return found
			}
		}
	}
}

type beginIterationParams struct {
	state *state.Store
	git   git.GitOps
}

// beginIteration records that a task iteration is starting.
func beginIteration(p beginIterationParams, task taskContext, iteration int) {
	p.state.TouchPlanRefresh()
	p.state.BeginIteration(task.id, task.title, iteration)
	p.git.TagTaskStart(task.id)
	p.state.UpdateStreamTask(task.id, task.title, task.info.Priority)
}

type logIterationBannerParams struct {
	backend tasks.Backend
	state   *state.Store
	logger  *logging.Logger
	version string
}

// logIterationBanner gathers context and delegates to the logger.
func logIterationBanner(p logIterationBannerParams, runIteration, maxIter, iteration int, task taskContext, lastAction analyzer.Action) {
	completed, _ := p.backend.CountCompleted()
	total, _ := p.backend.CountTotal()

	if runIteration > 1 {
		p.logger.DashedSeparator(logging.Yellow)
	}

	p.logger.IterationBanner(logging.BannerOpts{
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
		Description:  getBeadDescription(p.backend, task.id),
	})
}
