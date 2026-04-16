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

