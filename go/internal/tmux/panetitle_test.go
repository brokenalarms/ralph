package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Verifies that SetTask updates the task label visible in Title(),
// proving the main loop can communicate task context to the timer.
func TestSetTask_UpdatesTitle(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)
	p.SetTask("ralph-abc: Fix auth bug")

	title := p.Title()
	if !strings.HasPrefix(title, "ralph-abc: Fix auth bug ") {
		t.Errorf("Title() = %q, want prefix %q", title, "ralph-abc: Fix auth bug ")
	}
}

// Verifies that an empty task produces a "stream" fallback title,
// matching bash behavior when no .stream-task file exists.
func TestTitle_FallbackWhenNoTask(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)

	title := p.Title()
	if !strings.HasPrefix(title, "stream ") {
		t.Errorf("Title() = %q, want prefix %q", title, "stream ")
	}
}

// Verifies that clearing the task reverts to the fallback title.
func TestSetTask_ClearReverts(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)
	p.SetTask("some task")
	p.SetTask("")

	title := p.Title()
	if !strings.HasPrefix(title, "stream ") {
		t.Errorf("Title() after clear = %q, want prefix %q", title, "stream ")
	}
}

// Verifies that Task() returns the current label set by SetTask.
func TestTask_ReturnsCurrentLabel(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)

	if got := p.Task(); got != "" {
		t.Errorf("Task() before SetTask = %q, want empty", got)
	}

	p.SetTask("ralph-xyz: Deploy")
	if got := p.Task(); got != "ralph-xyz: Deploy" {
		t.Errorf("Task() = %q, want %q", got, "ralph-xyz: Deploy")
	}
}

// Verifies that ResetTimer affects the elapsed time shown in Title().
func TestResetTimer_ResetsElapsed(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)
	p.mu.Lock()
	p.started = time.Now().Add(-2 * time.Minute)
	p.mu.Unlock()

	title := p.Title()
	if !strings.Contains(title, "2m") {
		t.Errorf("Title() before reset = %q, expected ~2m elapsed", title)
	}

	p.ResetTimer()
	title = p.Title()
	if !strings.Contains(title, "0m") {
		t.Errorf("Title() after reset = %q, expected ~0m elapsed", title)
	}
}

// Verifies that Title() includes elapsed time in the expected format.
func TestTitle_ElapsedFormat(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)
	p.mu.Lock()
	p.started = time.Now().Add(-3*time.Minute - 7*time.Second)
	p.mu.Unlock()

	title := p.Title()
	if !strings.Contains(title, "3m07s") {
		t.Errorf("Title() = %q, want elapsed containing %q", title, "3m07s")
	}
}

// Verifies concurrent reads and writes don't race. Run with -race.
func TestConcurrentAccess(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)
	var wg sync.WaitGroup

	for i := range 10 {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			p.SetTask("task " + string(rune('A'+n)))
			p.ResetTimer()
		}(i)
		go func() {
			defer wg.Done()
			_ = p.Title()
			_ = p.Task()
		}()
	}

	wg.Wait()
}

// Verifies that syncFromFile reads the .stream-task file and updates the task,
// proving the cross-process communication between the loop and tmux outer process.
func TestSyncFromFile_UpdatesTask(t *testing.T) {
	dir := t.TempDir()
	p := NewPaneTitle("test-session", dir, PaneStream)

	os.WriteFile(filepath.Join(dir, ".stream-task"), []byte("ralph-abc: Fix auth bug"), 0o644)
	p.syncFromFile()

	if got := p.Task(); got != "ralph-abc: Fix auth bug" {
		t.Errorf("Task() after sync = %q, want %q", got, "ralph-abc: Fix auth bug")
	}
	title := p.Title()
	if !strings.HasPrefix(title, "ralph-abc: Fix auth bug ") {
		t.Errorf("Title() = %q, want prefix %q", title, "ralph-abc: Fix auth bug ")
	}
}

// Verifies that syncFromFile resets the elapsed timer when the task changes,
// so each task shows its own elapsed time rather than cumulative.
func TestSyncFromFile_ResetsTimerOnTaskChange(t *testing.T) {
	dir := t.TempDir()
	p := NewPaneTitle("test-session", dir, PaneStream)

	os.WriteFile(filepath.Join(dir, ".stream-task"), []byte("task-1"), 0o644)
	p.syncFromFile()

	p.mu.Lock()
	p.started = time.Now().Add(-5 * time.Minute)
	p.mu.Unlock()

	os.WriteFile(filepath.Join(dir, ".stream-task"), []byte("task-2"), 0o644)
	p.syncFromFile()

	title := p.Title()
	if !strings.HasPrefix(title, "task-2 0m") {
		t.Errorf("Title() after task change = %q, want prefix %q with reset elapsed", title, "task-2 0m")
	}
}

// Verifies that syncFromFile does not reset the timer when the task is unchanged.
func TestSyncFromFile_NoResetWhenTaskUnchanged(t *testing.T) {
	dir := t.TempDir()
	p := NewPaneTitle("test-session", dir, PaneStream)

	os.WriteFile(filepath.Join(dir, ".stream-task"), []byte("task-1"), 0o644)
	p.syncFromFile()

	p.mu.Lock()
	p.started = time.Now().Add(-3 * time.Minute)
	p.mu.Unlock()

	p.syncFromFile()

	title := p.Title()
	if !strings.Contains(title, "3m") {
		t.Errorf("Title() = %q, expected ~3m elapsed (timer should not reset)", title)
	}
}

// Verifies that syncFromFile is a no-op when ralphDir is empty.
func TestSyncFromFile_NoOpWithoutRalphDir(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)
	p.syncFromFile()

	if got := p.Task(); got != "" {
		t.Errorf("Task() = %q, want empty after no-op sync", got)
	}
}

// Verifies that long task labels are truncated with ellipsis so elapsed time stays visible.
func TestTitle_TruncatesLongTask(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)
	long := "ralph-9uu: [bug] Go: Auto-merge not firing — multiple tasks stacking on single branch"
	p.SetTask(long)

	title := p.Title()
	if len(title) > maxTitleLen {
		t.Errorf("Title() length = %d, want <= %d; title = %q", len(title), maxTitleLen, title)
	}
	if !strings.Contains(title, "...") {
		t.Errorf("Title() = %q, want ellipsis for truncated task", title)
	}
	if !strings.HasPrefix(title, "ralph-9uu:") {
		t.Errorf("Title() = %q, want to preserve bead ID prefix", title)
	}
}

// Verifies that short task labels are not truncated.
func TestTitle_ShortTaskNotTruncated(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)
	p.SetTask("ralph-abc: Fix auth bug")

	title := p.Title()
	if strings.Contains(title, "...") {
		t.Errorf("Title() = %q, short task should not be truncated", title)
	}
	if !strings.HasPrefix(title, "ralph-abc: Fix auth bug ") {
		t.Errorf("Title() = %q, want prefix %q", title, "ralph-abc: Fix auth bug ")
	}
}

// Verifies that RalphTitle includes branch name and elapsed time when a branch
// is set, so the user can see which branch the loop is on and how long it's been running.
func TestRalphTitle_WithBranch(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)
	p.mu.Lock()
	p.branch = "ralph/task/fix-auth"
	p.mu.Unlock()

	title := p.RalphTitle()
	if !strings.HasPrefix(title, "ralph/task/fix-auth ") {
		t.Errorf("RalphTitle() = %q, want prefix %q", title, "ralph/task/fix-auth ")
	}
	if !strings.Contains(title, "m") {
		t.Errorf("RalphTitle() = %q, want elapsed time format", title)
	}
}

// Verifies that RalphTitle falls back to "(go) ralph" when no branch is set.
func TestRalphTitle_Fallback(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)

	title := p.RalphTitle()
	if !strings.HasPrefix(title, "(go) ralph ") {
		t.Errorf("RalphTitle() = %q, want prefix %q", title, "(go) ralph ")
	}
}

// Verifies that syncBranch reads the .run-branch file and updates the branch
// name shown in the ralph pane title.
func TestSyncBranch_ReadsBranchFile(t *testing.T) {
	dir := t.TempDir()
	p := NewPaneTitle("test-session", dir, PaneStream)

	os.WriteFile(filepath.Join(dir, ".run-branch"), []byte("ralph/task/deploy-fix"), 0o644)
	p.syncBranch()

	p.mu.RLock()
	branch := p.branch
	p.mu.RUnlock()

	if branch != "ralph/task/deploy-fix" {
		t.Errorf("branch = %q, want %q", branch, "ralph/task/deploy-fix")
	}
}

// Verifies that syncBranch is a no-op when ralphDir is empty.
func TestSyncBranch_NoOpWithoutRalphDir(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)
	p.syncBranch()

	p.mu.RLock()
	branch := p.branch
	p.mu.RUnlock()

	if branch != "" {
		t.Errorf("branch = %q, want empty after no-op sync", branch)
	}
}

// Verifies that RalphTitle shows per-run elapsed time (not per-task),
// so the user can see total loop duration.
func TestRalphTitle_RunElapsed(t *testing.T) {
	p := NewPaneTitle("test-session", "", PaneStream)
	p.mu.Lock()
	p.branch = "main"
	p.runStarted = time.Now().Add(-5*time.Minute - 30*time.Second)
	p.mu.Unlock()

	title := p.RalphTitle()
	if !strings.Contains(title, "5m30s") {
		t.Errorf("RalphTitle() = %q, want elapsed containing %q", title, "5m30s")
	}
}

// Verifies that Run exits when the stop channel is closed.
func TestRun_StopsOnClose(t *testing.T) {
	p := NewPaneTitle("nonexistent-session", "", PaneStream)
	stop := make(chan struct{})

	done := make(chan struct{})
	go func() {
		p.Run(stop)
		close(done)
	}()

	close(stop)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after stop channel closed")
	}
}
