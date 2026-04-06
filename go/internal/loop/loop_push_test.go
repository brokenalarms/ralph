package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies that pushAndCreatePR fires for every completed task when signal
// is detected, regardless of whether auto-merge is enabled. This ensures the
// Go code owns the push/PR lifecycle.
func TestLoop_PushAndCreatePROnSignal(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 1,
			Completed: 0,
			Total:     2,
			NextTask:  "task A",
			NextID:    "ralph-aaa",
		},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			gm.HeadRevValue = fmt.Sprintf("commit%d", iterationCount)
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

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     false,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 2 signal-handler pushes (one per task) + 1 safety-net flush before exit.
	// Ship is idempotent, so the flush is harmless.
	if gm.ShipCalls+gm.FlushUnpushedCalls != 3 {
		t.Errorf("expected Ship+Flush called 3 times (2 signal + 1 flush), got Ship=%d Flush=%d", gm.ShipCalls, gm.FlushUnpushedCalls)
	}
}

// Verifies that pushAndCreatePR is NOT called when Claude exits without
// signaling completion (e.g. idle timeout or crash), preventing half-done
// work from being pushed.
func TestLoop_NoPushPRWithoutSignal(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "some task",
		NextID:    "ralph-xyz",
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: false},
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
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	if gm.ShipCalls != 0 {
		t.Errorf("Ship should not be called without signal, got %d calls", gm.ShipCalls)
	}
}

// Verifies that push is called after signal detection. The sync guard
// (fetch + rebase) is enforced internally by PushAndCreatePR's EnsureUpToDate
// — tested in git module.
func TestLoop_PushCalledAfterSignal(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/test/01-task",
	}

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Fix the bug",
		NextID:    "ralph-fix1",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true, OnSignalUsed: true},
	}

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	l.Run(context.Background())

	if gm.ShipCalls == 0 {
		t.Error("expected push to be called after signal detection")
	}
}

// When the last task completes and no tasks remain, the loop should still
// push and create a PR before exiting — not silently drop unpushed work.
func TestLoop_FlushesUnpushedWorkBeforeExit(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "last task",
			NextID:       "ralph-last",
			BackendLabel: "beads",
		},
	}

	logger := logging.New(nil)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		onRun: func() {
			// Simulate the task completing — no remaining tasks after this.
			backend.Lock()
			backend.Remaining = 0
			backend.Completed = 1
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Wait:          false,
	}, st, gm, logger)
	l.runner = runner

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Ship must be called during signal handling AND FlushUnpushedWork as a
	// safety net before exit. Both are idempotent, so the flush is harmless.
	if gm.ShipCalls+gm.FlushUnpushedCalls < 2 {
		t.Errorf("expected Ship+Flush called at least 2 times (signal + flush), got Ship=%d Flush=%d", gm.ShipCalls, gm.FlushUnpushedCalls)
	}
}

// When the last task completes and --wait is set, the loop should flush
// unpushed work before entering wait mode, not lose it.
func TestLoop_FlushesUnpushedWorkBeforeWait(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "last task",
			NextID:       "ralph-last",
			BackendLabel: "beads",
		},
	}

	logger := logging.New(nil)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Remaining = 0
			backend.Completed = 1
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	ctx, cancel := context.WithCancel(context.Background())

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Wait:          true,
	}, st, gm, logger)
	l.runner = runner
	l.cfg.IsOnline = func() bool { return true }
	l.cfg.WaitForInternet = func(context.Context, *logging.Logger) bool { return true }
	l.cfg.CheckGitHub = func(context.Context) error { return nil }

	waitEntered := make(chan struct{}, 1)
	l.cfg.OnWait = func() { waitEntered <- struct{}{} }

	go func() {
		<-waitEntered
		cancel()
	}()

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gm.ShipCalls+gm.FlushUnpushedCalls < 2 {
		t.Errorf("expected Ship+Flush called at least 2 times (signal + flush), got Ship=%d Flush=%d", gm.ShipCalls, gm.FlushUnpushedCalls)
	}
}

// When AutoMerge is enabled, flushUnpushedWork must squash-merge after
// pushing — same flow as every other task, no special case for last task.
func TestLoop_FlushSquashMergesBeforeExit(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "last task",
			NextID:       "ralph-last",
			BackendLabel: "beads",
		},
	}

	logger := logging.New(nil)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Remaining = 0
			backend.Completed = 1
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		AutoMerge:     true,
		Wait:          false,
	}, st, gm, logger)
	l.runner = runner

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gm.FlushUnpushedCalls == 0 || !gm.LastFlushAutoMerge {
		t.Error("expected FlushUnpushedWork called with autoMerge=true during flush before exit")
	}
}

// When AutoMerge is enabled and --wait is set, flushUnpushedWork must
// squash-merge before entering wait mode.
func TestLoop_FlushSquashMergesBeforeWait(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "last task",
			NextID:       "ralph-last",
			BackendLabel: "beads",
		},
	}

	logger := logging.New(nil)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Remaining = 0
			backend.Completed = 1
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	ctx, cancel := context.WithCancel(context.Background())

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		AutoMerge:     true,
		Wait:          true,
	}, st, gm, logger)
	l.runner = runner
	l.cfg.IsOnline = func() bool { return true }
	l.cfg.WaitForInternet = func(context.Context, *logging.Logger) bool { return true }
	l.cfg.CheckGitHub = func(context.Context) error { return nil }

	waitEntered := make(chan struct{}, 1)
	l.cfg.OnWait = func() { waitEntered <- struct{}{} }

	go func() {
		<-waitEntered
		cancel()
	}()

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gm.FlushUnpushedCalls == 0 || !gm.LastFlushAutoMerge {
		t.Error("expected FlushUnpushedWork called with autoMerge=true during flush before wait")
	}
}

// When AutoMerge is disabled, flushUnpushedWork must NOT attempt to merge.
func TestLoop_FlushSkipsMergeWhenAutoMergeDisabled(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "last task",
			NextID:       "ralph-last",
			BackendLabel: "beads",
		},
	}

	logger := logging.New(nil)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Remaining = 0
			backend.Completed = 1
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		AutoMerge:     false,
		Wait:          false,
	}, st, gm, logger)
	l.runner = runner

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gm.FlushUnpushedCalls == 0 {
		t.Error("expected FlushUnpushedWork to be called")
	}
	if gm.LastFlushAutoMerge {
		t.Error("expected FlushUnpushedWork called with autoMerge=false when AutoMerge disabled")
	}
}

// When the signal handler already merged the last task, the flush safety net
// must not merge again — otherwise multi-task runs get an extra merge call.
func TestLoop_FlushSkipsMergeWhenAlreadyMerged(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "last task",
			NextID:       "ralph-last",
			BackendLabel: "beads",
		},
	}

	logger := logging.New(nil)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Remaining = 0
			backend.Completed = 1
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		AutoMerge:     true,
		Wait:          false,
	}, st, gm, logger)
	l.runner = runner
	gm.ShipResult = git.ShipResult{PRNumber: 999}
	gm.MergeRetryResult = true

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// MergeWithRetry fires once in the signal handler. The flush must not
	// merge again because lastTaskMerged is set.
	if gm.MergeRetryCalls != 1 {
		t.Errorf("expected exactly 1 merge (signal handler only), got %d", gm.MergeRetryCalls)
	}
	if gm.LastFlushAutoMerge {
		t.Error("expected flush to skip merge (autoMerge=false) when last task already merged")
	}
}

// When the agent exits without a signal, the flush safety net must still
// push and merge so the last task's work is not lost.
func TestLoop_FlushMergesWhenSignalNotDetected(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "last task",
			NextID:       "ralph-last",
			BackendLabel: "beads",
		},
	}

	logger := logging.New(nil)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Remaining = 0
			backend.Completed = 1
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: false},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		AutoMerge:     true,
		Wait:          false,
	}, st, gm, logger)
	l.runner = runner

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Signal handler didn't fire, so merge only happens during flush.
	if gm.FlushUnpushedCalls != 1 || !gm.LastFlushAutoMerge {
		t.Errorf("expected exactly 1 flush with autoMerge=true, got FlushCalls=%d LastAutoMerge=%v", gm.FlushUnpushedCalls, gm.LastFlushAutoMerge)
	}
}
