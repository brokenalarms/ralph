package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// --verify-build script runs before pre-iteration tests. When the script
// exits non-zero, the build failure output is injected into the agent prompt
// so the agent knows the build is broken and must fix it first.
func TestLoop_VerifyBuild_FailureInjectedIntoPrompt(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	// Write templates with {{TEST_STATUS}} so the build failure context lands in the prompt.
	for _, name := range []string{"shared.md", "reflection.md", "signal.md", "feedback.md", "execution-bd.md", "bead-creation.md"} {
		os.WriteFile(filepath.Join(promptsDir, name), []byte("test"), 0o644)
	}
	os.WriteFile(filepath.Join(promptsDir, "internal.md"),
		[]byte("Assumptions\n{{TEST_STATUS}}\n{{TASK_INSTRUCTIONS}}\n{{ATTEMPT_HISTORY}}"), 0o644)

	// Create a verify-build script that exits non-zero with a specific message.
	scriptPath := filepath.Join(dir, "verify-build.sh")
	os.WriteFile(scriptPath, []byte("#!/bin/sh\necho 'Xcode project file is corrupted'\nexit 1\n"), 0o755)

	var capturedPrompt string
	backend := &testutil.StubBackend{
		Remaining:    1,
		Total:        1,
		NextTask:     "Fix login",
		NextID:       "ralph-vb1",
		BackendLabel: "beads",
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		VerifyBuild:   scriptPath,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})

	inner := &stubRunner{
		onRun: func() {
			backend.Remaining = 0
			backend.Completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	l.runner = &promptCapturingRunner{inner: inner, captured: &capturedPrompt}

	_ = l.Run(context.Background())

	if !strings.Contains(capturedPrompt, "BUILD IS BROKEN") {
		t.Errorf("prompt should include build failure instruction, got: %s", capturedPrompt[:min(300, len(capturedPrompt))])
	}
	if !strings.Contains(capturedPrompt, "Xcode project file is corrupted") {
		t.Errorf("prompt should include build failure output, got: %s", capturedPrompt[:min(300, len(capturedPrompt))])
	}
}

// --verify-build script exits 0: no build failure context is injected into
// the prompt, and the iteration proceeds normally.
func TestLoop_VerifyBuild_PassDoesNotInjectContext(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	for _, name := range []string{"shared.md", "reflection.md", "signal.md", "feedback.md", "execution-bd.md", "bead-creation.md"} {
		os.WriteFile(filepath.Join(promptsDir, name), []byte("test"), 0o644)
	}
	os.WriteFile(filepath.Join(promptsDir, "internal.md"),
		[]byte("Assumptions\n{{TEST_STATUS}}\n{{TASK_INSTRUCTIONS}}\n{{ATTEMPT_HISTORY}}"), 0o644)

	// Script that passes (exit 0).
	scriptPath := filepath.Join(dir, "verify-build.sh")
	os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0o755)

	var capturedPrompt string
	backend := &testutil.StubBackend{
		Remaining:    1,
		Total:        1,
		NextTask:     "Fix login",
		NextID:       "ralph-vb2",
		BackendLabel: "beads",
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		VerifyBuild:   scriptPath,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})

	inner := &stubRunner{
		onRun: func() {
			backend.Remaining = 0
			backend.Completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	l.runner = &promptCapturingRunner{inner: inner, captured: &capturedPrompt}

	_ = l.Run(context.Background())

	if strings.Contains(capturedPrompt, "BUILD IS BROKEN") {
		t.Error("prompt should not include build failure context when script passes")
	}
}

// When --verify-build is not set, loop behavior is unchanged.
func TestLoop_VerifyBuild_NotConfigured_NoEffect(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	for _, name := range []string{"shared.md", "reflection.md", "signal.md", "feedback.md", "execution-bd.md", "bead-creation.md"} {
		os.WriteFile(filepath.Join(promptsDir, name), []byte("test"), 0o644)
	}
	os.WriteFile(filepath.Join(promptsDir, "internal.md"),
		[]byte("Assumptions\n{{TEST_STATUS}}\n{{TASK_INSTRUCTIONS}}\n{{ATTEMPT_HISTORY}}"), 0o644)

	var capturedPrompt string
	backend := &testutil.StubBackend{
		Remaining:    1,
		Total:        1,
		NextTask:     "Fix login",
		NextID:       "ralph-vb3",
		BackendLabel: "beads",
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		// VerifyBuild intentionally not set
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})

	inner := &stubRunner{
		onRun: func() {
			backend.Remaining = 0
			backend.Completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	l.runner = &promptCapturingRunner{inner: inner, captured: &capturedPrompt}

	_ = l.Run(context.Background())

	if strings.Contains(capturedPrompt, "BUILD IS BROKEN") {
		t.Error("prompt should not contain build failure context when VerifyBuild is not set")
	}
}

// runVerifyBuild runs the script before pre-iteration tests — not after
// them — so the agent prompt combines both statuses correctly.
func TestLoop_VerifyBuild_RunsBeforePreIterationTests(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	for _, name := range []string{"shared.md", "reflection.md", "signal.md", "feedback.md", "execution-bd.md", "bead-creation.md"} {
		os.WriteFile(filepath.Join(promptsDir, name), []byte("test"), 0o644)
	}
	os.WriteFile(filepath.Join(promptsDir, "internal.md"),
		[]byte("Assumptions\n{{TEST_STATUS}}\n{{TASK_INSTRUCTIONS}}\n{{ATTEMPT_HISTORY}}"), 0o644)

	// Script that fails — build is broken.
	scriptPath := filepath.Join(dir, "verify-build.sh")
	os.WriteFile(scriptPath, []byte("#!/bin/sh\necho 'bundle invalid'\nexit 2\n"), 0o755)

	// Also create a passing test suite so pre-iteration tests pass.
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\ttrue\n"), 0o644)

	var capturedPrompt string
	backend := &testutil.StubBackend{
		Remaining:    1,
		Total:        1,
		NextTask:     "Fix login",
		NextID:       "ralph-vb4",
		BackendLabel: "beads",
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		VerifyBuild:   scriptPath,
		VerifyDir:     dir,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})

	inner := &stubRunner{
		onRun: func() {
			backend.Remaining = 0
			backend.Completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	l.runner = &promptCapturingRunner{inner: inner, captured: &capturedPrompt}

	_ = l.Run(context.Background())

	// Both build failure and test status appear in the prompt.
	if !strings.Contains(capturedPrompt, "BUILD IS BROKEN") {
		t.Error("prompt should contain build failure context")
	}
	if !strings.Contains(capturedPrompt, "bundle invalid") {
		t.Error("prompt should contain build failure output")
	}
	if !strings.Contains(capturedPrompt, "tests passing") {
		t.Errorf("prompt should also contain test status, got: %s", fmt.Sprintf("%.300s", capturedPrompt))
	}
}
