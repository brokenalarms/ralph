package loop

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/analyzer"
	"github.com/brokenalarms/ralph/internal/claude"
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
		state:       st,
		logger:      logger,
		taskBackend: backend,
		waitHook: &stubWaitHook{fn: func() {
			onWaitCalled = true
			backend.Lock()
			backend.Remaining = 1
			backend.Unlock()
		}},
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
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
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
		Git:         git.NewStub(git.StubRepoConfig{}),
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
	l.logIterationBanner(logIterationBannerParams{version: "1.0.0"}, 1, 10, task, analyzer.Warn)

	output := logBuf.String()
	if output == "" {
		t.Error("expected logIterationBanner to produce log output")
	}
}

// Verifies that logIterationBanner uses per-run progress (len(completedTasks))
// rather than lifetime backend counts, so 'done this run' resets on each process start.
func TestLogIterationBanner_ShowsPerRunCounts(t *testing.T) {
	_, st := setupTestDir(t)

	backend := &testutil.StubBackend{
		Remaining: 5,
		Completed: 100,
		Total:     200,
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
		completedTasks: []CompletedTask{
			{ID: "ralph-t1"},
			{ID: "ralph-t2"},
			{ID: "ralph-t3"},
		},
	}
	l.logIterationBanner(logIterationBannerParams{version: "1.0.0"}, 2, 50, task, analyzer.Continue)

	output := logBuf.String()
	if !strings.Contains(output, "3 done this run") {
		t.Errorf("banner should show '3 done this run' from completedTasks, got:\n%s", output)
	}
	if !strings.Contains(output, "5 remaining") {
		t.Errorf("banner should show '5 remaining' from backend.CountRemaining, got:\n%s", output)
	}
	if strings.Contains(output, "100") || strings.Contains(output, "200") {
		t.Errorf("banner must not show lifetime backend counts (100/200), got:\n%s", output)
	}
	if strings.Contains(output, "lifetime") {
		t.Errorf("banner must not contain 'lifetime', got:\n%s", output)
	}
}

// Verifies that the end-of-iteration log line shows elapsed time but no task counts,
// so the summary line does not duplicate the banner's information.
func TestProcessRunOutcome_NoTaskCountInIterationLog(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &testutil.StubBackend{
		Remaining: 5,
		Completed: 42,
		Total:     100,
	}

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		CallsPerHour: 80,
	}
	l := New(cfg, Modules{
		State:        st,
		Git:          git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	elapsed := 7*time.Minute + 58*time.Second
	result := claude.Result{SignalDetected: true}
	analysisResult := analyzer.Result{Action: analyzer.Continue}
	l.processRunOutcome(result, elapsed, 3, iterationPrompt{}, "ralph-abc", "next-task", analysisResult, "")

	output := logBuf.String()
	if !strings.Contains(output, "Run iteration 3 complete") {
		t.Errorf("end-of-iteration line should contain 'Run iteration 3 complete', got:\n%s", output)
	}
	if !strings.Contains(output, "7m58s") {
		t.Errorf("end-of-iteration line should contain elapsed time '7m58s', got:\n%s", output)
	}
	if strings.Contains(output, "tasks done") {
		t.Errorf("end-of-iteration line must not contain 'tasks done', got:\n%s", output)
	}
	if strings.Contains(output, "42/100") || strings.Contains(output, "42") {
		t.Errorf("end-of-iteration line must not contain lifetime task counts, got:\n%s", output)
	}
}
