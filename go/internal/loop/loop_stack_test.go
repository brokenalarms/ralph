package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// When Phase 1 push succeeds but CreatePR fails, completeTask skips the bead
// with the pr_creation_failed prefix. The pushed branch must be recorded in
// state so that the next iteration's setStackHead can find it as a candidate.
func TestLoop_PrCreationFailed_RecordsPushedBranch(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	pushed := "ralph/ralph-orph-task-whose-pr-fails"
	fs := buildFinalizeSetup(t, dir, "ralph-orph", "Task whose PR fails to create", backend, shipResult{
		prNumber:     0,
		pushedBranch: pushed,
	})

	fs.loop.completeTask(context.Background(), fs.p)

	branches, err := fs.st.GetPushedBranches()
	if err != nil {
		t.Fatalf("GetPushedBranches: %v", err)
	}
	found := false
	for _, b := range branches {
		if b == pushed {
			found = true
		}
	}
	if !found {
		t.Errorf("pushed branch %q not recorded in state after pr_creation_failed skip; got %v", pushed, branches)
	}
}

// When a normal ship succeeds (push + PR created), the branch is also
// recorded so it participates in stack head detection for the next task.
func TestLoop_ShipSucceeded_RecordsPushedBranch(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	pushed := "ralph/ralph-abc-fix-login"
	fs := buildFinalizeSetup(t, dir, "ralph-abc", "Fix login", backend, shipResult{
		prNumber:     42,
		pushedBranch: pushed,
	})

	fs.loop.completeTask(context.Background(), fs.p)

	branches, err := fs.st.GetPushedBranches()
	if err != nil {
		t.Fatalf("GetPushedBranches: %v", err)
	}
	found := false
	for _, b := range branches {
		if b == pushed {
			found = true
		}
	}
	if !found {
		t.Errorf("pushed branch %q not recorded in state after successful ship; got %v", pushed, branches)
	}
}

// After run-start ClearCompletedTasks, completedBranches returns nil even when
// a stale CompletedTask entry from a prior run with a known branch exists in
// state. Without the clear, legacyCompletedBranches returns the stale branch
// and setStackHead logs 'No stacked parents — <stale-branch> has no open PR'.
func TestLoop_CompletedBranches_NilAfterRunStartClear(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	_ = st.AddCompletedTask("stale-ralph-xyz", true)

	st.ClearPushedBranches()
	_ = st.ClearCompletedTasks()

	backend := &testutil.MutableBackend{
		Metadata: map[string]map[string]string{
			"stale-ralph-xyz": {"branch": "ralph/stale-ralph-xyz-prior-run"},
		},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir},
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		TaskBackend: backend,
		Logger:      logging.New(nil),
	})

	branches := l.completedBranches()
	if len(branches) != 0 {
		t.Errorf("completedBranches() should return nil after ClearCompletedTasks, got %v", branches)
	}
}

// completedBranches reads from state.GetPushedBranches() when it is non-empty,
// returning branches in chronological order (oldest first). This is the primary
// source for stack head detection.
func TestLoop_CompletedBranches_UsesPushedBranchesFromState(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	_ = st.AddPushedBranch("ralph/task-a")
	_ = st.AddPushedBranch("ralph/task-b")
	_ = st.AddPushedBranch("ralph/task-c")

	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		TaskBackend: &testutil.StubBackend{},
		Logger:      logger,
	})

	got := l.completedBranches()

	want := []string{"ralph/task-a", "ralph/task-b", "ralph/task-c"}
	if len(got) != len(want) {
		t.Fatalf("completedBranches() returned %d entries, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("completedBranches()[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestLoop_LegacyFallback_ReturnsCurrentRunBranch proves AC3: the legacy
// completedBranches fallback still returns branches for tasks completed
// within a single run after the run-start ClearCompletedTasks. Clearing at
// run start does not break mid-run stack detection — AddCompletedTask
// repopulates CompletedTasks as tasks finish, and legacyCompletedBranches
// reads those entries correctly.
func TestLoop_LegacyFallback_ReturnsCurrentRunBranch(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	st.ClearPushedBranches()
	_ = st.ClearCompletedTasks()

	_ = st.AddCompletedTask("current-task-abc", true)

	backend := &testutil.MutableBackend{
		Metadata: map[string]map[string]string{
			"current-task-abc": {"branch": "ralph/current-task-abc-fix-thing"},
		},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir},
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		TaskBackend: backend,
		Logger:      logging.New(nil),
	})

	branches := l.completedBranches()
	found := false
	for _, b := range branches {
		if b == "ralph/current-task-abc-fix-thing" {
			found = true
		}
	}
	if !found {
		t.Errorf("legacy fallback should return current-run branch after clear+AddCompletedTask, got %v", branches)
	}
}
