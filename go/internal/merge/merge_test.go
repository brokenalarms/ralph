package merge

import (
	"strings"
	"testing"
)

// Proves: auto_merge_current_branch returns cleanly when there is no
// worktree branch set (e.g. --no-worktree mode).
func TestAutoMerge_SkipsWhenNoWorktreeBranch(t *testing.T) {
	msg, err := AutoMerge("", "/tmp/project", "/tmp/project")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if msg != "" {
		t.Errorf("expected empty message, got %q", msg)
	}
}

// Proves: auto_merge_current_branch skips when work dir is the project
// dir (no git worktree isolation).
func TestAutoMerge_SkipsWhenWorkDirIsProjectDir(t *testing.T) {
	msg, err := AutoMerge("ralph/project/01-some-task", "/tmp/project", "/tmp/project")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if msg != "" {
		t.Errorf("expected empty message, got %q", msg)
	}
}

// Proves: auto_merge_current_branch fails gracefully when gh CLI is
// not available, with a descriptive error message.
func TestAutoMerge_WarnsWithoutGhCLI(t *testing.T) {
	// Save original PATH and set to empty so gh isn't found.
	t.Setenv("PATH", "/nonexistent")

	_, err := AutoMerge("ralph/project/01-task", "/tmp/worktree", "/tmp/project")
	if err == nil {
		t.Fatal("expected error when gh is not available")
	}
	if !strings.Contains(err.Error(), "gh CLI not found") {
		t.Errorf("error should mention 'gh CLI not found', got: %v", err)
	}
}
