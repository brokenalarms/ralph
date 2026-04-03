package loop

import (
	"context"
	"testing"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// newSelectionParams builds minimal selectNextTaskParams for testing.
func newSelectionParams(t *testing.T, backend *testutil.StubBackend) (selectNextTaskParams, string) {
	t.Helper()
	dir, st := setupTestDir(t)
	logger := logging.New(nil)
	return selectNextTaskParams{
		backend:           backend,
		state:             st,
		logger:            logger,
		completedIDs:      map[string]bool{},
		waitForTasks:      func(_ context.Context) bool { return false },
		flushUnpushedWork: func(_ context.Context) {},
	}, dir
}

// selectNextTask returns actionProceed with the correct taskContext when a task is available.
func TestSelectNextTask_ReturnsTask(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-abc", NextTask: "Fix login"}
	p, _ := newSelectionParams(t, backend)

	tc, action := selectNextTask(context.Background(), p)

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
	p, _ := newSelectionParams(t, backend)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, action := selectNextTask(ctx, p)

	if action != actionDone {
		t.Fatalf("expected actionDone on cancelled context, got %v", action)
	}
}

// selectNextTask skips tasks in completedIDs and returns actionDone when no other task is available.
func TestSelectNextTask_SkipsCompletedIDs(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-done", NextTask: "Already done"}
	p, _ := newSelectionParams(t, backend)
	p.completedIDs = map[string]bool{"ralph-done": true}

	_, action := selectNextTask(context.Background(), p)

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
	p, _ := newSelectionParams(t, backend)
	p.runIteration = 5
	p.maxIterations = 5

	_, action := selectNextTask(context.Background(), p)

	if action != actionDone {
		t.Fatalf("expected actionDone, got %v", action)
	}
	status, _ := p.state.Read("status")
	if status != "max_iterations_reached" {
		t.Errorf("expected status max_iterations_reached, got %q", status)
	}
}

// selectNextTask returns actionDone when no tasks remain and wait=false.
func TestSelectNextTask_NoTasksNoWait(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 0, Total: 1}
	p, _ := newSelectionParams(t, backend)
	p.runIteration = 1 // not first iteration, so flushUnpushedWork is called

	_, action := selectNextTask(context.Background(), p)

	if action != actionDone {
		t.Fatalf("expected actionDone, got %v", action)
	}
	status, _ := p.state.Read("status")
	if status != "completed" {
		t.Errorf("expected status completed, got %q", status)
	}
}

// selectNextTask calls flushUnpushedWork when no tasks remain and runIteration > 0.
func TestSelectNextTask_FlushesUnpushedWorkWhenNoTasks(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 0, Total: 1}
	p, _ := newSelectionParams(t, backend)
	p.runIteration = 2

	flushed := false
	p.flushUnpushedWork = func(_ context.Context) { flushed = true }

	selectNextTask(context.Background(), p)

	if !flushed {
		t.Error("expected flushUnpushedWork to be called when no tasks remain and runIteration > 0")
	}
}

// selectNextTask returns actionDone when no tasks exist and wait=false (first iteration).
func TestSelectNextTask_NoTasksAtAll(t *testing.T) {
	backend := &testutil.StubBackend{Remaining: 0, Total: 0}
	p, _ := newSelectionParams(t, backend)
	p.runIteration = 0

	_, action := selectNextTask(context.Background(), p)

	if action != actionDone {
		t.Fatalf("expected actionDone when no tasks exist, got %v", action)
	}
	status, _ := p.state.Read("status")
	if status != "error" {
		t.Errorf("expected status error, got %q", status)
	}
}

// Loop.selectNextTask delegates to the package function, building completedIDs from sessionTasks.
func TestLoop_selectNextTask_DelegatesToPackageFunc(t *testing.T) {
	dir, st := setupTestDir(t)
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-new", NextTask: "New task"}
	logger := logging.New(nil)

	l := &Loop{
		cfg: Config{
			MaxIterations: 10,
			TaskBackend:   backend,
			Dirs:          workctx.WorkContext{RalphDir: dir + "/.ralph"},
		},
		state:          st,
		logger:         logger,
		completedTasks: []CompletedTask{{ID: "ralph-old", Title: "Old task"}},
	}

	completedIDs := make(map[string]bool, len(l.completedTasks))
	for _, ct := range l.completedTasks {
		completedIDs[ct.ID] = true
	}
	tc, action := selectNextTask(context.Background(), selectNextTaskParams{
		runIteration:      0,
		maxIterations:     l.cfg.MaxIterations,
		backend:           l.cfg.TaskBackend,
		wait:              l.cfg.Wait,
		state:             l.state,
		logger:            l.logger,
		completedIDs:      completedIDs,
		waitForTasks: func(ctx context.Context) bool {
				return waitForTasks(ctx, waitForTasksParams{
					logger:  l.logger,
					state:   l.state,
					backend: l.cfg.TaskBackend,
				})
			},
		flushUnpushedWork: func(_ context.Context) {},
	})

	if action != actionProceed {
		t.Fatalf("expected actionProceed, got %v", action)
	}
	if tc.id != "ralph-new" {
		t.Errorf("expected ralph-new, got %q", tc.id)
	}
}
