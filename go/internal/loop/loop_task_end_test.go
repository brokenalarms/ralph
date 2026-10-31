package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// runSingleTaskLoop runs one iteration of a single-task loop with the given
// agent result and returns how many worktrees the loop tore down.
func runSingleTaskLoop(t *testing.T, result claude.Result) int {
	t.Helper()
	dir, st := setupTestDir(t)
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "A task", NextID: "ralph-one"},
	}
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	logger := logging.New(nil)
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   filepath.Join(dir, ".ralph"),
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		Runner:       &stubRunner{result: result},
	})
	_ = l.Run(context.Background())
	return gm.(git.StubInspector).GetRemoveWorktreeCalls()
}

// An interrupted task hasn't ended: its worktree holds the only copy of its
// work, and `ralph resume` finishes it from there.
func TestLoop_InterruptedTaskKeepsItsWorktree(t *testing.T) {
	if calls := runSingleTaskLoop(t, claude.Result{FeedbackKill: true}); calls != 0 {
		t.Errorf("expected the interrupted task's worktree to survive, got %d teardowns", calls)
	}
}

// A skip ends a task, so its worktree must go even when the loop exits straight
// after, as an app-wide halt does. Otherwise resume finds a worktree that
// belongs to no live task.
func TestFinishEndedTask_TearsDownSkippedTask(t *testing.T) {
	dir, st := setupTestDir(t)
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	l := &Loop{state: st, git: gm, logger: logging.New(nil), taskBackend: &testutil.TrackingBackend{}}

	l.skipTask("ralph-skip", tasks.SkipIdleTimeout, "")
	loop := &loopState{}
	l.finishEndedTask(loop)
	l.finishEndedTask(loop)

	if calls := gm.(git.StubInspector).GetRemoveWorktreeCalls(); calls != 1 {
		t.Errorf("expected exactly one teardown, got %d", calls)
	}
	if !loop.worktreeNeedsSetup {
		t.Error("expected the next task to be told to set up a worktree")
	}
}

// Parking a task at selection time doesn't end the current task. That task may
// own the worktree, so tearing it down would destroy live work.
func TestFinishEndedTask_ParkingAtSelectionKeepsWorktree(t *testing.T) {
	dir, st := setupTestDir(t)
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	l := &Loop{state: st, git: gm, logger: logging.New(nil), taskBackend: &testutil.TrackingBackend{}}

	l.parkTask("ralph-done", tasks.SkipAlreadyCompleted, "")
	l.finishEndedTask(&loopState{})

	if calls := gm.(git.StubInspector).GetRemoveWorktreeCalls(); calls != 0 {
		t.Errorf("expected no teardown for a selection-time park, got %d", calls)
	}
}

// Finding a task's PR already merged on resume ends that task, and the merged
// branch must not stay behind for the next task to inherit.
func TestOnResumeDone_EndsTheTask(t *testing.T) {
	dir, st := setupTestDir(t)
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	l := &Loop{state: st, git: gm, logger: logging.New(nil), taskBackend: &testutil.TrackingBackend{}}

	l.onResumeDone(context.Background(), "ralph-merged", "Merged task", git.ResumeTaskResult{AlreadyMerged: true})
	l.finishEndedTask(&loopState{})

	if calls := gm.(git.StubInspector).GetRemoveWorktreeCalls(); calls != 1 {
		t.Errorf("expected the merged task's worktree to be torn down, got %d teardowns", calls)
	}
}
