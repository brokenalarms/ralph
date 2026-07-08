package tasks

import (
	"testing"
	"time"
)

// Proves: UnauditedClosures builds the recent-closure audit window — it
// keeps only beads assigned to the given assignee, closed within the given
// window of now, and not yet stamped with "audited" metadata; it excludes
// non-matching assignees, closures older than the window, and beads already
// audited, and it is unaffected by bd's default 50-result list limit since
// it applies no result-count cap of its own.
func TestUnauditedClosures_FiltersAssigneeWindowAndAuditedMetadata(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	window := 72 * time.Hour

	closed := []ClosedTaskInfo{
		{ID: "ralph-in-window", Assignee: "ralph-loop", ClosedAt: now.Add(-1 * time.Hour), Metadata: nil},
		{ID: "ralph-self-work", Assignee: "ralph-task", ClosedAt: now.Add(-1 * time.Hour), Metadata: nil},
		{ID: "ralph-stale", Assignee: "ralph-loop", ClosedAt: now.Add(-73 * time.Hour), Metadata: nil},
		{ID: "ralph-already-audited", Assignee: "ralph-loop", ClosedAt: now.Add(-2 * time.Hour), Metadata: map[string]string{"audited": "1720180800"}},
	}

	got := UnauditedClosures(closed, "ralph-loop", now, window)

	if len(got) != 1 {
		t.Fatalf("UnauditedClosures() = %d items, want 1: %+v", len(got), got)
	}
	if got[0].ID != "ralph-in-window" {
		t.Errorf("UnauditedClosures()[0].ID = %q, want ralph-in-window", got[0].ID)
	}
}

// Proves: UnauditedClosures applies no result-count cap of its own — passing
// a 60-item closed set (larger than bd list's default 50-result truncation)
// returns all 60 matching beads, unaffected by that unrelated CLI default.
func TestUnauditedClosures_UnaffectedByDefaultListLimit(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	window := 72 * time.Hour

	closed := make([]ClosedTaskInfo, 60)
	for i := range closed {
		closed[i] = ClosedTaskInfo{
			ID:       "ralph-bulk",
			Assignee: "ralph-loop",
			ClosedAt: now.Add(-1 * time.Hour),
		}
	}

	got := UnauditedClosures(closed, "ralph-loop", now, window)

	if len(got) != 60 {
		t.Errorf("UnauditedClosures() = %d items, want 60 (no implicit truncation)", len(got))
	}
}

// Proves: UnauditedClosures returns nil (an empty window block) when no
// closures qualify — the caller omits the "$ audit-window" block entirely.
func TestUnauditedClosures_EmptyWhenNothingQualifies(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

	got := UnauditedClosures(nil, "ralph-loop", now, 72*time.Hour)

	if len(got) != 0 {
		t.Errorf("UnauditedClosures(nil, ...) = %d items, want 0", len(got))
	}
}
