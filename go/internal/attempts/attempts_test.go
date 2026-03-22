package attempts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestTracker(t *testing.T) *Tracker {
	t.Helper()
	return New(filepath.Join(t.TempDir(), ".ralph"))
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

// Proves: checklist tasks (no id) use a slugified task name as the
// attempt key.
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
