package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Proves: embedded prompts are extracted under <ralphDir>/prompts — a
// durable project-local path — not a system temp dir that macOS's
// periodic cleaner can purge out from under a long-running loop.
func TestExtractEmbeddedPrompts_WritesUnderRalphDir(t *testing.T) {
	ralphDir := t.TempDir()

	dir, err := extractEmbeddedPrompts(ralphDir)
	if err != nil {
		t.Fatalf("extractEmbeddedPrompts: %v", err)
	}
	if dir != filepath.Join(ralphDir, "prompts") {
		t.Fatalf("extracted to %s, want %s", dir, filepath.Join(ralphDir, "prompts"))
	}

	data, err := os.ReadFile(filepath.Join(dir, "shared.md"))
	if err != nil {
		t.Fatalf("shared.md missing after extraction: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("shared.md extracted empty")
	}

	// No .tmp staging files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no templates extracted")
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("staging file left behind: %s", e.Name())
		}
	}
}

// Proves: re-extraction over an existing prompts dir overwrites in place
// rather than erroring — every process start refreshes the templates,
// including over a dir a previous (possibly older) process wrote.
func TestExtractEmbeddedPrompts_OverwritesExistingDir(t *testing.T) {
	ralphDir := t.TempDir()

	dir, err := extractEmbeddedPrompts(ralphDir)
	if err != nil {
		t.Fatalf("first extraction: %v", err)
	}

	// Simulate a stale copy left by an older binary.
	stale := filepath.Join(dir, "shared.md")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	if _, err := extractEmbeddedPrompts(ralphDir); err != nil {
		t.Fatalf("re-extraction: %v", err)
	}

	data, err := os.ReadFile(stale)
	if err != nil {
		t.Fatalf("shared.md missing after re-extraction: %v", err)
	}
	if string(data) == "stale" {
		t.Fatal("re-extraction did not overwrite stale template")
	}
}
