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

// alwaysNotReadyBackend returns the same task on every selection, but IsReady
// always returns false — simulating a task that is perpetually blocked after
// selection. Used to drive consecutive no-agent iterations.
type alwaysNotReadyBackend struct {
	testutil.TrackingBackend
	mu sync.Mutex
}

func (b *alwaysNotReadyBackend) HasRemaining() (bool, error) { return true, nil }
func (b *alwaysNotReadyBackend) CountRemaining() (int, error) { return 1, nil }
func (b *alwaysNotReadyBackend) CountTotal() (int, error)    { return 1, nil }
func (b *alwaysNotReadyBackend) GetNextTaskInfo() (tasks.TaskInfo, error) {
	return tasks.TaskInfo{ID: "task-spin", Title: "Spinning task"}, nil
}
func (b *alwaysNotReadyBackend) IsReady(_ string) (bool, error) { return false, nil }

// Proves: two consecutive iterations where IsReady returns false (no agent
// invoked) cause the loop to halt with status=halted_no_agent_progress before
// iteration 3 begins.
func TestLoop_HaltsAfterTwoConsecutiveNoAgentIterations(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &alwaysNotReadyBackend{}
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

	if runnerCallCount != 0 {
		t.Errorf("expected agent runner to be called 0 times, got %d", runnerCallCount)
	}

	status, _ := st.Read("status")
	if status != "halted_no_agent_progress" {
		t.Errorf("expected status halted_no_agent_progress, got %q", status)
	}
}

// onceNotReadyBackend returns task-alpha on the first two selections.
// IsReady returns false on the first call (triggers a no-agent iteration),
// then true on subsequent calls (agent can run).
// After the runner fires, HasRemaining returns false so the loop exits cleanly.
type onceNotReadyBackend struct {
	testutil.TrackingBackend
	mu           sync.Mutex
	isReadyCalls int
	agentRan     bool
}

func (b *onceNotReadyBackend) HasRemaining() (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.agentRan, nil
}

func (b *onceNotReadyBackend) CountRemaining() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.agentRan {
		return 0, nil
	}
	return 1, nil
}

func (b *onceNotReadyBackend) CountTotal() (int, error) { return 1, nil }

func (b *onceNotReadyBackend) GetNextTaskInfo() (tasks.TaskInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.agentRan {
		return tasks.TaskInfo{}, nil
	}
	return tasks.TaskInfo{ID: "task-alpha", Title: "Alpha task"}, nil
}

func (b *onceNotReadyBackend) IsReady(_ string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.isReadyCalls++
	return b.isReadyCalls > 1, nil
}

func (b *onceNotReadyBackend) markAgentRan() {
	b.mu.Lock()
	b.agentRan = true
	b.mu.Unlock()
}

// Proves: one no-agent iteration (IsReady=false) followed by a normal agent
// run resets consecutiveNoAgentIters to 0. The loop continues and exits
// normally (status=completed), not halted_no_agent_progress.
func TestLoop_CounterResetsAfterAgentRun(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &onceNotReadyBackend{}
	backend.Remaining = 1
	backend.Total = 1

	runnerCallCount := 0
	runner := &stubRunner{
		onRun: func() {
			runnerCallCount++
			backend.markAgentRan()
		},
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

	status, _ := st.Read("status")
	if status == "halted_no_agent_progress" {
		t.Error("expected loop to exit normally, not halted_no_agent_progress")
	}
	if status != "completed" {
		t.Errorf("expected status completed, got %q", status)
	}
}
