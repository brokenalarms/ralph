package loop

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// After Ctrl+C (cancelled context), runAgent must return actionDone immediately
// without calling the runner — no "Agent model:" or test log lines should appear.
func TestRunAgent_CancelledContext_ReturnsImmediately(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	runnerCalled := false
	logger := logging.New(nil)
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:        st,
		Git:          git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		TaskBackend:  &testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix something", NextID: "ralph-t1"},
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = &stubRunner{onRun: func() { runnerCalled = true }}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := l.runAgent(ctx, taskContext{id: "ralph-t1", title: "Fix something"}, 1)

	if result.action != actionDone {
		t.Errorf("expected actionDone, got %v", result.action)
	}
	if runnerCalled {
		t.Error("runner must not be called when context is cancelled")
	}
}

// After Ctrl+C, runVerifyBuild must return "" immediately without running
// the script or emitting any log lines.
func TestRunVerifyBuild_CancelledContext_SkipsExecution(t *testing.T) {
	dir := t.TempDir()

	scriptPath := filepath.Join(dir, "verify-build.sh")
	os.WriteFile(scriptPath, []byte("#!/bin/sh\necho 'ran build check'\nexit 0\n"), 0o755)

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := runVerifyBuild(ctx, runVerifyBuildParams{
		verifyBuild: scriptPath,
		projectDir:  dir,
		testTimeout: 30 * time.Second,
		logger:      logger,
	})

	if result != "" {
		t.Errorf("expected empty string when cancelled, got %q", result)
	}
	if logBuf.Len() > 0 {
		t.Errorf("expected no log output when cancelled, got: %s", logBuf.String())
	}
}

// After Ctrl+C, runPreIterationTests must return "" immediately without
// emitting "Running test suite" or compile-check log lines.
func TestRunPreIterationTests_CancelledContext_SkipsTests(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	l := New(cfg, Modules{
		State:        st,
		Git:          git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		TaskBackend:  &testutil.StubBackend{Remaining: 0},
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := l.runPreIterationTests(ctx)

	if result != "" {
		t.Errorf("expected empty string when cancelled, got %q", result)
	}
	if logBuf.Len() > 0 {
		t.Errorf("expected no log output when cancelled, got: %s", logBuf.String())
	}
}
