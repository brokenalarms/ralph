package loop

import (
	"context"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

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

	tc, action := l.selectNextTask(context.Background(), selectNextTaskParams{
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

// selectNextTask returns actionDone when context is cancelled.
func TestSelectNextTask_ContextCancelled(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-abc", NextTask: "Fix login"}
	l, _ := newTestLoopForSelection(t, backend)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, action := l.selectNextTask(ctx, selectNextTaskParams{
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

	_, action := l.selectNextTask(context.Background(), selectNextTaskParams{
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

	_, action := l.selectNextTask(context.Background(), selectNextTaskParams{
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

// selectNextTask returns actionDone when no tasks remain and wait=false.
func TestSelectNextTask_NoTasksNoWait(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 0, Total: 1}
	l, _ := newTestLoopForSelection(t, backend)

	_, action := l.selectNextTask(context.Background(), selectNextTaskParams{
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

	_, action := l.selectNextTask(context.Background(), selectNextTaskParams{
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
	tc, action := l.selectNextTask(context.Background(), selectNextTaskParams{
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

	l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration: 0,
		completedIDs: map[string]bool{},
	})

	if backend.ResumeIDSet != "ralph-abc" {
		t.Errorf("expected SetResumeTaskID(ralph-abc) on first iteration, got %q", backend.ResumeIDSet)
	}
}

// selectNextTask exits with actionDone and status all_skipped when wait=true
// but all open tasks are in the skipped list — avoids infinite poll loop.
func TestSelectNextTask_WaitMode_AllSkipped(t *testing.T) {
	falseVal := false
	backend := &testutil.StubBackend{
		Remaining:          1,    // CountRemaining returns 1 (open task exists)
		HasRemainingResult: &falseVal, // HasRemaining returns false (all skipped)
		Total:              1,
		NextID:             "",
		NextTask:           "",
	}
	l, _ := newTestLoopForSelection(t, backend)

	_, action := l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration:  1,
		maxIterations: 100,
		wait:          true,
		completedIDs:  map[string]bool{},
	})

	if action != actionDone {
		t.Fatalf("expected actionDone when all tasks are skipped, got %v", action)
	}
	status, _ := l.state.Read("status")
	if status != "all_skipped" {
		t.Errorf("expected status all_skipped, got %q", status)
	}
}

// selectNextTask does NOT call SetResumeTaskID on subsequent iterations
// (runIteration > 0) to prevent back-to-back retries on the same task after
// a no-signal exit.
func TestSelectNextTask_NoResumeIDOnSubsequentIterations(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-abc", NextTask: "Fix login"}
	l, _ := newTestLoopForSelection(t, backend)
	l.state.Write("last_task_id", "ralph-abc")

	l.selectNextTask(context.Background(), selectNextTaskParams{
		runIteration: 1,
		completedIDs: map[string]bool{},
	})

	if backend.ResumeIDSet != "" {
		t.Errorf("expected no SetResumeTaskID call on iteration > 0, got %q", backend.ResumeIDSet)
	}
}
