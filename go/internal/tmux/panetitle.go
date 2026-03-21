package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// PaneTitle manages the stream pane title in a tmux session. A background
// ticker goroutine reads the .stream-task file (written by the loop process)
// and updates the tmux pane with the current task and elapsed time.
type PaneTitle struct {
	mu       sync.RWMutex
	task     string
	started  time.Time
	session  string
	ralphDir string
}

// NewPaneTitle creates a PaneTitle bound to the given tmux session name.
// ralphDir is the .ralph directory where .stream-task is written by the loop.
func NewPaneTitle(session, ralphDir string) *PaneTitle {
	return &PaneTitle{
		session:  session,
		ralphDir: ralphDir,
		started:  time.Now(),
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

const maxTitleLen = 60

// Title returns the formatted pane title: "<task> <elapsed>" or "stream <elapsed>".
// Long task labels are truncated so the elapsed time is always visible.
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
	task = truncateTask(task, maxTitleLen-1-len(stamp))
	return task + " " + stamp
}

func truncateTask(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// Run starts the background ticker that updates the tmux pane title every
// second. On each tick it reads the .stream-task file written by the loop
// process, resets the elapsed timer when the task changes, and updates the
// tmux pane. It blocks until stop is closed. Intended to be called as a goroutine.
func (p *PaneTitle) Run(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			p.syncFromFile()
			title := p.Title()
			exec.Command("tmux", "select-pane", "-t", p.session+":.1", "-T", title).Run() //nolint:errcheck
		}
	}
}

// syncFromFile reads the .stream-task file and updates the task label.
// Resets the elapsed timer when the task changes.
func (p *PaneTitle) syncFromFile() {
	if p.ralphDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(p.ralphDir, ".stream-task"))
	if err != nil {
		return
	}
	label := strings.TrimSpace(string(data))

	p.mu.Lock()
	defer p.mu.Unlock()
	if label != p.task {
		p.task = label
		p.started = time.Now()
	}
}
