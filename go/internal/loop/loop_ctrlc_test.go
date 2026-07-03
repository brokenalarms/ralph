package loop

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// After Ctrl+C (cancelled context), runAgent must return actionDone immediately
// without calling the runner — no "Agent model:" or test log lines should appear.
func TestRunAgent_CancelledContext_ReturnsImmediately(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	runnerCalled := false
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
		Git:          git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		TaskBackend:  &testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix something", NextID: "ralph-t1"},
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = &stubRunner{onRun: func() { runnerCalled = true }}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := l.runAgent(ctx, taskContext{id: "ralph-t1", title: "Fix something"}, 1)

	if result.action != actionDone {
		t.Errorf("expected actionDone, got %v", result.action)
	}
	if runnerCalled {
		t.Error("runner must not be called when context is cancelled")
	}
}

// Proves that when an agent iteration ends without emitting a completion signal
// (SignalDetected==false, action==actionProceed), the bead's claim is released
// via setPhaseInterrupted: status returns to open and phase is set to interrupted,
// so the bead is never left stranded in_progress with unpushed commits.
func TestLoop_NoSignal_ReleasesClaimViaSetPhaseInterrupted(t *testing.T) {
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
				NextTask:     "task with no signal",
				NextID:       "ralph-nosg1",
				BackendLabel: "beads",
			},
		},
	}

	// Agent runs but emits no completion signal (clean end_turn with no signal file).
	runner := &stubRunner{
		result: claude.Result{SignalDetected: false},
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

	var phaseValues []string
	for _, call := range backend.stateCalls {
		if call.id == "ralph-nosg1" && call.dimension == "phase" {
			phaseValues = append(phaseValues, call.value)
		}
	}
	if len(phaseValues) == 0 {
		t.Fatal("expected phase SetState calls for ralph-nosg1, got none")
	}
	if phaseValues[0] != "implementing" {
		t.Errorf("first phase SetState = %q, want implementing (ClaimTask must set phase=implementing)", phaseValues[0])
	}

	// The no-signal fall-through must release the claim via setPhaseInterrupted.
	last := phaseValues[len(phaseValues)-1]
	if last != "interrupted" {
		t.Errorf("last phase SetState = %q, want interrupted after no-signal fall-through; all phase values: %v", last, phaseValues)
	}

	// ReopenTask must be called so status reverts to open (claim released).
	var reopened bool
	for _, id := range backend.reopenCalls {
		if id == "ralph-nosg1" {
			reopened = true
			break
		}
	}
	if !reopened {
		t.Errorf("expected ReopenTask(ralph-nosg1) to release the claim on no-signal fall-through; reopen calls: %v", backend.reopenCalls)
	}
}

// After Ctrl+C, runPreIterationTests must return "" immediately without
// emitting "Running test suite" or compile-check log lines.
func TestRunPreIterationTests_CancelledContext_SkipsTests(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)
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
		Git:          git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		TaskBackend:  &testutil.StubBackend{Remaining: 0},
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := l.runPreIterationTests(ctx)

	if result != "" {
		t.Errorf("expected empty string when cancelled, got %q", result)
	}
	if logBuf.Len() > 0 {
		t.Errorf("expected no log output when cancelled, got: %s", logBuf.String())
	}
}
