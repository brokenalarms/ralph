package loop

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies that auto-merge fires once per task across a 3-task session:
// each iteration runs the agent, the ship pipeline returns Merged=true, and
// the loop advances to the next task. iterationCount == 3 is the observable
// that the whole pipeline completed once per task.
//
// Phase C migration notes:
//   - Dropped assertion on gm.MergeRetryCalls. The new stubRepo's Ship
//     returns cfg.Ship directly; it does not call MergeWithRetry internally.
//     MergeRetryCalls was counting an internal stub side-effect, not a
//     production invariant.
func TestLoop_AutoMergeFiresPerTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 1,
			Completed: 0,
			Total:     3,
			NextTask:  "task A",
			NextID:    "ralph-aaa",
		},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:         dir,
		WorkDir:            dir,
		Ship:               git.ShipResult{PRNumber: 99, Merged: true},
		MergeRetrySucceeds: true,
	})

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			gm.CommitAll("simulated agent commit")
			backend.Lock()
			defer backend.Unlock()
			backend.Completed = iterationCount
			switch iterationCount {
			case 1:
				backend.Remaining = 1
				backend.NextTask = "task B"
				backend.NextID = "ralph-bbb"
			case 2:
				backend.Remaining = 1
				backend.NextTask = "task C"
				backend.NextID = "ralph-ccc"
			default:
				backend.Remaining = 0
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

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 3 {
		t.Errorf("expected 3 iterations, got %d", iterationCount)
	}
}

// Verifies the full post-merge branch rename cycle: task A merges →
// between-task branch prep runs → next iteration renames to thematic branch
// for task B. Proves each successive task gets its own descriptively
// named branch via the observable GetWorktreeBranch() between iterations,
// rather than through internal call counts.
func TestLoop_PostMergeRenamesCycleFull(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0
	var branchDuringTaskA, branchDuringTaskB string

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 1,
			Completed: 0,
			Total:     2,
			NextTask:  "Fix tail leak",
			NextID:    "ralph-t1",
		},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:         dir,
		WorkDir:            dir,
		WorktreeBranch:     "main",
		Ship:               git.ShipResult{PRNumber: 99, Merged: true},
		MergeRetrySucceeds: true,
	})

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			gm.CommitAll("simulated agent commit")
			switch iterationCount {
			case 1:
				branchDuringTaskA = gm.GetWorktreeBranch()
				backend.Lock()
				backend.Completed = 1
				backend.Remaining = 1
				backend.NextTask = "Add retry logic"
				backend.NextID = "ralph-r2"
				backend.Unlock()
			case 2:
				branchDuringTaskB = gm.GetWorktreeBranch()
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

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Fatalf("expected 2 iterations, got %d", iterationCount)
	}

	if !strings.Contains(branchDuringTaskA, "ralph-t1-fix-tail-leak") {
		t.Errorf("task A branch should contain slug, got %q", branchDuringTaskA)
	}
	if !strings.Contains(branchDuringTaskB, "ralph-r2-add-retry-logic") {
		t.Errorf("task B branch should contain slug, got %q", branchDuringTaskB)
	}
	if branchDuringTaskA == branchDuringTaskB {
		t.Errorf("tasks should have different branches, both got %q", branchDuringTaskA)
	}
}
