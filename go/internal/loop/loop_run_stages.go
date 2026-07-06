package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// loopState bundles the local variables Run threads across iterations of its
// main loop. Passed by pointer so the stage helpers below can mutate it in
// place instead of Run mutating a handful of loose locals inline.
type loopState struct {
	runIteration         int
	lastAction           analyzer.Action
	lastTaskMerged       bool
	sessionTasks         []CompletedTask
	currentTaskID        string
	consecutiveSkipCount int
	worktreeNeedsSetup   bool
}

// branchSetupOutcome tells Run what to do after setupBranchForTask returns.
type branchSetupOutcome int

const (
	branchProceed  branchSetupOutcome = iota // branch is ready; continue the iteration
	branchContinue                           // transient error — skip this task, continue iterLoop
	branchHalt                               // unrecoverable error — break iterLoop
)

// setupBranchForTask prepares (or reuses) the task's branch for this
// iteration. It only calls into git.BranchForTask when the task changed
// since the last iteration or the worktree isn't already on a renamed
// per-task branch — otherwise the existing branch is reused as-is.
func (l *Loop) setupBranchForTask(ctx context.Context, task taskContext) branchSetupOutcome {
	if !task.changed && l.git.IsBranchRenamed() {
		return branchProceed
	}

	storedBranch, _ := l.taskBackend.GetMetadata(task.id, "branch")
	storedExternalRef, _ := l.taskBackend.GetExternalRef(task.id)
	completedBranches := l.completedBranches()
	branch, err := l.git.BranchForTask(ctx, task.id, task.title, git.BranchTaskMeta{
		Branch:            storedBranch,
		ExternalRef:       storedExternalRef,
		CompletedBranches: completedBranches,
	})
	if err != nil {
		if ctx.Err() != nil {
			l.state.Write("status", "stopped")
			l.setPhaseInterrupted(task.id)
			return branchHalt
		}
		var transportErr *git.TransportError
		if errors.As(err, &transportErr) {
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
				"Branch setup failed (transient transport error) — skipping task %s: %v", task.id, err)
			l.skipTask(task.id, tasks.SkipTransportError, transportErr.Op)
			l.consecutiveNoAgentIters++
			return branchContinue
		}
		l.state.Write("status", "error")
		return branchHalt
	}
	l.state.WriteRunBranch(branch)
	if task.id != "" && branch != "" && strings.Contains(branch, task.id) {
		_ = l.taskBackend.SetMetadata(task.id, "branch", branch)
	}
	return branchProceed
}

// resumeDecision tells Run what to do after resumeCheck returns.
type resumeDecision int

const (
	resumeRunAgent     resumeDecision = iota // no existing PR handling applies — run the agent
	resumeShipExisting                       // an open PR exists — ship it without running the agent
	resumeHandled                            // ResumeTask fully handled this task (e.g. already merged) — continue iterLoop
	resumeHalt                               // unrecoverable error — break iterLoop
)

// resumeCheck asks git.ResumeTask whether this task already has an in-flight
// PR that needs finishing (ship/merge, or discovery that it's already been
// merged) before falling back to running the agent from scratch.
func (l *Loop) resumeCheck(ctx context.Context, task taskContext) resumeDecision {
	branch, _ := l.taskBackend.GetMetadata(task.id, "branch")
	if branch != "" && !strings.Contains(branch, task.id) {
		branch = ""
	}
	externalRef, _ := l.taskBackend.GetExternalRef(task.id)
	if externalRef != "" {
		l.ensureActiveReviewers(ctx)
	}
	resumeResult, resumeErr := l.git.ResumeTask(ctx, git.ResumeTaskMeta{
		TaskID:      task.id,
		TaskTitle:   task.title,
		Branch:      branch,
		ExternalRef: externalRef,
	}, git.ResumeTaskOpts{
		AutoMerge:       l.cfg.AutoMerge,
		Reviewers:       l.activeReviewers,
		ReviewAddressed: l.reviewAddressedForTask(task.id, l.activeReviewers),
	})
	if resumeErr != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "ResumeTask: %v", resumeErr)
	}
	if resumeResult.PRURLToStore != "" && task.id != "" {
		_ = l.taskBackend.SetExternalRef(task.id, resumeResult.PRURLToStore)
	}
	if resumeResult.ClearMetadata && task.id != "" {
		_ = l.taskBackend.SetExternalRef(task.id, "")
		if resumeResult.NewBranch != "" {
			_ = l.taskBackend.SetMetadata(task.id, "branch", resumeResult.NewBranch)
		} else {
			_ = l.taskBackend.SetMetadata(task.id, "branch", "")
		}
	}
	if resumeResult.ShipFailedAfterPush {
		shipErrStr := "unknown Ship error"
		if resumeResult.ShipErr != nil {
			shipErrStr = resumeResult.ShipErr.Error()
		}
		l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error},
			"Task %s: Ship failed after pushed commits on branch %s: %s — Manual recovery: retry Ship manually after the gh issue resolves, or close the branch's PR if one was created",
			task.id, branch, shipErrStr)
		l.state.Write("status", "halted_ship_failed_with_pushed_work")
		return resumeHalt
	}
	if resumeResult.Handled {
		// Only the already-merged case reaches Handled now (ResumeTask is
		// discovery only). Close the bead and move on. This is forward
		// progress — the ready queue strictly shrinks — so reset the counter
		// the same way an agent invocation does, rather than incrementing it.
		l.onResumeDone(ctx, task.id, task.title, resumeResult)
		l.git.TagTaskEnd(task.id)
		l.state.WriteRunBranch("")
		l.consecutiveNoAgentIters = 0
		return resumeHandled
	}
	if resumeResult.ShipExisting {
		return resumeShipExisting
	}
	return resumeRunAgent
}

// dispatchAction tells Run what to do after dispatchAgentAction returns.
type dispatchAction int

const (
	dispatchFallthrough dispatchAction = iota // proceed to the signal-detection gate
	dispatchContinue                          // continue iterLoop — already handled
	dispatchBreak                             // break iterLoop
)

// dispatchOutcome carries the result of dispatchAgentAction back to Run.
type dispatchOutcome struct {
	action          dispatchAction
	haveOut         bool
	out             completeTaskOut
	skipSignalCheck bool
}

// dispatchAgentAction handles agentRun.action values other than
// actionProceed: retry, compaction-triggered ship, and the skip cascade.
func (l *Loop) dispatchAgentAction(ctx context.Context, task taskContext, agentRun agentRunResult, st *loopState) dispatchOutcome {
	switch agentRun.action {
	case actionRetry:
		return dispatchOutcome{action: dispatchContinue}
	case actionCompactionShip:
		// Compaction fired after the agent committed verified work.
		// Route through completeTask so the work ships normally.
		// skipSignalCheck prevents the signal-detection gate from reopening
		// a task that completeTask just closed.
		out := l.completeTask(ctx, completeTaskParams{
			result:     agentRun.result,
			headBefore: agentRun.prep.headBefore,
			workDir:    agentRun.prep.workDir,
			rawLogPath: agentRun.prep.rawLogPath,
			diffStat:   l.git.DiffStatRange(agentRun.prep.headBefore, l.git.HeadRev()),
			taskID:     task.id,
			nextTask:   task.title,
			notify:     l.cfg.Notify,
			ralphDir:   l.cfg.Dirs.RalphDir,
		})
		return dispatchOutcome{action: dispatchFallthrough, haveOut: true, out: out, skipSignalCheck: true}
	case actionSkip:
		// A skipped task's partial commits must be abandoned, not left
		// on the worktree branch. The flush safety-net (selectNextTask →
		// FlushUnpushedWork) pushes and auto-merges any branch ahead of
		// origin/main when no ready tasks remain, with no verification
		// gate — so a surviving skipped branch ships unverified work.
		// Tear down here (mirroring the signalSkipped path) so the next
		// iteration starts from a clean worktree.
		l.teardownWorktree()
		st.worktreeNeedsSetup = true
		// Task skipped. Same-reason recurrences already halted at the
		// skip site (haltAppWide) before reaching here — this numeric
		// cascade is the fallback for runs of skips with differing
		// reasons, which the reason-aware check does not catch.
		st.consecutiveSkipCount++
		if st.consecutiveSkipCount >= l.cascadeSkipLimit() {
			haltReason := fmt.Sprintf("cascade_skipped:%d", st.consecutiveSkipCount)
			l.logger.Emit(logging.Opts{Domain: logging.Analyzer, Level: logging.Error}, "Halting: %s", haltReason)
			l.state.Write("status", "halted_"+haltReason)
			return dispatchOutcome{action: dispatchBreak}
		}
		l.state.WriteRunBranch("")
		st.currentTaskID = ""
		return dispatchOutcome{action: dispatchContinue}
	default:
		return dispatchOutcome{action: dispatchBreak}
	}
}

// aftermathAction tells Run what to do after runAftermath returns.
type aftermathAction int

const (
	aftermathNext     aftermathAction = iota // fall through to the top of iterLoop
	aftermathContinue                        // continue iterLoop early
	aftermathBreak                           // break iterLoop
)

// runAftermath processes the completeTaskOut produced by either the
// ship-existing-PR path or the agent-run path: it records the completed
// task, fires the post-task hook (and evolve check), tears down the
// worktree when the task shipped or was skipped, and tags the task end.
func (l *Loop) runAftermath(ctx context.Context, task taskContext, haveOut bool, out completeTaskOut, st *loopState) aftermathAction {
	if haveOut {
		if out.ct != nil {
			st.sessionTasks = append(st.sessionTasks, *out.ct)
			emitTaskSummary(*out.ct, l.logger)
		}
		if out.merged {
			st.lastTaskMerged = true
			// A task actually shipped — real progress breaks the skip streak,
			// since whatever reason caused prior skips is not app-wide.
			l.resetSkipStreak()
		}
		if out.action == signalRetry {
			return aftermathContinue
		}
		// signalSkipped or signalComplete: fire post-task hook and check for
		// binary rebuild. On signalRetry the task is not yet done — skipped above.
		if ctx.Err() == nil {
			if res := l.postTaskAndMaybeEvolve(ctx, task.id, out.prNumber, out.merged); res == signalEvolve {
				return aftermathBreak
			}
		}
		if out.action == signalSkipped {
			l.teardownWorktree()
			st.worktreeNeedsSetup = true
			l.state.WriteRunBranch("")
			return aftermathContinue
		}
		// signalComplete: fall through to tagTaskEnd
	}
	l.git.TagTaskEnd(task.id)
	if out.merged || out.prNumber > 0 {
		l.teardownWorktree()
		st.worktreeNeedsSetup = true
	}
	l.state.WriteRunBranch("")
	st.currentTaskID = ""
	return aftermathNext
}
