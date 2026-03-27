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

// Tests that TaskCompleted sends an OSC 9 notification with bead ID, title, and summary.
func TestTaskCompleted_Full(t *testing.T) {
	var buf bytes.Buffer
	writer = &buf
	t.Cleanup(func() { writer = nil })

	TaskCompleted("ralph-abc", "Fix login bug", "Fixed auth token expiry handling")

	got := buf.String()
	want := "\033]9;Task done: [ralph-abc] Fix login bug — Fixed auth token expiry handling\007"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Tests that TaskCompleted works with ID and title but no summary.
func TestTaskCompleted_NoSummary(t *testing.T) {
	var buf bytes.Buffer
	writer = &buf
	t.Cleanup(func() { writer = nil })

	TaskCompleted("ralph-xyz", "Add caching", "")

	got := buf.String()
	want := "\033]9;Task done: [ralph-xyz] Add caching\007"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Tests that TaskCompleted with empty fields still produces a valid notification.
func TestTaskCompleted_Empty(t *testing.T) {
	var buf bytes.Buffer
	writer = &buf
	t.Cleanup(func() { writer = nil })

	TaskCompleted("", "", "")

	got := buf.String()
	want := "\033]9;Task done\007"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
