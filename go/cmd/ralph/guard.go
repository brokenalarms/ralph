package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/config"
)

// guardBlockExitCode is the exit code a PreToolUse hook returns to block the
// tool call. Claude Code treats exit code 2 as "deny the tool and surface the
// hook's stderr to the model"; any other non-zero code is a non-blocking error.
const guardBlockExitCode = 2

// guardEditToolMatcher is the PreToolUse matcher for the file-mutation tools
// the main-checkout guard covers.
const guardEditToolMatcher = "Edit|Write|NotebookEdit"

// hookPayload is the subset of the Claude Code PreToolUse hook JSON that the
// guard reads from stdin. The target path lives under tool_input.file_path for
// Edit/Write and tool_input.notebook_path for NotebookEdit; cwd is the
// session's working directory, used to resolve a relative target path.
type hookPayload struct {
	CWD       string `json:"cwd"`
	ToolInput struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"tool_input"`
}

// guardEditDecision reports whether a file-mutation tool call must be blocked.
// It returns block=true when the resolved target path is inside projectDir but
// NOT inside an allowed worktree root (<project>/.ralph/worktrees or
// <project>/.claude/worktrees) — i.e. the call targets the protected main
// checkout. Paths outside projectDir entirely, and paths inside either
// worktree root (a ralph task worktree or a Claude-managed subagent isolation
// worktree), are allowed. A relative target is resolved against the payload's
// cwd (falling back to the process working directory) before checking.
func guardEditDecision(payload io.Reader, projectDir string) (block bool, resolvedPath string, err error) {
	data, err := io.ReadAll(payload)
	if err != nil {
		return false, "", err
	}
	var p hookPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return false, "", fmt.Errorf("parsing hook payload: %w", err)
	}

	target := p.ToolInput.FilePath
	if target == "" {
		target = p.ToolInput.NotebookPath
	}
	if target == "" {
		// No file path in the payload — nothing to protect.
		return false, "", nil
	}

	resolved := target
	if !filepath.IsAbs(resolved) {
		base := p.CWD
		if base == "" {
			base, _ = os.Getwd()
		}
		resolved = filepath.Join(base, resolved)
	}
	resolved = filepath.Clean(resolved)

	projectDir = filepath.Clean(projectDir)
	if !pathWithin(projectDir, resolved) {
		return false, resolved, nil
	}
	for _, root := range []string{
		filepath.Join(projectDir, ".ralph", "worktrees"),
		filepath.Join(projectDir, ".claude", "worktrees"),
	} {
		if pathWithin(root, resolved) {
			return false, resolved, nil
		}
	}
	return true, resolved, nil
}

// pathWithin reports whether child is parent itself or lies beneath it. It
// compares cleaned paths via filepath.Rel so that a sibling sharing a name
// prefix (e.g. /a/bc under /a/b) is not treated as contained.
func pathWithin(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// handleGuardEdit is the hidden `ralph guard-edit` subcommand. It reads a
// PreToolUse hook payload from stdin and blocks the tool call (guardBlockExitCode)
// when it targets the protected main checkout. A malformed payload fails open
// (exit 0) so a hook glitch never wedges legitimate editing.
func handleGuardEdit(sub config.Subcommand) int {
	projectDir := ""
	for i := 0; i < len(sub.Args); i++ {
		if sub.Args[i] == "--project-dir" && i+1 < len(sub.Args) {
			projectDir = sub.Args[i+1]
			i++
		}
	}

	block, path, err := guardEditDecision(os.Stdin, projectDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ralph guard-edit: %v\n", err)
		return 0
	}
	if block {
		fmt.Fprintf(os.Stderr,
			"ralph: refusing to edit %s — it is inside the main project checkout %s. "+
				"ralph task sessions must only edit files inside their assigned worktree "+
				"(under .ralph/worktrees/ or .claude/worktrees/). Use the worktree path, not an absolute path into the main checkout.\n",
			path, projectDir)
		return guardBlockExitCode
	}
	return 0
}

// writeTaskGuardSettings installs the main-checkout guard into the task
// session's worktree by writing a Claude Code settings file that registers a
// PreToolUse hook for Edit/Write/NotebookEdit. The hook shells out to
// `<ralphExe> guard-edit --project-dir <projectDir>`, which blocks edits into
// the main checkout. The file is written under the worktree's own .claude/
// directory (settings.local.json, additive to any tracked settings.json) so it
// loads at session start and is removed when the worktree is cleaned up.
// Nothing is written to the project root or user-level Claude settings.
func writeTaskGuardSettings(workDir, projectDir, ralphExe string) error {
	hookCmd := fmt.Sprintf("%s guard-edit --project-dir %s", shellQuote(ralphExe), shellQuote(projectDir))
	settings := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": guardEditToolMatcher,
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": hookCmd,
						},
					},
				},
			},
		},
	}

	claudeDir := filepath.Join(workDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", claudeDir, err)
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	dest := filepath.Join(claudeDir, "settings.local.json")
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	return nil
}

// shellQuote wraps s in single quotes for safe embedding in the hook command
// string, which Claude Code executes via the shell. Any embedded single quote
// is escaped using the '\” idiom.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
