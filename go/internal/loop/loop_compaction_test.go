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

// Proves: when the agent runner returns Result{Compacted: true}, the loop
// skips the current task with reason 'compaction_detected' and continues to
// the next iteration rather than retrying or crashing.
func TestLoop_CompactingEventSkipsTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{}
	backend.Remaining = 1
	backend.Total = 1
	backend.NextID = "task-compact"
	backend.NextTask = "Task that triggers compaction"

	runnerCallCount := 0
	runner := &stubRunner{
		onRun: func() {
			runnerCallCount++
			backend.Lock()
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{Compacted: true},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		RemoteURL:  "https://github.com/owner/repo.git",
	})

	logger := logging.New(nil)
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
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Runner:       runner,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if runnerCallCount != 1 {
		t.Errorf("expected agent runner to be called exactly once, got %d", runnerCallCount)
	}

	backend.SkipMu.Lock()
	skippedIDs := append([]string{}, backend.SkippedIDs...)
	skipReasons := append([]string{}, backend.SkipReasons...)
	backend.SkipMu.Unlock()

	if len(skippedIDs) == 0 {
		t.Fatal("expected task-compact to be skipped, but no tasks were skipped")
	}
	if skippedIDs[0] != "task-compact" {
		t.Errorf("expected skipped task ID 'task-compact', got %q", skippedIDs[0])
	}
	if skipReasons[0] != "compaction_detected" {
		t.Errorf("expected skip reason 'compaction_detected', got %q", skipReasons[0])
	}

	// The skip must tear down the worktree so the agent's partial commits are
	// abandoned. Otherwise the branch survives and the wait-mode flush safety-net
	// (selectNextTask → FlushUnpushedWork) auto-merges unverified work to main.
	insp, ok := gm.(git.StubInspector)
	if !ok {
		t.Fatal("stub git must implement StubInspector")
	}
	if insp.GetRemoveWorktreeCalls() == 0 {
		t.Error("expected worktree teardown after compaction skip (RemoveWorktree), but it was never called — partial commits would survive for the flush safety-net to merge")
	}
}
