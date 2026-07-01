package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// clearTaskTestLoop builds a Loop with a real state store for testing
// ClearCurrentTask call sites. The caller fills in gCfg to control git
// stub behavior; ProjectDir/WorkDir default to the temp dir if empty.
func clearTaskTestLoop(t *testing.T, backend tasks.Backend, gCfg git.StubRepoConfig) (*Loop, *state.Store) {
	t.Helper()
	dir, st := setupTestDir(t)
	if gCfg.ProjectDir == "" {
		gCfg.ProjectDir = dir
	}
	if gCfg.WorkDir == "" {
		gCfg.WorkDir = dir
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   filepath.Join(dir, ".ralph"),
		},
		MaxIterations: 10,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          git.NewStub(gCfg),
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	return l, st
}

// assertCurrentTaskCleared fails if current_task_id is non-empty in the store.
func assertCurrentTaskCleared(t *testing.T, st *state.Store) {
	t.Helper()
	id, _ := st.Read("current_task_id")
	if id != "" {
		t.Errorf("expected current_task_id cleared, got %q", id)
	}
}

// TestCloseResumedTask_AlreadyMerged_ClearsCurrentTask: the AlreadyMerged path
// in closeResumedTask calls ClearCurrentTask before delegating to CloseTask.
func TestCloseResumedTask_AlreadyMerged_ClearsCurrentTask(t *testing.T) {
	backend := &testutil.StubBackend{}
	l, st := clearTaskTestLoop(t, backend, git.StubRepoConfig{})
	st.BeginIteration("ralph-xyz", "Fix auth", 0)

	l.closeResumedTask(context.Background(), "ralph-xyz", "Fix auth",
		git.ResumeTaskResult{AlreadyMerged: true})

	assertCurrentTaskCleared(t, st)
}

// TestCloseResumedTask_SuccessfulClose_ClearsCurrentTask: when CloseTask
// succeeds in closeResumedTask (non-AlreadyMerged path), ClearCurrentTask
// is called in the else branch.
func TestCloseResumedTask_SuccessfulClose_ClearsCurrentTask(t *testing.T) {
	backend := &testutil.StubBackend{}
	l, st := clearTaskTestLoop(t, backend, git.StubRepoConfig{})
	st.BeginIteration("ralph-xyz", "Fix auth", 0)

	l.closeResumedTask(context.Background(), "ralph-xyz", "Fix auth",
		git.ResumeTaskResult{Merged: true})

	assertCurrentTaskCleared(t, st)
}

// TestCompleteTask_PRStateMerged_ClearsCurrentTask: when completeTask detects
// a previously-merged PR during the no-new-commits path, ClearCurrentTask is
// called before returning signalSkipped.
func TestCompleteTask_PRStateMerged_ClearsCurrentTask(t *testing.T) {
	backend := &testutil.MutableBackend{
		ExternalRefs: map[string]string{
			"ralph-xyz": "https://github.com/owner/repo/pull/42",
		},
	}
	gCfg := git.StubRepoConfig{
		HeadRev:   "abc123",
		RemoteURL: "https://github.com/owner/repo.git",
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs:       []git.StubPR{{Number: 42, State: git.PRStateMerged}},
		},
	}
	l, st := clearTaskTestLoop(t, backend, gCfg)
	st.BeginIteration("ralph-xyz", "Fix auth", 0)

	// headBefore == HeadRev → no-commits path; OnSignalUsed skips verification.
	l.completeTask(context.Background(), completeTaskParams{
		taskID:     "ralph-xyz",
		nextTask:   "Fix auth",
		headBefore: "abc123",
		result:     claude.Result{OnSignalUsed: true},
	})

	assertCurrentTaskCleared(t, st)
}

// TestCompleteTask_NoCommitsNoExistingPR_ClearsCurrentTask: when completeTask
// closes a task directly (no new commits, no existing PR), ClearCurrentTask
// is called after the successful CloseTask.
func TestCompleteTask_NoCommitsNoExistingPR_ClearsCurrentTask(t *testing.T) {
	backend := &testutil.StubBackend{}
	gCfg := git.StubRepoConfig{HeadRev: "abc123"}
	l, st := clearTaskTestLoop(t, backend, gCfg)
	st.BeginIteration("ralph-xyz", "Fix auth", 0)

	// headBefore == HeadRev and no existing PR → direct close path.
	l.completeTask(context.Background(), completeTaskParams{
		taskID:     "ralph-xyz",
		nextTask:   "Fix auth",
		headBefore: "abc123",
		result:     claude.Result{OnSignalUsed: true},
	})

	assertCurrentTaskCleared(t, st)
}

// TestCompleteTask_CIInfraFailure_ClearsCurrentTask: when CI infrastructure
// fails (zero job steps) and completeTask closes the bead, ClearCurrentTask
// is called after the successful close.
func TestCompleteTask_CIInfraFailure_ClearsCurrentTask(t *testing.T) {
	backend := &testutil.StubBackend{}
	gCfg := git.StubRepoConfig{
		Ship: git.ShipResult{
			PRNumber:              42,
			CIFailure:             true,
			InfrastructureFailure: true,
			CIFailureDetail:       &git.CIFailureError{PRNumber: 42},
		},
	}
	l, st := clearTaskTestLoop(t, backend, gCfg)
	l.cfg.AutoMerge = true
	st.BeginIteration("ralph-xyz", "Fix auth", 0)

	// headBefore="" skips the no-commits check; AutoMerge drives Ship to return
	// the infrastructure-failure result, triggering the CI infra close path.
	l.completeTask(context.Background(), completeTaskParams{
		taskID:   "ralph-xyz",
		nextTask: "Fix auth",
		result:   claude.Result{OnSignalUsed: true},
	})

	assertCurrentTaskCleared(t, st)
}

// TestCompleteTask_MainClose_ClearsCurrentTask: when completeTask merges via
// AutoMerge and closes the bead on the main close path, ClearCurrentTask is
// called after the successful CloseTask.
func TestCompleteTask_MainClose_ClearsCurrentTask(t *testing.T) {
	backend := &testutil.StubBackend{}
	gCfg := git.StubRepoConfig{
		Ship: git.ShipResult{PRNumber: 42, Merged: true},
	}
	l, st := clearTaskTestLoop(t, backend, gCfg)
	l.cfg.AutoMerge = true
	st.BeginIteration("ralph-xyz", "Fix auth", 0)

	// headBefore="" skips the no-commits check; AutoMerge drives Ship to merge.
	l.completeTask(context.Background(), completeTaskParams{
		taskID:   "ralph-xyz",
		nextTask: "Fix auth",
		result:   claude.Result{OnSignalUsed: true},
	})

	assertCurrentTaskCleared(t, st)
}

// TestSkipTask_ClearsCurrentTask: skipTask calls ClearCurrentTask so the next
// iteration doesn't attempt to resume the skipped bead.
func TestSkipTask_ClearsCurrentTask(t *testing.T) {
	backend := &testutil.StubBackend{}
	l, st := clearTaskTestLoop(t, backend, git.StubRepoConfig{})
	st.BeginIteration("ralph-xyz", "Fix auth", 0)

	l.skipTask("ralph-xyz", "test_reason", "")

	assertCurrentTaskCleared(t, st)
}
