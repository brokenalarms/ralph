package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/config"
)

// payloadFor builds a PreToolUse hook JSON payload with the given file_path.
func payloadFor(filePath string) string {
	return fmt.Sprintf(`{"cwd":%q,"tool_name":"Edit","tool_input":{"file_path":%q}}`, "/tmp", filePath)
}

// Proves: an Edit targeting a file inside the main project checkout — but
// outside any worktree root — is blocked, so a task session (or its subagents)
// cannot contaminate the main checkout via absolute paths.
func TestGuardEditDecision_BlocksMainCheckoutPath(t *testing.T) {
	project := "/Users/dev/project"
	block, _, err := guardEditDecision(strings.NewReader(payloadFor(project+"/src/main.go")), project)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !block {
		t.Fatal("expected edit inside main checkout to be blocked")
	}
}

// Proves: an Edit inside the session's own task worktree
// (<project>/.ralph/worktrees/...) is allowed, so the task manager can work
// normally in its assigned worktree.
func TestGuardEditDecision_AllowsSessionWorktreePath(t *testing.T) {
	project := "/Users/dev/project"
	path := project + "/.ralph/worktrees/ralph-task-20260710-01/src/main.go"
	block, _, err := guardEditDecision(strings.NewReader(payloadFor(path)), project)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block {
		t.Fatal("expected edit inside the session worktree to be allowed")
	}
}

// Proves: an Edit inside a Claude-managed subagent isolation worktree
// (<project>/.claude/worktrees/<name>) is allowed, so spawned agents editing
// their own isolation worktree are not blocked — only the main checkout is
// protected.
func TestGuardEditDecision_AllowsClaudeWorktreePath(t *testing.T) {
	project := "/Users/dev/project"
	path := project + "/.claude/worktrees/some-theme/src/main.go"
	block, _, err := guardEditDecision(strings.NewReader(payloadFor(path)), project)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block {
		t.Fatal("expected edit inside a .claude/worktrees isolation worktree to be allowed")
	}
}

// Proves: an Edit entirely outside the project directory is allowed — the
// guard protects only the configured project's main checkout.
func TestGuardEditDecision_AllowsPathOutsideProject(t *testing.T) {
	project := "/Users/dev/project"
	block, _, err := guardEditDecision(strings.NewReader(payloadFor("/Users/dev/other/file.go")), project)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if block {
		t.Fatal("expected edit outside the project dir to be allowed")
	}
}

// Proves: a relative file_path is resolved against the payload's cwd before
// the containment check, so a relative path that lands inside the main
// checkout is still blocked.
func TestGuardEditDecision_ResolvesRelativePath(t *testing.T) {
	project := "/Users/dev/project"
	// cwd is the project root; a relative "src/main.go" resolves into the
	// main checkout and must be blocked.
	payload := fmt.Sprintf(`{"cwd":%q,"tool_input":{"file_path":"src/main.go"}}`, project)
	block, resolved, err := guardEditDecision(strings.NewReader(payload), project)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !block {
		t.Fatalf("expected relative path resolved into main checkout to be blocked, resolved=%q", resolved)
	}
	if resolved != filepath.Join(project, "src/main.go") {
		t.Errorf("resolved = %q, want %q", resolved, filepath.Join(project, "src/main.go"))
	}
}

// Proves: a NotebookEdit payload (which carries notebook_path, not file_path)
// targeting the main checkout is blocked — the guard covers all three
// file-mutation tools.
func TestGuardEditDecision_BlocksNotebookPath(t *testing.T) {
	project := "/Users/dev/project"
	payload := fmt.Sprintf(`{"tool_input":{"notebook_path":%q}}`, project+"/analysis.ipynb")
	block, _, err := guardEditDecision(strings.NewReader(payload), project)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !block {
		t.Fatal("expected NotebookEdit into main checkout to be blocked")
	}
}

// Proves: handleGuardEdit returns the blocking exit code (2) when the hook
// payload targets the main checkout, so Claude Code denies the tool call.
func TestHandleGuardEdit_BlockingExitCode(t *testing.T) {
	project := t.TempDir()
	payloadPath := filepath.Join(project, "payload.json")
	if err := os.WriteFile(payloadPath, []byte(payloadFor(filepath.Join(project, "main.go"))), 0o644); err != nil {
		t.Fatalf("writing payload: %v", err)
	}

	f, err := os.Open(payloadPath)
	if err != nil {
		t.Fatalf("opening payload: %v", err)
	}
	defer f.Close()

	origStdin := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = origStdin }()

	code := handleGuardEdit(config.Subcommand{Name: "guard-edit", Args: []string{"--project-dir", project}})
	if code != guardBlockExitCode {
		t.Fatalf("handleGuardEdit exit code = %d, want %d (blocking)", code, guardBlockExitCode)
	}
	if guardBlockExitCode != 2 {
		t.Errorf("guardBlockExitCode = %d, want 2 (Claude Code's PreToolUse deny code)", guardBlockExitCode)
	}
}

// Proves: writeTaskGuardSettings writes a settings file inside the session
// worktree's .claude dir registering a PreToolUse hook that covers Edit,
// Write, and NotebookEdit and invokes the resolved ralph executable path (not
// a bare name relying on PATH) with the project dir. Nothing is written to the
// project root.
func TestWriteTaskGuardSettings(t *testing.T) {
	project := t.TempDir()
	workDir := filepath.Join(project, ".ralph", "worktrees", "ralph-task-20260710-01")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	ralphExe := "/opt/ralph/bin/ralph"

	if err := writeTaskGuardSettings(workDir, project, ralphExe); err != nil {
		t.Fatalf("writeTaskGuardSettings: %v", err)
	}

	settingsPath := filepath.Join(workDir, ".claude", "settings.local.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("reading settings file: %v", err)
	}

	var settings struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings file is not valid JSON: %v", err)
	}

	if len(settings.Hooks.PreToolUse) != 1 || len(settings.Hooks.PreToolUse[0].Hooks) != 1 {
		t.Fatalf("expected exactly one PreToolUse hook entry, got %+v", settings.Hooks.PreToolUse)
	}
	entry := settings.Hooks.PreToolUse[0]
	for _, tool := range []string{"Edit", "Write", "NotebookEdit"} {
		if !strings.Contains(entry.Matcher, tool) {
			t.Errorf("matcher %q does not cover %s", entry.Matcher, tool)
		}
	}
	cmd := entry.Hooks[0].Command
	if !strings.Contains(cmd, ralphExe) {
		t.Errorf("hook command %q does not reference resolved ralph executable %q", cmd, ralphExe)
	}
	if !strings.Contains(cmd, "guard-edit") {
		t.Errorf("hook command %q does not invoke the guard-edit subcommand", cmd)
	}
	if !strings.Contains(cmd, project) {
		t.Errorf("hook command %q does not pass the project dir %q", cmd, project)
	}

	// Nothing written to the project root itself — only inside the worktree.
	if _, err := os.Stat(filepath.Join(project, "settings.local.json")); !os.IsNotExist(err) {
		t.Errorf("guard settings must not be written to the project root")
	}
	if _, err := os.Stat(filepath.Join(project, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Errorf("guard settings must not be written to the project-root .claude dir")
	}
}
