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

// PaneTitle manages pane titles in a tmux session. A background ticker
// goroutine reads signal files written by the loop process and updates
// both the stream pane (task + per-task elapsed) and the ralph pane
// (branch name + per-run elapsed).
type PaneTitle struct {
	mu         sync.RWMutex
	task       string
	started    time.Time
	runStarted time.Time
	branch     string
	session    string
	ralphDir   string
	streamPane int
}

// NewPaneTitle creates a PaneTitle bound to the given tmux session name.
// ralphDir is the .ralph directory where signal files are written by the loop.
// NewPaneTitle creates a PaneTitle bound to the given tmux session name.
// ralphDir is the .ralph directory where signal files are written by the loop.
// streamPane is the tmux pane index for the stream pane (1 in standard, 2 in commander).
func NewPaneTitle(session, ralphDir string, streamPane int) *PaneTitle {
	now := time.Now()
	return &PaneTitle{
		session:    session,
		ralphDir:   ralphDir,
		started:    now,
		runStarted: now,
		streamPane: streamPane,
	}
}

// SetTask updates the current task label shown in the stream pane title.
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

// Title returns the formatted stream pane title: "<task> <elapsed>" or "stream <elapsed>".
// Long task labels are truncated so the elapsed time is always visible.
func (p *PaneTitle) Title() string {
	p.mu.RLock()
	task := p.task
	started := p.started
	p.mu.RUnlock()

	elapsed := time.Since(started)
	stamp := formatElapsed(elapsed)

	if task == "" {
		return "stream " + stamp
	}
	task = truncateTask(task, maxTitleLen-1-len(stamp))
	return task + " " + stamp
}

// RalphTitle returns the formatted ralph pane title showing the branch name
// and total run elapsed time, e.g. "ralph/task/fix-auth 5m23s".
func (p *PaneTitle) RalphTitle() string {
	p.mu.RLock()
	branch := p.branch
	runStarted := p.runStarted
	p.mu.RUnlock()

	elapsed := time.Since(runStarted)
	stamp := formatElapsed(elapsed)

	if branch == "" {
		return "(go) ralph " + stamp
	}
	return branch + " " + stamp
}

func formatElapsed(d time.Duration) string {
	mins := int(d.Minutes())
	secs := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", mins, secs)
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

// Run starts the background ticker that updates tmux pane titles every
// second. On each tick it reads signal files written by the loop process,
// resets timers when tasks change, and updates both pane 0 (ralph) and
// pane 1 (stream). It blocks until stop is closed.
func (p *PaneTitle) Run(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			p.syncFromFile()
			p.syncBranch()

			title := p.Title()
			streamTarget := fmt.Sprintf("%s:.%d", p.session, p.streamPane)
			exec.Command("tmux", "select-pane", "-t", streamTarget, "-T", title).Run() //nolint:errcheck

			ralphTitle := p.RalphTitle()
			exec.Command("tmux", "select-pane", "-t", p.session+":.0", "-T", ralphTitle).Run() //nolint:errcheck
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

// syncBranch reads the .run-branch file and updates the branch name
// shown in the ralph pane title.
func (p *PaneTitle) syncBranch() {
	if p.ralphDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(p.ralphDir, ".run-branch"))
	if err != nil {
		return
	}
	branch := strings.TrimSpace(string(data))

	p.mu.Lock()
	p.branch = branch
	p.mu.Unlock()
}
