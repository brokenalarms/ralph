package notify

import (
	"bytes"
	"testing"
)

// Tests that TaskMerged sends an OSC 9 escape sequence with the bead ID and title.
func TestTaskMerged_WithIDAndTitle(t *testing.T) {
	var buf bytes.Buffer
	writer = &buf
	t.Cleanup(func() { writer = nil })

	TaskMerged("ralph-abc", "Fix login bug")

	got := buf.String()
	want := "\033]9;Task merged: [ralph-abc] Fix login bug\007"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Tests that TaskMerged works with only a task ID (no title).
func TestTaskMerged_IDOnly(t *testing.T) {
	var buf bytes.Buffer
	writer = &buf
	t.Cleanup(func() { writer = nil })

	TaskMerged("ralph-xyz", "")

	got := buf.String()
	want := "\033]9;Task merged: [ralph-xyz]\007"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Tests that TaskMerged works with no ID or title.
func TestTaskMerged_Empty(t *testing.T) {
	var buf bytes.Buffer
	writer = &buf
	t.Cleanup(func() { writer = nil })

	TaskMerged("", "")

	got := buf.String()
	want := "\033]9;Task merged\007"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
