package loop

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/attempts"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verify"
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

			gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

			l := New(Config{
				Dirs: workctx.WorkContext{
					ProjectDir: dir,
					WorkDir:    dir,
					RalphDir:   ralphDir,
				},
				MaxIterations: 5,
				CallsPerHour:  80,
				TaskBackend:   tt.backend,
			}, st, gm, logger)

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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)
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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)
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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)
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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)
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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)
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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	var logBuf bytes.Buffer

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.NewWithWriter(&logBuf))

	// Override the runner to return different results per iteration.
	l.runner = &rateLimitStubRunner{
		backend: backend,
		counter: &iterationCount,
	}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }
	l.verifier.deps.LLMVerify = func(verify.VerifyOpts) verify.Result {
		return verify.Result{Passed: true}
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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		Version:       "1.2.3",
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)
	l.runner = runner

	_ = l.Run(context.Background())

	output := logBuf.String()
	if !strings.Contains(output, "Ralph v1.2.3") {
		t.Errorf("expected 'Ralph v1.2.3' in iteration banner, got:\n%s", output)
	}
}

// --- handleRunResult tests ---

// handleRunResultCall invokes the package-level handleRunResult using l's dependencies.
func handleRunResultCall(l *Loop, ctx context.Context, result claude.Result, runErr error, taskID, nextTask, headBefore string, runIteration int) loopAction {
	return handleRunResult(ctx, handleRunResultParams{
		result:              result,
		runErr:              runErr,
		taskID:              taskID,
		nextTask:            nextTask,
		headBefore:          headBefore,
		runIteration:        runIteration,
		isOnlineFunc:        l.cfg.IsOnline,
		waitForInternetFunc: l.cfg.WaitForInternet,
		logger:              l.logger,
		git:                 l.git,
		attempts:            l.attempts,
		limiter:             l.limiter,
		backend:             l.cfg.TaskBackend,
		state:               l.state,
		skipTask:            skipTask,
	})
}

// newHandleRunResultLoop creates a minimal Loop for testing handleRunResult.
func newHandleRunResultLoop(t *testing.T) (*Loop, string) {
	t.Helper()
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}, st, gm, logger)

	return l, ralphDir
}

// Verifies that when Claude returns an error and the machine is offline,
// handleRunResult waits for internet and returns actionRetry with decremented counters.
func TestHandleRunResult_OfflineReturnsRetry(t *testing.T) {
	l, _ := newHandleRunResultLoop(t)

	l.cfg.IsOnline = func() bool { return false }
	l.cfg.WaitForInternet = func(_ context.Context, _ *logging.Logger) bool { return true }

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
	l, _ := newHandleRunResultLoop(t)

	l.cfg.IsOnline = func() bool { return false }
	l.cfg.WaitForInternet = func(_ context.Context, _ *logging.Logger) bool { return false }

	runIter := 3
	action := handleRunResultCall(l, context.Background(), claude.Result{}, fmt.Errorf("connection refused"),
		"task-1", "Do stuff", "abc123", runIter)

	if action != actionDone {
		t.Fatalf("expected actionDone, got %d", action)
	}
}

// Verifies that the FeedbackKill path records an attempt and returns actionRetry
// with decremented counters.
func TestHandleRunResult_FeedbackKillReturnsRetry(t *testing.T) {
	l, ralphDir := newHandleRunResultLoop(t)

	l.cfg.IsOnline = func() bool { return true }

	runIter := 3
	result := claude.Result{FeedbackKill: true}
	action := handleRunResultCall(l, context.Background(), result, nil,
		"task-fk", "Feedback task", "abc123", runIter)

	if action != actionRetry {
		t.Fatalf("expected actionRetry, got %d", action)
	}

	tracker := attempts.New(ralphDir)
	history := tracker.Read("task-fk", "Feedback task")
	if !strings.Contains(history, "user feedback") {
		t.Errorf("expected attempt recorded with feedback context, got: %s", history)
	}
}

// Verifies that the IdleTimeout path records an attempt and returns actionRetry
// with decremented counters.
func TestHandleRunResult_IdleTimeoutReturnsRetry(t *testing.T) {
	l, ralphDir := newHandleRunResultLoop(t)

	l.cfg.IsOnline = func() bool { return true }

	runIter := 3
	result := claude.Result{IdleTimeout: true}
	action := handleRunResultCall(l, context.Background(), result, nil,
		"task-it", "Idle task", "abc123", runIter)

	if action != actionRetry {
		t.Fatalf("expected actionRetry, got %d", action)
	}

	tracker := attempts.New(ralphDir)
	history := tracker.Read("task-it", "Idle task")
	if !strings.Contains(history, "idle timeout") {
		t.Errorf("expected attempt recorded with idle timeout, got: %s", history)
	}
}

// Verifies that after MaxIdleTimeoutFailures consecutive idle timeouts,
// handleRunResult skips the task instead of retrying.
func TestHandleRunResult_IdleTimeoutSkipsAfterMaxFailures(t *testing.T) {
	l, ralphDir := newHandleRunResultLoop(t)

	l.cfg.IsOnline = func() bool { return true }

	backend := &testutil.StubBackend{}
	l.cfg.TaskBackend = backend

	tracker := attempts.New(ralphDir)
	for i := 0; i < attempts.MaxIdleTimeoutFailures-1; i++ {
		tracker.RecordIdleTimeoutFailure("task-it-max")
	}

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
	l, _ := newHandleRunResultLoop(t)

	l.cfg.IsOnline = func() bool { return true }

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
	l, _ := newHandleRunResultLoop(t)

	l.cfg.IsOnline = func() bool { return true }

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
	l, _ := newHandleRunResultLoop(t)

	l.cfg.IsOnline = func() bool { return true }

	runIter := 3
	action := handleRunResultCall(l, context.Background(), claude.Result{}, nil,
		"task-ok", "Normal task", "abc123", runIter)

	if action != actionProceed {
		t.Fatalf("expected actionProceed, got %d", action)
	}
}

// resolveByPRState sends TaskCompleted and TaskMerged when PR is already merged and Notify is enabled.
func TestResolveByPRState_Merged_NotifyEnabled(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:   "Fix login",
				NextID:    "ralph-rm1",
			},
		},
	}

	ghStub := git.NewStubGitHub()
	ghStub.PRState = "MERGED"
	gm := &testutil.StubGit{
		ProjectDir: dir,
		WorkDir:    dir,
		GitHubStub: ghStub,
	}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Notify:        true,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}

	var buf bytes.Buffer
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

	resolved := resolveByPRState(context.Background(), resolveByPRStateParams{
		taskID:   "ralph-rm1",
		nextTask: "Fix login",
		prNumber: 99,
		backend:  backend,
		git:      gm,
		logger:   l.logger,
		attempts: l.attempts,
		state:    l.state,
		notify:   l.cfg.Notify,
		ralphDir: ralphDir,
		verifier: l.verifier,
	})
	if !resolved {
		t.Fatal("expected resolveByPRState to return true for MERGED PR")
	}

	got := buf.String()
	if !strings.Contains(got, "Task done: [ralph-rm1] Fix login") {
		t.Errorf("expected TaskCompleted notification, got %q", got)
	}
	if !strings.Contains(got, "Task merged: [ralph-rm1] Fix login") {
		t.Errorf("expected TaskMerged notification, got %q", got)
	}
}

// resolveByPRState sends TaskCompleted (no TaskMerged) when PR is OPEN and Notify is enabled.
func TestResolveByPRState_Open_NotifyEnabled(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:   "Add cache",
				NextID:    "ralph-ro1",
			},
		},
	}

	branchName := "ralph-ro1/add-cache"
	ghStub2 := git.NewStubGitHub()
	ghStub2.PRHead = branchName
	gm := &testutil.StubGit{
		ProjectDir:          dir,
		WorkDir:             dir,
		RemoteBranchCommits: true,
		GitHubStub:          ghStub2,
	}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Notify:        true,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}

	var buf bytes.Buffer
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

	resolved := resolveByPRState(context.Background(), resolveByPRStateParams{
		taskID:   "ralph-ro1",
		nextTask: "Add cache",
		prNumber: 88,
		backend:  backend,
		git:      gm,
		logger:   l.logger,
		attempts: l.attempts,
		state:    l.state,
		notify:   l.cfg.Notify,
		ralphDir: ralphDir,
		verifier: l.verifier,
	})
	if !resolved {
		t.Fatal("expected resolveByPRState to return true for OPEN PR")
	}

	got := buf.String()
	if !strings.Contains(got, "Task done: [ralph-ro1] Add cache") {
		t.Errorf("expected TaskCompleted notification, got %q", got)
	}
	if strings.Contains(got, "Task merged") {
		t.Errorf("expected no TaskMerged notification for OPEN PR, got %q", got)
	}
}

// resolveByPRState does NOT send TaskCompleted when Notify is disabled, but still sends TaskMerged.
func TestResolveByPRState_Merged_NotifyDisabled(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:   "Fix logout",
				NextID:    "ralph-rd1",
			},
		},
	}

	ghStub3 := git.NewStubGitHub()
	ghStub3.PRState = "MERGED"
	gm := &testutil.StubGit{
		ProjectDir: dir,
		WorkDir:    dir,
		GitHubStub: ghStub3,
	}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Notify:        false,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}

	var buf bytes.Buffer
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

	resolved := resolveByPRState(context.Background(), resolveByPRStateParams{
		taskID:   "ralph-rd1",
		nextTask: "Fix logout",
		prNumber: 77,
		backend:  backend,
		git:      gm,
		logger:   l.logger,
		attempts: l.attempts,
		state:    l.state,
		notify:   l.cfg.Notify,
		ralphDir: ralphDir,
		verifier: l.verifier,
	})
	if !resolved {
		t.Fatal("expected resolveByPRState to return true for MERGED PR")
	}

	got := buf.String()
	if strings.Contains(got, "Task done") {
		t.Errorf("expected no TaskCompleted when Notify=false, got %q", got)
	}
	if !strings.Contains(got, "Task merged: [ralph-rd1] Fix logout") {
		t.Errorf("expected TaskMerged even when Notify=false, got %q", got)
	}
}
