package loop

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Orchestrator status messages ("All tasks complete!", "No tasks found") must
// use the [o] actor prefix, not the task backend label (e.g. [beads] without [o]).
// The [o][beads] tag is valid — it marks orchestrator-initiated beads operations.
func TestLoop_OrchestratorMessagesUseLoopPrefix(t *testing.T) {
	tests := []struct {
		name    string
		backend *testutil.StubBackend
		want    string // substring expected in log output
	}{
		{
			name: "all tasks complete uses orchestrator actor prefix",
			backend: &testutil.StubBackend{
				Remaining: 0, Completed: 3, Total: 3, BackendLabel: "beads",
			},
			want: "[o]",
		},
		{
			name: "no tasks error uses orchestrator actor prefix",
			backend: &testutil.StubBackend{
				Remaining: 0, Completed: 0, Total: 0, BackendLabel: "beads",
			},
			want: "[o]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, st := setupTestDir(t)
			ralphDir := filepath.Join(dir, ".ralph")

			var logBuf strings.Builder
			logger := logging.New(&logBuf)

			gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
			cfg := Config{
				Dirs: workctx.WorkContext{
					ProjectDir: dir,
					WorkDir:    dir,
					RalphDir:   ralphDir,
				},
				MaxIterations: 5,
				CallsPerHour:  80,
			}
			l := New(cfg, Modules{
				State:       st,
				Git:         gm,
				TaskBackend: tt.backend,
				Logger:      logger,
				Verifier:    newTestVerifier(t, cfg, logger),
				Connectivity: onlineStubConnectivity(),
			})

			l.Run(context.Background())

			output := logBuf.String()
			if !strings.Contains(output, tt.want) {
				t.Errorf("expected %q in log output:\n%s", tt.want, output)
			}
		})
	}
}

// Verifies that when a task has a description, the log output includes
// the description on a separate line after the task title.
func TestLoop_LogsTaskDescription(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	var logBuf strings.Builder
	logger := logging.New(&logBuf)

	backend := &testutil.StubBackend{
		Remaining:    1,
		Completed:    0,
		Total:        1,
		NextTask:     "Fix the auth module",
		NextID:       "ralph-abc",
		BackendLabel: "beads",
		Description:  "Auth tokens are expiring too early due to clock skew",
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = &stubRunner{
		onRun: func() {
			backend.Remaining = 0
			backend.Completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	l.Run(context.Background())

	output := logBuf.String()
	if !strings.Contains(output, "ralph-abc: Fix the auth module") {
		t.Errorf("expected task banner with bead ID and title:\n%s", output)
	}
	if !strings.Contains(output, "═") {
		t.Error("expected ═ separator characters in task banner")
	}
	if !strings.Contains(output, "Auth tokens are expiring too early due to clock skew") {
		t.Errorf("expected task description in log output:\n%s", output)
	}
	if strings.Contains(output, "Next task:") {
		t.Error("redundant 'Next task:' line should be removed")
	}
	if strings.Contains(output, "→ implementing") {
		t.Error("redundant '→ implementing' line should be removed")
	}
}

// Verifies that a multi-line bead description is truncated to 3 lines in the
// stream log with a dim "… (N more lines)" indicator, so the operator sees only
// the gist rather than the full agent spec.
func TestLoop_LongDescriptionTruncatedInStream(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	var streamBuf bytes.Buffer
	logger := logging.NewWithWriter(&streamBuf)

	desc := "Line one of the description\nLine two goes here\nLine three is next\nLine four is hidden\nLine five is hidden"

	backend := &testutil.StubBackend{
		Remaining:    1,
		Completed:    0,
		Total:        1,
		NextTask:     "Fix the auth module",
		NextID:       "ralph-abc",
		BackendLabel: "beads",
		Description:  desc,
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = &stubRunner{
		onRun: func() {
			backend.Remaining = 0
			backend.Completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	l.Run(context.Background())

	output := streamBuf.String()

	for _, want := range []string{"Line one", "Line two", "Line three"} {
		if !strings.Contains(output, want) {
			t.Errorf("stream missing %q:\n%s", want, output)
		}
	}
	for _, absent := range []string{"Line four", "Line five"} {
		if strings.Contains(output, absent) {
			t.Errorf("stream should not contain truncated line %q:\n%s", absent, output)
		}
	}
	if !strings.Contains(output, "2 more lines") {
		t.Errorf("stream missing truncation indicator:\n%s", output)
	}
}

// Verifies that when a task has no description, no extra description line
// is logged — only the task title appears.
func TestLoop_NoDescriptionOmitsLine(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	var logBuf strings.Builder
	logger := logging.New(&logBuf)

	backend := &testutil.StubBackend{
		Remaining:    1,
		Completed:    0,
		Total:        1,
		NextTask:     "Fix the auth module",
		NextID:       "ralph-abc",
		BackendLabel: "beads",
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = &stubRunner{
		onRun: func() {
			backend.Remaining = 0
			backend.Completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	l.Run(context.Background())

	output := logBuf.String()
	if !strings.Contains(output, "ralph-abc: Fix the auth module") {
		t.Errorf("expected task banner with bead ID and title:\n%s", output)
	}
	if strings.Contains(output, "Next task:") {
		t.Error("redundant 'Next task:' line should be removed")
	}
	// Count lines containing "description" — there should be none since
	// the backend returns an empty description.
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Description:") {
			t.Errorf("unexpected description line in log output when description is empty:\n%s", output)
			break
		}
	}
}

func TestLoop_DashedSeparatorBetweenIterations(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0
	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        2,
			NextTask:     "task A",
			NextID:       "ralph-aaa",
			BackendLabel: "beads",
		},
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			if iterationCount == 1 {
				backend.Lock()
				backend.Completed = 1
				backend.Remaining = 1
				backend.NextTask = "task B"
				backend.NextID = "ralph-bbb"
				backend.Unlock()
			} else {
				backend.Lock()
				backend.Completed = 2
				backend.Remaining = 0
				backend.Unlock()
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)
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
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	output := logBuf.String()
	if !strings.Contains(output, "─") {
		t.Error("expected dashed separator (─) between iterations")
	}
	if iterationCount != 2 {
		t.Errorf("expected 2 iterations, got %d", iterationCount)
	}
}

// Verifies that the loop prints a task separator banner with the bead ID
// when a new task starts, replacing the old per-line magenta prefix.
func TestLoop_TaskBannerOnNewTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "fix the thing",
			NextID:       "ralph-l337",
			BackendLabel: "beads",
		},
	}

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)
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
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	output := logBuf.String()
	if !strings.Contains(output, "ralph-l337: fix the thing") {
		t.Errorf("expected task banner with bead ID and title, got: %s", output)
	}
	if !strings.Contains(output, "═") {
		t.Error("expected ═ separator characters in task banner")
	}
}

// Verifies that when Claude reports a rate limit, the loop waits until
// the reset time and retries the iteration instead of counting it as
// stagnation.
func TestLoop_RateLimitWaitsAndRetries(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0
	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "fix the bug",
			NextID:       "ralph-rl1",
			BackendLabel: "beads",
		},
	}

	// First call returns rate limited with a reset time in the past (so
	// WaitUntil returns immediately). Second call completes the task.
	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			if iterationCount >= 2 {
				backend.Lock()
				backend.Completed = 1
				backend.Remaining = 0
				backend.Unlock()
			}
		},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	var logBuf bytes.Buffer
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
	logger := logging.NewWithWriter(&logBuf)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				return "YES: looks good", nil
			}},
		}),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})

	// Override the runner to return different results per iteration.
	l.runner = &rateLimitStubRunner{
		backend: backend,
		counter: &iterationCount,
	}

	_ = l.Run(context.Background())

	output := logBuf.String()
	t.Logf("Output: %s", output)
	if !strings.Contains(output, "rate limit") && !strings.Contains(output, "Rate limit") {
		t.Errorf("expected rate limit log message, got: %s", output)
	}
	if !strings.Contains(output, "resuming") {
		t.Errorf("expected 'resuming' after rate limit wait, got: %s", output)
	}
	// rateLimitStubRunner tracks its own calls.
	rlRunner := l.runner.(*rateLimitStubRunner)
	if rlRunner.calls < 2 {
		t.Errorf("expected at least 2 Claude calls (rate limit + retry), got %d", rlRunner.calls)
	}
	_ = runner // silence unused
	_ = iterationCount
}

type rateLimitStubRunner struct {
	backend *testutil.MutableBackend
	counter *int
	calls   int
}

func (r *rateLimitStubRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	r.calls++
	if r.calls == 1 {
		return claude.Result{
			RateLimited: true,
			ResetAt:     time.Now().Add(-1 * time.Second),
		}, nil
	}
	r.backend.Lock()
	r.backend.Completed = 1
	r.backend.Remaining = 0
	r.backend.Unlock()
	return claude.Result{SignalDetected: true, Summary: "done"}, nil
}

func (r *rateLimitStubRunner) StopStreaming() {}

func (r *rateLimitStubRunner) InjectMessage(_ string) error { return nil }

func (r *rateLimitStubRunner) Query(_ context.Context, _, _, _ string, _ []string) (string, error) {
	return "NO: stub", nil
}

// Health dashboard is logged between iterations in verbose mode so operators
// can detect process leaks, stale signal files, and growing state.json.

// Health dashboard is logged between iterations in verbose mode so operators
// can detect process leaks, stale signal files, and growing state.json.
// Verifies that the iteration banner includes the Ralph version when
// Config.Version is set, so operators can tell which build is running.
func TestLoop_IterationBannerShowsVersion(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "check version",
			NextID:       "ralph-ver1",
			BackendLabel: "beads",
		},
	}

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		Version:       "1.2.3",
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	output := logBuf.String()
	if !strings.Contains(output, "Ralph v1.2.3") {
		t.Errorf("expected 'Ralph v1.2.3' in iteration banner, got:\n%s", output)
	}
}

// --- handleRunResult tests ---

// handleRunResultCall invokes handleRunResult on l's Loop.
func handleRunResultCall(l *Loop, ctx context.Context, result claude.Result, runErr error, taskID, nextTask, headBefore string, runIteration int) loopAction {
	return l.handleRunResult(ctx, result, runErr, taskID, nextTask, headBefore, runIteration)
}

// newHandleRunResultLoop creates a minimal Loop for testing handleRunResult.
// conn is the Connectivity stub to inject; pass onlineStubConnectivity() for the
// default online case.
func newHandleRunResultLoop(t *testing.T, conn Connectivity) (*Loop, string) {
	t.Helper()
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	logger := logging.New(nil)
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  nil,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: conn,
	})

	return l, ralphDir
}

// Verifies that when Claude returns an error and the machine is offline,
// handleRunResult waits for internet and returns actionRetry with decremented counters.
func TestHandleRunResult_OfflineReturnsRetry(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, &stubConnectivity{offline: true})

	runIter := 3
	action := handleRunResultCall(l, context.Background(), claude.Result{}, fmt.Errorf("connection refused"),
		"task-1", "Do stuff", "abc123", runIter)

	if action != actionRetry {
		t.Fatalf("expected actionRetry, got %d", action)
	}
}

// Verifies that when offline and the context is cancelled while waiting,
// handleRunResult returns actionDone.
func TestHandleRunResult_OfflineContextCancelledReturnsBreak(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, &stubConnectivity{offline: true, waitDeclined: true})

	runIter := 3
	action := handleRunResultCall(l, context.Background(), claude.Result{}, fmt.Errorf("connection refused"),
		"task-1", "Do stuff", "abc123", runIter)

	if action != actionDone {
		t.Fatalf("expected actionDone, got %d", action)
	}
}

// Verifies that the FeedbackKill path records an in-memory attempt and returns actionRetry.
func TestHandleRunResult_FeedbackKillReturnsRetry(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	runIter := 3
	result := claude.Result{FeedbackKill: true}
	action := handleRunResultCall(l, context.Background(), result, nil,
		"task-fk", "Feedback task", "abc123", runIter)

	if action != actionRetry {
		t.Fatalf("expected actionRetry, got %d", action)
	}

	found := false
	for _, ev := range l.taskAttempts {
		if strings.Contains(ev.Analysis, "user_feedback") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected in-memory attempt recorded with user_feedback analysis")
	}
}

// Verifies that the IdleTimeout path records an in-memory attempt and returns actionRetry.
func TestHandleRunResult_IdleTimeoutReturnsRetry(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	runIter := 3
	result := claude.Result{IdleTimeout: true}
	action := handleRunResultCall(l, context.Background(), result, nil,
		"task-it", "Idle task", "abc123", runIter)

	if action != actionRetry {
		t.Fatalf("expected actionRetry, got %d", action)
	}

	found := false
	for _, ev := range l.taskAttempts {
		if strings.Contains(ev.Analysis, "idle_timeout") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected in-memory attempt recorded with idle_timeout analysis")
	}
}

// Verifies that after MaxIdleTimeoutFailures consecutive idle timeouts,
// handleRunResult skips the task instead of retrying.
func TestHandleRunResult_IdleTimeoutSkipsAfterMaxFailures(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	backend := &testutil.StubBackend{}
	l.taskBackend = backend

	// Seed in-memory idle timeout count to one below the threshold
	l.currentTaskID = "task-it-max"
	l.taskIdleTimeouts = l.maxIdleTimeoutFailures() - 1

	runIter := 3
	result := claude.Result{IdleTimeout: true}
	action := handleRunResultCall(l, context.Background(), result, nil,
		"task-it-max", "Idle task", "abc123", runIter)

	if action != actionRetry {
		t.Fatalf("expected actionRetry, got %d", action)
	}

	if backend.SkippedTask != "task-it-max" {
		t.Errorf("expected task skipped in backend, got %q", backend.SkippedTask)
	}
}

// Verifies the RateLimited path calls WaitUntil on the limiter and returns
// actionRetry with decremented counters.
func TestHandleRunResult_RateLimitedReturnsRetry(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	resetAt := time.Now().Add(-1 * time.Second)
	runIter := 3
	result := claude.Result{RateLimited: true, ResetAt: resetAt}
	action := handleRunResultCall(l, context.Background(), result, nil,
		"task-rl", "Rate limited task", "abc123", runIter)

	if action != actionRetry {
		t.Fatalf("expected actionRetry, got %d", action)
	}
}

// Verifies that when the rate limit wait is interrupted by context cancellation,
// handleRunResult returns actionDone.
func TestHandleRunResult_RateLimitedContextCancelledReturnsBreak(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resetAt := time.Now().Add(10 * time.Minute)
	runIter := 3
	result := claude.Result{RateLimited: true, ResetAt: resetAt}
	action := handleRunResultCall(l, ctx, result, nil,
		"task-rl", "Rate limited task", "abc123", runIter)

	if action != actionDone {
		t.Fatalf("expected actionDone, got %d", action)
	}
}

// Verifies that a normal successful result returns actionProceed without
// modifying the iteration counters.
func TestHandleRunResult_NormalReturnsResultProceed(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	runIter := 3
	action := handleRunResultCall(l, context.Background(), claude.Result{}, nil,
		"task-ok", "Normal task", "abc123", runIter)

	if action != actionProceed {
		t.Fatalf("expected actionProceed, got %d", action)
	}
}

// Verifies that a Compacted result causes the task to be skipped with reason
// 'compaction_detected' and returns actionSkip — compaction means the agent
// hit a context leak and continuing would waste iterations.
func TestHandleRunResult_CompactedSkipsTask(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	backend := &testutil.StubBackend{}
	l.taskBackend = backend

	action := handleRunResultCall(l, context.Background(), claude.Result{Compacted: true}, nil,
		"task-cmp", "Compacting task", "abc123", 1)

	if action != actionSkip {
		t.Fatalf("expected actionSkip, got %d", action)
	}
	if backend.SkippedTask != "task-cmp" {
		t.Errorf("expected task skipped in backend, got %q", backend.SkippedTask)
	}
	if backend.SkipReason != "compaction_detected" {
		t.Errorf("expected skip reason 'compaction_detected', got %q", backend.SkipReason)
	}
}

// onResumeDone sends only TaskMerged (not TaskCompleted+TaskMerged pair) when PR is already
// merged and Notify is enabled — one notification per task, never two.
func TestOnResumeDone_Merged_NotifyEnabled(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix login",
				NextID:    "ralph-rm1",
			},
		},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		RemoteURL:  "https://github.com/owner/repo.git",
	})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		Notify:        true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})
	l.runner = &stubRunner{}

	var buf bytes.Buffer
	prev := notify.SetCommandRunner(func(_ string, args ...string) error {
		for _, a := range args {
			buf.WriteString(a)
			buf.WriteByte(' ')
		}
		return nil
	})
	t.Cleanup(func() { notify.SetCommandRunner(prev) })

	l.onResumeDone(context.Background(), "ralph-rm1", "Fix login", git.ResumeTaskResult{
		Handled:       true,
		AlreadyMerged: true,
		Merged:        true,
		PRNumber:      99,
	})

	got := buf.String()
	if strings.Contains(got, "Task done") {
		t.Errorf("expected no TaskCompleted notification for merged task, got %q", got)
	}
	if !strings.Contains(got, "Task merged: [ralph-rm1] Fix login") {
		t.Errorf("expected TaskMerged notification, got %q", got)
	}
}

// onResumeDone sends TaskCompleted (no TaskMerged) when PR is OPEN and Notify is enabled.
func TestOnResumeDone_Open_NotifyEnabled(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Add cache",
				NextID:    "ralph-ro1",
			},
		},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		RemoteURL:  "https://github.com/owner/repo.git",
	})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		Notify:        true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})
	l.runner = &stubRunner{}

	var buf bytes.Buffer
	prev := notify.SetCommandRunner(func(_ string, args ...string) error {
		for _, a := range args {
			buf.WriteString(a)
			buf.WriteByte(' ')
		}
		return nil
	})
	t.Cleanup(func() { notify.SetCommandRunner(prev) })

	l.onResumeDone(context.Background(), "ralph-ro1", "Add cache", git.ResumeTaskResult{
		Handled:  true,
		Merged:   false, // PR is open, not yet merged
		PRNumber: 88,
	})

	got := buf.String()
	if !strings.Contains(got, "Task done: [ralph-ro1] Add cache") {
		t.Errorf("expected TaskCompleted notification, got %q", got)
	}
	if strings.Contains(got, "Task merged") {
		t.Errorf("expected no TaskMerged notification for OPEN PR, got %q", got)
	}
}

// Proves completing two tasks in one process emits two separate
// === TASK COMPLETE === blocks immediately when each task finishes, not one
// accumulated dump at the end.
func TestLoop_EmitsPerTaskSummaryForEachCompletedTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    2,
			Completed:    0,
			Total:        2,
			NextTask:     "first task",
			NextID:       "ralph-t1",
			BackendLabel: "beads",
		},
	}

	callCount := 0
	runner := &stubRunner{
		result: claude.Result{SignalDetected: true, Summary: "implemented the fix"},
		onRun: func() {
			callCount++
			backend.Lock()
			if callCount == 1 {
				backend.NextID = "ralph-t2"
				backend.NextTask = "second task"
				backend.Completed = 1
				backend.Remaining = 1
			} else {
				backend.Completed = 2
				backend.Remaining = 0
			}
			backend.Unlock()
		},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
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

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	out := logBuf.String()
	count := strings.Count(out, "TASK COMPLETE")
	if count != 2 {
		t.Errorf("expected 2 TASK COMPLETE blocks (one per task), got %d\noutput:\n%s", count, out)
	}
	if !strings.Contains(out, "ralph-t1") {
		t.Errorf("expected first task ID ralph-t1 in output\noutput:\n%s", out)
	}
	if !strings.Contains(out, "ralph-t2") {
		t.Errorf("expected second task ID ralph-t2 in output\noutput:\n%s", out)
	}
	pos1 := strings.Index(out, "ralph-t1")
	pos2 := strings.Index(out, "ralph-t2")
	if pos1 < 0 || pos2 < 0 || pos1 >= pos2 {
		t.Errorf("expected first task block before second task block in output\noutput:\n%s", out)
	}
}

// onResumeDone sends no notifications when Notify is disabled — neither TaskCompleted nor TaskMerged.
func TestOnResumeDone_Merged_NotifyDisabled(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix logout",
				NextID:    "ralph-rd1",
			},
		},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		RemoteURL:  "https://github.com/owner/repo.git",
	})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		Notify:        false,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})
	l.runner = &stubRunner{}

	var buf bytes.Buffer
	prev := notify.SetCommandRunner(func(_ string, args ...string) error {
		for _, a := range args {
			buf.WriteString(a)
			buf.WriteByte(' ')
		}
		return nil
	})
	t.Cleanup(func() { notify.SetCommandRunner(prev) })

	l.onResumeDone(context.Background(), "ralph-rd1", "Fix logout", git.ResumeTaskResult{
		Handled:       true,
		AlreadyMerged: true,
		Merged:        true,
		PRNumber:      77,
	})

	got := buf.String()
	if strings.Contains(got, "Task done") {
		t.Errorf("expected no TaskCompleted when Notify=false, got %q", got)
	}
	if strings.Contains(got, "Task merged") {
		t.Errorf("expected no TaskMerged when Notify=false, got %q", got)
	}
}
