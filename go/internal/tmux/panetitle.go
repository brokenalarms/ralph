package tmux

import (
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// PaneTitle manages the stream pane title in a tmux session. The main loop
// goroutine sets the current task via SetTask, and a background ticker
// goroutine reads it to update the tmux pane with elapsed time.
type PaneTitle struct {
	mu      sync.RWMutex
	task    string
	started time.Time
	session string
}

// NewPaneTitle creates a PaneTitle bound to the given tmux session name.
func NewPaneTitle(session string) *PaneTitle {
	return &PaneTitle{
		session: session,
		started: time.Now(),
	}
}

// SetTask updates the current task label shown in the pane title.
// Pass an empty string to clear the task (title falls back to "stream").
func (p *PaneTitle) SetTask(label string) {
	p.mu.Lock()
	p.task = label
	p.mu.Unlock()
}

// Task returns the current task label.
func (p *PaneTitle) Task() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.task
}

// ResetTimer resets the elapsed timer to now. Called at the start of each
// Claude execution to track per-task elapsed time.
func (p *PaneTitle) ResetTimer() {
	p.mu.Lock()
	p.started = time.Now()
	p.mu.Unlock()
}

// Title returns the formatted pane title: "<task> <elapsed>" or "stream <elapsed>".
func (p *PaneTitle) Title() string {
	p.mu.RLock()
	task := p.task
	started := p.started
	p.mu.RUnlock()

	elapsed := time.Since(started)
	mins := int(elapsed.Minutes())
	secs := int(elapsed.Seconds()) % 60
	stamp := fmt.Sprintf("%dm%02ds", mins, secs)

	if task == "" {
		return "stream " + stamp
	}
	return task + " " + stamp
}

// Run starts the background ticker that updates the tmux pane title every
// second. It blocks until stop is closed. Intended to be called as a goroutine.
func (p *PaneTitle) Run(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			title := p.Title()
			exec.Command("tmux", "select-pane", "-t", p.session+":.1", "-T", title).Run() //nolint:errcheck
		}
	}
}
