package loop

import (
	"context"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/tasks"
)

const (
	// waitPollInterval is the delay between task polls while the backend is
	// answering normally.
	waitPollInterval = 5 * time.Second

	// pollFailureNotifyAfter is how many consecutive failures pass before the
	// user is notified. Set past the first couple of retries so a backend
	// restart does not raise an alert.
	pollFailureNotifyAfter = 4
)

// defaultPollFailureBackoffs is the delay schedule applied while task polling
// is failing. The final entry is the sustained retry rate for the rest of the
// outage, which is what bounds the cost of a backend that stays broken.
var defaultPollFailureBackoffs = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
}

// defaultPollFailureHaltAfter bounds a single outage. Past this the loop stops
// instead of retrying indefinitely.
const defaultPollFailureHaltAfter = 30 * time.Minute

// pollForTasks checks once for new tasks. Returns (found=true, _) if tasks
// are available, (false, done=true) if a stop condition was hit.
//
// A failing poll is not a stop condition on its own: the backend may be
// restarting. Consecutive failures are tracked so waitForTasks can back off
// and, past PollFailureHaltAfter, give up rather than hammer a backend that
// cannot answer.
func (l *Loop) pollForTasks() (found, done bool) {
	if l.state.CheckStop() {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "Stop file detected - halting")
		l.state.Write("status", "stopped")
		return false, true
	}
	hasRemaining, err := tasks.Poll(l.taskBackend)
	if err != nil {
		l.notePollFailure(err)
		return false, l.pollFailuresExhausted()
	}
	l.notePollSuccess()
	if hasRemaining {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Success}, "New tasks detected!")
		l.state.TouchPlanRefresh()
		return true, false
	}
	return false, false
}

// notePollFailure records one failed poll and warns. The warning is emitted
// on every failure while the delay is short and once per backoff step after
// that, because the delay itself is what throttles the log.
func (l *Loop) notePollFailure(err error) {
	l.consecutivePollFailures++
	if l.firstPollFailureAt.IsZero() {
		l.firstPollFailureAt = time.Now()
	}
	l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn},
		"Task check error during wait (failure %d, retrying in %s): %v",
		l.consecutivePollFailures, l.pollBackoff(), err)

	// Notify once per outage rather than once per retry: a backend down for
	// hours should produce one alert, not hundreds.
	if !l.pollFailureNotified && l.consecutivePollFailures >= pollFailureNotifyAfter {
		l.pollFailureNotified = true
		if l.cfg.Notify {
			notify.BackendUnavailable(l.cfg.Dirs.ProjectDir, l.consecutivePollFailures, err, time.Now())
		}
	}
}

// notePollSuccess clears the failure run so a recovered backend returns to the
// base poll interval immediately.
func (l *Loop) notePollSuccess() {
	if l.consecutivePollFailures > 0 {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Success},
			"Task polling recovered after %d consecutive failures", l.consecutivePollFailures)
	}
	l.consecutivePollFailures = 0
	l.firstPollFailureAt = time.Time{}
	l.pollFailureNotified = false
}

// pollFailuresExhausted reports whether polling has been failing long enough
// that continuing is pointless. Bounded by wall-clock time rather than a retry
// count so the answer does not change when the backoff schedule does.
func (l *Loop) pollFailuresExhausted() bool {
	if l.firstPollFailureAt.IsZero() {
		return false
	}
	limit := l.cfg.PollFailureHaltAfter
	if limit == 0 {
		limit = defaultPollFailureHaltAfter
	}
	if time.Since(l.firstPollFailureAt) < limit {
		return false
	}
	l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn},
		"Task backend unreachable for %s (%d consecutive failures) - halting",
		limit, l.consecutivePollFailures)
	l.state.Write("status", "stopped")
	return true
}

// pollBackoff returns the delay before the next poll: the base interval while
// polling is healthy, then successive entries of the backoff schedule, holding
// at the last entry for the remainder of an outage.
func (l *Loop) pollBackoff() time.Duration {
	if l.consecutivePollFailures == 0 {
		return waitPollInterval
	}
	schedule := l.cfg.PollFailureBackoffs
	if schedule == nil {
		schedule = defaultPollFailureBackoffs
	}
	if len(schedule) == 0 {
		return waitPollInterval
	}
	if l.consecutivePollFailures > len(schedule) {
		return schedule[len(schedule)-1]
	}
	return schedule[l.consecutivePollFailures-1]
}

func (l *Loop) waitForTasks(ctx context.Context) bool {
	l.logger.Emit(logging.Opts{Domain: logging.Beads}, "Waiting for new tasks (polling every %s)...", waitPollInterval)
	l.state.Write("status", "waiting")
	l.state.UpdateStreamTask("", "Waiting for tasks...", nil)
	l.state.TouchPlanRefresh()
	if l.waitHook != nil {
		l.waitHook.OnWait()
	}

	// Check immediately before waiting for the first tick.
	if found, done := l.pollForTasks(); found || done {
		return found
	}

	// Confirmed idle: no task is running and none is queued. This is the only
	// safe window for storage maintenance, which rewrites backend history.
	l.maybeCompactStore()

	// A timer rather than a ticker: the delay is recomputed after every poll so
	// a failing backend is retried on the backoff schedule instead of every
	// waitPollInterval. Polling a backend that answers with an error is not
	// free — each attempt costs a bd process, and against a wedged Dolt server
	// each one also strands a temp file.
	timer := time.NewTimer(l.pollBackoff())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			l.state.Write("status", "stopped")
			return false
		case <-timer.C:
			if found, done := l.pollForTasks(); found || done {
				return found
			}
			timer.Reset(l.pollBackoff())
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
func (l *Loop) logIterationBanner(p logIterationBannerParams, runIteration, maxIter int, task taskContext, lastAction analyzer.Action) {
	doneThisRun := len(l.completedTasks)
	remaining, err := l.taskBackend.CountRemaining()
	if err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CountRemaining error: %v", err)
	}

	if runIteration > 1 {
		l.logger.DashedSeparator(logging.Yellow)
	}

	baseBranch := ""
	if l.git != nil {
		baseBranch = l.git.DetectDefaultBranch()
	}

	l.logger.IterationBanner(logging.BannerOpts{
		RunIteration: runIteration,
		MaxIteration: maxIter,
		DoneThisRun:  doneThisRun,
		Remaining:    remaining,
		TaskID:       task.id,
		TaskTitle:    task.title,
		TaskChanged:  task.changed,
		Priority:     task.info.Priority,
		Version:      p.version,
		BaseBranch:   baseBranch,
		WarnPhase:    lastAction == analyzer.Warn,
		Description:  l.taskDescription(task.id),
	})
}
