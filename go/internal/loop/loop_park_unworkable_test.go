package loop

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies that when a result has both RateLimited and IdleTimeout set (the
// belt-and-suspenders case — claude.Runner's post-exit fallback normally
// clears IdleTimeout when it reclassifies a run as RateLimited, but
// handleRunResult must not depend on that), the RateLimited branch takes
// precedence: the run waits and retries without incrementing
// taskIdleTimeouts or calling skipTask.
func TestHandleRunResult_RateLimitedTakesPrecedenceOverIdleTimeout(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	backend := &testutil.StubBackend{}
	l.taskBackend = backend
	l.currentTaskID = "task-both"
	l.taskIdleTimeouts = l.maxIdleTimeoutFailures() - 1

	resetAt := time.Now().Add(-1 * time.Second)
	result := claude.Result{RateLimited: true, IdleTimeout: true, ResetAt: resetAt}
	action := handleRunResultCall(l, context.Background(), result, nil,
		"task-both", "Throttled task", "abc123", 3)

	if action != actionRetry {
		t.Fatalf("expected actionRetry, got %d", action)
	}
	if l.taskIdleTimeouts != l.maxIdleTimeoutFailures()-1 {
		t.Errorf("expected taskIdleTimeouts unchanged at %d, got %d", l.maxIdleTimeoutFailures()-1, l.taskIdleTimeouts)
	}
	if backend.SkippedTask != "" {
		t.Errorf("expected skipTask not called, got skipped task %q", backend.SkippedTask)
	}
}

// Verifies criterion 4 (existing behavior preserved): a genuine idle timeout
// with NO throttle evidence — RateLimited is false, so the RateLimited branch
// that now precedes the IdleTimeout branch is skipped — still increments
// taskIdleTimeouts and skips the task via SkipIdleTimeout once the count
// reaches maxIdleTimeoutFailures. The git stub reports a landed commit so the
// failed-start cap is bypassed and the idle-timeout cap is the branch that
// fires.
func TestHandleRunResult_GenuineIdleTimeoutIncrementsAndSkips(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())
	l.git = git.NewStub(git.StubRepoConfig{LogOnelineResult: "abc123 commit landed"})

	backend := &testutil.StubBackend{}
	l.taskBackend = backend
	l.currentTaskID = "task-idle-genuine"
	l.taskIdleTimeouts = l.maxIdleTimeoutFailures() - 1

	result := claude.Result{IdleTimeout: true}
	action := handleRunResultCall(l, context.Background(), result, nil,
		"task-idle-genuine", "Genuinely idle task", "abc123", 3)

	if action != actionRetry {
		t.Fatalf("expected actionRetry, got %d", action)
	}
	if l.taskIdleTimeouts != l.maxIdleTimeoutFailures() {
		t.Errorf("expected taskIdleTimeouts incremented to %d, got %d", l.maxIdleTimeoutFailures(), l.taskIdleTimeouts)
	}
	if backend.SkippedTask != "task-idle-genuine" {
		t.Errorf("expected task skipped, got %q", backend.SkippedTask)
	}
	if backend.SkipReason != string(tasks.SkipIdleTimeout) {
		t.Errorf("expected skip reason %q, got %q", tasks.SkipIdleTimeout, backend.SkipReason)
	}
}

// Proves: a Compacted result parks the task even when the task has open
// dependents — the strand guard is removed, so open deps never block parking.
func TestHandleRunResult_CompactedSkipsTask_WithOpenDependents(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	backend := &testutil.StubBackend{
		OpenDependents: []string{"dep-1"},
	}
	l.taskBackend = backend

	action := handleRunResultCall(l, context.Background(), claude.Result{Compacted: true}, nil,
		"task-cmp-dep", "Compacting task with dep", "abc123", 1)

	if action != actionSkip {
		t.Fatalf("expected actionSkip, got %d", action)
	}
	if backend.SkippedTask != "task-cmp-dep" {
		t.Errorf("expected task parked despite open dependents, got %q", backend.SkippedTask)
	}
}

// Proves: when a task has already failed twice with no commits (persistent
// metadata counter), the next idle timeout parks it instead of retrying —
// the counter survives loop restarts by living in bead metadata. Open
// dependents must not block parking (strand guard removed, AC5).
func TestHandleRunResult_IdleTimeout_PersistentCountParksAfterTwo(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			OpenDependents: []string{"dep-1"},
		},
		Metadata: map[string]map[string]string{
			"task-persistent": {"failed_starts": "1"},
		},
	}
	l.taskBackend = backend

	result := claude.Result{IdleTimeout: true}
	action := handleRunResultCall(l, context.Background(), result, nil,
		"task-persistent", "Idle task (2nd attempt)", "abc123", 1)

	if action != actionRetry {
		t.Fatalf("expected actionRetry (park-and-move-on path), got %d", action)
	}
	if backend.StubBackend.SkippedTask != "task-persistent" {
		t.Errorf("expected task parked after 2nd persistent failure despite open dependents, got %q", backend.StubBackend.SkippedTask)
	}
}

// Proves: wall-clock timeout uses the same persistent counter as idle timeout —
// a task with failed_starts=1 in metadata is parked on the 2nd wall-clock
// timeout (no commits), even when it has open dependents (AC3, AC5).
func TestHandleRunResult_WallClockTimeout_PersistentCountParksAfterTwo(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			OpenDependents: []string{"dep-1"},
		},
		Metadata: map[string]map[string]string{
			"task-wallclock": {"failed_starts": "1"},
		},
	}
	l.taskBackend = backend

	result := claude.Result{WallClockTimeout: true}
	action := handleRunResultCall(l, context.Background(), result, nil,
		"task-wallclock", "Wall-clock task (2nd attempt)", "abc123", 1)

	if action != actionRetry {
		t.Fatalf("expected actionRetry (park-and-move-on path), got %d", action)
	}
	if backend.StubBackend.SkippedTask != "task-wallclock" {
		t.Errorf("expected task parked after 2nd wall-clock timeout despite open dependents, got %q", backend.StubBackend.SkippedTask)
	}
}

// Proves: the first wall-clock timeout with no commits does NOT park — the task
// is given one retry before the persistent cap fires on the 2nd failure (AC3).
func TestHandleRunResult_WallClockTimeout_PersistentCountRetryOnFirst(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{},
	}
	l.taskBackend = backend

	result := claude.Result{WallClockTimeout: true}
	action := handleRunResultCall(l, context.Background(), result, nil,
		"task-wallclock-first", "Wall-clock task (1st attempt)", "abc123", 1)

	if action != actionRetry {
		t.Fatalf("expected actionRetry, got %d", action)
	}
	if backend.StubBackend.SkippedTask != "" {
		t.Errorf("expected no skip on 1st wall-clock failure, but got SkippedTask=%q", backend.StubBackend.SkippedTask)
	}
	v, _ := backend.GetMetadata("task-wallclock-first", "failed_starts")
	if v != "1" {
		t.Errorf("expected failed_starts=1 in metadata after 1st wall-clock timeout, got %q", v)
	}
}

// Proves: the first idle timeout with no commits does NOT park — the task is
// given one retry before the persistent cap fires on the 2nd failure.
func TestHandleRunResult_IdleTimeout_PersistentCountRetryOnFirst(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{},
	}
	l.taskBackend = backend

	result := claude.Result{IdleTimeout: true}
	action := handleRunResultCall(l, context.Background(), result, nil,
		"task-first-timeout", "Idle task (1st attempt)", "abc123", 1)

	if action != actionRetry {
		t.Fatalf("expected actionRetry, got %d", action)
	}
	// First failure → count = 1 → retry, NOT skip.
	if backend.StubBackend.SkippedTask != "" {
		t.Errorf("expected no skip on 1st failure, but got SkippedTask=%q", backend.StubBackend.SkippedTask)
	}
	// Verify the counter was persisted.
	v, _ := backend.GetMetadata("task-first-timeout", "failed_starts")
	if v != "1" {
		t.Errorf("expected failed_starts=1 in metadata, got %q", v)
	}
}

// Proves: MaxFailedStarts=3 in config allows 2 failed starts before parking —
// a task with failed_starts=1 in metadata is NOT parked on the 2nd attempt
// (count=2 < 3), only on the 3rd.
func TestMaxFailedStarts_ConfigOverridesDefault(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())
	l.cfg.MaxFailedStarts = 3

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{},
		Metadata: map[string]map[string]string{
			"task-cfg-cap": {"failed_starts": "1"},
		},
	}
	l.taskBackend = backend

	result := claude.Result{IdleTimeout: true}
	action := handleRunResultCall(l, context.Background(), result, nil,
		"task-cfg-cap", "Task with custom failed-start cap", "abc123", 1)

	if action != actionRetry {
		t.Fatalf("expected actionRetry (count=2 < MaxFailedStarts=3), got %d", action)
	}
	if backend.StubBackend.SkippedTask != "" {
		t.Errorf("expected no skip at 2nd attempt with MaxFailedStarts=3, got %q", backend.StubBackend.SkippedTask)
	}
}

// Proves: MaxCompactionParks=2 in config allows one compaction retry before
// parking — the task is not parked on the first compaction (count=1 < 2),
// and compaction_parks=1 is persisted for the next attempt.
func TestMaxCompactionParks_ConfigOverridesDefault(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())
	l.cfg.MaxCompactionParks = 2

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{},
	}
	l.taskBackend = backend

	action := handleRunResultCall(l, context.Background(), claude.Result{Compacted: true}, nil,
		"task-compact", "Compacting task", "abc123", 1)

	if backend.StubBackend.SkippedTask != "" {
		t.Errorf("expected no skip on first compaction with MaxCompactionParks=2, got %q", backend.StubBackend.SkippedTask)
	}
	v, _ := backend.GetMetadata("task-compact", "compaction_parks")
	if v != "1" {
		t.Errorf("expected compaction_parks=1 in metadata, got %q", v)
	}
	if action != actionRetry {
		t.Fatalf("expected actionRetry on first compaction below cap, got %d", action)
	}
}

// Proves the ralph-qlmy non-retryable run-count guarantee: when compaction
// (park cap=1) already caused a skip on one task, a second task hitting
// compaction halts the whole loop app-wide on its very first run — 1 run on
// task A + 1 run on task B, 2 total, never a third.
func TestHandleRunResult_CompactionSameReason_SecondTaskHaltsOnFirstRun(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	backend := &testutil.MutableBackend{StubBackend: testutil.StubBackend{}}
	l.taskBackend = backend

	actionA := handleRunResultCall(l, context.Background(), claude.Result{Compacted: true}, nil,
		"task-a", "Task A", "abc123", 1)
	if actionA != actionSkip {
		t.Fatalf("task A: expected actionSkip, got %d", actionA)
	}
	if backend.StubBackend.SkippedTask != "task-a" {
		t.Fatalf("task A: expected task-a parked, got %q", backend.StubBackend.SkippedTask)
	}

	actionB := handleRunResultCall(l, context.Background(), claude.Result{Compacted: true}, nil,
		"task-b", "Task B", "abc123", 1)
	if actionB != actionDone {
		t.Fatalf("task B: expected actionDone (app-wide halt on first run), got %d", actionB)
	}
	if backend.StubBackend.SkippedTask != "task-b" {
		t.Errorf("task B: expected task-b also parked before halting, got %q", backend.StubBackend.SkippedTask)
	}

	status, _ := l.state.Read("status")
	if status != "halted_app_wide:"+string(tasks.SkipCompaction) {
		t.Errorf("expected status halted_app_wide:%s, got %q", tasks.SkipCompaction, status)
	}
}

// Proves the ralph-qlmy retryable run-count guarantee: when failed_start
// (in-task cap=2) already caused a skip on one task (after burning its full
// 2-run retry bracket), a second task hitting the same reason halts the
// whole loop app-wide on its very first run instead of also burning its own
// 2-run bracket — 2 runs on task A + 1 run on task B, 3 total, never a
// fourth (AC2: "without that later task re-running its in-task retry
// bracket").
func TestHandleRunResult_FailedStartSameReason_SecondTaskHaltsOnFirstRun(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	backend := &testutil.MutableBackend{StubBackend: testutil.StubBackend{}}
	l.taskBackend = backend

	result := claude.Result{IdleTimeout: true}

	// Task A: 1st run below cap → retry, no skip yet.
	actionA1 := handleRunResultCall(l, context.Background(), result, nil,
		"task-a", "Task A (1st attempt)", "abc123", 1)
	if actionA1 != actionRetry {
		t.Fatalf("task A run 1: expected actionRetry, got %d", actionA1)
	}
	if backend.StubBackend.SkippedTask != "" {
		t.Fatalf("task A run 1: expected no skip yet, got %q", backend.StubBackend.SkippedTask)
	}

	// Task A: 2nd run reaches the cap → skip, recording the streak reason.
	actionA2 := handleRunResultCall(l, context.Background(), result, nil,
		"task-a", "Task A (2nd attempt)", "abc123", 2)
	if actionA2 != actionRetry {
		t.Fatalf("task A run 2: expected actionRetry (skip-and-move-on path), got %d", actionA2)
	}
	if backend.StubBackend.SkippedTask != "task-a" {
		t.Fatalf("task A run 2: expected task-a parked, got %q", backend.StubBackend.SkippedTask)
	}

	// Task B: 1st run already matches the streak reason — halt immediately,
	// never reaching a 2nd run for task B.
	actionB1 := handleRunResultCall(l, context.Background(), result, nil,
		"task-b", "Task B (1st attempt)", "abc123", 1)
	if actionB1 != actionDone {
		t.Fatalf("task B run 1: expected actionDone (app-wide halt), got %d", actionB1)
	}
	if backend.StubBackend.SkippedTask != "task-b" {
		t.Errorf("task B run 1: expected task-b also parked before halting, got %q", backend.StubBackend.SkippedTask)
	}

	status, _ := l.state.Read("status")
	if status != "halted_app_wide:"+string(tasks.SkipFailedStart) {
		t.Errorf("expected status halted_app_wide:%s, got %q", tasks.SkipFailedStart, status)
	}
}

// Proves AC3: a different skip reason does not trigger the same-reason
// app-wide halt. Task A skips on compaction (park cap=1); task B, hitting an
// unrelated idle timeout with no commits, independently runs its own
// failed_start retry bracket (cap=2) to completion without being
// short-circuited, since failed_start_limit_reached != compaction_detected.
func TestHandleRunResult_MixedReasonSkips_NoAppWideHalt(t *testing.T) {
	l, _ := newHandleRunResultLoop(t, onlineStubConnectivity())

	backend := &testutil.MutableBackend{StubBackend: testutil.StubBackend{}}
	l.taskBackend = backend

	actionA := handleRunResultCall(l, context.Background(), claude.Result{Compacted: true}, nil,
		"task-a", "Task A", "abc123", 1)
	if actionA != actionSkip {
		t.Fatalf("task A: expected actionSkip, got %d", actionA)
	}

	result := claude.Result{IdleTimeout: true}
	for i := 1; i <= 2; i++ {
		action := handleRunResultCall(l, context.Background(), result, nil,
			"task-b", "Task B", "abc123", i)
		if action == actionDone {
			t.Fatalf("task B run %d: unexpected app-wide halt for a differing reason", i)
		}
	}

	if backend.StubBackend.SkippedTask != "task-b" {
		t.Errorf("expected task-b parked after its own 2-failure failed_start bracket, got %q", backend.StubBackend.SkippedTask)
	}

	status, _ := l.state.Read("status")
	if strings.Contains(status, "app_wide") {
		t.Errorf("expected no app-wide halt for mixed reasons, got status %q", status)
	}
}

// Proves: a task that always idle-times-out is parked after 2 attempts and the
// loop continues — exercising l.Run(), not just handleRunResult. The task has
// an open dependent to prove the strand guard is removed (AC5 from ralph-n4u3).
func TestLoop_IdleTimeoutTask_ParksAfterTwoAttempts(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:      1,
				Total:          1,
				NextID:         "task-idle-dep",
				NextTask:       "Task that always idle-times-out",
				OpenDependents: []string{"task-dep-1"},
			},
		},
	}

	var runCount atomic.Int32
	runner := &stubRunner{
		onRun: func() {
			n := runCount.Add(1)
			if n >= 2 {
				backend.Lock()
				backend.Remaining = 0
				backend.Unlock()
			}
		},
		result: claude.Result{IdleTimeout: true},
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

	if runCount.Load() < 2 {
		t.Fatalf("expected agent to run at least 2 times, got %d", runCount.Load())
	}

	backend.SkipMu.Lock()
	skippedIDs := append([]string(nil), backend.SkippedIDs...)
	skipReasons := append([]string(nil), backend.SkipReasons...)
	backend.SkipMu.Unlock()
	if len(skippedIDs) == 0 {
		t.Fatal("expected task to be parked (SkipTask called) after 2 idle timeouts despite open dependent, but no tasks were skipped")
	}
	if skippedIDs[0] != "task-idle-dep" {
		t.Errorf("expected SkipTask(task-idle-dep), got %q", skippedIDs[0])
	}
	if skipReasons[0] != "failed_start_limit_reached" {
		t.Errorf("expected skip reason 'failed_start_limit_reached', got %q", skipReasons[0])
	}

	status, _ := st.Read("status")
	if strings.HasPrefix(status, "halted_skip_would_strand_dependents") {
		t.Errorf("loop halted with strand error even though strand guard is removed; got %q", status)
	}
	if strings.HasPrefix(status, "halted_cascade_skipped") {
		t.Errorf("loop halted with cascade skip from a single task; got %q", status)
	}
}
