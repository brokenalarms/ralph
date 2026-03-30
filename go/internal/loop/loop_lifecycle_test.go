package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies the loop exits with "completed" when the task backend reports
// no remaining tasks, proving the "all tasks done" exit path works.
func TestLoop_AllTasksComplete(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &stubBackend{
		remaining: 0,
		completed: 3,
		total:     3,
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

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

	backend := &stubBackend{
		remaining: 0,
		completed: 0,
		total:     0,
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

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

	backend := &stubBackend{
		remaining: 5,
		total:     5,
		nextTask:  "Some task",
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

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

	backend := &stubBackend{
		remaining: 5,
		total:     5,
		nextTask:  "Some task",
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

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

	backend := &stubBackend{
		remaining: 0,
		completed: 1,
		total:     1,
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

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

// Verifies the stream task file is written with task ID and description,
// proving the tmux pane title integration works correctly.
// updateStreamTask is a standalone function — no Loop required.
func TestLoop_UpdateStreamTask(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	updateStreamTask(ralphDir, "ralph-abc", "Add feature X", nil)

	data, err := os.ReadFile(filepath.Join(ralphDir, ".stream-task"))
	if err != nil {
		t.Fatalf("expected stream task file, got error: %v", err)
	}
	if string(data) != "ralph-abc: Add feature X" {
		t.Errorf("expected 'ralph-abc: Add feature X', got %q", string(data))
	}

	updateStreamTask(ralphDir, "", "Add feature Y", nil)
	data, _ = os.ReadFile(filepath.Join(ralphDir, ".stream-task"))
	if string(data) != "Add feature Y" {
		t.Errorf("expected 'Add feature Y', got %q", string(data))
	}

	p := 3
	updateStreamTask(ralphDir, "ralph-xyz", "Some task", &p)
	data, _ = os.ReadFile(filepath.Join(ralphDir, ".stream-task"))
	got := string(data)
	if !strings.Contains(got, "[P3]") {
		t.Errorf("stream task with priority should include [P3], got %q", got)
	}
	if !strings.Contains(got, "ralph-xyz") {
		t.Errorf("stream task should include task ID, got %q", got)
	}
}

// Verifies writeRunBranch persists the current branch name to .run-branch
// so the shell pane-title updater displays the correct branch.
// writeRunBranch is a standalone function — no Loop required.
func TestLoop_WriteRunBranch(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	writeRunBranch(ralphDir, "ralph/project/01-fix-bug")

	data, err := os.ReadFile(filepath.Join(ralphDir, ".run-branch"))
	if err != nil {
		t.Fatalf("expected .run-branch file, got error: %v", err)
	}
	if string(data) != "ralph/project/01-fix-bug" {
		t.Errorf("expected 'ralph/project/01-fix-bug', got %q", string(data))
	}
}

// writeRunBranch defaults to "ralph" when branch is empty.
func TestLoop_WriteRunBranch_Default(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	writeRunBranch(ralphDir, "")

	data, err := os.ReadFile(filepath.Join(ralphDir, ".run-branch"))
	if err != nil {
		t.Fatalf("expected .run-branch file, got error: %v", err)
	}
	if string(data) != "ralph" {
		t.Errorf("expected 'ralph', got %q", string(data))
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

	backend := &mutableBackend{
		remaining: 0,
		completed: 1,
		total:     1,
		nextTask:  "first task",
		nextID:    "t-1",
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

	var (
		callsMu sync.Mutex
		calls   int
	)
	runner := &stubRunner{
		onRun: func() {
			callsMu.Lock()
			calls++
			callsMu.Unlock()
			backend.mu.Lock()
			backend.remaining = 0
			backend.completed++
			backend.mu.Unlock()
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

	// After the loop enters wait mode, inject a new task. After the Claude
	// call completes, the loop will re-enter wait mode; cancel the context
	// so the test doesn't hang.
	go func() {
		time.Sleep(200 * time.Millisecond)
		backend.mu.Lock()
		backend.remaining = 1
		backend.total++
		backend.nextTask = "second task"
		backend.nextID = "t-2"
		backend.mu.Unlock()

		for {
			time.Sleep(50 * time.Millisecond)
			callsMu.Lock()
			c := calls
			callsMu.Unlock()
			if c >= 1 {
				time.Sleep(100 * time.Millisecond)
				cancel()
				return
			}
		}
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

	backend := &stubBackend{
		remaining: 0,
		completed: 1,
		total:     1,
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

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

	backend := &stubBackend{
		remaining: 0,
		completed: 1,
		total:     1,
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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

	go func() {
		time.Sleep(150 * time.Millisecond)
		os.WriteFile(filepath.Join(ralphDir, "stop"), nil, 0o644)
	}()

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

	backend := &stubBackend{
		remaining: 0,
		completed: 2,
		total:     2,
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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
	mutableBackend
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
		mutableBackend: mutableBackend{
			remaining: 1,
			completed: 0,
			total:     1,
			nextTask:  "add lifecycle tracking",
			nextID:    "ralph-lc1",
			label:     "beads",
		},
	}

	runner := &stubRunner{
		onRun: func() {
			backend.mu.Lock()
			backend.completed = 1
			backend.remaining = 0
			backend.mu.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

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
		mutableBackend: mutableBackend{
			remaining: 1,
			completed: 0,
			total:     1,
			nextTask:  "broken task",
			nextID:    "ralph-brk",
			label:     "beads",
		},
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

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
	backend := &mutableBackend{
		remaining: 2,
		completed: 0,
		total:     2,
		nextTask:  "task A",
		nextID:    "ralph-aaa",
		label:     "beads",
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			if iterationCount >= 2 {
				backend.mu.Lock()
				backend.remaining = 0
				backend.completed = 2
				backend.mu.Unlock()
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

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
