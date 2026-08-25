package loop

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})

	logger := logging.New(nil)
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})

	logger := logging.New(nil)
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})

	logger := logging.New(nil)
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})

	logger := logging.New(nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("expected no error on context cancel, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "stopped" {
		t.Errorf("expected status 'stopped', got %q", finalState.Status)
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
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})

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
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		Wait:          true,
	}
	waitCount := 0
	waitEntered := make(chan struct{}, 2)
	waitHook := &stubWaitHook{fn: func() {
		waitCount++
		waitEntered <- struct{}{}
	}}
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		Runner:       runner,
		WaitHook:     waitHook,
	})

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
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		Wait:          true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		WaitHook:     &stubWaitHook{fn: func() { cancel() }},
	})

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
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		Wait:          true,
	}
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		WaitHook: &stubWaitHook{fn: func() {
			os.WriteFile(filepath.Join(ralphDir, "stop"), nil, 0o644)
		}},
	})

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
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		Wait:          false,
	}
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

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
	stateCalls  []stateCall
	reopenCalls []string
}

type stateCall struct {
	id, dimension, value string
}

func (s *stateTrackingBackend) SetState(id, dimension, value, reason string) error {
	s.stateCalls = append(s.stateCalls, stateCall{id, dimension, value})
	return nil
}

func (s *stateTrackingBackend) ReopenTask(id string) error {
	s.reopenCalls = append(s.reopenCalls, id)
	return nil
}

// Verifies that the loop sets phase=implementing when starting a task, and that
// phase=verified is never set (the close gate it gated was removed — close is
// now driven purely by control flow + canonical status).
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	if len(backend.stateCalls) < 1 {
		t.Fatalf("expected at least 1 SetState call (phase=implementing), got %d", len(backend.stateCalls))
	}

	first := backend.stateCalls[0]
	if first.id != "ralph-lc1" || first.dimension != "phase" || first.value != "implementing" {
		t.Errorf("first SetState = %+v, want phase=implementing for ralph-lc1", first)
	}

	// phase=verified is no longer set — the close gate that consumed it is gone.
	for _, c := range backend.stateCalls {
		if c.dimension == "phase" && c.value == "verified" {
			t.Errorf("phase=verified must not be set (close gate removed), got: %+v", c)
		}
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   &stubVerifyHook{passed: false, reason: "tests failed"},
	})
	l.runner = runner

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

// Verifies that pressing Ctrl-C (context cancellation) during a task sets
// phase=interrupted on the bead, so the task manager treats it as safe to
// update without user confirmation.
func TestLoop_CtrlC_SetsPhaseInterrupted(t *testing.T) {
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
				NextTask:     "task being interrupted",
				NextID:       "ralph-ctrlc",
				BackendLabel: "beads",
			},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	runner := &stubRunner{
		onRun: func() {
			cancel()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})
	l.runner = runner

	_ = l.Run(ctx)

	var phaseValues []string
	for _, call := range backend.stateCalls {
		if call.id == "ralph-ctrlc" && call.dimension == "phase" {
			phaseValues = append(phaseValues, call.value)
		}
	}

	if len(phaseValues) == 0 {
		t.Fatal("expected at least one phase SetState call for ralph-ctrlc")
	}
	last := phaseValues[len(phaseValues)-1]
	if last != "interrupted" {
		t.Errorf("last phase SetState = %q, want %q; all phase values: %v", last, "interrupted", phaseValues)
	}

	// The interrupt path must also release the claim (status back to open) so
	// the orphaned in_progress task returns to bd ready and the loop cannot
	// deadlock waiting on a task it claimed but never finished.
	var reopened bool
	for _, id := range backend.reopenCalls {
		if id == "ralph-ctrlc" {
			reopened = true
			break
		}
	}
	if !reopened {
		t.Errorf("expected ReopenTask(ralph-ctrlc) to release the claim on interrupt; reopen calls: %v", backend.reopenCalls)
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:         st,
		Git:           gm,
		TaskBackend:   backend,
		Logger:        logger,
		Verifier:      newTestVerifier(t, cfg, logger),
		Connectivity:  onlineStubConnectivity(),
		Runner:        runner,
		IterationHook: &stubIterationHook{fn: func() { callCount++ }},
	})

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if callCount != 2 {
		t.Errorf("OnIterationStart called %d times, want 2", callCount)
	}
}

// Proves that when the agent is killed by user feedback (FeedbackKill), the
// iteration releases its claim on the task via ReopenTask before returning to
// task selection, so the task is never stranded in_progress between iterations.
func TestLoop_ActionRetry_ReleasesClaimBeforeNextSelection(t *testing.T) {
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
				NextTask:     "task to retry",
				NextID:       "ralph-retry1",
				BackendLabel: "beads",
			},
		},
	}

	runner := &stubRunner{
		result: claude.Result{FeedbackKill: true},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		Runner:       runner,
	})

	_ = l.Run(context.Background())

	var reopened bool
	for _, id := range backend.reopenCalls {
		if id == "ralph-retry1" {
			reopened = true
			break
		}
	}
	if !reopened {
		t.Errorf("expected ReopenTask(ralph-retry1) to release the claim on actionRetry; reopen calls: %v", backend.reopenCalls)
	}
}

// Proves that when an agent signals completion but verification fails (signalRetry),
// the iteration releases its claim on the task via ReopenTask before returning to
// task selection, so the task is never stranded in_progress between iterations.
func TestLoop_SignalRetry_ReleasesClaimBeforeNextSelection(t *testing.T) {
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
				NextTask:     "task that fails verify",
				NextID:       "ralph-retry2",
				BackendLabel: "beads",
			},
		},
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		Runner:       runner,
		VerifyHook:   &stubVerifyHook{passed: false, reason: "tests failed"},
	})

	_ = l.Run(context.Background())

	var reopened bool
	for _, id := range backend.reopenCalls {
		if id == "ralph-retry2" {
			reopened = true
			break
		}
	}
	if !reopened {
		t.Errorf("expected ReopenTask(ralph-retry2) to release the claim on signalRetry; reopen calls: %v", backend.reopenCalls)
	}
}

// Proves the 8x70 evolve-restart resume path: a task claimed mid-implementation
// has its claim released at the iteration boundary (releaseClaimForRetry), so
// when a new loop session starts after an evolve-restart, the task is status=open
// and re-selected normally — never triggering halted_blocked_by_in_progress.
func TestLoop_EvolveRestart_ReleasedClaimResumesWithoutHalt(t *testing.T) {
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
				NextTask:     "task interrupted by evolve",
				NextID:       "ralph-evo1",
				BackendLabel: "beads",
			},
		},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	logger := logging.New(nil)
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}

	// Session 1: agent gets FeedbackKill (simulates mid-implementation interrupt).
	// The iteration boundary must call releaseClaimForRetry so the task returns to
	// status=open before the session exits.
	l1 := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		Runner:       &stubRunner{result: claude.Result{FeedbackKill: true}},
	})
	_ = l1.Run(context.Background())

	// The claim must have been released at the iteration boundary so the task is
	// status=open (not in_progress) when the evolve-restart spawns a new session.
	var released bool
	for _, id := range backend.reopenCalls {
		if id == "ralph-evo1" {
			released = true
			break
		}
	}
	if !released {
		t.Fatalf("expected ReopenTask(ralph-evo1) to release the claim at the iteration boundary; reopen calls: %v", backend.reopenCalls)
	}

	// current_task_id must persist in state — the new session reads this to
	// resume the same task after an evolve-restart.
	resumeID, _ := st.Read("current_task_id")
	if resumeID != "ralph-evo1" {
		t.Fatalf("expected current_task_id=ralph-evo1 in state for evolve-restart resume; got %q", resumeID)
	}

	// Session 2: simulates the new loop binary started by the orchestrator after
	// evolve-restart. Task is open (claim released); the loop must re-select it
	// and run the agent without entering halted_blocked_by_in_progress.
	run2Called := false
	l2 := New(cfg, Modules{
		State:        st, // same state — current_task_id is set
		Git:          gm,
		TaskBackend:  backend, // same backend — Remaining=1 (task is open)
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		Runner: &stubRunner{
			onRun: func() {
				run2Called = true
				backend.Lock()
				backend.Remaining = 0
				backend.Completed = 1
				backend.Unlock()
			},
			result: claude.Result{SignalDetected: true, Summary: "done"},
		},
		VerifyHook: passingVerifyHook(),
	})
	_ = l2.Run(context.Background())

	if !run2Called {
		t.Error("expected agent to run in the evolve-restart resume session — task must be re-selected as open")
	}
	finalStatus, _ := st.Read("status")
	if finalStatus == "halted_blocked_by_in_progress" {
		t.Errorf("evolve-restart resume must not enter halted_blocked_by_in_progress (claim was released at iteration boundary)")
	}
}

// waitForTasks returns true when a skipped bead is reassigned back to
// ralph-loop (making HasRemaining true), proving that Poll picks it up
// with no separate skip filter needed.
func TestLoop_WaitMode_PicksUpReassignedBead(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &testutil.MutableBackend{}
	backend.Remaining = 0
	backend.Completed = 1
	backend.Total = 1
	backend.BackendLabel = "beads"

	logger := logging.New(nil)
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		Wait:          true,
	}
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		WaitHook: &stubWaitHook{fn: func() {
			// Simulate bead being reassigned back to ralph-loop:
			// HasRemaining now returns true.
			backend.Lock()
			backend.Remaining = 1
			backend.Unlock()
		}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	got := l.waitForTasks(ctx)
	if !got {
		t.Fatal("waitForTasks should return true after bead reassigned back to ralph-loop")
	}
}
