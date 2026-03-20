package tmux

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// Verifies that SetTask updates the task label visible in Title(),
// proving the main loop can communicate task context to the timer.
func TestSetTask_UpdatesTitle(t *testing.T) {
	p := NewPaneTitle("test-session")
	p.SetTask("ralph-abc: Fix auth bug")

	title := p.Title()
	if !strings.HasPrefix(title, "ralph-abc: Fix auth bug ") {
		t.Errorf("Title() = %q, want prefix %q", title, "ralph-abc: Fix auth bug ")
	}
}

// Verifies that an empty task produces a "stream" fallback title,
// matching bash behavior when no .stream-task file exists.
func TestTitle_FallbackWhenNoTask(t *testing.T) {
	p := NewPaneTitle("test-session")

	title := p.Title()
	if !strings.HasPrefix(title, "stream ") {
		t.Errorf("Title() = %q, want prefix %q", title, "stream ")
	}
}

// Verifies that clearing the task reverts to the fallback title.
func TestSetTask_ClearReverts(t *testing.T) {
	p := NewPaneTitle("test-session")
	p.SetTask("some task")
	p.SetTask("")

	title := p.Title()
	if !strings.HasPrefix(title, "stream ") {
		t.Errorf("Title() after clear = %q, want prefix %q", title, "stream ")
	}
}

// Verifies that Task() returns the current label set by SetTask.
func TestTask_ReturnsCurrentLabel(t *testing.T) {
	p := NewPaneTitle("test-session")

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
	p := NewPaneTitle("test-session")
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
	p := NewPaneTitle("test-session")
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
	p := NewPaneTitle("test-session")
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

// Verifies that Run exits when the stop channel is closed.
func TestRun_StopsOnClose(t *testing.T) {
	p := NewPaneTitle("nonexistent-session")
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
