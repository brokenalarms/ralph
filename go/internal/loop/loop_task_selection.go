package loop

import (
	"context"
	"fmt"
	"strings"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// loopAction describes what Run() should do after a phase completes.
type loopAction int

const (
	actionProceed        loopAction = iota // continue to next phase
	actionRetry                            // re-run the current iteration
	actionDone                             // exit the loop
	actionSkip                             // task skipped by analyzer, continue to next task
	actionCompactionShip                   // compaction fired but branch has verified commits — ship them
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
// The waited return value is true when waitForTasks was entered during this
// selection. iterLoop uses this to trigger postTaskAndMaybeEvolve before
// starting the new iteration — catching any binary rebuild that occurred
// while the loop was idle.
//
// Handles: max iterations, context cancellation, stop file, no remaining
// tasks (with wait mode), empty backend response, and completed-task dedup.
func (l *Loop) selectNextTask(ctx context.Context, p selectNextTaskParams) (taskContext, loopAction, bool) {
	return l.selectNextTaskInner(ctx, p, 0, false)
}

const maxSelectionAttempts = 50

func (l *Loop) selectNextTaskInner(ctx context.Context, p selectNextTaskParams, attempts int, waited bool) (taskContext, loopAction, bool) {
	if attempts >= maxSelectionAttempts {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Exhausted %d task selection attempts", maxSelectionAttempts)
		l.state.Write("status", "error")
		return taskContext{}, actionDone, waited
	}
	maxIter := l.state.ReadMaxIterations(p.maxIterations)

	if maxIter > 0 && p.runIteration >= maxIter {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "Max iterations (%d) reached", maxIter)
		l.state.Write("status", "max_iterations_reached")
		return taskContext{}, actionDone, waited
	}

	if err := ctx.Err(); err != nil {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "Interrupted — stopping")
		l.state.Write("status", "stopped")
		return taskContext{}, actionDone, waited
	}

	if l.state.CheckStop() {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "Stop file detected - halting")
		l.state.Write("status", "stopped")
		return taskContext{}, actionDone, waited
	}

	if l.consecutiveNoAgentIters >= 2 {
		l.logger.Emit(logging.Opts{Level: logging.Error}, "Halting: %d consecutive iterations with no agent run", l.consecutiveNoAgentIters)
		l.state.Write("status", "halted_no_agent_progress")
		return taskContext{}, actionDone, waited
	}

	// Reap loop-owned orphaned claims that block higher-priority open tasks before
	// the ready-queue check. Runs regardless of queue state so a non-empty backlog
	// does not starve higher-priority blocked dependents. External stuck claims
	// (stuckHalt) are left to the empty-queue path below.
	if l.reaperLoopOrphanedClaims() == stuckRetry {
		return l.selectNextTaskInner(ctx, p, attempts+1, waited)
	}

	avail, err := tasks.CheckAvailability(l.taskBackend)
	if err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task check error: %v", err)
	}
	if !avail.HasRemaining {
		if p.runIteration == 0 && !avail.HasAny && !p.wait {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Error}, "No tasks found — run ralph task to create tasks")
			l.state.Write("status", "error")
			return taskContext{}, actionDone, waited
		}
		if p.runIteration > 0 {
			l.flushUnpushedWork(ctx, p.lastTaskMerged)
		}
		switch l.detectStuckInProgress() {
		case stuckHalt:
			return taskContext{}, actionDone, waited
		case stuckRetry:
			return l.selectNextTaskInner(ctx, p, attempts+1, waited)
		}
		if !p.wait {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Success}, "All tasks complete!")
			l.state.Write("status", "completed")
			return taskContext{}, actionDone, waited
		}
		if resumed := l.waitForTasks(ctx); !resumed {
			return taskContext{}, actionDone, true
		}
		// Tasks appeared — re-enter selection to pick one up.
		return l.selectNextTaskInner(ctx, p, attempts+1, true)
	}

	// Only resume to the last task when starting fresh (first iteration of
	// this session). Mid-session, the agent exited without a completion
	// signal, so forcing a resume to the same task causes back-to-back
	// retries on the same ID. Let the backend pick by priority instead.
	var resumeID string
	if p.runIteration == 0 {
		resumeID, _ = l.state.Read("current_task_id")
		if resumeID != "" {
			if action, ok := l.validateResumeState(ctx, resumeID, waited); !ok {
				return taskContext{}, action, waited
			}
			// Re-read after potential clear by validateResumeState.
			resumeID, _ = l.state.Read("current_task_id")
		}
	}
	taskInfo, _ := tasks.Next(l.taskBackend, resumeID)
	taskID, nextTask := taskInfo.ID, taskInfo.Title

	if taskID == "" && nextTask == "" {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task backend returned empty — no task to run")
		switch l.detectStuckInProgress() {
		case stuckHalt:
			return taskContext{}, actionDone, waited
		case stuckRetry:
			return l.selectNextTaskInner(ctx, p, attempts+1, waited)
		}
		if p.wait {
			if resumed := l.waitForTasks(ctx); !resumed {
				return taskContext{}, actionDone, true
			}
			return l.selectNextTaskInner(ctx, p, attempts+1, true)
		}
		return taskContext{}, actionDone, waited
	}

	if p.completedIDs[taskID] {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task %s already completed this session — skipping", taskID)
		l.skipTask(taskID, tasks.SkipAlreadyCompleted, "")
		// Recurse to try the next task. runIteration stays the same since
		// we didn't actually run anything.
		return l.selectNextTaskInner(ctx, p, attempts+1, waited)
	}

	lastID, _ := l.state.Read("current_task_id")
	lastTask, _ := l.state.Read("last_task")
	changed := isNewTask(lastID, lastTask, taskID, nextTask)
	return taskContext{
		info:    taskInfo,
		id:      taskID,
		title:   nextTask,
		changed: changed,
	}, actionProceed, waited
}

// stuckOutcome is the result of detectStuckInProgress.
type stuckOutcome int

const (
	stuckNone  stuckOutcome = iota // no stuck state detected
	stuckRetry                     // loop-owned claims reopened; caller must re-enter selection
	stuckHalt                      // external actor holds the bead; caller must halt
)

// detectStuckInProgress checks whether the empty ready queue is caused by
// in_progress tasks that have open dependents.
//   - stuckNone: no blocking in_progress tasks detected.
//   - stuckRetry: loop-owned orphaned claims were reopened; caller should re-enter selection.
//   - stuckHalt: non-loop actor holds a stuck task; caller should write
//     halted_blocked_by_in_progress and exit.
func (l *Loop) detectStuckInProgress() stuckOutcome {
	stuck, err := tasks.DetectBlockedByInProgress(l.taskBackend)
	if err != nil || stuck == nil {
		return stuckNone
	}
	if len(stuck.ExternalStuckTaskIDs) > 0 {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Error},
			"Halting: stuck in_progress task(s) [%s] are blocking [%s] — close or reassign the stuck task(s) to unblock",
			strings.Join(stuck.ExternalStuckTaskIDs, ", "), strings.Join(stuck.BlockedTaskIDs, ", "))
		l.state.Write("status", "halted_blocked_by_in_progress")
		return stuckHalt
	}
	for _, id := range stuck.LoopOwnedStuckTaskIDs {
		if reopenErr := l.taskBackend.ReopenTask(id); reopenErr != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Error},
				"Failed to reopen orphaned claim %s: %v — halting", id, reopenErr)
			l.state.Write("status", "halted_blocked_by_in_progress")
			return stuckHalt
		}
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Info},
			"Self-recovered orphaned claim %s (reopened) — re-entering selection", id)
	}
	return stuckRetry
}

// reaperLoopOrphanedClaims detects loop-owned in_progress claims that block
// higher-priority open tasks and reopens them so they re-enter selection.
// Returns stuckRetry when any orphaned claims were reopened, stuckNone when
// none exist. Does not check external stuck claims — those are handled by
// detectStuckInProgress in the empty-queue path.
func (l *Loop) reaperLoopOrphanedClaims() stuckOutcome {
	stuck, err := tasks.DetectBlockedByInProgress(l.taskBackend)
	if err != nil || stuck == nil || len(stuck.LoopOwnedStuckTaskIDs) == 0 {
		return stuckNone
	}
	for _, id := range stuck.LoopOwnedStuckTaskIDs {
		if reopenErr := l.taskBackend.ReopenTask(id); reopenErr != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Error},
				"Failed to reopen orphaned claim %s: %v — halting", id, reopenErr)
			l.state.Write("status", "halted_blocked_by_in_progress")
			return stuckHalt
		}
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Info},
			"Self-recovered orphaned claim %s (reopened) — re-entering selection", id)
	}
	return stuckRetry
}

// validateResumeState checks whether a resumed task is genuinely resumable.
// If the bead has phase=verified AND an external-ref pointing to a merged PR
// AND is still open/in_progress, the loop attempts to close it. If the close
// is rejected by a dependency block, the loop halts with
// status=halted_inconsistent_resume_state rather than silently retrying
// (which was the bug that wasted 50 iterations on sharpe-68w).
//
// Returns (actionDone, false) if the caller should stop (halt or close
// succeeded and resumeID is cleared), or (_, true) if the task is genuinely
// resumable and iteration should continue normally.
func (l *Loop) validateResumeState(ctx context.Context, resumeID string, waited bool) (loopAction, bool) {
	// A resumed task that already shipped a PR (external-ref set) may have been
	// merged while the loop was down. Detect and reconcile that; otherwise let
	// the iteration proceed (and re-verify) normally.
	externalRef, _ := l.taskBackend.GetExternalRef(resumeID)
	prNum := parsePRNumber(externalRef)
	if prNum == 0 {
		return actionProceed, true
	}
	prState, err := l.git.GetPRState(ctx, prNum)
	if err != nil || prState != git.PRStateMerged {
		return actionProceed, true
	}

	// PR merged + bead still open: inconsistent state.
	// Attempt close — if it succeeds, clear and continue fresh selection.
	// If dep-blocked, halt with all diagnostic context.
	closeReason := fmt.Sprintf("Fixed in %s (recovered from inconsistent resume state)", externalRef)
	closeErr := l.taskBackend.CloseTask(resumeID, closeReason)
	if closeErr == nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads}, "Closed stale in-flight task %s (PR #%d merged)", resumeID, prNum)
		l.state.ClearCurrentTask()
		return actionProceed, true
	}

	// Close failed — extract dep blockers and halt.
	blockers := tasks.ParseDependencyBlock(closeErr)
	var blockerStr string
	if len(blockers) > 0 {
		blockerStr = strings.Join(blockers, ", ")
	} else {
		blockerStr = closeErr.Error()
	}
	l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Error},
		"Task %s: PR #%d merged — bead not closed; close blocked by [%s]. Manual recovery: bd close %s --force --reason 'dep block cleared'",
		resumeID, prNum, blockerStr, resumeID)
	l.state.Write("status", "halted_inconsistent_resume_state")
	return actionDone, false
}
