package loop

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/ratelimit"
	"github.com/brokenalarms/ralph/internal/testutil"
)

// Verifies that pollForTasks is a package function: returns found=true when
// the backend reports tasks remaining, without accessing any Loop struct field.
func TestPollForTasks_PackageFunction(t *testing.T) {
	dir, st := setupTestDir(t)
	logger := logging.New(nil)

	backend := &testutil.StubBackend{Remaining: 1}

	found, done := pollForTasks(pollForTasksParams{
		state:   st,
		backend: backend,
		logger:  logger,
	})

	_ = dir
	if !found {
		t.Error("expected found=true when backend has remaining tasks")
	}
	if done {
		t.Error("expected done=false when tasks are available")
	}
}

// Verifies that waitForTasks is a package function: it detects newly available
// tasks added by onWaitFunc and returns true, without accessing any Loop field.
func TestWaitForTasks_PackageFunction(t *testing.T) {
	_, st := setupTestDir(t)
	logger := logging.New(nil)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{Remaining: 0},
	}

	onWaitCalled := false
	onWaitFunc := func() {
		onWaitCalled = true
		backend.Lock()
		backend.Remaining = 1
		backend.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	found := waitForTasks(ctx, waitForTasksParams{
		logger:     logger,
		state:      st,
		backend:    backend,
		onWaitFunc: onWaitFunc,
	})

	if !found {
		t.Fatal("expected waitForTasks to find tasks after onWaitFunc added them")
	}
	if !onWaitCalled {
		t.Error("expected onWaitFunc to be called")
	}
}

// Verifies that beginIteration is a package function: it records the task title
// and iteration number in state, without accessing any Loop struct field.
func TestBeginIteration_PackageFunction(t *testing.T) {
	dir, st := setupTestDir(t)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	task := taskContext{id: "ralph-abc", title: "Fix auth"}

	beginIteration(beginIterationParams{
		state: st,
		git:   gm,
	}, task, 3)

	storeState, _ := st.Load()
	if storeState.LastTask != "Fix auth" {
		t.Errorf("expected LastTask='Fix auth' in state, got %q", storeState.LastTask)
	}
	if storeState.Iteration != 3 {
		t.Errorf("expected Iteration=3 in state, got %d", storeState.Iteration)
	}
}

// Verifies that waitForRate is a package function: it returns true immediately
// when the rate limiter allows the call, without accessing any Loop field.
func TestWaitForRate_PackageFunction(t *testing.T) {
	dir, _ := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	logger := logging.New(nil)
	limiter := ratelimit.New(ralphDir, 80)

	allowed := waitForRate(context.Background(), waitForRateParams{
		limiter:      limiter,
		callsPerHour: 80,
		logger:       logger,
	})

	if !allowed {
		t.Error("expected waitForRate to return true when limiter allows")
	}
}

// Verifies that logIterationBanner is a package function taking lastAction as
// a parameter: it emits log output when called without any Loop field access.
func TestLogIterationBanner_PackageFunction(t *testing.T) {
	_, st := setupTestDir(t)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Fix login",
		NextID:    "ralph-abc",
	}

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)
	task := taskContext{id: "ralph-abc", title: "Fix login"}

	logIterationBanner(logIterationBannerParams{
		backend: backend,
		state:   st,
		logger:  logger,
		version: "1.0.0",
	}, 1, 10, 1, task, analyzer.Warn)

	output := logBuf.String()
	if output == "" {
		t.Error("expected logIterationBanner to produce log output")
	}
}
