package loop

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/attempts"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verify"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies that when verification fails (e.g. tests don't pass), the task
// is NOT closed — it's recorded as a failed attempt so the next iteration
// can retry, preventing ralph from falsely closing beads.
func TestLoop_VerificationFailureBlocksClose(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.StubBackend{
		Remaining:    1,
		Total:        1,
		NextTask:     "fix the bug",
		NextID:       "ralph-bug",
		BackendLabel: "beads",
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: true, Summary: "fixed it"},
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, BaseBranch: "main"}

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
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.verifyFunc = func(context.Context, string, string) (bool, string) {
		return false, "test suite failed"
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) {
		t.Error("push should not be called when verification fails")
		return "", nil
	}

	_ = l.Run(context.Background())

	// Task should NOT be recorded as completed
	if _, err := os.Stat(filepath.Join(ralphDir, ".completed-tasks")); !os.IsNotExist(err) {
		t.Error("task should not be recorded as completed when verification fails")
	}

	// Attempt should be recorded
	history := l.attempts.Read("ralph-bug", "fix the bug")
	if history == "" {
		t.Error("expected a failed attempt to be recorded")
	}
	if !strings.Contains(history, "verification failed") {
		t.Errorf("attempt should mention verification failure, got: %s", history)
	}
}

// Verifies that when verification passes, the normal completion flow
// proceeds — task is closed, PR is pushed, and completed-tasks is recorded.
func TestLoop_VerificationPassAllowsClose(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	pushCalled := false

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "add feature",
			NextID:       "ralph-feat",
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
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, BaseBranch: "main"}

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
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.verifyFunc = func(context.Context, string, string) (bool, string) {
		return true, ""
	}
	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) {
		pushCalled = true
		return "", nil
	}

	_ = l.Run(context.Background())

	if !pushCalled {
		t.Error("push should be called when verification passes")
	}

	data, err := os.ReadFile(filepath.Join(ralphDir, ".completed-tasks"))
	if err != nil {
		t.Fatalf("expected .completed-tasks file: %v", err)
	}
	if !strings.Contains(string(data), "ralph-feat") {
		t.Errorf("expected ralph-feat in completed tasks, got: %s", string(data))
	}
}

// Verifies that the default behavior (no VerifyDir set, no verifyFunc)
// allows tasks to close without verification, preserving backwards
// compatibility for projects that opt out.
func TestLoop_NoVerificationByDefault(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	pushCalled := false

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "simple task",
			NextID:       "ralph-simple",
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

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, BaseBranch: "main"}

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
		// VerifyDir deliberately not set
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) {
		pushCalled = true
		return "", nil
	}

	_ = l.Run(context.Background())

	if !pushCalled {
		t.Error("push should be called when no verification is configured")
	}
}

// When merge fails with a CI error, the loop leaves the task open for retry.
// CI fix agent spawning during the merge pipeline is tested in git module
// (TestMergeWithRetry_DelegatesCIFailure).
// When CI fails, the task is still closed because the PR exists — merge
// is a separate concern from task completion.
func TestLoop_CIFailureStillClosesTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Fix CI failure",
		NextID:    "ralph-ci1",
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		BaseBranch: "main",
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-ci-test",
		State:          st,
		Logger:         logging.New(nil),
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
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "99", nil }
	l.mergeFunc = func(context.Context) (bool, error) {
		return false, &git.CIFailureError{
			PRNumber: "99",
			Failures: []git.CICheckResult{
				{Name: "test", State: "FAILURE", Bucket: "fail"},
			},
		}
	}

	_ = l.Run(context.Background())

	if backend.SkippedTask != "ralph-ci1" {
		t.Errorf("expected ralph-ci1 deferred in backend, got %q", backend.SkippedTask)
	}
}

// When mergeFunc succeeds, the loop closes the task and records the merge.
// Conflict recovery (rebase + force-push + retry) is tested in git module
// (TestMergeWithRetry_RecoversFromConflict).
func TestLoop_MergeSuccessClosesTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Add feature",
		NextID:    "ralph-mc1",
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		BaseBranch: "main",
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-conflict-test",
		State:          st,
		Logger:         logging.New(nil),
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
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "42", nil }

	merged := false
	l.mergeFunc = func(context.Context) (bool, error) {
		merged = true
		return true, nil
	}

	_ = l.Run(context.Background())

	if !merged {
		t.Error("expected merge to be called when AutoMerge is enabled")
	}
}

// When mergeFunc eventually succeeds (simulating CI fix + retry), the loop
// closes the task. CI fix agent spawning and retry logic are tested in git
// module (TestMergeWithRetry_DelegatesCIFailure).
func TestLoop_MergeEventualSuccessClosesTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Fix CI failure",
		NextID:    "ralph-ci2",
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		BaseBranch: "main",
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-ci-fix",
		State:          st,
		Logger:         logging.New(nil),
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
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}
	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) { return "", nil }

	l.mergeFunc = func(context.Context) (bool, error) {
		return true, nil
	}

	_ = l.Run(context.Background())

	tasks := l.SessionTasks()
	if len(tasks) == 0 {
		t.Error("expected at least one completed task after successful merge")
	}
}

// When the CI fix agent fails twice (CI keeps failing), the loop gives up
// after the maximum retry count rather than looping forever.
func TestLoop_CIFailureExhaustsRetries(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Retry exhaustion",
		NextID:    "ralph-ci3",
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		BaseBranch: "main",
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-ci-exhaust",
		State:          st,
		Logger:         logging.New(nil),
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
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}
	l.newRunnerFunc = func() claudeRunner {
		return &stubRunner{result: claude.Result{SignalDetected: true}}
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

	// mergeFunc returning error means merge pipeline failed (retry exhaustion
	// is tested in git module: TestMergeWithRetry_ExhaustsRetries).
	l.mergeFunc = func(context.Context) (bool, error) {
		return false, &git.CIFailureError{
			PRNumber: "99",
			Failures: []git.CICheckResult{
				{Name: "test", State: "FAILURE", Bucket: "fail"},
			},
		}
	}

	_ = l.Run(context.Background())

	// Task should remain open since merge failed.
	s, _ := st.Load()
	if s.Status == "completed" {
		t.Error("expected status not to be 'completed' when merge fails")
	}
}

// When mergeFunc returns an error, the loop does not close the task —
// ensuring failed merges leave the task open for retry. The combined
// conflict+CI retry pipeline is tested in git module
// (TestMergeWithRetry_RecoversFromConflict, TestMergeWithRetry_DelegatesCIFailure).
func TestLoop_MergeFailureLeavesTaskOpen(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Mixed errors",
		NextID:    "ralph-mixed",
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		BaseBranch: "main",
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-mixed",
		State:          st,
		Logger:         logging.New(nil),
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
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "99", nil }

	l.mergeFunc = func(context.Context) (bool, error) {
		return false, fmt.Errorf("merge failed")
	}

	var buf bytes.Buffer
	logger := logging.New(&buf)
	l.logger = logger

	_ = l.Run(context.Background())

	output := buf.String()
	if !strings.Contains(output, "Auto-merge") {
		t.Errorf("expected log output to contain 'Auto-merge', got: %s", output)
	}
}

// Verifies that after MaxMergeFailures consecutive merge failures, the loop
// skips the task instead of retrying indefinitely. Merge failures are tracked
// across iterations via the attempts tracker.
// Merge failures no longer cause task skipping — the PR exists, work is done.
func TestLoop_MergeFailureStillClosesTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "Stubborn task",
				NextID:       "ralph-stub",
				BackendLabel: "beads",
			},
		},
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		BaseBranch: "main",
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-stubborn",
		State:          st,
		Logger:         logging.New(nil),
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
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "42", nil }
	l.mergeFunc = func(context.Context) (bool, error) {
		return false, fmt.Errorf("push denied by remote")
	}

	_ = l.Run(context.Background())

	// Task should be deferred in backend — merge failed, PR exists for manual review.
	backend.SkipMu.Lock()
	defer backend.SkipMu.Unlock()
	if len(backend.SkippedIDs) != 1 || backend.SkippedIDs[0] != "ralph-stub" {
		t.Errorf("expected ralph-stub deferred in backend, got %v", backend.SkippedIDs)
	}
}

// Merge failure skips the task — PR exists, work is done. No retry counting.
func TestLoop_MergeFailureClosesTaskNoRetryCount(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "Fixable task",
				NextID:       "ralph-fix",
				BackendLabel: "beads",
			},
		},
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		BaseBranch: "main",
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-fixable",
		State:          st,
		Logger:         logging.New(nil),
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
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "50", nil }
	l.mergeFunc = func(context.Context) (bool, error) {
		return false, fmt.Errorf("merge conflict")
	}

	_ = l.Run(context.Background())

	// Task should be deferred in backend — merge failed, PR exists for manual review.
	backend.SkipMu.Lock()
	defer backend.SkipMu.Unlock()
	if len(backend.SkippedIDs) != 1 || backend.SkippedIDs[0] != "ralph-fix" {
		t.Errorf("expected ralph-fix deferred in backend, got %v", backend.SkippedIDs)
	}
}

// Verifies that successful merge clears the merge failure counter.
func TestLoop_SuccessfulMergeClearsMergeFailures(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "Recovering task",
				NextID:       "ralph-rec",
				BackendLabel: "beads",
			},
		},
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		BaseBranch: "main",
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-recover",
		State:          st,
		Logger:         logging.New(nil),
	}

	// Seed 2 prior failures.
	tracker := attempts.New(ralphDir)
	tracker.RecordMergeFailure("ralph-rec")
	tracker.RecordMergeFailure("ralph-rec")

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "42", nil }
	l.mergeFunc = func(context.Context) (bool, error) {
		return true, nil
	}

	_ = l.Run(context.Background())

	if count := tracker.MergeFailureCount("ralph-rec"); count != 0 {
		t.Errorf("expected merge failures cleared after successful merge, got %d", count)
	}
}

// Verifies that pre-iteration test results are stored in state.json
// so they persist across restarts and evolve cycles.
func TestLoop_PreIterationTestResultsPersistedInState(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	// Create a Makefile with passing tests so VerifyDir detects a runner
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\ttrue\n"), 0o644)

	backend := &testutil.StubBackend{
		Remaining:    1,
		Completed:    0,
		Total:        1,
		NextTask:     "Add feature",
		NextID:       "ralph-pre",
		BackendLabel: "beads",
	}

	runner := &stubRunner{
		onRun: func() {
			backend.Remaining = 0
			backend.Completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, BaseBranch: "main"}

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
		VerifyDir:     dir,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

	_ = l.Run(context.Background())

	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.LastTestResult == "" {
		t.Error("expected last_test_result to be set in state after pre-iteration tests")
	}
	if s.LastTestTime == "" {
		t.Error("expected last_test_time to be set in state")
	}
}

type signalCallingRunner struct {
	onRun  func()
	result claude.Result
}

func (s *signalCallingRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	if s.onRun != nil {
		s.onRun()
	}
	if cfg.OnSignal != nil {
		cfg.OnSignal("")
		return claude.Result{
			SignalDetected: true,
			OnSignalUsed:   true,
			Summary:        s.result.Summary,
		}, nil
	}
	return s.result, nil
}

func (s *signalCallingRunner) StopStreaming() {}

func (s *signalCallingRunner) InjectMessage(_ string) error { return nil }

// Verifies that LLM verification pass logs with green (Success) color
// and LLM verification reject logs with red (Error) color.

// Verifies that LLM verification pass logs with green (Success) color
// and LLM verification reject logs with red (Error) color.
func TestLoop_LLMVerificationLogColors(t *testing.T) {
	tests := []struct {
		name      string
		passed    bool
		reason    string
		details   string
		wantColor string
		wantMsg   string
	}{
		{
			name:      "LLM pass logs green",
			passed:    true,
			reason:    "diff matches requirements",
			wantColor: logging.Green,
			wantMsg:   "LLM verified: diff matches requirements",
		},
		{
			name:      "LLM reject logs red",
			passed:    false,
			details:   "missing error handling",
			wantColor: logging.Red,
			wantMsg:   "LLM verification rejected (attempt 1/3): missing error handling",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, st := setupTestDir(t)
			ralphDir := filepath.Join(dir, ".ralph")
			promptsDir := filepath.Join(dir, "prompts")
			createPromptTemplates(t, promptsDir)
			os.WriteFile(filepath.Join(promptsDir, "verify-llm.md"), []byte("fix: {{LLM_FEEDBACK}}"), 0o644)

			backend := &testutil.MutableBackend{
				StubBackend: testutil.StubBackend{
					Remaining:    1,
					Completed:    0,
					Total:        1,
					NextTask:     "add colored logs",
					NextID:       "ralph-color",
					BackendLabel: "beads",
				},
			}

			runner := &signalCallingRunner{
				onRun: func() {
					backend.Lock()
					backend.Completed = 1
					backend.Remaining = 0
					backend.Unlock()
				},
				result: claude.Result{Summary: "done"},
			}

			var logBuf bytes.Buffer
			logger := logging.NewWithWriter(&logBuf)

			gm := &git.Manager{ProjectDir: dir, WorkDir: dir, BaseBranch: "main"}

			llmResult := verify.Result{
				Passed:  tt.passed,
				Reason:  tt.reason,
				Details: tt.details,
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
				TaskBackend:   backend,
				VerifyDir:     dir,
			}, st, gm, logger)
			l.runner = runner
			l.verifier.deps.LLMVerify = func(verify.VerifyOpts) verify.Result {
				return llmResult
			}
			l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }
			l.newRunnerFunc = func() claudeRunner {
				return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "fixed"}}
			}

			_ = l.Run(context.Background())

			output := logBuf.String()
			if !strings.Contains(output, tt.wantMsg) {
				t.Errorf("expected %q in output, got:\n%s", tt.wantMsg, output)
			}
			// Verify the message line uses the expected color by checking that
			// the line containing the message also contains the expected ANSI code.
			for _, line := range strings.Split(output, "\n") {
				if strings.Contains(line, tt.wantMsg) {
					if !strings.Contains(line, tt.wantColor) {
						t.Errorf("line with %q should use color %q, got:\n%s", tt.wantMsg, tt.name, line)
					}
					break
				}
			}
		})
	}
}
