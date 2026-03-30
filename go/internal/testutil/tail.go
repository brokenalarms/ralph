package testutil

import (
	"os"
	"sync"
)

// TailStub simulates a log-tailing process without spawning a real tail
// command or goroutine. It records start/stop calls and allows tests to
// inject lines that would appear in the tailed output.
type TailStub struct {
	mu      sync.Mutex
	started bool
	stopped bool
	calls   []string
	lines   []string
	stopCh  chan struct{}
	file    string // path being tailed
}

// NewTailStub creates a TailStub for the given log file path.
func NewTailStub(logFile string) *TailStub {
	return &TailStub{
		file:   logFile,
		stopCh: make(chan struct{}),
	}
}

// Start records a start call. Returns the stop channel for the caller.
func (t *TailStub) Start() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.started = true
	t.calls = append(t.calls, "start")
	return t.stopCh
}

// Stop records a stop call and closes the stop channel.
func (t *TailStub) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	t.calls = append(t.calls, "stop")
	select {
	case <-t.stopCh:
	default:
		close(t.stopCh)
	}
}

// WriteLine appends a line to the tailed output. If a real file path was
// given, it also writes the line to that file so tests checking file
// contents see it.
func (t *TailStub) WriteLine(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if t.file != "" {
		f, err := os.OpenFile(t.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			f.WriteString(line + "\n")
			f.Close()
		}
	}
}

// Lines returns all lines written to the stub.
func (t *TailStub) Lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.lines))
	copy(out, t.lines)
	return out
}

// Calls returns all recorded lifecycle calls.
func (t *TailStub) Calls() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.calls))
	copy(out, t.calls)
	return out
}

// Started returns whether Start was called.
func (t *TailStub) Started() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.started
}

// Stopped returns whether Stop was called.
func (t *TailStub) Stopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}

// File returns the path being tailed.
func (t *TailStub) File() string {
	return t.file
}
