package loop

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Branch name format uses beadID-slug without sequence number,
// proving the old TaskSeq pattern has been removed.
// completeTask: verifies that a successful signal pushes, closes the
// task, and returns signalComplete for the normal (non-evolve) path.
func TestLoop_CompleteTask_ClosesTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix auth bug",
				NextID:    "ralph-xyz",
			},
		},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, HeadRev: "after", Ship: git.ShipResult{PRNumber: 42}})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = &stubRunner{}

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-xyz",
		nextTask:   "Fix auth bug",
		ralphDir:   ralphDir,
	})

	if out.action != signalComplete {
		t.Errorf("expected signalComplete, got %d", out.action)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-xyz" {
		t.Errorf("expected CloseTask for ralph-xyz, got %v", backend.ClosedIDs)
	}
}

// completeTask: verification failure returns signalRetry so the loop
// continues without closing the task.
func TestLoop_CompleteTask_VerificationFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-abc"}},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook: &stubVerifyHook{passed: false, reason: "tests failed"},
	})
	l.runner = &stubRunner{}

	out := l.completeTask(context.Background(), completeTaskParams{
		result:   claude.Result{},
		taskID:   "ralph-abc",
		nextTask: "Fix bug",
		ralphDir: ralphDir,
	})

	if out.action != signalRetry {
		t.Errorf("expected signalRetry, got %d", out.action)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("task should not be closed on verification failure, got %v", backend.ClosedIDs)
	}
}

// completeTask: if the task was skipped during verification (tracked in
// sessionSkippedIDs), it returns signalSkipped without pushing or merging.
func TestCompleteTask_SkippedTask_DoesNotPush(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-skipped"}},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, HeadRev: "after", Ship: git.ShipResult{PRNumber: 99}})
	var logBuf bytes.Buffer
	logger := logging.New(&logBuf)
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = &stubRunner{}

	// Mark the task as skipped in the loop's session set (as skipTask would do).
	l.sessionSkippedIDs = map[string]bool{"ralph-skipped": true}

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-skipped",
		nextTask:   "Fix bug",
		ralphDir:   ralphDir,
	})

	if out.action != signalSkipped {
		t.Errorf("expected signalSkipped, got %d", out.action)
	}

	// Ship should NOT have been called — no push attempt. Observable via
	// bead-close absence below (ship would lead to close).

	// Task should NOT be closed.
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("skipped task should not be closed, got %v", backend.ClosedIDs)
	}

	if !strings.Contains(logBuf.String(), "was skipped during verification") {
		t.Errorf("expected skip log message, got: %s", logBuf.String())
	}
}

// Full re-release flow: a bead skipped earlier in the session (skipTask
// tracks it in sessionSkippedIDs) is reassigned back to ralph-loop and
// re-selected by selectNextTask in the same session. selectNextTask must
// clear the stale sessionSkippedIDs entry so that completeTask's push guard
// does not fire when the re-worked task completes.
func TestLoop_ReReleasedTask_SelectedThenCompleted_PushProceeds(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-reup"}},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, HeadRev: "after", Ship: git.ShipResult{PRNumber: 7}})
	logger := logging.New(nil)
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook:  passingVerifyHook(),
	})
	l.runner = &stubRunner{}

	// Simulate the bead having been skipped earlier this session (e.g. a
	// push_failed skip on a prior iteration).
	l.skipTask("ralph-reup", tasks.SkipPushFailed, "prior push failure")
	if !l.sessionSkippedIDs["ralph-reup"] {
		t.Fatal("precondition: expected ralph-reup to be tracked in sessionSkippedIDs after skipTask")
	}

	// Simulate the operator re-releasing the bead (bd update -a=ralph-loop):
	// it is ready again and the loop re-selects it fresh this session.
	tc, action, _ := l.selectNextTask(context.Background(), selectNextTaskParams{
		completedIDs: map[string]bool{},
	})
	if action != actionProceed {
		t.Fatalf("expected actionProceed on re-selection, got %v", action)
	}
	if tc.id != "ralph-reup" {
		t.Fatalf("expected re-selected task ralph-reup, got %q", tc.id)
	}

	// The re-worked task now completes. The push guard must not fire since
	// this iteration's selection cleared the stale skip entry.
	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     tc.id,
		nextTask:   tc.title,
		ralphDir:   ralphDir,
	})

	if out.action == signalSkipped {
		t.Fatal("push guard fired for re-released task — stale sessionSkippedIDs entry was not cleared on re-selection")
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-reup" {
		t.Errorf("expected ralph-reup to be closed after successful push, got %v", backend.ClosedIDs)
	}
}

// completeTask completes normally when operations are fast, proving the
// post-signal pipeline closes the task on the happy path.
func TestCompleteTask_CompletesFastHappyPath(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix login", NextID: "ralph-fast"}},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, HeadRev: "after", Ship: git.ShipResult{PRNumber: 42}})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook:  passingVerifyHook(),
	})
	l.runner = &stubRunner{}

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-fast",
		nextTask:   "Fix login",
		ralphDir:   ralphDir,
	})

	if out.action != signalComplete {
		t.Errorf("expected signalComplete, got %d", out.action)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-fast" {
		t.Errorf("expected task ralph-fast closed, got %v", backend.ClosedIDs)
	}
}



// runPostTask executes the configured script after task completion with
// RALPH_TASK_ID, RALPH_PR_NUMBER, and RALPH_MERGED env vars.
func TestLoop_PostTaskScript_RunsWithEnvVars(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	// Create a script that writes env vars to a file for verification.
	envFile := filepath.Join(dir, "post-task-env.txt")
	scriptPath := filepath.Join(dir, "post-task.sh")
	os.WriteFile(scriptPath, []byte(fmt.Sprintf("#!/bin/sh\necho \"TASK=$RALPH_TASK_ID PR=$RALPH_PR_NUMBER MERGED=$RALPH_MERGED\" > %s\n", envFile)), 0o755)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-pt1"}},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:         dir,
		WorkDir:            dir,
		HeadRev:            "after",
		Ship:               git.ShipResult{PRNumber: 99, Merged: true},
		MergeRetrySucceeds: true,
	})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		PostTask:      scriptPath,
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook:  passingVerifyHook(),
	})
	l.runner = &stubRunner{}

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-pt1",
		nextTask:   "Fix bug",
		ralphDir:   ralphDir,
	})

	if out.action != signalComplete {
		t.Fatalf("expected signalComplete, got %d", out.action)
	}

	// Post-task runs from postTaskAndMaybeEvolve (called by iterLoop after completeTask).
	l.postTaskAndMaybeEvolve(context.Background(), "ralph-pt1", out.prNumber, out.merged)

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("post-task script did not run: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := "TASK=ralph-pt1 PR=99 MERGED=true"
	if got != want {
		t.Errorf("env vars: got %q, want %q", got, want)
	}
}

// runPostTask is not called when verification fails (signalRetry).
func TestLoop_PostTaskScript_NotCalledOnRetry(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	envFile := filepath.Join(dir, "post-task-env.txt")
	scriptPath := filepath.Join(dir, "post-task.sh")
	os.WriteFile(scriptPath, []byte(fmt.Sprintf("#!/bin/sh\necho ran > %s\n", envFile)), 0o755)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-pt2"}},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		PostTask:      scriptPath,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook: &stubVerifyHook{passed: false, reason: "tests failed"},
	})
	l.runner = &stubRunner{}

	out := l.completeTask(context.Background(), completeTaskParams{
		result:   claude.Result{},
		taskID:   "ralph-pt2",
		nextTask: "Fix bug",
		ralphDir: ralphDir,
	})

	if out.action != signalRetry {
		t.Fatalf("expected signalRetry, got %d", out.action)
	}

	if _, err := os.Stat(envFile); err == nil {
		t.Error("post-task script should not run on verification failure")
	}
}

// runPostTask warns but continues when the script exits non-zero.
func TestLoop_PostTaskScript_NonZeroExitWarns(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	scriptPath := filepath.Join(dir, "post-task.sh")
	os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 1\n"), 0o755)

	var logBuf bytes.Buffer
	logger := logging.New(&logBuf)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-pt3"}},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, HeadRev: "after", Ship: git.ShipResult{PRNumber: 50}})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		PostTask:      scriptPath,
	}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = &stubRunner{}

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-pt3",
		nextTask:   "Fix bug",
		ralphDir:   ralphDir,
	})

	if out.action != signalComplete {
		t.Fatalf("expected signalComplete despite script failure, got %d", out.action)
	}

	// Post-task runs from postTaskAndMaybeEvolve (called by iterLoop after completeTask).
	l.postTaskAndMaybeEvolve(context.Background(), "ralph-pt3", out.prNumber, out.merged)

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "exited with error") {
		t.Errorf("expected warning about script error in log, got: %s", logOutput)
	}
}

// runPostTask logs a message naming both the ralph:post-task npm script and
// ralph-post-task Makefile target when neither is configured.
func TestLoop_PostTaskScript_LogsWhenNotConfigured(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	var logBuf bytes.Buffer
	logger := logging.New(&logBuf)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-npt"}},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:         dir,
		WorkDir:            dir,
		HeadRev:            "after",
		Ship:               git.ShipResult{PRNumber: 10, Merged: true},
		MergeRetrySucceeds: true,
	})
	cfg := Config{
			Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
			MaxIterations: 1,
			CallsPerHour:  80,
			AutoMerge:     true,
		}
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = &stubRunner{}

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-npt",
		nextTask:   "Fix bug",
		ralphDir:   ralphDir,
	})

	// Post-task runs from postTaskAndMaybeEvolve (called by iterLoop after completeTask).
	l.postTaskAndMaybeEvolve(context.Background(), "ralph-npt", out.prNumber, out.merged)

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "No post_task config and no ralph:post-task npm script or ralph-post-task Makefile target found — skipping post-task") {
		t.Errorf("expected 'skipping post-task' log message mentioning both npm script and Makefile target, got: %s", logOutput)
	}
}

// runPostTask is called in the no-commits path (signalSkipped) with merged=false.
func TestLoop_PostTaskScript_CalledOnNoCommitsPath(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	envFile := filepath.Join(dir, "post-task-env.txt")
	scriptPath := filepath.Join(dir, "post-task.sh")
	os.WriteFile(scriptPath, []byte(fmt.Sprintf("#!/bin/sh\necho \"TASK=$RALPH_TASK_ID PR=$RALPH_PR_NUMBER MERGED=$RALPH_MERGED\" > %s\n", envFile)), 0o755)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-pt4"}},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, HeadRev: "before"})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		PostTask:      scriptPath,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = &stubRunner{}

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{OnSignalUsed: true},
		headBefore: "before",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-pt4",
		nextTask:   "Fix bug",
		ralphDir:   ralphDir,
	})

	if out.action != signalSkipped {
		t.Fatalf("expected signalSkipped, got %d", out.action)
	}

	// Post-task runs from postTaskAndMaybeEvolve (called by iterLoop after completeTask).
	l.postTaskAndMaybeEvolve(context.Background(), "ralph-pt4", out.prNumber, out.merged)

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("post-task script did not run on no-commits path: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := "TASK=ralph-pt4 PR=0 MERGED=false"
	if got != want {
		t.Errorf("env vars: got %q, want %q", got, want)
	}
}

// runPostTask detects ralph:post-task in package.json and runs it even when
// no --post-task CLI flag is set, proving package.json is the priority source.
func TestLoop_PostTaskScript_PackageJSONDetection(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	// Write package.json with ralph:post-task script that records env vars.
	envFile := filepath.Join(dir, "post-task-env.txt")
	scriptPath := filepath.Join(dir, "post-task.sh")
	os.WriteFile(scriptPath, []byte(fmt.Sprintf("#!/bin/sh\necho \"TASK=$RALPH_TASK_ID PR=$RALPH_PR_NUMBER MERGED=$RALPH_MERGED\" > %s\n", envFile)), 0o755)
	pkgJSON := fmt.Sprintf(`{"scripts":{"ralph:post-task":"sh %s"}}`, scriptPath)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0o644)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-pkg1"}},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:         dir,
		WorkDir:            dir,
		HeadRev:            "after",
		Ship:               git.ShipResult{PRNumber: 77, Merged: true},
		MergeRetrySucceeds: true,
	})
	cfg :=

		// No PostTask CLI flag — detection must come from package.json alone.
		Config{
			Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
			MaxIterations: 1,
			CallsPerHour:  80,
			AutoMerge:     true,
		}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = &stubRunner{}

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-pkg1",
		nextTask:   "Fix bug",
		ralphDir:   ralphDir,
	})

	if out.action != signalComplete {
		t.Fatalf("expected signalComplete, got %d", out.action)
	}

	// Post-task runs from postTaskAndMaybeEvolve (called by iterLoop after completeTask).
	l.postTaskAndMaybeEvolve(context.Background(), "ralph-pkg1", out.prNumber, out.merged)

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("ralph:post-task script was not run: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := "TASK=ralph-pkg1 PR=77 MERGED=true"
	if got != want {
		t.Errorf("env vars: got %q, want %q", got, want)
	}
}

// completeTask fires a TaskCompleted notification after post-task when
// Notify is enabled.
func TestCompleteTask_NotifyEnabled_SendsTaskCompleted(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix auth bug",
				NextID:    "ralph-ntf",
			},
		},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, HeadRev: "after", Ship: git.ShipResult{PRNumber: 42}})
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
		VerifyHook: passingVerifyHook(),
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

	l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true, Summary: "Fixed token expiry"},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-ntf",
		nextTask:   "Fix auth bug",
		notify:     true,
		ralphDir:   ralphDir,
	})

	got := buf.String()
	if !strings.Contains(got, "Task done: [ralph-ntf] Fix auth bug") {
		t.Errorf("expected TaskCompleted notification, got %q", got)
	}
	if !strings.Contains(got, "Fixed token expiry") {
		t.Errorf("expected summary in notification, got %q", got)
	}
}

// completeTask does NOT send TaskCompleted when Notify is disabled.
func TestCompleteTask_NotifyDisabled_NoNotification(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix auth bug",
				NextID:    "ralph-ntf2",
			},
		},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, HeadRev: "after", Ship: git.ShipResult{PRNumber: 42}})
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
		VerifyHook: passingVerifyHook(),
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

	l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true, Summary: "Fixed token expiry"},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-ntf2",
		nextTask:   "Fix auth bug",
		notify:     false,
		ralphDir:   ralphDir,
	})

	got := buf.String()
	if strings.Contains(got, "Task done") {
		t.Errorf("expected no TaskCompleted notification when Notify=false, got %q", got)
	}
}

// completeTask fires TaskCompleted on the no-commits path when Notify is enabled.
func TestCompleteTask_NotifyOnNoCommitsPath(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Update docs",
				NextID:    "ralph-nc1",
			},
		},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, HeadRev: "before"})
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
		VerifyHook: passingVerifyHook(),
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

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true, Summary: "Updated README"},
		headBefore: "before",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-nc1",
		nextTask:   "Update docs",
		notify:     true,
		ralphDir:   ralphDir,
	})

	if out.action != signalSkipped {
		t.Errorf("expected signalSkipped for no-commits path, got %d", out.action)
	}

	got := buf.String()
	if !strings.Contains(got, "Task done: [ralph-nc1] Update docs") {
		t.Errorf("expected TaskCompleted on no-commits path, got %q", got)
	}
}

// completeTask fires exactly one notification (TaskMerged) when a task is shipped and merged —
// not the prior TaskCompleted+TaskMerged pair. This prevents the notification burst where users
// receive 2 notifications per merged task, multiplied across back-to-back completions.
func TestCompleteTask_MergedPath_SingleNotification(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Next task",
				NextID:    "ralph-mrg",
			},
		},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		HeadRev:    "after",
		Ship:       git.ShipResult{PRNumber: 42, Merged: true},
	})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		Notify:        true,
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook:  passingVerifyHook(),
	})
	l.runner = &stubRunner{}

	var notifyCalls []string
	prev := notify.SetCommandRunner(func(_ string, args ...string) error {
		notifyCalls = append(notifyCalls, strings.Join(args, " "))
		return nil
	})
	t.Cleanup(func() { notify.SetCommandRunner(prev) })

	l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true, Summary: "Fixed it"},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-mrg",
		nextTask:   "Next task",
		notify:     true,
		ralphDir:   ralphDir,
	})

	if len(notifyCalls) != 1 {
		t.Errorf("expected exactly 1 notification for merged task, got %d: %v", len(notifyCalls), notifyCalls)
	}
	if len(notifyCalls) > 0 && !strings.Contains(notifyCalls[0], "Task merged") {
		t.Errorf("expected TaskMerged notification, got %q", notifyCalls[0])
	}
	if len(notifyCalls) > 0 && strings.Contains(notifyCalls[0], "Task done") {
		t.Errorf("expected no TaskCompleted notification for merged path, got %q", notifyCalls[0])
	}
}

// completeTask: when Ship returns a non-zero PRNumber on a diverged branch,
// the loop takes the normal PR-created close path and does NOT log "No PR
// created" or close the bead with the orphan reason "no PR". This is the
// regression guard for the bug where diverged branches had their work
// orphaned by Ship returning an empty ShipResult.
func TestCompleteTask_DivergedBranch_CreatesPR(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Continue work",
				NextID:    "ralph-div1",
			},
		},
	}

	// Ship returns a non-zero PRNumber — simulating a diverged branch where
	// BranchHasUnmergedWork=true causes CreatePR to be called and succeed.
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		HeadRev:    "after",
		Ship:       git.ShipResult{PRNumber: 55},
	})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook:  passingVerifyHook(),
	})
	l.runner = &stubRunner{}

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-div1",
		nextTask:   "Continue work",
		ralphDir:   ralphDir,
	})

	if out.action != signalComplete {
		t.Errorf("expected signalComplete, got %d", out.action)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-div1" {
		t.Errorf("expected CloseTask for ralph-div1, got %v", backend.ClosedIDs)
	}
	for _, reason := range backend.CloseReasons {
		if strings.Contains(reason, "no PR") {
			t.Errorf("loop took 'No PR created' path on diverged branch; close reason: %q", reason)
		}
	}
}

