package loop

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies the loop exits with "completed" when the task backend reports
// no remaining tasks, proving the "all tasks done" exit path works.
func TestLoop_AllTasksComplete(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &testutil.StubBackend{
		Remaining: 0,
		Completed: 3,
		Total:     3,
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", finalState.Status)
	}
}

// Verifies the loop exits with "error" when there are zero tasks and it's
// the first iteration, proving the "no tasks found" guard works.
func TestLoop_NoTasksError(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &testutil.StubBackend{
		Remaining: 0,
		Completed: 0,
		Total:     0,
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "error" {
		t.Errorf("expected status 'error', got %q", finalState.Status)
	}
}

// Verifies the loop exits with "stopped" when the stop file is present,
// proving the graceful shutdown mechanism works.
func TestLoop_StopFileDetection(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	// Create a stop file before the loop starts.
	os.WriteFile(filepath.Join(ralphDir, "stop"), []byte(""), 0o644)

	backend := &testutil.StubBackend{
		Remaining: 5,
		Total:     5,
		NextTask:  "Some task",
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "stopped" {
		t.Errorf("expected status 'stopped', got %q", finalState.Status)
	}

	// Stop file should be cleaned up.
	if _, err := os.Stat(filepath.Join(ralphDir, "stop")); err == nil {
		t.Error("stop file should have been removed")
	}
}

// Verifies the loop exits with "stopped" when the context is cancelled,
// proving the context-based cancellation path works.
func TestLoop_ContextCancellation(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &testutil.StubBackend{
		Remaining: 5,
		Total:     5,
		NextTask:  "Some task",
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	logger := logging.New(nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("expected no error on context cancel, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "stopped" {
		t.Errorf("expected status 'stopped', got %q", finalState.Status)
	}
}

// Verifies the loop writes max_iterations to state so users can edit it
// mid-run and the loop picks up the changed value each iteration.
func TestLoop_MaxIterationsFromState(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &testutil.StubBackend{
		Remaining: 0,
		Completed: 1,
		Total:     1,
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)

	_ = l.Run(context.Background())

	maxIter := st.ReadMaxIterations(0)
	if maxIter != 10 {
		t.Errorf("expected max_iterations=10 in state, got %d", maxIter)
	}
}

// Verifies that --wait keeps the loop alive when tasks complete, then resumes
// when new tasks appear. Without --wait, the loop would exit immediately.
func TestLoop_WaitResumeOnNewTasks(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    0,
			Completed:    1,
			Total:        1,
			NextTask:     "first task",
			NextID:       "t-1",
			BackendLabel: "beads",
		},
	}

	logger := logging.New(nil)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	var (
		callsMu sync.Mutex
		calls   int
	)
	runner := &stubRunner{
		onRun: func() {
			callsMu.Lock()
			calls++
			callsMu.Unlock()
			backend.Lock()
			backend.Remaining = 0
			backend.Completed++
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	ctx, cancel := context.WithCancel(context.Background())

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Wait:          true,
	}, st, gm, logger)
	l.runner = runner
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

	waitCount := 0
	waitEntered := make(chan struct{}, 2)
	l.onWaitFunc = func() {
		waitCount++
		waitEntered <- struct{}{}
	}

	go func() {
		<-waitEntered
		backend.Lock()
		backend.Remaining = 1
		backend.Total++
		backend.NextTask = "second task"
		backend.NextID = "t-2"
		backend.Unlock()

		<-waitEntered
		cancel()
	}()

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	callsMu.Lock()
	finalCalls := calls
	callsMu.Unlock()
	if finalCalls != 1 {
		t.Errorf("expected 1 Claude call (for second task), got %d", finalCalls)
	}
}

// Verifies that --wait exits cleanly when cancelled via context (Ctrl-C).
func TestLoop_WaitExitOnCancel(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &testutil.StubBackend{
		Remaining:    0,
		Completed:    1,
		Total:        1,
		BackendLabel: "beads",
	}

	logger := logging.New(nil)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Wait:          true,
	}, st, gm, logger)

	ctx, cancel := context.WithCancel(context.Background())
	l.onWaitFunc = func() { cancel() }

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "stopped" {
		t.Errorf("expected status 'stopped' after cancel, got %q", finalState.Status)
	}
}

// Verifies that --wait exits cleanly when stop file is detected during polling.
func TestLoop_WaitExitOnStopFile(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &testutil.StubBackend{
		Remaining:    0,
		Completed:    1,
		Total:        1,
		BackendLabel: "beads",
	}

	logger := logging.New(nil)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Wait:          true,
	}, st, gm, logger)

	l.onWaitFunc = func() {
		os.WriteFile(filepath.Join(ralphDir, "stop"), nil, 0o644)
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "stopped" {
		t.Errorf("expected status 'stopped' after stop file, got %q", finalState.Status)
	}
}

// Verifies that without --wait, the loop exits immediately when no tasks remain,
// confirming the default behavior is unchanged.
func TestLoop_NoWaitExitsImmediately(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &testutil.StubBackend{
		Remaining:    0,
		Completed:    2,
		Total:        2,
		BackendLabel: "beads",
	}

	logger := logging.New(nil)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Wait:          false,
	}, st, gm, logger)

	start := time.Now()
	err := l.Run(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if elapsed > 1*time.Second {
		t.Errorf("loop took %s without --wait, expected immediate exit", elapsed)
	}

	finalState, _ := st.Load()
	if finalState.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", finalState.Status)
	}
}

type stateTrackingBackend struct {
	testutil.MutableBackend
	stateCalls []stateCall
}

type stateCall struct {
	id, dimension, value string
}

func (s *stateTrackingBackend) SetState(id, dimension, value, reason string) error {
	s.stateCalls = append(s.stateCalls, stateCall{id, dimension, value})
	return nil
}

// Verifies that the loop sets phase=implementing when starting a task and
// phase=verified after verification passes, ensuring the bd close guard
// will allow the task to be closed.

// Verifies that the loop sets phase=implementing when starting a task and
// phase=verified after verification passes, ensuring the bd close guard
// will allow the task to be closed.
func TestLoop_LifecycleStates(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stateTrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "add lifecycle tracking",
				NextID:       "ralph-lc1",
				BackendLabel: "beads",
			},
		},
	}

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.verifyFunc = func(context.Context, string, string) (bool, string) {
		return true, ""
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

	_ = l.Run(context.Background())

	if len(backend.stateCalls) < 2 {
		t.Fatalf("expected at least 2 SetState calls, got %d", len(backend.stateCalls))
	}

	first := backend.stateCalls[0]
	if first.id != "ralph-lc1" || first.dimension != "phase" || first.value != "implementing" {
		t.Errorf("first SetState = %+v, want phase=implementing for ralph-lc1", first)
	}

	last := backend.stateCalls[len(backend.stateCalls)-1]
	if last.id != "ralph-lc1" || last.dimension != "phase" || last.value != "verified" {
		t.Errorf("last SetState = %+v, want phase=verified for ralph-lc1", last)
	}
}

// Verifies that phase=verified is NOT set when verification fails,
// ensuring the close guard will reject a premature close.
func TestLoop_LifecycleStates_NoVerifiedOnFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stateTrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "broken task",
				NextID:       "ralph-brk",
				BackendLabel: "beads",
			},
		},
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.verifyFunc = func(context.Context, string, string) (bool, string) {
		return false, "tests failed"
	}

	_ = l.Run(context.Background())

	for _, call := range backend.stateCalls {
		if call.dimension == "phase" && call.value == "verified" {
			t.Error("phase=verified should not be set when verification fails")
		}
	}

	hasImplementing := false
	for _, call := range backend.stateCalls {
		if call.dimension == "phase" && call.value == "implementing" {
			hasImplementing = true
		}
	}
	if !hasImplementing {
		t.Error("phase=implementing should still be set at task start")
	}
}

// Verifies OnIterationStart is called once per iteration, so the resume
// script is regenerated each time (not only on exit).
func TestLoop_OnIterationStartCalledEachIteration(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	callCount := 0
	iterationCount := 0
	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    2,
			Completed:    0,
			Total:        2,
			NextTask:     "task A",
			NextID:       "ralph-aaa",
			BackendLabel: "beads",
		},
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			backend.Lock()
			if iterationCount == 1 {
				// Switch to a different task for the second iteration
				backend.NextID = "ralph-bbb"
				backend.NextTask = "task B"
			}
			if iterationCount >= 2 {
				backend.Remaining = 0
				backend.Completed = 2
			}
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		TaskBackend:   backend,
		OnIterationStart: func() {
			callCount++
		},
	}, st, gm, logging.New(nil))
	l.runner = runner

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if callCount != 2 {
		t.Errorf("OnIterationStart called %d times, want 2", callCount)
	}
}

// Clearing skipped_tasks from state.json during wait-mode polling causes the
// loop to refresh the backend's skip set, making previously-skipped tasks
// eligible for selection without restarting the loop.
func TestLoop_WaitMode_ReReadsSkippedTasksOnTick(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	// Seed state.json with a skipped task.
	st.AddSkippedTask("ralph-xyz")

	backend := &testutil.MutableBackend{}
	backend.Remaining = 0
	backend.Completed = 1
	backend.Total = 1
	backend.BackendLabel = "beads"

	logger := logging.New(nil)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Wait:          true,
	}, st, gm, logger)

	l.onWaitFunc = func() {
		s, _ := st.Load()
		s.SkippedTasks = nil
		st.Save(s)
		backend.Lock()
		backend.Remaining = 1
		backend.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	got := l.waitForTasks(ctx)
	if !got {
		t.Fatal("waitForTasks should return true after skipped tasks cleared")
	}

	backend.Lock()
	calls := backend.LastSkippedIDs
	backend.Unlock()

	if len(calls) == 0 {
		t.Fatal("expected SetSkippedIDs to be called during wait polling")
	}
	// The last call should be an empty slice (skipped_tasks cleared).
	last := calls[len(calls)-1]
	if len(last) != 0 {
		t.Errorf("expected empty skip list after clearing, got %v", last)
	}
}
