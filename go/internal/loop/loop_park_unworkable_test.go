package loop

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

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
