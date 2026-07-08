package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/logging"
)

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
