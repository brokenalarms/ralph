package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Proves: ScreenshotsForBead returns files matching the bead ID prefix
// and ignores files belonging to other beads.
func TestScreenshotsForBead_MatchesPrefix(t *testing.T) {
	dir := t.TempDir()
	ssDir := filepath.Join(dir, "screenshots")
	os.MkdirAll(ssDir, 0o755)

	os.WriteFile(filepath.Join(ssDir, "ralph-abc-01-login-broken.png"), []byte("img"), 0o644)
	os.WriteFile(filepath.Join(ssDir, "ralph-abc-02-modal-gap.png"), []byte("img"), 0o644)
	os.WriteFile(filepath.Join(ssDir, "ralph-xyz-01-unrelated.png"), []byte("img"), 0o644)

	got := ScreenshotsForBead(dir, "ralph-abc")
	if len(got) != 2 {
		t.Fatalf("expected 2 screenshots, got %d: %v", len(got), got)
	}
	for _, p := range got {
		if !strings.Contains(p, "ralph-abc-") {
			t.Errorf("unexpected path %q", p)
		}
	}
}

// Proves: ScreenshotsForBead returns nil when no screenshots directory exists.
func TestScreenshotsForBead_NoDir(t *testing.T) {
	got := ScreenshotsForBead(t.TempDir(), "ralph-abc")
	if got != nil {
		t.Errorf("expected nil for missing dir, got %v", got)
	}
}

// Proves: ScreenshotsForBead returns nil for empty bead ID.
func TestScreenshotsForBead_EmptyID(t *testing.T) {
	got := ScreenshotsForBead("/some/dir", "")
	if got != nil {
		t.Errorf("expected nil for empty ID, got %v", got)
	}
}

// Proves: ScreenshotsForBead skips subdirectories even if they match the prefix.
func TestScreenshotsForBead_SkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	ssDir := filepath.Join(dir, "screenshots")
	os.MkdirAll(filepath.Join(ssDir, "ralph-abc-subdir"), 0o755)
	os.WriteFile(filepath.Join(ssDir, "ralph-abc-01-shot.png"), []byte("img"), 0o644)

	got := ScreenshotsForBead(dir, "ralph-abc")
	if len(got) != 1 {
		t.Fatalf("expected 1 screenshot (skipping dir), got %d", len(got))
	}
}

// Proves: FormatScreenshotContext produces a prompt section with numbered
// file paths for the agent to read.
func TestFormatScreenshotContext_WithPaths(t *testing.T) {
	paths := []string{
		"/proj/.ralph/screenshots/ralph-abc-01-login.png",
		"/proj/.ralph/screenshots/ralph-abc-02-modal.png",
	}
	got := FormatScreenshotContext(paths)

	if !strings.Contains(got, "## Screenshots") {
		t.Error("missing section header")
	}
	if !strings.Contains(got, "Read tool") {
		t.Error("missing Read tool instruction")
	}
	if !strings.Contains(got, "1. `/proj/.ralph/screenshots/ralph-abc-01-login.png`") {
		t.Error("missing first screenshot path")
	}
	if !strings.Contains(got, "2. `/proj/.ralph/screenshots/ralph-abc-02-modal.png`") {
		t.Error("missing second screenshot path")
	}
}

// Proves: FormatScreenshotContext returns empty string when no screenshots exist.
func TestFormatScreenshotContext_Empty(t *testing.T) {
	got := FormatScreenshotContext(nil)
	if got != "" {
		t.Errorf("expected empty string for nil paths, got %q", got)
	}
}
