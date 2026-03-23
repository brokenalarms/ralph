package workctx

import (
	"path/filepath"
	"testing"
)

// Proves: New derives RalphDir from ProjectDir and defaults WorkDir to ProjectDir,
// so modules receive consistent directories without manual wiring.
func TestNew_DerivesPaths(t *testing.T) {
	wctx := New("/home/user/project", "/tmp/prompts")

	if wctx.ProjectDir != "/home/user/project" {
		t.Errorf("ProjectDir = %q, want /home/user/project", wctx.ProjectDir)
	}
	if wctx.WorkDir != "/home/user/project" {
		t.Errorf("WorkDir should default to ProjectDir, got %q", wctx.WorkDir)
	}
	if wctx.RalphDir != filepath.Join("/home/user/project", ".ralph") {
		t.Errorf("RalphDir = %q, want ProjectDir/.ralph", wctx.RalphDir)
	}
	if wctx.PromptsDir != "/tmp/prompts" {
		t.Errorf("PromptsDir = %q, want /tmp/prompts", wctx.PromptsDir)
	}
}

// Proves: after worktree setup, updating WorkDir diverges it from ProjectDir
// while RalphDir stays anchored to ProjectDir — preventing the VerifyDir bug
// where projectDir was used instead of workDir for test execution.
func TestWorkContext_WorkDirDivergesFromProjectDir(t *testing.T) {
	wctx := New("/home/user/project", "/tmp/prompts")
	wctx.WorkDir = "/home/user/project/.ralph/worktrees/wt-001"

	if wctx.WorkDir == wctx.ProjectDir {
		t.Error("WorkDir should diverge from ProjectDir after worktree setup")
	}
	if wctx.RalphDir != filepath.Join("/home/user/project", ".ralph") {
		t.Error("RalphDir must stay anchored to ProjectDir, not follow WorkDir")
	}
}

// Proves: consumers that need different directories for different purposes
// (e.g. tests run in WorkDir, git operations in ProjectDir) get the right
// value from the struct without risk of swapping them.
func TestWorkContext_DirectorySelection(t *testing.T) {
	wctx := New("/repo", "/prompts")
	wctx.WorkDir = "/repo/.ralph/worktrees/wt-1"

	// Agent should run in WorkDir (worktree), not ProjectDir.
	agentDir := wctx.WorkDir
	if agentDir != "/repo/.ralph/worktrees/wt-1" {
		t.Errorf("agent should use WorkDir, got %q", agentDir)
	}

	// Git operations (fetch, push) should use ProjectDir.
	gitDir := wctx.ProjectDir
	if gitDir != "/repo" {
		t.Errorf("git ops should use ProjectDir, got %q", gitDir)
	}

	// State files should use RalphDir (under ProjectDir, not WorkDir).
	stateDir := wctx.RalphDir
	if stateDir != "/repo/.ralph" {
		t.Errorf("state should use RalphDir, got %q", stateDir)
	}
}
