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
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Branch name format uses beadID-slug without sequence number,
// proving the old TaskSeq pattern has been removed.
// handlePostSignal: verifies that a successful signal pushes, closes the
// task, and returns signalComplete for the normal (non-evolve) path.
func TestLoop_HandlePostSignal_ClosesTask(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
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

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil), BaseBranch: "main"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "42", nil }
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }

	// Create a commit so HeadRev() returns something different from headBefore=""
	writeFile(t, project, "fix.go", "package main\n")
	run(t, "git", "-C", project, "add", "fix.go")
	run(t, "git", "-C", project, "commit", "-m", "fix auth bug")

	runIter, iter := 1, 1
	action := l.handlePostSignal(postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    project,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-xyz",
		nextTask:   "Fix auth bug",
		diffStat:   "",
	}, &runIter, &iter)

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

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, BaseBranch: "main"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return false, "tests failed" }

	runIter, iter := 1, 1
	action := l.handlePostSignal(postSignalParams{
		ctx:      context.Background(),
		result:   claude.Result{},
		taskID:   "ralph-abc",
		nextTask:   "Fix bug",
	}, &runIter, &iter)

	if action != signalRetry {
		t.Errorf("expected signalRetry, got %d", action)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("task should not be closed on verification failure, got %v", backend.ClosedIDs)
	}
}

// handlePostSignal returns within PostSignalTimeout even when push blocks
// indefinitely, proving the timeout prevents infinite stalls from rate limits
// or network issues.
func TestHandlePostSignal_PostSignalTimeout_AbortsStuckPush(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Fix bug", NextID: "ralph-timeout"}},
	}

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil), BaseBranch: "main"}
	var logBuf bytes.Buffer
	logger := logging.New(&logBuf)

	l := New(Config{
		Dirs:              workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:     1,
		CallsPerHour:      80,
		TaskBackend:       backend,
		PostSignalTimeout: 50 * time.Millisecond,
	}, st, gm, logger)
	l.runner = &stubRunner{}
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }
	l.pushPRFunc = func(ctx context.Context, _, _, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	writeFile(t, project, "fix.go", "package main\n")
	run(t, "git", "-C", project, "add", "fix.go")
	run(t, "git", "-C", project, "commit", "-m", "fix")

	runIter, iter := 1, 1
	done := make(chan postSignalAction, 1)
	go func() {
		done <- l.handlePostSignal(postSignalParams{
			ctx:        context.Background(),
			result:     claude.Result{SignalDetected: true},
			headBefore: "",
			workDir:    project,
			rawLogPath: filepath.Join(ralphDir, "raw.log"),
			taskID:     "ralph-timeout",
			nextTask:   "Fix bug",
		}, &runIter, &iter)
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
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Fix login", NextID: "ralph-fast"}},
	}

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil), BaseBranch: "main"}
	l := New(Config{
		Dirs:              workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:     1,
		CallsPerHour:      80,
		TaskBackend:       backend,
		PostSignalTimeout: 5 * time.Second,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "42", nil }

	writeFile(t, project, "fix.go", "package main\n")
	run(t, "git", "-C", project, "add", "fix.go")
	run(t, "git", "-C", project, "commit", "-m", "fix")

	runIter, iter := 1, 1
	action := l.handlePostSignal(postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    project,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-fast",
		nextTask:   "Fix login",
	}, &runIter, &iter)

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
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Slow merge", NextID: "ralph-slow"}},
	}

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil), BaseBranch: "main"}
	l := New(Config{
		Dirs:              workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:     1,
		CallsPerHour:      80,
		TaskBackend:       backend,
		AutoMerge:         true,
		PostSignalTimeout: 50 * time.Millisecond,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "99", nil }
	l.mergeFunc = func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	writeFile(t, project, "fix.go", "package main\n")
	run(t, "git", "-C", project, "add", "fix.go")
	run(t, "git", "-C", project, "commit", "-m", "fix")

	runIter, iter := 1, 1
	done := make(chan postSignalAction, 1)
	go func() {
		done <- l.handlePostSignal(postSignalParams{
			ctx:        context.Background(),
			result:     claude.Result{SignalDetected: true},
			headBefore: "",
			workDir:    project,
			rawLogPath: filepath.Join(ralphDir, "raw.log"),
			taskID:     "ralph-slow",
			nextTask:   "Slow merge",
		}, &runIter, &iter)
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
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	// Create a script that writes env vars to a file for verification.
	envFile := filepath.Join(project, "post-task-env.txt")
	scriptPath := filepath.Join(project, "post-task.sh")
	os.WriteFile(scriptPath, []byte(fmt.Sprintf("#!/bin/sh\necho \"TASK=$RALPH_TASK_ID PR=$RALPH_PR_NUMBER MERGED=$RALPH_MERGED\" > %s\n", envFile)), 0o755)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Fix bug", NextID: "ralph-pt1"}},
	}

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil), BaseBranch: "main"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		PostTask:      scriptPath,
		AutoMerge:     true,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "99", nil }
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }
	l.mergeFunc = func(context.Context) (bool, error) { return true, nil }

	writeFile(t, project, "fix.go", "package main\n")
	run(t, "git", "-C", project, "commit", "-m", "fix")

	runIter, iter := 1, 1
	action := l.handlePostSignal(postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    project,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-pt1",
		nextTask:   "Fix bug",
	}, &runIter, &iter)

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

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, BaseBranch: "main"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		PostTask:      scriptPath,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return false, "tests failed" }

	runIter, iter := 1, 1
	action := l.handlePostSignal(postSignalParams{
		ctx:      context.Background(),
		result:   claude.Result{},
		taskID:   "ralph-pt2",
		nextTask:   "Fix bug",
	}, &runIter, &iter)

	if action != signalRetry {
		t.Fatalf("expected signalRetry, got %d", action)
	}

	if _, err := os.Stat(envFile); err == nil {
		t.Error("post-task script should not run on verification failure")
	}
}

// runPostTask warns but continues when the script exits non-zero.
func TestLoop_PostTaskScript_NonZeroExitWarns(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	scriptPath := filepath.Join(project, "post-task.sh")
	os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 1\n"), 0o755)

	var logBuf bytes.Buffer
	logger := logging.New(&logBuf)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Fix bug", NextID: "ralph-pt3"}},
	}

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logger, BaseBranch: "main"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		PostTask:      scriptPath,
	}, st, gm, logger)
	l.runner = &stubRunner{}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "50", nil }
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }

	writeFile(t, project, "fix.go", "package main\n")
	run(t, "git", "-C", project, "commit", "-m", "fix")

	runIter, iter := 1, 1
	action := l.handlePostSignal(postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    project,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-pt3",
		nextTask:   "Fix bug",
	}, &runIter, &iter)

	if action != signalComplete {
		t.Fatalf("expected signalComplete despite script failure, got %d", action)
	}

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "exited with error") {
		t.Errorf("expected warning about script error in log, got: %s", logOutput)
	}
}

// runPostTask is called in the no-commits path (signalSkipped) with merged=false.
func TestLoop_PostTaskScript_CalledOnNoCommitsPath(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	envFile := filepath.Join(project, "post-task-env.txt")
	scriptPath := filepath.Join(project, "post-task.sh")
	os.WriteFile(scriptPath, []byte(fmt.Sprintf("#!/bin/sh\necho \"TASK=$RALPH_TASK_ID PR=$RALPH_PR_NUMBER MERGED=$RALPH_MERGED\" > %s\n", envFile)), 0o755)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask:   "Fix bug", NextID: "ralph-pt4"}},
	}

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil), BaseBranch: "main"}
	headBefore := gm.HeadRev()

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		PostTask:      scriptPath,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }

	runIter, iter := 1, 1
	action := l.handlePostSignal(postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{OnSignalUsed: true},
		headBefore: headBefore,
		workDir:    project,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-pt4",
		nextTask:   "Fix bug",
	}, &runIter, &iter)

	if action != signalSkipped {
		t.Fatalf("expected signalSkipped, got %d", action)
	}

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("post-task script did not run on no-commits path: %v", err)
	}
	got := strings.TrimSpace(string(data))
	want := "TASK=ralph-pt4 PR= MERGED=false"
	if got != want {
		t.Errorf("env vars: got %q, want %q", got, want)
	}
}

// handlePostSignal fires a TaskCompleted notification after post-task when
// Notify is enabled.
func TestHandlePostSignal_NotifyEnabled_SendsTaskCompleted(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
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

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil), BaseBranch: "main"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Notify:        true,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "42", nil }
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }

	var buf bytes.Buffer
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

	writeFile(t, project, "fix.go", "package main\n")
	run(t, "git", "-C", project, "add", "fix.go")
	run(t, "git", "-C", project, "commit", "-m", "fix auth bug")

	runIter, iter := 1, 1
	l.handlePostSignal(postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true, Summary: "Fixed token expiry"},
		headBefore: "",
		workDir:    project,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-ntf",
		nextTask:   "Fix auth bug",
	}, &runIter, &iter)

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
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
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

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil), BaseBranch: "main"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Notify:        false,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "42", nil }
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }

	var buf bytes.Buffer
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

	writeFile(t, project, "fix.go", "package main\n")
	run(t, "git", "-C", project, "add", "fix.go")
	run(t, "git", "-C", project, "commit", "-m", "fix auth bug")

	runIter, iter := 1, 1
	l.handlePostSignal(postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true, Summary: "Fixed token expiry"},
		headBefore: "",
		workDir:    project,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-ntf2",
		nextTask:   "Fix auth bug",
	}, &runIter, &iter)

	got := buf.String()
	if strings.Contains(got, "Task done") {
		t.Errorf("expected no TaskCompleted notification when Notify=false, got %q", got)
	}
}

// handlePostSignal fires TaskCompleted on the no-commits path when Notify is enabled.
func TestHandlePostSignal_NotifyOnNoCommitsPath(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
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

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil), BaseBranch: "main"}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Notify:        true,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }

	var buf bytes.Buffer
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

	headRev := gm.HeadRev()

	runIter, iter := 1, 1
	action := l.handlePostSignal(postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true, Summary: "Updated README"},
		headBefore: headRev,
		workDir:    project,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-nc1",
		nextTask:   "Update docs",
	}, &runIter, &iter)

	if action != signalSkipped {
		t.Errorf("expected signalSkipped for no-commits path, got %d", action)
	}

	got := buf.String()
	if !strings.Contains(got, "Task done: [ralph-nc1] Update docs") {
		t.Errorf("expected TaskCompleted on no-commits path, got %q", got)
	}
}

// resolveByPRState sends TaskCompleted and TaskMerged when PR is already merged and Notify is enabled.
func TestResolveByPRState_Merged_NotifyEnabled(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
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

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil), BaseBranch: "main"}
	gm.GitHub = &git.StubGitHub{IsAvailable: true, PRState: "MERGED"}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Notify:        true,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}

	var buf bytes.Buffer
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

	resolved := l.resolveByPRState(context.Background(), "ralph-rm1", "Fix login", "99")
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
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
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

	// Create and push a feature branch so prChainIsHealthy passes.
	branchName := "ralph-ro1/add-cache"
	run(t, "git", "-C", project, "checkout", "-b", branchName)
	writeFile(t, project, "cache.go", "package main\n")
	run(t, "git", "-C", project, "add", "cache.go")
	run(t, "git", "-C", project, "commit", "-m", "add cache")
	run(t, "git", "-C", project, "push", "-u", "origin", branchName)
	run(t, "git", "-C", project, "checkout", "main")

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil), BaseBranch: "main"}
	gm.GitHub = &git.StubGitHub{IsAvailable: true, PRState: "OPEN", PRHead: branchName}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Notify:        true,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}

	var buf bytes.Buffer
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

	resolved := l.resolveByPRState(context.Background(), "ralph-ro1", "Add cache", "88")
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
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
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

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil), BaseBranch: "main"}
	gm.GitHub = &git.StubGitHub{IsAvailable: true, PRState: "MERGED"}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Notify:        false,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}

	var buf bytes.Buffer
	prev := notify.SetWriter(&buf)
	t.Cleanup(func() { notify.SetWriter(prev) })

	resolved := l.resolveByPRState(context.Background(), "ralph-rd1", "Fix logout", "77")
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
