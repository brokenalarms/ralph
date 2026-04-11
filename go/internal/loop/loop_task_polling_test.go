package loop

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies that l.pollForTasks returns found=true when the backend reports
// tasks remaining, without requiring any params struct.
func TestPollForTasks_PackageFunction(t *testing.T) {
	dir, st := setupTestDir(t)
	logger := logging.New(nil)
	backend := &testutil.StubBackend{Remaining: 1}
	_ = dir

	l := &Loop{
		state:       st,
		logger:      logger,
		taskBackend: backend,
	}
	found, done := l.pollForTasks()

	if !found {
		t.Error("expected found=true when backend has remaining tasks")
	}
	if done {
		t.Error("expected done=false when tasks are available")
	}
}

// Verifies that l.waitForTasks detects newly available tasks added by OnWait
// and returns true.
func TestWaitForTasks_PackageFunction(t *testing.T) {
	_, st := setupTestDir(t)
	logger := logging.New(nil)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{Remaining: 0},
	}

	onWaitCalled := false
	l := &Loop{
		cfg: Config{
			OnWait: func() {
				onWaitCalled = true
				backend.Lock()
				backend.Remaining = 1
				backend.Unlock()
			},
		},
		state:       st,
		logger:      logger,
		taskBackend: backend,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	found := l.waitForTasks(ctx)

	if !found {
		t.Fatal("expected waitForTasks to find tasks after OnWait added them")
	}
	if !onWaitCalled {
		t.Error("expected OnWait to be called")
	}
}

// Verifies that l.beginIteration records the task title and iteration number
// in state.
func TestBeginIteration_PackageFunction(t *testing.T) {
	dir, st := setupTestDir(t)
	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}
	task := taskContext{id: "ralph-abc", title: "Fix auth"}

	l := &Loop{state: st, git: gm, logger: logging.New(nil)}
	l.beginIteration(task, 3)

	storeState, _ := st.Load()
	if storeState.LastTask != "Fix auth" {
		t.Errorf("expected LastTask='Fix auth' in state, got %q", storeState.LastTask)
	}
	if storeState.Iteration != 3 {
		t.Errorf("expected Iteration=3 in state, got %d", storeState.Iteration)
	}
}

// Verifies that waitForRate returns true immediately when the rate limiter
// allows the call, exercised through the Loop method.
func TestWaitForRate_AllowsWhenUnderLimit(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	cfg := Config{
		Dirs:         workctx.WorkContext{RalphDir: ralphDir},
		CallsPerHour: 80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         &git.StubRepo{},
		TaskBackend: nil,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	allowed := l.waitForRate(context.Background())

	if !allowed {
		t.Error("expected waitForRate to return true when limiter allows")
	}
}

// Verifies that l.logIterationBanner emits log output when called.
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

	l := &Loop{
		state:       st,
		logger:      logger,
		taskBackend: backend,
	}
	l.logIterationBanner(logIterationBannerParams{version: "1.0.0"}, 1, 10, 1, task, analyzer.Warn)

	output := logBuf.String()
	if output == "" {
		t.Error("expected logIterationBanner to produce log output")
	}
}
