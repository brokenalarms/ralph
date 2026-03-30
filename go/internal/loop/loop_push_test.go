package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies that pushAndCreatePR fires for every completed task when signal
// is detected, regardless of whether auto-merge is enabled. This ensures the
// Go code owns the push/PR lifecycle.
func TestLoop_PushAndCreatePROnSignal(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(10)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	pushPRCalls := 0
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

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			fname := fmt.Sprintf("task%d.go", iterationCount)
			os.WriteFile(filepath.Join(project, fname), []byte("package main\n"), 0o644)
			run(t, "git", "-C", project, "add", fname)
			run(t, "git", "-C", project, "commit", "-m", fmt.Sprintf("task %d", iterationCount))
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

	gm := &git.Manager{
		ProjectDir: project,
		BaseBranch: "main",
		WorkDir:    project,
		Logger:     logging.New(nil),
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    project,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     false,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.pushPRFunc = func(_ context.Context, _, taskDesc, _ string) (string, error) {
		pushPRCalls++
		return "", nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 2 signal-handler pushes (one per task) + 1 safety-net flush before exit.
	// PushAndCreatePR is idempotent, so the flush is harmless.
	if pushPRCalls != 3 {
		t.Errorf("expected pushAndCreatePR called 3 times (2 signal + 1 flush), got %d", pushPRCalls)
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

	pushPRCalls := 0

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "some task",
		NextID:    "ralph-xyz",
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: false},
	}

	gm := &git.Manager{
		ProjectDir: dir,
		BaseBranch: "main",
		WorkDir:    dir,
		Logger:     logging.New(nil),
	}

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
	l.pushPRFunc = func(_ context.Context, _, taskDesc, _ string) (string, error) {
		pushPRCalls++
		return "", nil
	}
	l.mergeFunc = func(context.Context) (bool, error) {
		t.Error("auto-merge should not be called without signal")
		return false, nil
	}

	_ = l.Run(context.Background())

	if pushPRCalls != 0 {
		t.Errorf("pushAndCreatePR should not be called without signal, got %d calls", pushPRCalls)
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

	gm := &git.Manager{
		ProjectDir:     dir,
		BaseBranch: "main",
		RalphDir:       ralphDir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/test/01-task",
		State:          st,
		Logger:         logging.New(nil),
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

	pushCalled := false
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) {
		pushCalled = true
		return "", nil
	}

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true, OnSignalUsed: true},
	}

	l.Run(context.Background())

	if !pushCalled {
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
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger, BaseBranch: "main"}

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

	var pushCalls int
	l.pushPRFunc = func(_ context.Context, taskID, taskDesc, _ string) (string, error) {
		pushCalls++
		return "", nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Push must be called: once during signal handling AND once as a flush
	// before exit. PushAndCreatePR is idempotent (returns early when no new
	// commits), so the safety-net call is harmless if the first succeeded.
	if pushCalls < 2 {
		t.Errorf("expected pushPRFunc called at least 2 times (signal + flush), got %d", pushCalls)
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
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger, BaseBranch: "main"}

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

	var pushCalls int
	l.pushPRFunc = func(_ context.Context, taskID, taskDesc, _ string) (string, error) {
		pushCalls++
		return "", nil
	}

	// Cancel after entering wait to prevent hanging.
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if pushCalls < 2 {
		t.Errorf("expected pushPRFunc called at least 2 times (signal + flush), got %d", pushCalls)
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
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger, BaseBranch: "main"}

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

	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) { return "", nil }

	var mergeCalls int
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mergeCalls == 0 {
		t.Error("expected mergeFunc called during flush before exit, got 0 calls")
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
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger, BaseBranch: "main"}

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

	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) { return "", nil }

	var mergeCalls int
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		return true, nil
	}

	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mergeCalls == 0 {
		t.Error("expected mergeFunc called during flush before wait, got 0 calls")
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
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger, BaseBranch: "main"}

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

	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) { return "", nil }

	var mergeCalls int
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mergeCalls != 0 {
		t.Errorf("expected mergeFunc NOT called when AutoMerge disabled, got %d calls", mergeCalls)
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
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger, BaseBranch: "main"}

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

	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) { return "", nil }

	var mergeCalls int
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Merge fires once in the signal handler. The flush must not merge again
	// because lastTaskMerged is set.
	if mergeCalls != 1 {
		t.Errorf("expected exactly 1 merge (signal handler only), got %d", mergeCalls)
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
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger, BaseBranch: "main"}

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

	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) { return "", nil }

	var mergeCalls int
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Signal handler didn't fire, so merge only happens during flush.
	if mergeCalls != 1 {
		t.Errorf("expected exactly 1 merge (flush only), got %d", mergeCalls)
	}
}
