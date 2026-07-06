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

// alreadyMergedBackend serves two tasks, one after another, both of which
// the resume path discovers are already merged (git stub always returns
// ResumeTaskResult{Handled: true}). Once both are closed, no tasks remain.
type alreadyMergedBackend struct {
	testutil.TrackingBackend
}

func (b *alreadyMergedBackend) closedCount() int {
	b.CloseMu.Lock()
	defer b.CloseMu.Unlock()
	return len(b.ClosedIDs)
}

func (b *alreadyMergedBackend) HasRemaining() (bool, error) { return b.closedCount() < 2, nil }
func (b *alreadyMergedBackend) CountRemaining() (int, error) {
	return 2 - b.closedCount(), nil
}
func (b *alreadyMergedBackend) CountTotal() (int, error) { return 2, nil }
func (b *alreadyMergedBackend) GetNextTaskInfo() (tasks.TaskInfo, error) {
	switch b.closedCount() {
	case 0:
		return tasks.TaskInfo{ID: "task-merged-1", Title: "Merged task one"}, nil
	case 1:
		return tasks.TaskInfo{ID: "task-merged-2", Title: "Merged task two"}, nil
	default:
		return tasks.TaskInfo{}, nil
	}
}
func (b *alreadyMergedBackend) IsReady(_ string) (bool, error) { return true, nil }

// Proves: two consecutive iterations resolved via the resume path's
// already-merged Handled case (bead closed, no agent run) do NOT halt with
// halted_no_agent_progress — the counter resets on each Handled close just
// as it does on an agent invocation, so the loop proceeds to close both
// tasks and then exits normally once the queue is empty.
func TestLoop_AlreadyMergedResumeDoesNotHaltNoAgentProgress(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &alreadyMergedBackend{}
	backend.Remaining = 2
	backend.Total = 2

	runnerCallCount := 0
	runner := &stubRunner{
		onRun:  func() { runnerCallCount++ },
		result: claude.Result{},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		RemoteURL:  "https://github.com/owner/repo.git",
		ResumeTaskResult: git.ResumeTaskResult{
			Handled:       true,
			AlreadyMerged: true,
		},
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
		t.Errorf("expected agent runner to be called 0 times (both tasks closed via resume), got %d", runnerCallCount)
	}

	if got := backend.closedCount(); got != 2 {
		t.Errorf("expected both tasks closed, got %d closed: %v", got, backend.ClosedIDs)
	}

	status, _ := st.Read("status")
	if status == "halted_no_agent_progress" {
		t.Error("expected loop not to halt with halted_no_agent_progress after already-merged closes")
	}
	if status != "completed" {
		t.Errorf("expected status completed, got %q", status)
	}
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
