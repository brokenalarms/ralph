package loop

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// readyRaceBackend simulates the dep-graph mutation race:
// - GetNextTaskInfo returns task-x on the first call, then an empty task
// - IsReady("task-x") returns false (dep was added after selection)
// - IsReady for all other ids returns true
// The test asserts that the agent runner is never called.
type readyRaceBackend struct {
	testutil.TrackingBackend
	mu              sync.Mutex
	selectCallCount int
	notReadyID      string
}

func (b *readyRaceBackend) HasRemaining() (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.selectCallCount == 0, nil
}

func (b *readyRaceBackend) CountRemaining() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.selectCallCount == 0 {
		return 1, nil
	}
	return 0, nil
}

func (b *readyRaceBackend) CountTotal() (int, error) { return 1, nil }

func (b *readyRaceBackend) GetNextTaskInfo() (tasks.TaskInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.selectCallCount++
	if b.selectCallCount == 1 {
		return tasks.TaskInfo{ID: b.notReadyID, Title: "Task X"}, nil
	}
	return tasks.TaskInfo{}, nil
}

func (b *readyRaceBackend) IsReady(id string) (bool, error) {
	if id == b.notReadyID {
		return false, nil
	}
	return true, nil
}

// Proves: when IsReady returns false before agent invocation, the iteration is
// skipped without calling the agent runner. The next selection picks a
// different task (or exits when none remain).
func TestRunAgent_SkipsIterationWhenTaskNotReady(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &readyRaceBackend{notReadyID: "task-x"}
	backend.Remaining = 1
	backend.Total = 1

	runnerCallCount := 0
	runner := &stubRunner{
		onRun: func() { runnerCallCount++ },
		result: claude.Result{},
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

	if runnerCallCount != 0 {
		t.Errorf("expected agent runner to be called 0 times (task was not ready), got %d", runnerCallCount)
	}

	finalState, _ := st.Load()
	if finalState.Status != "completed" {
		t.Errorf("expected loop to exit with status 'completed', got %q", finalState.Status)
	}
}
