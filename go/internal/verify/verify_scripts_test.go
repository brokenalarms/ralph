package verify

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// After Ctrl+C, RunVerifyBuild must return "" immediately without running
// the script or emitting any log lines.
func TestRunVerifyBuild_CancelledContext_SkipsExecution(t *testing.T) {
	dir := t.TempDir()

	scriptPath := filepath.Join(dir, "verify-build.sh")
	os.WriteFile(scriptPath, []byte("#!/bin/sh\necho 'ran build check'\nexit 0\n"), 0o755)

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := RunVerifyBuild(ctx, RunVerifyBuildParams{
		VerifyBuild: scriptPath,
		ProjectDir:  dir,
		TestTimeout: 30 * time.Second,
		Logger:      logger,
	})

	if result != "" {
		t.Errorf("expected empty string when cancelled, got %q", result)
	}
	if logBuf.Len() > 0 {
		t.Errorf("expected no log output when cancelled, got: %s", logBuf.String())
	}
}

// RunVerifyBuild sources its "build is broken" instruction from the
// status-build-broken.md prompt template instead of a hardcoded Go string —
// a distinctive marker written to the template must appear verbatim in the
// returned message.
func TestRunVerifyBuild_FailureMessage_LoadedFromTemplate(t *testing.T) {
	dir := t.TempDir()
	promptsDir := t.TempDir()

	os.WriteFile(filepath.Join(promptsDir, "status-build-broken.md"),
		[]byte("- CUSTOM BUILD BROKEN MARKER"), 0o644)

	scriptPath := filepath.Join(dir, "verify-build.sh")
	os.WriteFile(scriptPath, []byte("#!/bin/sh\necho 'boom'\nexit 1\n"), 0o755)

	result := RunVerifyBuild(context.Background(), RunVerifyBuildParams{
		VerifyBuild: scriptPath,
		ProjectDir:  dir,
		PromptsDir:  promptsDir,
		TestTimeout: 30 * time.Second,
		Logger:      logging.New(nil),
	})

	if !strings.Contains(result, "CUSTOM BUILD BROKEN MARKER") {
		t.Errorf("expected message sourced from status-build-broken.md template, got: %q", result)
	}
	if !strings.Contains(result, "boom") {
		t.Errorf("expected build failure output included, got: %q", result)
	}
}

// RunPostTask passes RALPH_PROJECT_DIR set to projectDir so project-level
// scripts like sync-and-build.sh can operate on the main checkout instead
// of the worktree branch.
func TestRunPostTask_PassesProjectDir(t *testing.T) {
	worktree := t.TempDir()
	project := t.TempDir()

	envFile := filepath.Join(worktree, "env.txt")
	scriptPath := filepath.Join(worktree, "post-task.sh")
	os.WriteFile(scriptPath, []byte(fmt.Sprintf("#!/bin/sh\necho \"$RALPH_PROJECT_DIR\" > %s\n", envFile)), 0o755)

	RunPostTask(context.Background(), RunPostTaskParams{
		PostTask:    scriptPath,
		WorktreeDir: worktree,
		ProjectDir:  project,
		Logger:      logging.New(nil),
	}, "ralph-test", 0, false)

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("post-task script did not run: %v", err)
	}
	got := strings.TrimSpace(string(data))
	wantResolved, _ := filepath.EvalSymlinks(project)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("RALPH_PROJECT_DIR=%q, want %q", got, project)
	}
}

// RunPostTask runs the script from worktreeDir, not projectDir, so
// post-task scripts cannot dirty the main checkout.
func TestRunPostTask_RunsInWorktreeDir(t *testing.T) {
	worktree := t.TempDir()
	project := t.TempDir()

	cwdFile := filepath.Join(worktree, "cwd.txt")
	scriptPath := filepath.Join(worktree, "post-task.sh")
	os.WriteFile(scriptPath, []byte(fmt.Sprintf("#!/bin/sh\npwd > %s\n", cwdFile)), 0o755)

	RunPostTask(context.Background(), RunPostTaskParams{
		PostTask:    scriptPath,
		WorktreeDir: worktree,
		ProjectDir:  project,
		Logger:      logging.New(nil),
	}, "ralph-test", 0, true)

	got := strings.TrimSpace(func() string {
		data, _ := os.ReadFile(cwdFile)
		return string(data)
	}())
	// Resolve symlinks for macOS /var → /private/var.
	wantResolved, _ := filepath.EvalSymlinks(worktree)
	if got != wantResolved {
		t.Errorf("post-task ran in %q, want worktreeDir %q", got, wantResolved)
	}
}

// RunPostTask runs the PostTask config value even when package.json also has
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

	RunPostTask(context.Background(), RunPostTaskParams{
		PostTask:    cfgScript,
		WorktreeDir: dir,
		ProjectDir:  dir,
		Logger:      logging.New(nil),
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
