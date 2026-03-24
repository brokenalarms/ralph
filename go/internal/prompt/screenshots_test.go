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
	for _, s := range got {
		if !strings.Contains(s.Path, "ralph-abc-") {
			t.Errorf("unexpected path %q", s.Path)
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
// file paths and descriptions for the agent to read.
func TestFormatScreenshotContext_WithDescriptions(t *testing.T) {
	ss := []Screenshot{
		{Path: "/proj/.ralph/screenshots/ralph-abc-01-login.png", Description: "Login button misaligned"},
		{Path: "/proj/.ralph/screenshots/ralph-abc-02-modal.png", Description: ""},
	}
	got := FormatScreenshotContext(ss)

	if !strings.Contains(got, "## Screenshots") {
		t.Error("missing section header")
	}
	if !strings.Contains(got, "Read tool") {
		t.Error("missing Read tool instruction")
	}
	if !strings.Contains(got, "ralph-abc-01-login.png`") {
		t.Error("missing first screenshot path")
	}
	if !strings.Contains(got, "Login button misaligned") {
		t.Error("missing first screenshot description")
	}
	if !strings.Contains(got, "ralph-abc-02-modal.png`") {
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

// Proves: ScreenshotsForBead reads .desc sidecar files to populate descriptions.
func TestScreenshotsForBead_ReadsDescriptions(t *testing.T) {
	dir := t.TempDir()
	ssDir := filepath.Join(dir, "screenshots")
	os.MkdirAll(ssDir, 0o755)

	os.WriteFile(filepath.Join(ssDir, "ralph-abc-01-login.png"), []byte("img"), 0o644)
	os.WriteFile(filepath.Join(ssDir, "ralph-abc-01-login.png.desc"), []byte("Login button is misaligned on mobile"), 0o644)
	os.WriteFile(filepath.Join(ssDir, "ralph-abc-02-modal.png"), []byte("img"), 0o644)

	got := ScreenshotsForBead(dir, "ralph-abc")
	if len(got) != 2 {
		t.Fatalf("expected 2 screenshots, got %d", len(got))
	}
	if got[0].Description != "Login button is misaligned on mobile" {
		t.Errorf("expected description from .desc file, got %q", got[0].Description)
	}
	if got[1].Description != "" {
		t.Errorf("expected empty description when no .desc file, got %q", got[1].Description)
	}
}

// Proves: SaveScreenshot creates the screenshots directory, writes the image
// with correct naming, and creates a .desc sidecar file.
func TestSaveScreenshot(t *testing.T) {
	dir := t.TempDir()
	imgData := []byte("fake png data")

	path, err := SaveScreenshot(dir, "ralph-xyz", imgData, "broken-layout", "Header overlaps sidebar")
	if err != nil {
		t.Fatalf("SaveScreenshot: %v", err)
	}

	if !strings.HasSuffix(path, "ralph-xyz-01-broken-layout.png") {
		t.Errorf("unexpected path %q", path)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read screenshot: %v", err)
	}
	if string(content) != "fake png data" {
		t.Errorf("screenshot content mismatch")
	}

	desc, err := os.ReadFile(path + ".desc")
	if err != nil {
		t.Fatalf("read .desc: %v", err)
	}
	if string(desc) != "Header overlaps sidebar" {
		t.Errorf("description mismatch: %q", string(desc))
	}
}

// Proves: SaveScreenshot auto-increments the sequence number for multiple
// screenshots on the same bead.
func TestSaveScreenshot_AutoIncrement(t *testing.T) {
	dir := t.TempDir()
	ssDir := filepath.Join(dir, "screenshots")
	os.MkdirAll(ssDir, 0o755)
	os.WriteFile(filepath.Join(ssDir, "ralph-xyz-01-first.png"), []byte("img"), 0o644)
	os.WriteFile(filepath.Join(ssDir, "ralph-xyz-02-second.png"), []byte("img"), 0o644)

	path, err := SaveScreenshot(dir, "ralph-xyz", []byte("img"), "third-issue", "Third screenshot")
	if err != nil {
		t.Fatalf("SaveScreenshot: %v", err)
	}
	if !strings.HasSuffix(path, "ralph-xyz-03-third-issue.png") {
		t.Errorf("expected sequence 03, got %q", path)
	}
}
