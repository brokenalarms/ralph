package loop

import (
	"context"
	"fmt"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// phasedBackend overrides GetState to return a configurable phase per task ID.
// All other methods delegate to the embedded TrackingBackend (and through it,
// MutableBackend.GetExternalRef, StubBackend defaults).
type phasedBackend struct {
	testutil.TrackingBackend
	phases map[string]string
}

func (b *phasedBackend) GetState(taskID, key string) (string, error) {
	if key == "phase" {
		return b.phases[taskID], nil
	}
	return "", nil
}

// newTestLoopForSelection builds a minimal Loop for testing selectNextTask.
func newTestLoopForSelection(t *testing.T, backend *testutil.StubBackend) (*Loop, string) {
	t.Helper()
	dir, st := setupTestDir(t)
	l := &Loop{
		cfg: Config{
			MaxIterations: 100,
			Dirs:          workctx.WorkContext{RalphDir: dir + "/.ralph"},
		},
		state:       st,
		git:         git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		logger:      logging.New(nil),
		taskBackend: backend,
	}
	return l, dir
}

// selectNextTask returns actionProceed with the correct taskContext when a task is available.
func TestSelectNextTask_ReturnsTask(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-abc", NextTask: "Fix login"}
	l, _ := newTestLoopForSelection(t, backend)

	tc, action, _ := l.selectNextTask(context.Background(), selectNextTaskParams{
		completedIDs: map[string]bool{},
	})

	if action != actionProceed {
		t.Fatalf("expected actionProceed, got %v", action)
	}
	if tc.id != "ralph-abc" {
		t.Errorf("expected task ID ralph-abc, got %q", tc.id)
	}
	if tc.title != "Fix login" {
		t.Errorf("expected title 'Fix login', got %q", tc.title)
	}
}

// selectNextTask clears a stale sessionSkippedIDs entry for the task it
// selects, so a bead skipped earlier in the session and then re-released
// (bd update <id> -a=ralph-loop) doesn't have completeTask's push guard
// fire on its next, unrelated completion.
func TestSelectNextTask_ClearsStaleSessionSkippedID(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-abc", NextTask: "Fix login"}
	l, _ := newTestLoopForSelection(t, backend)
	l.sessionSkippedIDs = map[string]bool{"ralph-abc": true}

	tc, action, _ := l.selectNextTask(context.Background(), selectNextTaskParams{
		completedIDs: map[string]bool{},
	})

	if action != actionProceed {
		t.Fatalf("expected actionProceed, got %v", action)
	}
	if tc.id != "ralph-abc" {
		t.Errorf("expected task ID ralph-abc, got %q", tc.id)
	}
	if l.sessionSkippedIDs["ralph-abc"] {
		t.Error("expected stale sessionSkippedIDs entry for ralph-abc to be cleared on fresh selection")
	}
}

// selectNextTask returns actionDone when context is cancelled.
func TestSelectNextTask_ContextCancelled(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-abc", NextTask: "Fix login"}
	l, _ := newTestLoopForSelection(t, backend)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, action, _ := l.selectNextTask(ctx, selectNextTaskParams{
		completedIDs: map[string]bool{},
	})

	if action != actionDone {
		t.Fatalf("expected actionDone on cancelled context, got %v", action)
	}
}

// selectNextTask skips tasks in completedIDs and returns actionDone when no other task is available.
func TestSelectNextTask_SkipsCompletedIDs(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-done", NextTask: "Already done"}
	l, _ := newTestLoopForSelection(t, backend)

	_, action, _ := l.selectNextTask(context.Background(), selectNextTaskParams{
		completedIDs: map[string]bool{"ralph-done": true},
	})

	if action != actionDone {
		t.Fatalf("expected actionDone after exhausting attempts, got %v", action)
	}
	if backend.SkippedTask != "ralph-done" {
		t.Errorf("expected ralph-done to be skipped, got %q", backend.SkippedTask)
	}
}

// selectNextTask returns actionDone and writes "max_iterations_reached" when runIteration >= maxIterations.
func TestSelectNextTask_MaxIterationsReached(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-abc", NextTask: "Fix login"}
	l, _ := newTestLoopForSelection(t, backend)

	_, action, _ := l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration:  5,
		maxIterations: 5,
		completedIDs:  map[string]bool{},
	})

	if action != actionDone {
		t.Fatalf("expected actionDone, got %v", action)
	}
	status, _ := l.state.Read("status")
	if status != "max_iterations_reached" {
		t.Errorf("expected status max_iterations_reached, got %q", status)
	}
}

// selectNextTask never reports max_iterations_reached when maxIterations is 0
// (unlimited) — a run past the old default of 50 must keep going.
func TestSelectNextTask_MaxIterationsZeroIsUnlimited(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-abc", NextTask: "Fix login"}
	l, _ := newTestLoopForSelection(t, backend)
	// newTestLoopForSelection seeds state via setupTestDir's st.Init(5); clear
	// the positive state override so ReadMaxIterations falls back to the
	// unlimited default passed below instead of the fixture's 5.
	l.state.Write("max_iterations", "0")

	_, action, _ := l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration:  51,
		maxIterations: 0,
		completedIDs:  map[string]bool{},
	})

	if action != actionProceed {
		t.Fatalf("expected actionProceed with unlimited maxIterations, got %v", action)
	}
	status, _ := l.state.Read("status")
	if status == "max_iterations_reached" {
		t.Errorf("expected status to not be max_iterations_reached, got %q", status)
	}
}

// selectNextTask returns actionDone when no tasks remain and wait=false.
func TestSelectNextTask_NoTasksNoWait(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 0, Total: 1}
	l, _ := newTestLoopForSelection(t, backend)

	_, action, _ := l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration: 1, // not first iteration, so flushUnpushedWork is called
		completedIDs: map[string]bool{},
	})

	if action != actionDone {
		t.Fatalf("expected actionDone, got %v", action)
	}
	status, _ := l.state.Read("status")
	if status != "completed" {
		t.Errorf("expected status completed, got %q", status)
	}
}

// selectNextTask returns actionDone when no tasks exist and wait=false (first iteration).
func TestSelectNextTask_NoTasksAtAll(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 0, Total: 0}
	l, _ := newTestLoopForSelection(t, backend)

	_, action, _ := l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration: 0,
		completedIDs: map[string]bool{},
	})

	if action != actionDone {
		t.Fatalf("expected actionDone when no tasks exist, got %v", action)
	}
	status, _ := l.state.Read("status")
	if status != "error" {
		t.Errorf("expected status error, got %q", status)
	}
}

// l.selectNextTask uses completedIDs from the session to skip already-done tasks.
func TestLoop_selectNextTask_DelegatesToPackageFunc(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-new", NextTask: "New task"}
	l, _ := newTestLoopForSelection(t, backend)
	l.completedTasks = []CompletedTask{{ID: "ralph-old", Title: "Old task"}}

	completedIDs := make(map[string]bool, len(l.completedTasks))
	for _, ct := range l.completedTasks {
		completedIDs[ct.ID] = true
	}
	tc, action, _ := l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration:  0,
		maxIterations: l.cfg.MaxIterations,
		wait:          l.cfg.Wait,
		completedIDs:  completedIDs,
	})

	if action != actionProceed {
		t.Fatalf("expected actionProceed, got %v", action)
	}
	if tc.id != "ralph-new" {
		t.Errorf("expected ralph-new, got %q", tc.id)
	}
}

// selectNextTask calls SetResumeTaskID on the first iteration (runIteration==0)
// so a fresh start or resume picks up where it left off.
func TestSelectNextTask_SetResumeIDOnFirstIteration(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-abc", NextTask: "Fix login"}
	l, _ := newTestLoopForSelection(t, backend)
	l.state.Write("last_task_id", "ralph-abc")

	_, _, _ = l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration: 0,
		completedIDs: map[string]bool{},
	})

	if backend.ResumeIDSet != "ralph-abc" {
		t.Errorf("expected SetResumeTaskID(ralph-abc) on first iteration, got %q", backend.ResumeIDSet)
	}
}

// When wait=true and the ralph-loop inbox is empty (e.g. all tasks skipped and
// reassigned to ralph-task), selectNextTask must NOT exit immediately — it falls
// through to waitForTasks and polls for work. The loop exits only when Ctrl-C or
// a new task appears. Proved by: the waitHook fires (confirming waitForTasks was
// entered), and context cancellation from the hook causes actionDone with status
// "stopped". This is the AC5 preservation test: open tasks exist (in ralph-task
// inbox) but none are workable by the loop (loop inbox is empty).
func TestSelectNextTask_WaitMode_AllSkipped(t *testing.T) {
	falseVal := false
	backend := &testutil.StubBackend{
		Remaining:          1,         // CountRemaining returns 1 (open task exists globally)
		HasRemainingResult: &falseVal, // HasRemaining returns false (loop inbox is empty)
		Total:              1,
		NextID:             "",
		NextTask:           "",
	}
	l, _ := newTestLoopForSelection(t, backend)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waitHookCalled := false
	l.waitHook = &stubWaitHook{fn: func() {
		waitHookCalled = true
		cancel()
	}}

	_, action, _ := l.selectNextTask(ctx, selectNextTaskParams{
		runIteration:  1,
		maxIterations: 100,
		wait:          true,
		completedIDs:  map[string]bool{},
	})

	if !waitHookCalled {
		t.Error("expected waitForTasks to be entered (waitHook was not called)")
	}
	if action != actionDone {
		t.Fatalf("expected actionDone after context cancel in waitForTasks, got %v", action)
	}
	status, _ := l.state.Read("status")
	if status != "stopped" {
		t.Errorf("expected status %q after context cancel, got %q", "stopped", status)
	}
}

// selectNextTask does NOT call SetResumeTaskID on subsequent iterations
// (runIteration > 0) to prevent back-to-back retries on the same task after
// a no-signal exit.
func TestSelectNextTask_NoResumeIDOnSubsequentIterations(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-abc", NextTask: "Fix login"}
	l, _ := newTestLoopForSelection(t, backend)
	l.state.Write("last_task_id", "ralph-abc")

	_, _, _ = l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration: 1,
		completedIDs: map[string]bool{},
	})

	if backend.ResumeIDSet != "" {
		t.Errorf("expected no SetResumeTaskID call on iteration > 0, got %q", backend.ResumeIDSet)
	}
}

// TestSelectNextTask_HaltsOnInconsistentResumeState reproduces the sharpe-68w
// scenario: the loop resumes with current_task_id pointing to a bead whose
// phase=verified and whose PR is already merged, but CloseTask is rejected by
// a dependency block. The loop must halt with
// status=halted_inconsistent_resume_state on iteration 0.
func TestSelectNextTask_HaltsOnInconsistentResumeState(t *testing.T) {
	closeErr := fmt.Errorf("cannot close sharpe-68w: blocked by open issues [sharpe-8i2] (use --force to override)")
	backend := &phasedBackend{
		TrackingBackend: testutil.TrackingBackend{
			MutableBackend: testutil.MutableBackend{
				StubBackend: testutil.StubBackend{
					Remaining: 1,
					Total:     1,
					NextID:    "sharpe-8i2",
					NextTask:  "Dependent task",
				},
				ExternalRefs: map[string]string{
					"sharpe-68w": "https://github.com/owner/repo/pull/42",
				},
			},
			CloseErr: closeErr,
		},
		phases: map[string]string{"sharpe-68w": "verified"},
	}

	dir, st := setupTestDir(t)
	l := &Loop{
		cfg: Config{
			MaxIterations: 10,
			Dirs:          workctx.WorkContext{RalphDir: dir + "/.ralph"},
		},
		state: st,
		git: git.NewStub(git.StubRepoConfig{
			ProjectDir: dir,
			WorkDir:    dir,
			RemoteURL:  "https://github.com/owner/repo.git",
			GitHub: git.StubGitHubConfig{
				Available: true,
				PRs:       []git.StubPR{{Number: 42, State: git.PRStateMerged}},
			},
		}),
		logger:      logging.New(nil),
		taskBackend: backend,
	}

	// Seed a resume target pointing to the inconsistent bead.
	st.Write("current_task_id", "sharpe-68w")

	_, action, _ := l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration:  0,
		maxIterations: 10,
		completedIDs:  map[string]bool{},
	})

	if action != actionDone {
		t.Fatalf("expected actionDone on inconsistent resume, got %v", action)
	}
	status, _ := st.Read("status")
	if status != "halted_inconsistent_resume_state" {
		t.Errorf("expected status halted_inconsistent_resume_state, got %q", status)
	}
}

// selectNextTask halts with halted_blocked_by_in_progress when bd ready is empty
// because the only open tasks are blocked by an in_progress task held by a non-loop
// actor (human or external process). Proves: the loop does not enter the silent
// wait-for-tasks poll, and does not try to self-recover an external hold.
func TestSelectNextTask_HaltsOnBlockedByExternalInProgress(t *testing.T) {
	falseVal := false
	backend := &testutil.StubBackend{
		Remaining:          2,         // CountRemaining > 0: tasks exist
		HasRemainingResult: &falseVal, // bd ready empty: nothing is ready
		Total:              2,
		NextID:             "",
		NextTask:           "",
		AllInProgressTasks: []tasks.TaskInfo{{ID: "ralph-stuck", Title: "Stuck task", Assignee: "human-user"}},
		OpenDependents:     []string{"ralph-blocked"},
	}
	l, _ := newTestLoopForSelection(t, backend)

	waitHookCalled := false
	l.waitHook = &stubWaitHook{fn: func() { waitHookCalled = true }}

	_, action, _ := l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration:  1,
		maxIterations: 100,
		wait:          true,
		completedIDs:  map[string]bool{},
	})

	if action != actionDone {
		t.Fatalf("expected actionDone when blocked by external in_progress, got %v", action)
	}
	status, _ := l.state.Read("status")
	if status != "halted_blocked_by_in_progress" {
		t.Errorf("expected status halted_blocked_by_in_progress, got %q", status)
	}
	if waitHookCalled {
		t.Error("expected waitForTasks NOT to be entered when blocked by in_progress task")
	}
}

// reopenableBackend is a StubBackend that simulates the state transition when
// ReopenTask is called: the task moves from in_progress to open, so it
// disappears from AllInProgressTasks and becomes selectable via HasRemaining.
type reopenableBackend struct {
	testutil.StubBackend
	reopenCalls []string
}

func (b *reopenableBackend) ReopenTask(id string) error {
	b.reopenCalls = append(b.reopenCalls, id)
	// After reopen: task is open (not in_progress), clears the blocking stall.
	b.AllInProgressTasks = nil
	b.HasRemainingResult = nil // reverts to Remaining > 0 (task is now selectable)
	return nil
}

// orphanWithReadyQueueBackend simulates the target scenario: lower-priority
// ready work exists alongside a loop-owned orphaned in_progress claim that
// blocks a higher-priority task. After ReopenTask, the orphaned claim becomes
// the next selectable task (highest priority once reopened).
type orphanWithReadyQueueBackend struct {
	testutil.StubBackend
	reopenCalls []string
}

func (b *orphanWithReadyQueueBackend) ReopenTask(id string) error {
	b.reopenCalls = append(b.reopenCalls, id)
	b.AllInProgressTasks = nil
	b.NextID = id             // reopened task is now highest priority
	b.NextTask = "Reopened orphaned task"
	return nil
}

// selectNextTask reopens a loop-owned orphaned in_progress claim and re-enters
// selection instead of halting. Proves: a claim abandoned by a prior loop
// session (assignee=ralph-loop, status=in_progress, blocking a dependent) is
// self-recovered without human intervention.
func TestSelectNextTask_LoopOwnedOrphanedClaim_ReopensAndContinues(t *testing.T) {
	falseVal := false
	backend := &reopenableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:          2,         // CountRemaining > 0: tasks exist
			HasRemainingResult: &falseVal, // bd ready empty: orphaned claim blocks selection
			Total:              2,
			NextID:             "ralph-orphaned",
			NextTask:           "Orphaned task",
			AllInProgressTasks: []tasks.TaskInfo{{ID: "ralph-orphaned", Title: "Orphaned task", Assignee: "ralph-loop"}},
			OpenDependents:     []string{"ralph-blocked"},
		},
	}
	l, _ := newTestLoopForSelection(t, &backend.StubBackend)
	l.taskBackend = backend // override with the stateful backend

	waitHookCalled := false
	l.waitHook = &stubWaitHook{fn: func() { waitHookCalled = true }}

	tc, action, _ := l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration:  1,
		maxIterations: 100,
		wait:          false,
		completedIDs:  map[string]bool{},
	})

	// The orphaned claim is reopened — loop does NOT halt.
	if action != actionProceed {
		t.Fatalf("expected actionProceed after self-recovering orphaned claim, got %v", action)
	}
	if tc.id != "ralph-orphaned" {
		t.Errorf("expected task ralph-orphaned to be re-selected, got %q", tc.id)
	}
	status, _ := l.state.Read("status")
	if status == "halted_blocked_by_in_progress" {
		t.Error("loop-owned orphaned claim must NOT produce halted_blocked_by_in_progress")
	}
	if len(backend.reopenCalls) == 0 || backend.reopenCalls[0] != "ralph-orphaned" {
		t.Errorf("expected ReopenTask(ralph-orphaned), got reopenCalls=%v", backend.reopenCalls)
	}
	if waitHookCalled {
		t.Error("waitForTasks must NOT be entered during self-recovery")
	}
}

// selectNextTask reopens a loop-owned orphaned in_progress claim and re-enters
// selection even when lower-priority ready work exists in the queue. Proves: the
// orphan reap fires before the availability check, not only when the queue is empty,
// so an orphaned claim blocking higher-priority work is recovered before any
// lower-priority task is selected.
func TestSelectNextTask_OrphanedClaimReopens_WhenReadyQueueNonEmpty(t *testing.T) {
	backend := &orphanWithReadyQueueBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 3, // lower-priority ready work exists; HasRemaining = true
			Total:     3,
			NextID:    "ralph-low",
			NextTask:  "Low priority task",
			AllInProgressTasks: []tasks.TaskInfo{
				{ID: "ralph-orphaned", Title: "Orphaned task", Assignee: "ralph-loop"},
			},
			OpenDependents: []string{"ralph-high-priority"},
		},
	}
	l, _ := newTestLoopForSelection(t, &backend.StubBackend)
	l.taskBackend = backend

	tc, action, _ := l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration:  1,
		maxIterations: 100,
		completedIDs:  map[string]bool{},
	})

	if action != actionProceed {
		t.Fatalf("expected actionProceed after self-recovering orphaned claim, got %v", action)
	}
	// The orphaned task must be re-selected, not the lower-priority ready task.
	if tc.id != "ralph-orphaned" {
		t.Errorf("expected orphaned task ralph-orphaned to be re-selected, got %q (ralph-low must not win when orphan blocks higher-priority work)", tc.id)
	}
	if len(backend.reopenCalls) == 0 || backend.reopenCalls[0] != "ralph-orphaned" {
		t.Errorf("expected ReopenTask(ralph-orphaned), got reopenCalls=%v", backend.reopenCalls)
	}
	status, _ := l.state.Read("status")
	if status == "halted_blocked_by_in_progress" {
		t.Error("loop-owned orphaned claim must NOT produce halted_blocked_by_in_progress")
	}
}

// selectNextTask does NOT falsely halt when the backlog is genuinely empty
// (no open tasks at all, no in_progress tasks with blocked dependents).
// Proves: the empty-backlog path still enters the wait/complete path, not the stall path.
func TestSelectNextTask_EmptyBacklog_DoesNotFalselyHalt(t *testing.T) {
	falseVal := false
	backend := &testutil.StubBackend{
		Remaining:          0,         // no tasks of any kind
		HasRemainingResult: &falseVal, // bd ready empty
		Total:              0,
		NextID:             "",
		NextTask:           "",
		AllInProgressTasks: nil, // no in_progress tasks blocking anything
		OpenDependents:     nil,
	}
	l, _ := newTestLoopForSelection(t, backend)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waitHookCalled := false
	l.waitHook = &stubWaitHook{fn: func() {
		waitHookCalled = true
		cancel()
	}}

	_, action, _ := l.selectNextTask(ctx, selectNextTaskParams{
		runIteration:  1,
		maxIterations: 100,
		wait:          true,
		completedIDs:  map[string]bool{},
	})

	if !waitHookCalled {
		t.Error("expected waitForTasks to be entered for a genuinely empty backlog")
	}
	if action != actionDone {
		t.Fatalf("expected actionDone after context cancel in waitForTasks, got %v", action)
	}
	status, _ := l.state.Read("status")
	if status == "halted_blocked_by_in_progress" {
		t.Error("empty backlog must NOT produce halted_blocked_by_in_progress status")
	}
}
