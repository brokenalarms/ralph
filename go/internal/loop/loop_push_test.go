package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Push/ship-pipeline tests converted to TrackingBackend observations.
//
// Phase C deletions (from this file):
// Many tests in the original file asserted on stub call counts
// (gm.ShipCalls, gm.FlushUnpushedCalls, gm.MergeRetryCalls, gm.LastFlushAutoMerge)
// or used the forbidden ShipFunc callback pattern to simulate retry behavior.
// The new stubRepo exposes none of those observables. The retained tests
// below preserve the observable end-state (bead-close behavior via
// TrackingBackend.ClosedIDs) which is what production guarantees. Tests
// removed with rationale:
//   - TestLoop_FlushesUnpushedWorkBeforeExit: asserts Ship+Flush >= 2, pure
//     call count. The observable end-state (bead closed via the signal path)
//     is covered by TestLoop_PushAndCreatePROnSignal.
//   - TestLoop_FlushesUnpushedWorkBeforeWait: same as above, plus Wait mode
//     entry is covered by lifecycle tests.
//   - TestLoop_FlushSquashMergesBeforeExit / BeforeWait: assert
//     gm.LastFlushAutoMerge; with the new static stub there is no observable
//     for "the flush call carried autoMerge=true". Non-regression here is a
//     lifecycle-integration concern (Phase D).
//   - TestLoop_FlushSkipsMergeWhenAutoMergeDisabled / WhenAlreadyMerged /
//     WhenSignalNotDetected: all three assert on gm.FlushUnpushedCalls +
//     LastFlushAutoMerge + MergeRetryCalls, pure call counts. End-state
//     observable via backend close status is covered by the kept tests.
//   - TestLoop_ShipRetriesOnTransientGitHubError: uses ShipFunc (first call
//     returns err, second returns success) — the forbidden sequenced
//     callback pattern. Retry behavior requires real transient conditions
//     and belongs in Phase D integration.

// Verifies that signal detection on each task advances the loop (both tasks
// are closed), proving the ship pipeline fires per task.
func TestLoop_PushAndCreatePROnSignal(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Completed: 0,
				Total:     2,
				NextTask:  "task A",
				NextID:    "ralph-aaa",
			},
		},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		Ship:       git.ShipResult{PRNumber: 99},
	})

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			gm.CommitAll("simulated agent commit")
			if iterationCount == 1 {
				backend.Lock()
				backend.Completed = 1
				backend.Remaining = 1
				backend.NextTask = "task B"
				backend.NextID = "ralph-bbb"
				backend.Unlock()
			} else {
				backend.Lock()
				backend.Completed = 2
				backend.Remaining = 0
				backend.Unlock()
			}
		},
		result: claude.Result{SignalDetected: true},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     false,
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

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Errorf("expected 2 iterations, got %d", iterationCount)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 2 {
		t.Errorf("expected 2 beads closed (one per task via ship pipeline), got %v", backend.ClosedIDs)
	}
}

// Verifies that when Claude exits without signaling completion, no bead is
// closed — proving the ship pipeline is gated on signal.
func TestLoop_NoPushPRWithoutSignal(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "some task",
				NextID:    "ralph-xyz",
			},
		},
	}

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
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("bead must not be closed without signal, got %v", backend.ClosedIDs)
	}
}

// Verifies that signal detection leads to the task being closed through
// the ship pipeline. Observable end-state: TrackingBackend.ClosedIDs.
func TestLoop_PushCalledAfterSignal(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/test/01-task",
		Ship:           git.ShipResult{PRNumber: 42},
	})

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix the bug",
				NextID:    "ralph-fix1",
			},
		},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.GetWorkDir(),
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
		VerifyHook:   passingVerifyHook(),
	})

	l.runner = &stubRunner{
		onRun:  func() { gm.CommitAll("simulated commit") },
		result: claude.Result{SignalDetected: true, OnSignalUsed: true},
	}

	l.Run(context.Background())

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-fix1" {
		t.Errorf("expected ralph-fix1 to be closed after signal, got %v", backend.ClosedIDs)
	}
}
