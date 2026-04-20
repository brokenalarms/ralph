package loop

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
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

// completeTask: if the task was already skipped (e.g. verification
// rejected 3 times), it returns signalSkipped without pushing or merging.
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

	// Mark the task as skipped in state before calling completeTask.
	if err := st.AddSkippedTask("ralph-skipped"); err != nil {
		t.Fatalf("AddSkippedTask: %v", err)
	}

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


// completeTask completes normally within PostSignalTimeout when operations
// are fast, proving the timeout doesn't interfere with successful flows.
func TestCompleteTask_PostSignalTimeout_DoesNotInterfereWhenFast(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix login", NextID: "ralph-fast"}},
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, HeadRev: "after", Ship: git.ShipResult{PRNumber: 42}})
	cfg := Config{
		Dirs:              workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:     1,
		CallsPerHour:      80,
		PostSignalTimeout: 5 * time.Second,
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
		result:            claude.Result{SignalDetected: true},
		headBefore:        "",
		workDir:           dir,
		rawLogPath:        filepath.Join(ralphDir, "raw.log"),
		taskID:            "ralph-fast",
		nextTask:          "Fix login",
		postSignalTimeout: l.cfg.PostSignalTimeout,
		ralphDir:          ralphDir,
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

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "exited with error") {
		t.Errorf("expected warning about script error in log, got: %s", logOutput)
	}
}

// runPostTask logs a clear message at startup when no ralph:post-task script
// is found in package.json and no --post-task CLI flag is set.
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
	cfg :=

		// No PostTask CLI flag and no package.json — must log "skipping post-task".
		Config{
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

	l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-npt",
		nextTask:   "Fix bug",
		ralphDir:   ralphDir,
	})

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "No post_task config and no ralph:post-task script found in package.json — skipping post-task") {
		t.Errorf("expected 'skipping post-task' log message, got: %s", logOutput)
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
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

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
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

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
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

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

// runPostTask checkouts out main in projectDir before running the post-task
// script when merged=true, so scripts that do git pull --rebase see fresh main.
func TestRunPostTask_MergedTrue_ChecksOutMainInProjectDir(t *testing.T) {
	projectDir := initPostTaskRepo(t)
	// Start on a feature branch whose remote ref has been deleted (simulating a
	// merged PR's scenario — the remote tracking ref no longer exists).
	runGit(t, projectDir, "checkout", "-b", "fix/some-feature")

	currentBranch := currentGitBranch(t, projectDir)
	if currentBranch != "fix/some-feature" {
		t.Fatalf("setup: expected to be on fix/some-feature, got %q", currentBranch)
	}

	envFile := filepath.Join(projectDir, "post-task-env.txt")
	scriptPath := filepath.Join(projectDir, "post-task.sh")
	os.WriteFile(scriptPath, []byte(fmt.Sprintf("#!/bin/sh\ngit -C %s rev-parse --abbrev-ref HEAD > %s\n", projectDir, envFile)), 0o755)

	var logBuf bytes.Buffer
	runPostTask(context.Background(), runPostTaskParams{
		postTask:      scriptPath,
		worktreeDir:   projectDir,
		projectDir:    projectDir,
		defaultBranch: "main",
		logger:        logging.New(&logBuf),
	}, "ralph-test", 0, true)

	got := strings.TrimSpace(func() string {
		data, _ := os.ReadFile(envFile)
		return string(data)
	}())
	if got != "main" {
		t.Errorf("post-task script ran on branch %q, want %q — projectDir was not switched to main", got, "main")
	}
}

// runPostTask leaves projectDir unchanged when merged=false, even when the
// directory is on a feature branch.
func TestRunPostTask_MergedFalse_SkipsCheckout(t *testing.T) {
	projectDir := initPostTaskRepo(t)
	runGit(t, projectDir, "checkout", "-b", "fix/in-progress")

	envFile := filepath.Join(projectDir, "post-task-env.txt")
	scriptPath := filepath.Join(projectDir, "post-task.sh")
	os.WriteFile(scriptPath, []byte(fmt.Sprintf("#!/bin/sh\ngit -C %s rev-parse --abbrev-ref HEAD > %s\n", projectDir, envFile)), 0o755)

	runPostTask(context.Background(), runPostTaskParams{
		postTask:      scriptPath,
		worktreeDir:   projectDir,
		projectDir:    projectDir,
		defaultBranch: "main",
		logger:        logging.New(nil),
	}, "ralph-test", 0, false)

	got := strings.TrimSpace(func() string {
		data, _ := os.ReadFile(envFile)
		return string(data)
	}())
	if got != "fix/in-progress" {
		t.Errorf("expected projectDir to stay on fix/in-progress (merged=false), got %q", got)
	}
}

// runPostTask is a no-op for the checkout step when projectDir is already on
// main — git checkout main when already on main succeeds silently.
func TestRunPostTask_AlreadyOnMain_CheckoutIsNoOp(t *testing.T) {
	projectDir := initPostTaskRepo(t)

	envFile := filepath.Join(projectDir, "post-task-env.txt")
	scriptPath := filepath.Join(projectDir, "post-task.sh")
	os.WriteFile(scriptPath, []byte(fmt.Sprintf("#!/bin/sh\ngit -C %s rev-parse --abbrev-ref HEAD > %s\n", projectDir, envFile)), 0o755)

	var logBuf bytes.Buffer
	runPostTask(context.Background(), runPostTaskParams{
		postTask:      scriptPath,
		worktreeDir:   projectDir,
		projectDir:    projectDir,
		defaultBranch: "main",
		logger:        logging.New(&logBuf),
	}, "ralph-test", 0, true)

	got := strings.TrimSpace(func() string {
		data, _ := os.ReadFile(envFile)
		return string(data)
	}())
	if got != "main" {
		t.Errorf("expected projectDir to remain on main, got %q", got)
	}
	logOutput := logBuf.String()
	if strings.Contains(logOutput, "exited with error") {
		t.Errorf("checkout from main to main should not produce errors, got log: %s", logOutput)
	}
}

// runPostTask runs the post_task config value even when package.json also has
// a ralph:post-task script, proving config.toml takes priority over package.json.
func TestRunPostTask_ConfigTOMLOverridesPackageJSON(t *testing.T) {
	dir := t.TempDir()

	// Write a package.json ralph:post-task that records which source ran.
	pkgScriptFile := filepath.Join(dir, "pkg-ran.txt")
	pkgScript := filepath.Join(dir, "pkg-post-task.sh")
	os.WriteFile(pkgScript, []byte(fmt.Sprintf("#!/bin/sh\necho package > %s\n", pkgScriptFile)), 0o755)
	pkgJSON := fmt.Sprintf(`{"scripts":{"ralph:post-task":"sh %s"}}`, pkgScript)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0o644)

	// Write a config.toml post_task that records which source ran.
	cfgScriptFile := filepath.Join(dir, "cfg-ran.txt")
	cfgScript := filepath.Join(dir, "cfg-post-task.sh")
	os.WriteFile(cfgScript, []byte(fmt.Sprintf("#!/bin/sh\necho config > %s\n", cfgScriptFile)), 0o755)

	runPostTask(context.Background(), runPostTaskParams{
		postTask:    cfgScript,
		worktreeDir: dir,
		projectDir:  dir,
		logger:      logging.New(nil),
	}, "ralph-test", 0, false)

	// Config script must have run.
	if _, err := os.Stat(cfgScriptFile); err != nil {
		t.Errorf("config post_task script did not run: %v", err)
	}
	// Package.json script must NOT have run.
	if _, err := os.Stat(pkgScriptFile); err == nil {
		t.Error("package.json ralph:post-task ran but config.toml post_task should have taken priority")
	}
}

// initPostTaskRepo creates a local git repo on main with one commit. Returns
// the project dir path.
func initPostTaskRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "test")
	runGit(t, dir, "config", "user.email", "test@test")
	runGit(t, dir, "commit", "--allow-empty", "-m", "init")
	return dir
}

// runGit runs a git command in dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", cmd...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// currentGitBranch returns the current branch name in dir.
func currentGitBranch(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse --abbrev-ref HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
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

