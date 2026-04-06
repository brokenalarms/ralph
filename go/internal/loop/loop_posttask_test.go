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
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// handlePostSignalCall invokes the package-level handlePostSignal using l's dependencies,
// and applies the session-task side effects back onto l.
func handlePostSignalCall(l *Loop, p postSignalParams) postSignalAction {
	opts := handlePostSignalOpts{
		postSignalTimeout: l.cfg.PostSignalTimeout,
		autoMerge:         l.cfg.AutoMerge,
		evolve:            l.cfg.Evolve,
		notify:            l.cfg.Notify,
		ralphDir:          l.cfg.Dirs.RalphDir,
		git:               l.git,
		backend:           l.cfg.TaskBackend,
		state:             l.state,
		logger:            l.logger,
		attempts:          l.attempts,
		verifyFn: func(ctx context.Context, headBefore string) (bool, string) {
			if l.cfg.OnVerify != nil {
				return l.cfg.OnVerify(ctx, l.git.GetWorkDir(), headBefore)
			}
			return l.verifier.VerifyCompletion(ctx, l.git.GetWorkDir(), headBefore)
		},
		pushSignalPRFn: func(p postSignalParams) (int, string) {
			return pushSignalPR(p.ctx, p, pushSignalPROpts{
				git:                 l.git,
				backend:             l.cfg.TaskBackend,
				logger:              l.logger,
				isOnlineFunc:        l.cfg.IsOnline,
				waitForInternetFunc: l.cfg.WaitForInternet,
				shipFn: func(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
					return l.git.Ship(ctx, opts)
				},
			})
		},
		finalizePRFn: func(fp finalizePRParams) finalizePRResult {
			fp.autoMerge = l.cfg.AutoMerge
			fp.git = l.git
			fp.logger = l.logger
			fp.backend = l.cfg.TaskBackend
			fp.state = l.state
			fp.attempts = l.attempts
			fp.verifier = l.verifier
			return finalizePR(fp)
		},
		buildCTFn: func(taskID, nextTask, summary string, prNumber int, _ string) CompletedTask {
			return buildCompletedTask(taskID, nextTask, summary, prNumber, l.git)
		},
		runPostTaskFn: func(ctx context.Context, taskID string, prNumber int, merged bool) {
			runPostTask(ctx, runPostTaskParams{
				postTask:   l.cfg.PostTask,
				projectDir: l.cfg.Dirs.ProjectDir,
				logger:     l.logger,
			}, taskID, prNumber, merged)
		},
	}
	out := handlePostSignal(p, opts)
	if out.ct != nil {
		l.completedTasks = append(l.completedTasks, *out.ct)
	}
	return out.action
}

// Branch name format uses beadID-slug without sequence number,
// proving the old TaskSeq pattern has been removed.
// handlePostSignal: verifies that a successful signal pushes, closes the
// task, and returns signalComplete for the normal (non-evolve) path.
func TestLoop_HandlePostSignal_ClosesTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:   "Fix auth bug",
				NextID:    "ralph-xyz",
			},
		},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	gm.ShipResult = git.ShipResult{PRNumber: 42}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	action := handlePostSignalCall(l, postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-xyz",
		nextTask:   "Fix auth bug",
		diffStat:   "",
	})

	if action != signalComplete {
		t.Errorf("expected signalComplete, got %d", action)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-xyz" {
		t.Errorf("expected CloseTask for ralph-xyz, got %v", backend.ClosedIDs)
	}
}

// handlePostSignal: verification failure returns signalRetry so the loop
// continues without closing the task.
func TestLoop_HandlePostSignal_VerificationFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Fix bug", NextID: "ralph-abc"}},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return false, "tests failed" }

	action := handlePostSignalCall(l, postSignalParams{
		ctx:      context.Background(),
		result:   claude.Result{},
		taskID:   "ralph-abc",
		nextTask:   "Fix bug",
	})

	if action != signalRetry {
		t.Errorf("expected signalRetry, got %d", action)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("task should not be closed on verification failure, got %v", backend.ClosedIDs)
	}
}

// handlePostSignal: if the task was already skipped (e.g. verification
// rejected 3 times), it returns signalSkipped without pushing or merging.
func TestHandlePostSignal_SkippedTask_DoesNotPush(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-skipped"}},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	var logBuf bytes.Buffer
	logger := logging.New(&logBuf)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)
	l.runner = &stubRunner{}
	gm.ShipResult = git.ShipResult{PRNumber: 99}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	// Mark the task as skipped in state before calling handlePostSignal.
	if err := st.AddSkippedTask("ralph-skipped"); err != nil {
		t.Fatalf("AddSkippedTask: %v", err)
	}

	action := handlePostSignalCall(l, postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-skipped",
		nextTask:   "Fix bug",
	})

	if action != signalSkipped {
		t.Errorf("expected signalSkipped, got %d", action)
	}

	// Ship should NOT have been called — no push attempt.
	if gm.ShipCalls > 0 {
		t.Error("Ship was called for a skipped task — rejected work should not be pushed")
	}

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

// handlePostSignal returns within PostSignalTimeout even when push blocks
// indefinitely, proving the timeout prevents infinite stalls from rate limits
// or network issues.
func TestHandlePostSignal_PostSignalTimeout_AbortsStuckPush(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Fix bug", NextID: "ralph-timeout"}},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	var logBuf bytes.Buffer
	logger := logging.New(&logBuf)

	l := New(Config{
		Dirs:              workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:     1,
		CallsPerHour:      80,
		TaskBackend:       backend,
		PostSignalTimeout: 50 * time.Millisecond,
	}, st, gm, logger)
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }
	gm.ShipFunc = func(ctx context.Context, _ git.ShipOpts) (git.ShipResult, error) {
		<-ctx.Done()
		return git.ShipResult{}, ctx.Err()
	}

	done := make(chan postSignalAction, 1)
	go func() {
		done <- handlePostSignalCall(l, postSignalParams{
			ctx:        context.Background(),
			result:     claude.Result{SignalDetected: true},
			headBefore: "",
			workDir:    dir,
			rawLogPath: filepath.Join(ralphDir, "raw.log"),
			taskID:     "ralph-timeout",
			nextTask:   "Fix bug",
		})
	}()

	select {
	case action := <-done:
		if action != signalComplete {
			t.Errorf("expected signalComplete, got %d", action)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handlePostSignal hung — PostSignalTimeout did not fire")
	}

	output := logBuf.String()
	if !strings.Contains(output, "Post-signal timeout") {
		t.Errorf("expected timeout log message, got: %s", output)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) > 0 {
		t.Errorf("task should not be closed when timeout fires before push, got %v", backend.ClosedIDs)
	}
}

// handlePostSignal completes normally within PostSignalTimeout when operations
// are fast, proving the timeout doesn't interfere with successful flows.
func TestHandlePostSignal_PostSignalTimeout_DoesNotInterfereWhenFast(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Fix login", NextID: "ralph-fast"}},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	l := New(Config{
		Dirs:              workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:     1,
		CallsPerHour:      80,
		TaskBackend:       backend,
		PostSignalTimeout: 5 * time.Second,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }
	gm.ShipResult = git.ShipResult{PRNumber: 42}

	action := handlePostSignalCall(l, postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-fast",
		nextTask:   "Fix login",
	})

	if action != signalComplete {
		t.Errorf("expected signalComplete, got %d", action)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-fast" {
		t.Errorf("expected task ralph-fast closed, got %v", backend.ClosedIDs)
	}
}

// handlePostSignal cancels a blocking merge when the post-signal timeout
// fires, so the orchestrator doesn't stall on a rate-limited API call.
func TestHandlePostSignal_PostSignalTimeout_CancelsMerge(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Slow merge", NextID: "ralph-slow"}},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	l := New(Config{
		Dirs:              workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:     1,
		CallsPerHour:      80,
		TaskBackend:       backend,
		AutoMerge:         true,
		PostSignalTimeout: 50 * time.Millisecond,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }
	gm.ShipResult = git.ShipResult{PRNumber: 99}
	gm.MergeRetryFunc = func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	done := make(chan postSignalAction, 1)
	go func() {
		done <- handlePostSignalCall(l, postSignalParams{
			ctx:        context.Background(),
			result:     claude.Result{SignalDetected: true},
			headBefore: "",
			workDir:    dir,
			rawLogPath: filepath.Join(ralphDir, "raw.log"),
			taskID:     "ralph-slow",
			nextTask:   "Slow merge",
		})
	}()

	select {
	case <-done:
		// Returned within timeout — merge was cancelled, not hung
	case <-time.After(5 * time.Second):
		t.Fatal("handlePostSignal hung — PostSignalTimeout did not cancel merge")
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
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Fix bug", NextID: "ralph-pt1"}},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		PostTask:      scriptPath,
		AutoMerge:     true,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	gm.ShipResult = git.ShipResult{PRNumber: 99}
	gm.MergeRetryResult = true
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	action := handlePostSignalCall(l, postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-pt1",
		nextTask:   "Fix bug",
	})

	if action != signalComplete {
		t.Fatalf("expected signalComplete, got %d", action)
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
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Fix bug", NextID: "ralph-pt2"}},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		PostTask:      scriptPath,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return false, "tests failed" }

	action := handlePostSignalCall(l, postSignalParams{
		ctx:      context.Background(),
		result:   claude.Result{},
		taskID:   "ralph-pt2",
		nextTask:   "Fix bug",
	})

	if action != signalRetry {
		t.Fatalf("expected signalRetry, got %d", action)
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
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Fix bug", NextID: "ralph-pt3"}},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		PostTask:      scriptPath,
	}, st, gm, logger)
	l.runner = &stubRunner{}
	gm.ShipResult = git.ShipResult{PRNumber: 50}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	action := handlePostSignalCall(l, postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-pt3",
		nextTask:   "Fix bug",
	})

	if action != signalComplete {
		t.Fatalf("expected signalComplete despite script failure, got %d", action)
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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	gm.ShipResult = git.ShipResult{PRNumber: 10}
	gm.MergeRetryResult = true

	// No PostTask CLI flag and no package.json — must log "skipping post-task".
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		AutoMerge:     true,
	}, st, gm, logger)
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	handlePostSignalCall(l, postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-npt",
		nextTask:   "Fix bug",
	})

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "No ralph:post-task script found in package.json and no --post-task CLI flag — skipping post-task") {
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
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Fix bug", NextID: "ralph-pt4"}},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "before"}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		PostTask:      scriptPath,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	action := handlePostSignalCall(l, postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{OnSignalUsed: true},
		headBefore: "before",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-pt4",
		nextTask:   "Fix bug",
	})

	if action != signalSkipped {
		t.Fatalf("expected signalSkipped, got %d", action)
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

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	gm.ShipResult = git.ShipResult{PRNumber: 77}
	gm.MergeRetryResult = true

	// No PostTask CLI flag — detection must come from package.json alone.
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		AutoMerge:     true,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	action := handlePostSignalCall(l, postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-pkg1",
		nextTask:   "Fix bug",
	})

	if action != signalComplete {
		t.Fatalf("expected signalComplete, got %d", action)
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

// handlePostSignal fires a TaskCompleted notification after post-task when
// Notify is enabled.
func TestHandlePostSignal_NotifyEnabled_SendsTaskCompleted(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:   "Fix auth bug",
				NextID:    "ralph-ntf",
			},
		},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Notify:        true,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	gm.ShipResult = git.ShipResult{PRNumber: 42}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	var buf bytes.Buffer
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

	handlePostSignalCall(l, postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true, Summary: "Fixed token expiry"},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-ntf",
		nextTask:   "Fix auth bug",
	})

	got := buf.String()
	if !strings.Contains(got, "Task done: [ralph-ntf] Fix auth bug") {
		t.Errorf("expected TaskCompleted notification, got %q", got)
	}
	if !strings.Contains(got, "Fixed token expiry") {
		t.Errorf("expected summary in notification, got %q", got)
	}
}

// handlePostSignal does NOT send TaskCompleted when Notify is disabled.
func TestHandlePostSignal_NotifyDisabled_NoNotification(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:   "Fix auth bug",
				NextID:    "ralph-ntf2",
			},
		},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Notify:        false,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	gm.ShipResult = git.ShipResult{PRNumber: 42}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	var buf bytes.Buffer
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

	handlePostSignalCall(l, postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true, Summary: "Fixed token expiry"},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-ntf2",
		nextTask:   "Fix auth bug",
	})

	got := buf.String()
	if strings.Contains(got, "Task done") {
		t.Errorf("expected no TaskCompleted notification when Notify=false, got %q", got)
	}
}

// handlePostSignal fires TaskCompleted on the no-commits path when Notify is enabled.
func TestHandlePostSignal_NotifyOnNoCommitsPath(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:   "Update docs",
				NextID:    "ralph-nc1",
			},
		},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "before"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Notify:        true,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	var buf bytes.Buffer
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

	action := handlePostSignalCall(l, postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true, Summary: "Updated README"},
		headBefore: "before",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-nc1",
		nextTask:   "Update docs",
	})

	if action != signalSkipped {
		t.Errorf("expected signalSkipped for no-commits path, got %d", action)
	}

	got := buf.String()
	if !strings.Contains(got, "Task done: [ralph-nc1] Update docs") {
		t.Errorf("expected TaskCompleted on no-commits path, got %q", got)
	}
}

// handlePostSignal cancels when a feedback file appears during the post-signal
// pipeline, proving that feedback during CI/merge/review aborts processing
// and CloseTask is never called.
func TestHandlePostSignal_FeedbackFileStopsPostSignal(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix bug", NextID: "ralph-feedback"}},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after"}
	var logBuf bytes.Buffer
	logger := logging.New(&logBuf)

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)
	l.runner = &stubRunner{}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	// Ship blocks until context is cancelled, simulating a stuck CI/merge step.
	gm.ShipFunc = func(ctx context.Context, _ git.ShipOpts) (git.ShipResult, error) {
		<-ctx.Done()
		return git.ShipResult{}, ctx.Err()
	}

	// Write the feedback file after a short delay.
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.WriteFile(filepath.Join(ralphDir, "feedback"), nil, 0o644)
	}()

	done := make(chan postSignalAction, 1)
	go func() {
		done <- handlePostSignalCall(l, postSignalParams{
			ctx:        context.Background(),
			result:     claude.Result{SignalDetected: true},
			headBefore: "",
			workDir:    dir,
			rawLogPath: filepath.Join(ralphDir, "raw.log"),
			taskID:     "ralph-feedback",
			nextTask:   "Fix bug",
		})
	}()

	select {
	case action := <-done:
		if action != signalComplete {
			t.Errorf("expected signalComplete, got %d", action)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handlePostSignal hung — feedback file did not cancel context")
	}

	output := logBuf.String()
	if !strings.Contains(output, "Feedback signal detected during post-signal pipeline") {
		t.Errorf("expected feedback log message, got: %s", output)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) > 0 {
		t.Errorf("task should not be closed when feedback arrives, got %v", backend.ClosedIDs)
	}
}
