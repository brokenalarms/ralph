package attempts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestTracker(t *testing.T) *Tracker {
	t.Helper()
	return New(Config{RalphDir: filepath.Join(t.TempDir(), ".ralph")})
}

// Proves: attempt log files are created in the attempts directory
// with the task id as filename.
func TestRecord_CreatesFileForBDTask(t *testing.T) {
	tr := newTestTracker(t)
	err := tr.Record("ralph-abc", "Fix the bug", "did stuff", "2 files changed", "continue")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	path := filepath.Join(tr.attemptsDir(), "ralph-abc.log")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected attempt file at %s", path)
	}
}

// Proves: tasks without an ID use a slugified task name as the attempt key.
func TestRecord_UsesSlugifiedNameWhenNoTaskID(t *testing.T) {
	tr := newTestTracker(t)
	err := tr.Record("", "Fix the auth bug", "tried auth", "", "continue")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	path := filepath.Join(tr.attemptsDir(), "fix-the-auth-bug.log")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected attempt file at %s", path)
	}
}

// Proves: multiple attempts on the same task append sequentially
// with incrementing numbers.
func TestRecord_AppendsWithIncrementingAttemptNumbers(t *testing.T) {
	tr := newTestTracker(t)
	tr.Record("t1", "Task one", "first try", "", "continue")
	tr.Record("t1", "Task one", "second try", "", "warn:stuck")

	content := tr.Read("t1", "Task one")
	if !strings.Contains(content, "### Attempt 1") {
		t.Error("missing Attempt 1")
	}
	if !strings.Contains(content, "### Attempt 2") {
		t.Error("missing Attempt 2")
	}
	if !strings.Contains(content, "first try") {
		t.Error("missing first try summary")
	}
	if !strings.Contains(content, "second try") {
		t.Error("missing second try summary")
	}
}

// Proves: recorded attempts include the summary, diff stat, and
// analysis result.
func TestRecord_CapturesSummaryDiffStatAndAnalysis(t *testing.T) {
	tr := newTestTracker(t)
	tr.Record("t2", "Deploy service", "deployed v2", "3 files changed, 10 insertions", "continue")

	content := tr.Read("t2", "Deploy service")
	if !strings.Contains(content, "Summary: deployed v2") {
		t.Error("missing summary")
	}
	if !strings.Contains(content, "3 files changed") {
		t.Error("missing diff stat")
	}
	if !strings.Contains(content, "Analysis: continue") {
		t.Error("missing analysis")
	}
}

// Proves: when no diff stat, the log says "Changes: none" instead
// of blank.
func TestRecord_ShowsChangesNoneWhenNoDiffStat(t *testing.T) {
	tr := newTestTracker(t)
	tr.Record("t3", "Empty task", "nothing happened", "", "continue")

	content := tr.Read("t3", "Empty task")
	if !strings.Contains(content, "Changes: none") {
		t.Errorf("expected 'Changes: none', got: %s", content)
	}
}

// Proves: Count returns 0 for a new task and the correct count after recording attempts.
func TestCount_ReturnsAttemptCount(t *testing.T) {
	tr := newTestTracker(t)

	if got := tr.Count("t-count", "Count task"); got != 0 {
		t.Errorf("expected 0 before any attempts, got %d", got)
	}

	tr.Record("t-count", "Count task", "first try", "", "continue")
	if got := tr.Count("t-count", "Count task"); got != 1 {
		t.Errorf("expected 1 after first attempt, got %d", got)
	}

	tr.Record("t-count", "Count task", "second try", "", "continue")
	if got := tr.Count("t-count", "Count task"); got != 2 {
		t.Errorf("expected 2 after second attempt, got %d", got)
	}
}

// Proves: read returns the full log content for lookup.
func TestRead_ReturnsRecordedAttempts(t *testing.T) {
	tr := newTestTracker(t)
	tr.Record("t4", "My task", "try one", "", "continue")

	history := tr.Read("t4", "My task")
	if !strings.Contains(history, "### Attempt 1") {
		t.Error("missing Attempt 1")
	}
	if !strings.Contains(history, "try one") {
		t.Error("missing summary")
	}
}

// Proves: read returns empty when no attempts exist.
func TestRead_ReturnsEmptyForNewTask(t *testing.T) {
	tr := newTestTracker(t)
	history := tr.Read("new-task", "Brand new task")
	if history != "" {
		t.Errorf("expected empty, got %q", history)
	}
}

// Proves: when more than 3 attempts are recorded, Read returns only
// the last 3 so the prompt doesn't bloat with stale history.
func TestRead_CapsAtLastThreeAttempts(t *testing.T) {
	tr := newTestTracker(t)
	for i := 1; i <= 5; i++ {
		tr.Record("t6", "Capped task", fmt.Sprintf("try %d", i), "", "continue")
	}

	history := tr.Read("t6", "Capped task")
	if strings.Contains(history, "try 1") || strings.Contains(history, "try 2") {
		t.Error("expected attempts 1 and 2 to be excluded from prompt")
	}
	for _, want := range []string{"### Attempt 3", "### Attempt 4", "### Attempt 5"} {
		if !strings.Contains(history, want) {
			t.Errorf("missing %s in capped output", want)
		}
	}
}

// Proves: when 3 or fewer attempts exist, Read returns all of them.
func TestRead_ReturnsAllWhenUnderCap(t *testing.T) {
	tr := newTestTracker(t)
	for i := 1; i <= 3; i++ {
		tr.Record("t7", "Under cap", fmt.Sprintf("try %d", i), "", "continue")
	}

	history := tr.Read("t7", "Under cap")
	for _, want := range []string{"### Attempt 1", "### Attempt 2", "### Attempt 3"} {
		if !strings.Contains(history, want) {
			t.Errorf("missing %s — should return all when at or under cap", want)
		}
	}
}

// Proves: RecentReflections returns the N most recent reflection files
// sorted by modification time, excluding the current task.
func TestRecentReflections_ReturnsLastNByMtime(t *testing.T) {
	tr := newTestTracker(t)
	refDir := filepath.Join(tr.ralphDir, "reflections")
	os.MkdirAll(refDir, 0o755)

	// Write 4 reflections with staggered mtimes
	files := []struct {
		name    string
		content string
	}{
		{"ralph-aaa.md", "# Task A\n## What was discovered\n- Found bug A"},
		{"ralph-bbb.md", "# Task B\n## What was discovered\n- Found bug B"},
		{"ralph-ccc.md", "# Task C\n## What was discovered\n- Found bug C"},
		{"ralph-ddd.md", "# Task D\n## What was discovered\n- Found bug D"},
	}
	base := time.Now().Add(-4 * time.Hour)
	for i, f := range files {
		path := filepath.Join(refDir, f.name)
		os.WriteFile(path, []byte(f.content), 0o644)
		mtime := base.Add(time.Duration(i) * time.Hour)
		os.Chtimes(path, mtime, mtime)
	}

	// Request last 2, excluding ralph-ddd (the current task)
	result := tr.RecentReflections("ralph-ddd", 2)
	if len(result) != 2 {
		t.Fatalf("expected 2 reflections, got %d", len(result))
	}
	// Should be ralph-bbb and ralph-ccc (most recent excluding ddd)
	if result[0].TaskID != "ralph-bbb" {
		t.Errorf("expected ralph-bbb first, got %s", result[0].TaskID)
	}
	if result[1].TaskID != "ralph-ccc" {
		t.Errorf("expected ralph-ccc second, got %s", result[1].TaskID)
	}
	if !strings.Contains(result[0].Content, "Found bug B") {
		t.Error("reflection B content missing")
	}
}

// Proves: RecentReflections returns empty when no reflections exist.
func TestRecentReflections_EmptyWhenNoneExist(t *testing.T) {
	tr := newTestTracker(t)
	result := tr.RecentReflections("ralph-xxx", 3)
	if len(result) != 0 {
		t.Errorf("expected empty, got %d reflections", len(result))
	}
}

// Proves: merge failures are tracked per-task with incrementing count.
func TestRecordMergeFailure_IncrementsCount(t *testing.T) {
	tr := newTestTracker(t)

	count, err := tr.RecordMergeFailure("ralph-abc")
	if err != nil {
		t.Fatalf("RecordMergeFailure: %v", err)
	}
	if count != 1 {
		t.Errorf("expected count 1, got %d", count)
	}

	count, err = tr.RecordMergeFailure("ralph-abc")
	if err != nil {
		t.Fatalf("RecordMergeFailure: %v", err)
	}
	if count != 2 {
		t.Errorf("expected count 2, got %d", count)
	}

	if got := tr.MergeFailureCount("ralph-abc"); got != 2 {
		t.Errorf("MergeFailureCount: expected 2, got %d", got)
	}
}

// Proves: merge failure count returns 0 for tasks with no failures.
func TestMergeFailureCount_ZeroForNewTask(t *testing.T) {
	tr := newTestTracker(t)
	if got := tr.MergeFailureCount("ralph-new"); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

// Proves: ClearMergeFailures resets the counter so retries start fresh.
func TestClearMergeFailures_ResetsCount(t *testing.T) {
	tr := newTestTracker(t)
	tr.RecordMergeFailure("ralph-abc")
	tr.RecordMergeFailure("ralph-abc")
	tr.ClearMergeFailures("ralph-abc")

	if got := tr.MergeFailureCount("ralph-abc"); got != 0 {
		t.Errorf("expected 0 after clear, got %d", got)
	}
}

// Proves: empty taskID is a no-op for merge failure tracking.
func TestRecordMergeFailure_EmptyTaskIDNoOp(t *testing.T) {
	tr := newTestTracker(t)
	count, err := tr.RecordMergeFailure("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 for empty task ID, got %d", count)
	}
}

// Proves: ClearForTasks removes attempt files for multiple task IDs at once.
func TestClearForTasks_RemovesMultiple(t *testing.T) {
	tr := newTestTracker(t)
	tr.Record("ralph-abc", "", "try 1", "", "continue")
	tr.Record("ralph-def", "", "try 1", "", "continue")
	tr.Record("ralph-ghi", "", "try 1", "", "continue")

	tr.ClearForTasks([]string{"ralph-abc", "ralph-def"})

	if tr.Read("ralph-abc", "") != "" {
		t.Error("ralph-abc attempts should be cleared")
	}
	if tr.Read("ralph-def", "") != "" {
		t.Error("ralph-def attempts should be cleared")
	}
	if tr.Read("ralph-ghi", "") == "" {
		t.Error("ralph-ghi attempts should be preserved")
	}
}

// Proves: clear removes the attempt file so re-attempts start fresh
// after resolution.
func TestClear_RemovesAttemptFile(t *testing.T) {
	tr := newTestTracker(t)
	tr.Record("t5", "Done task", "completed", "", "continue")

	path := filepath.Join(tr.attemptsDir(), "t5.log")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file before clear")
	}

	tr.Clear("t5", "Done task")
	if _, err := os.Stat(path); err == nil {
		t.Error("expected file to be removed after clear")
	}
}
