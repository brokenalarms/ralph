package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Proves: ReadReflections reads all .md files from the reflections directory
// and concatenates them with separators.
func TestReadReflections_ReadsAllFiles(t *testing.T) {
	dir := t.TempDir()
	refDir := filepath.Join(dir, "reflections")
	os.MkdirAll(refDir, 0o755)

	os.WriteFile(filepath.Join(refDir, "ralph-abc.md"), []byte("# Task ABC\n\n## What was discovered\n- Found bug X"), 0o644)
	os.WriteFile(filepath.Join(refDir, "ralph-def.md"), []byte("# Task DEF\n\n## What was discovered\n- Found pattern Y"), 0o644)

	result, err := ReadReflections(dir)
	if err != nil {
		t.Fatalf("ReadReflections: %v", err)
	}
	if !strings.Contains(result, "Task ABC") {
		t.Error("missing content from ralph-abc.md")
	}
	if !strings.Contains(result, "Task DEF") {
		t.Error("missing content from ralph-def.md")
	}
	if !strings.Contains(result, "---") {
		t.Error("missing separator between reflections")
	}
}

// Proves: ReadReflections returns empty string when no reflections directory exists.
func TestReadReflections_MissingDir(t *testing.T) {
	result, err := ReadReflections(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Error("expected empty string for missing dir")
	}
}

// Proves: ReadReflections skips non-.md files in the reflections directory.
func TestReadReflections_SkipsNonMarkdown(t *testing.T) {
	dir := t.TempDir()
	refDir := filepath.Join(dir, "reflections")
	os.MkdirAll(refDir, 0o755)

	os.WriteFile(filepath.Join(refDir, "ralph-abc.md"), []byte("# ABC"), 0o644)
	os.WriteFile(filepath.Join(refDir, "notes.txt"), []byte("should be skipped"), 0o644)

	result, err := ReadReflections(dir)
	if err != nil {
		t.Fatalf("ReadReflections: %v", err)
	}
	if strings.Contains(result, "should be skipped") {
		t.Error("non-.md files should be skipped")
	}
	if !strings.Contains(result, "ABC") {
		t.Error("should include .md files")
	}
}

// Proves: ArchiveReflections moves .md files to reflections/archived/ and
// returns the task IDs that were archived.
func TestArchiveReflections_MovesFilesToArchived(t *testing.T) {
	dir := t.TempDir()
	refDir := filepath.Join(dir, "reflections")
	os.MkdirAll(refDir, 0o755)

	os.WriteFile(filepath.Join(refDir, "ralph-abc.md"), []byte("# Task ABC"), 0o644)
	os.WriteFile(filepath.Join(refDir, "ralph-def.md"), []byte("# Task DEF"), 0o644)

	archived, err := ArchiveReflections(dir)
	if err != nil {
		t.Fatalf("ArchiveReflections: %v", err)
	}

	if len(archived) != 2 {
		t.Fatalf("expected 2 archived, got %d", len(archived))
	}

	// Source files should be gone
	if _, err := os.Stat(filepath.Join(refDir, "ralph-abc.md")); !os.IsNotExist(err) {
		t.Error("ralph-abc.md should be removed from reflections/")
	}

	// Archived files should exist
	data, err := os.ReadFile(filepath.Join(refDir, "archived", "ralph-abc.md"))
	if err != nil {
		t.Fatal("ralph-abc.md should exist in archived/")
	}
	if string(data) != "# Task ABC" {
		t.Error("archived content should be preserved")
	}

	// ReadReflections should return empty after archiving
	result, err := ReadReflections(dir)
	if err != nil {
		t.Fatalf("ReadReflections: %v", err)
	}
	if result != "" {
		t.Error("ReadReflections should return empty after archiving")
	}
}

// Proves: ArchiveReflections is a no-op when no reflections directory exists.
func TestArchiveReflections_NoopWhenEmpty(t *testing.T) {
	archived, err := ArchiveReflections(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(archived) != 0 {
		t.Error("expected empty list for missing dir")
	}
}

// Proves: BuildReviewPrompt includes the four post-mortem responsibilities
// when reflections content is provided.
func TestBuildReviewPrompt_PostMortemResponsibilities(t *testing.T) {
	dir := promptsDir(t)
	reflections := "# Task ABC\n## What was discovered\n- Found recurring pattern"

	result, err := BuildReviewPrompt(dir, "/tmp/project", "/tmp/project/.ralph", reflections)
	if err != nil {
		t.Fatalf("BuildReviewPrompt error: %v", err)
	}

	// Verify structural properties, not prompt wording.
	if result == "" {
		t.Error("review prompt should not be empty")
	}
	if !strings.Contains(result, "Found recurring pattern") {
		t.Error("review prompt should include the reflections content")
	}
}

// Proves: BuildReviewPrompt includes the actual reflection content in the prompt
// so the agent can analyze it.
func TestBuildReviewPrompt_IncludesReflectionContent(t *testing.T) {
	dir := promptsDir(t)
	reflections := "# Task XYZ\n## What was discovered\n- Exit code handling issue"

	result, err := BuildReviewPrompt(dir, "/tmp/project", "/tmp/project/.ralph", reflections)
	if err != nil {
		t.Fatalf("BuildReviewPrompt error: %v", err)
	}

	if !strings.Contains(result, "Exit code handling issue") {
		t.Error("review prompt should include reflection content")
	}
}

// Proves: BuildReviewPrompt works with empty reflections, falling back to
// refactor-only mode.
func TestBuildReviewPrompt_EmptyReflections(t *testing.T) {
	dir := promptsDir(t)

	result, err := BuildReviewPrompt(dir, "/tmp/project", "/tmp/project/.ralph", "")
	if err != nil {
		t.Fatalf("BuildReviewPrompt error: %v", err)
	}

	if !strings.Contains(result, "No reflections found") {
		t.Error("should indicate when no reflections are available")
	}
	if !strings.Contains(result, "Refactor Opportunities") {
		t.Error("should still include refactor section even without reflections")
	}
}

// Proves: BuildReviewPrompt instructs the agent to create beads for approved actions.
func TestBuildReviewPrompt_CreatesBeadsForActions(t *testing.T) {
	dir := promptsDir(t)

	result, err := BuildReviewPrompt(dir, "/tmp/project", "/tmp/project/.ralph", "some reflections")
	if err != nil {
		t.Fatalf("BuildReviewPrompt error: %v", err)
	}

	if !strings.Contains(result, "bd create") {
		t.Error("review prompt should instruct creating beads for approved actions")
	}
}

// Proves: ReviewBootstrapPrompt is non-empty so the review session auto-starts
// its analysis without waiting for user input.
func TestReviewBootstrapPrompt_NonEmpty(t *testing.T) {
	if ReviewBootstrapPrompt == "" {
		t.Fatal("ReviewBootstrapPrompt must be non-empty to trigger startup")
	}
}
