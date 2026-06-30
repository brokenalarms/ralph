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

// Proves: when compaction fires after the agent committed work that passes
// verification, the loop routes through completeTask/ship instead of
// skipping. The task is closed, not skipped, and the worktree is not torn down.
func TestLoop_CompactionAfterVerifiedCommit_Ships(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{}
	backend.Remaining = 1
	backend.Total = 1
	backend.NextID = "task-compact-ship"
	backend.NextTask = "Task that compacts after verified commit"

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{Compacted: true},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:         dir,
		WorkDir:            dir,
		RemoteURL:          "https://github.com/owner/repo.git",
		LogOnelineResult:   "abc1234 fix: implement the task",
		Ship:               git.ShipResult{PRNumber: 77},
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
		VerifyHook:   passingVerifyHook(),
	})

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// Task must be closed (shipped), not skipped.
	backend.SkipMu.Lock()
	skippedIDs := append([]string{}, backend.SkippedIDs...)
	backend.SkipMu.Unlock()
	if len(skippedIDs) != 0 {
		t.Errorf("expected no skipped tasks when compaction fires after verified commit, got %v", skippedIDs)
	}

	backend.CloseMu.Lock()
	closedIDs := append([]string{}, backend.ClosedIDs...)
	backend.CloseMu.Unlock()
	if len(closedIDs) == 0 {
		t.Fatal("expected task to be closed (shipped) when compaction fires after verified commit, but no tasks were closed")
	}
	if closedIDs[0] != "task-compact-ship" {
		t.Errorf("expected closed task ID 'task-compact-ship', got %q", closedIDs[0])
	}

	// Worktree is torn down after shipping — same as the normal signal path.
	// The PR exists on the remote; the local worktree is no longer needed.
	insp, ok := gm.(git.StubInspector)
	if !ok {
		t.Fatal("stub git must implement StubInspector")
	}
	if insp.GetRemoveWorktreeCalls() == 0 {
		t.Error("expected worktree teardown after shipping compacted work (same as normal completion path)")
	}
}

// Proves: when compaction fires after the agent committed work but verification
// fails, the loop still skips (not ships) and tears down the worktree.
func TestLoop_CompactionAfterCommitVerificationFails_Skips(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{}
	backend.Remaining = 1
	backend.Total = 1
	backend.NextID = "task-compact-fail"
	backend.NextTask = "Task that compacts but verification fails"

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{Compacted: true},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:       dir,
		WorkDir:          dir,
		RemoteURL:        "https://github.com/owner/repo.git",
		LogOnelineResult: "abc1234 fix: implement the task",
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
		VerifyHook:   &stubVerifyHook{passed: false, reason: "tests_failed"},
	})

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// Task must be skipped (verification failed).
	backend.SkipMu.Lock()
	skippedIDs := append([]string{}, backend.SkippedIDs...)
	skipReasons := append([]string{}, backend.SkipReasons...)
	backend.SkipMu.Unlock()
	if len(skippedIDs) == 0 {
		t.Fatal("expected task to be skipped when verification fails after compaction, but no tasks were skipped")
	}
	if skippedIDs[0] != "task-compact-fail" {
		t.Errorf("expected skipped task ID 'task-compact-fail', got %q", skippedIDs[0])
	}
	if skipReasons[0] != "compaction_detected" {
		t.Errorf("expected skip reason 'compaction_detected', got %q", skipReasons[0])
	}

	// Worktree must be torn down (unverified work must not survive).
	insp, ok := gm.(git.StubInspector)
	if !ok {
		t.Fatal("stub git must implement StubInspector")
	}
	if insp.GetRemoveWorktreeCalls() == 0 {
		t.Error("expected worktree teardown after compaction+verification-failure, but RemoveWorktree was never called")
	}
}

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

// Proves: compaction parks a task even when that task has open dependents —
// the strand guard is gone, so the loop never halts on strand. The task is
// parked and the loop continues to other ready work (AC1, AC4, AC5 from
// ralph-n4u3).
func TestLoop_CompactingTask_ParksWithOpenDependents(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:      1,
				Total:          1,
				NextID:         "task-compact-dep",
				NextTask:       "Task that compacts with open dep",
				OpenDependents: []string{"task-dep-1"},
			},
		},
	}

	runner := &stubRunner{
		onRun: func() {
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
		MaxIterations: 5,
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

	// Task must be parked (SkipTask called) even though it has an open dependent.
	backend.SkipMu.Lock()
	skippedIDs := append([]string(nil), backend.SkippedIDs...)
	skipReasons := append([]string(nil), backend.SkipReasons...)
	backend.SkipMu.Unlock()
	if len(skippedIDs) == 0 {
		t.Fatal("expected task to be parked (SkipTask called) despite open dependent, but no tasks were skipped")
	}
	if skippedIDs[0] != "task-compact-dep" {
		t.Errorf("expected SkipTask(task-compact-dep), got %q", skippedIDs[0])
	}
	if skipReasons[0] != "compaction_detected" {
		t.Errorf("expected skip reason 'compaction_detected', got %q", skipReasons[0])
	}

	// Loop must not halt with a strand error.
	status, _ := st.Read("status")
	if strings.HasPrefix(status, "halted_skip_would_strand_dependents") {
		t.Errorf("loop halted with strand error even though strand guard is removed; got %q", status)
	}
}
