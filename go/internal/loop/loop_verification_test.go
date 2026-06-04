package loop

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verifier"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Phase C migration notes for this file:
//
// Merge-retry tests were reframed off the forbidden MergeRetryFunc callback.
// Production's Ship internally drives MergeWithRetry when AutoMerge=true; the
// new stubRepo's Ship instead returns cfg.Ship directly. Outcomes that used
// to emerge from inside MergeRetryFunc (CIFailure, generic merge error,
// success) are now expressed as the Ship result the stub returns, which is
// what the loop observes in production anyway.
//
// ShipCalls>0 assertions were replaced with TrackingBackend.ClosedIDs
// observations (bead-close is the downstream effect of a successful ship).

// Verifies that when verification fails (e.g. tests don't pass), the task
// is NOT closed — it's recorded as a failed attempt so the next iteration
// can retry, preventing ralph from falsely closing beads.
func TestLoop_VerificationFailureBlocksClose(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Total:        1,
				NextTask:     "fix the bug",
				NextID:       "ralph-bug",
				BackendLabel: "beads",
			},
		},
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: true, Summary: "fixed it"},
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
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   &stubVerifyHook{passed: false, reason: "test suite failed"},
	})
	l.runner = runner

	_ = l.Run(context.Background())

	// Ship gating is observable: bead must not be closed when verification fails.
	backend.CloseMu.Lock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("no bead should be closed when verification fails, got %v", backend.ClosedIDs)
	}
	backend.CloseMu.Unlock()

	// Task should NOT be recorded as completed
	if _, err := os.Stat(filepath.Join(ralphDir, ".completed-tasks")); !os.IsNotExist(err) {
		t.Error("task should not be recorded as completed when verification fails")
	}

	// Attempt should be recorded in-memory
	found := false
	for _, ev := range l.taskAttempts {
		if strings.Contains(ev.Summary, "verification failed") || strings.Contains(ev.Analysis, "verification_failed") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a failed attempt to be recorded with verification failure context")
	}
}

// Verifies that when verification passes, the normal completion flow
// proceeds — task is closed, PR is pushed, and completed-tasks is recorded.
func TestLoop_VerificationPassAllowsClose(t *testing.T) {
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
				NextTask:     "add feature",
				NextID:       "ralph-feat",
				BackendLabel: "beads",
			},
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

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		Ship:       git.ShipResult{PRNumber: 42},
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
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

	backend.CloseMu.Lock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-feat" {
		t.Errorf("expected ralph-feat to be closed via ship pipeline, got %v", backend.ClosedIDs)
	}
	backend.CloseMu.Unlock()

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

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "simple task",
				NextID:       "ralph-simple",
				BackendLabel: "beads",
			},
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

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		Ship:       git.ShipResult{PRNumber: 99},
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	backend.CloseMu.Lock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-simple" {
		t.Errorf("expected ralph-simple closed when no verification configured, got %v", backend.ClosedIDs)
	}
	backend.CloseMu.Unlock()
}

// When CI fails during merge, the task stays open — CIFailureError means
// tests are still failing and the PR needs manual investigation.
//
// Reframed: instead of programming MergeRetryFunc to return CIFailureError,
// configure Ship to return the same CIFailure outcome directly.
func TestLoop_CIFailureLeavesTaskOpen(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix CI failure",
				NextID:    "ralph-ci1",
			},
		},
	}

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/project/01-ci-test",
		Ship: git.ShipResult{
			PRNumber:  99,
			CIFailure: true,
			CIFailureDetail: &git.CIFailureError{
				PRNumber: 99,
				Failures: []git.CICheckResult{
					{Name: "test", State: "FAILURE", Bucket: "fail"},
				},
			},
		},
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}

	_ = l.Run(context.Background())

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("task should NOT be closed when CI fails, got %v", backend.ClosedIDs)
	}
}

// When Ship reports Merged=true, the loop closes the task.
func TestLoop_MergeSuccessClosesTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Add feature",
				NextID:    "ralph-mc1",
			},
		},
	}

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/project/01-conflict-test",
		Ship:           git.ShipResult{PRNumber: 42, Merged: true},
	})

	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}

	_ = l.Run(context.Background())

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-mc1" {
		t.Errorf("expected ralph-mc1 closed on successful merge, got %v", backend.ClosedIDs)
	}
}

// When Ship reports Merged=true after (implicit) retry, the loop closes the
// task. Retry behavior itself is exercised by git-module tests; here we
// observe only the terminal Ship outcome.
func TestLoop_MergeEventualSuccessClosesTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix CI failure",
				NextID:    "ralph-ci2",
			},
		},
	}

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:         dir,
		WorkDir:            workDir,
		WorktreeBranch:     "ralph/project/01-ci-fix",
		Ship:               git.ShipResult{PRNumber: 42, Merged: true},
		MergeRetrySucceeds: true,
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}

	_ = l.Run(context.Background())

	tasks := l.SessionTasks()
	if len(tasks) == 0 {
		t.Error("expected at least one completed task after successful merge")
	}
}

// When CI keeps failing across retries, the loop leaves the task open —
// status must not be "completed".
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

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/project/01-ci-exhaust",
		Ship: git.ShipResult{
			PRNumber:  99,
			CIFailure: true,
			CIFailureDetail: &git.CIFailureError{
				PRNumber: 99,
				Failures: []git.CICheckResult{
					{Name: "test", State: "FAILURE", Bucket: "fail"},
				},
			},
		},
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations:      1,
		CallsPerHour:       80,
		AutoMerge:          true,
		InfraRetryBackoffs: make([]time.Duration, 3),
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: claude.Result{SignalDetected: true}}
			},
		}),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}

	_ = l.Run(context.Background())

	s, _ := st.Load()
	if s.Status == "completed" {
		t.Error("expected status not to be 'completed' when merge fails")
	}
}

// When Ship returns a non-CI error (plain ShipErr, not CIFailureError), the
// loop closes the task with "merge pending" — the work is done, only merge
// failed.
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

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/project/01-mixed",
		Ship:           git.ShipResult{PRNumber: 99, Merged: false},
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
	}
	var buf bytes.Buffer
	logger := logging.New(&buf)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}

	_ = l.Run(context.Background())

	output := buf.String()
	if !strings.Contains(output, "merge pending") {
		t.Errorf("expected log output to contain 'merge pending' for non-CI merge failure, got: %s", output)
	}
}

// When merge fails, the task is closed (not skipped) — the PR exists, work
// is verified done. Stack head detection can find the unmerged branch for
// the next task.
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

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/project/01-stubborn",
		Ship:           git.ShipResult{PRNumber: 42, Merged: false},
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}

	_ = l.Run(context.Background())

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-stub" {
		t.Errorf("expected ralph-stub closed in backend, got %v", backend.ClosedIDs)
	}
	backend.SkipMu.Lock()
	defer backend.SkipMu.Unlock()
	if len(backend.SkippedIDs) != 0 {
		t.Errorf("task should not be skipped when merge fails, got %v", backend.SkippedIDs)
	}
}

// Merge failure closes the task without retrying — the work is verified
// done, this is not a failure that needs retrying.
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

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/project/01-fixable",
		Ship:           git.ShipResult{PRNumber: 50, Merged: false},
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}

	_ = l.Run(context.Background())

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-fix" {
		t.Errorf("expected ralph-fix closed in backend, got %v", backend.ClosedIDs)
	}
	backend.SkipMu.Lock()
	defer backend.SkipMu.Unlock()
	if len(backend.SkippedIDs) != 0 {
		t.Errorf("task should not be skipped when merge fails, got %v", backend.SkippedIDs)
	}
}

// Verifies that pre-iteration test results are stored in state.json
// so they persist across restarts and evolve cycles.
func TestLoop_PreIterationTestResultsPersistedInState(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\ttrue\n"), 0o644)

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
	logger := logging.New(nil)
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

func (s *signalCallingRunner) Query(_ context.Context, _, _, _ string, _ []string) (string, error) {
	return "NO: stub", nil
}

// Verifies that LLM verification pass logs with green (Success) color
// and LLM verification reject logs with red (Error) color.
//
// The old version mutated gm.HeadRevValue inside onRun to simulate an
// agent commit; the new stub advances head via gm.CommitAll() which the
// runner callback invokes.
func TestLoop_LLMVerificationLogColors(t *testing.T) {
	tests := []struct {
		name       string
		queryReply string
		wantColor  string
		wantMsg    string
	}{
		{
			name:       "LLM pass logs green",
			queryReply: "YES: diff matches requirements",
			wantColor:  logging.Green,
			wantMsg:    "LLM verified: LLM verified: YES: diff matches requirements",
		},
		{
			name:       "LLM reject logs red",
			queryReply: "NO: missing error handling",
			wantColor:  logging.Red,
			wantMsg:    "LLM verification rejected: NO: missing error handling",
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

			gm := git.NewStub(git.StubRepoConfig{
				ProjectDir:     dir,
				WorkDir:        dir,
				HeadRev:        "before",
				DiffFullResult: "diff --git a/x b/x",
			})

			runner := &signalCallingRunner{
				onRun: func() {
					backend.Lock()
					backend.Completed = 1
					backend.Remaining = 0
					backend.Unlock()
					// Simulate the agent making a commit before signaling so
					// the post-signal commit check passes (HeadRev changes
					// via CommitAll advancing commitSeq).
					gm.CommitAll("simulated agent commit")
				},
				result: claude.Result{Summary: "done"},
			}

			var logBuf bytes.Buffer
			logger := logging.NewWithWriter(&logBuf)
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
			reply := tt.queryReply
			l := New(cfg, Modules{
				State:       st,
				Git:         gm,
				TaskBackend: backend,
				Logger:      logger,
				Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
					querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
						return reply, nil
					}},
					newRunner: func() verifier.Runner {
						return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "fixed"}}
					},
				}),
				Connectivity: onlineStubConnectivity(),
			})
			l.runner = runner

			_ = l.Run(context.Background())

			output := logBuf.String()
			if !strings.Contains(output, tt.wantMsg) {
				t.Errorf("expected %q in output, got:\n%s", tt.wantMsg, output)
			}
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

// TestRun_ConfigVerifyBypassesStartupGate proves that setting cfg.Verify allows
// the loop to start without a ralph:verify script — the greenfield use case where
// the first tasks create the project scaffolding that will contain the test suite.
func TestRun_ConfigVerifyBypassesStartupGate(t *testing.T) {
	dir, st := setupTestDir(t)
	// Remove the Makefile that setupTestDir creates — simulate a greenfield project
	// with no ralph:verify script anywhere.
	os.Remove(filepath.Join(dir, "Makefile"))

	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		RemoteURL:  "https://github.com/owner/repo.git",
	})

	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		Verify:        "true",
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  &testutil.TrackingBackend{},
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: &stubConnectivity{},
	})

	err := l.Run(context.Background())
	// The loop should NOT fail with "no ralph:verify script found".
	// It may stop for other reasons (no tasks, max iterations) — that's fine.
	if err != nil && strings.Contains(err.Error(), "no ralph:verify script found") {
		t.Errorf("startup gate should pass when cfg.Verify is set, got: %v", err)
	}
}

// Keep a reference to fmt so the import stays used even though all explicit
// callers were reframed away. fmt is retained because the fmt.Errorf
// fallback remains useful for any future test additions in this file.
var _ = fmt.Sprintf
