package testutil

import "sync"

// Process abstracts the lifecycle of an external process (Start/Kill/Wait)
// for testability. Production code wraps exec.Cmd; tests inject ProcessStub
// to record calls without spawning real processes.
type Process interface {
	Start() error
	Kill() error
	Wait() error
	Pid() int
}

// ProcessStub records Start/Kill/Wait calls without spawning a real process.
// Use it to verify process cleanup, signal handling, and lifecycle management.
type ProcessStub struct {
	mu       sync.Mutex
	calls    []string
	started  bool
	killed   bool
	waited   bool
	StartErr error
	KillErr  error
	WaitErr  error
	fakePid  int
	waitCh   chan struct{} // closed when Release is called, unblocking Wait
}

// NewProcessStub creates a ProcessStub with the given fake PID.
// Wait blocks until Release is called, simulating a long-running process.
func NewProcessStub(pid int) *ProcessStub {
	return &ProcessStub{
		fakePid: pid,
		waitCh:  make(chan struct{}),
	}
}

func (p *ProcessStub) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "start")
	p.started = true
	return p.StartErr
}

func (p *ProcessStub) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "kill")
	p.killed = true
	// Release the Wait blocker when killed.
	select {
	case <-p.waitCh:
	default:
		close(p.waitCh)
	}
	return p.KillErr
}

func (p *ProcessStub) Wait() error {
	<-p.waitCh
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "wait")
	p.waited = true
	return p.WaitErr
}

func (p *ProcessStub) Pid() int {
	return p.fakePid
}

// Release unblocks Wait without killing, simulating normal process exit.
func (p *ProcessStub) Release() {
	select {
	case <-p.waitCh:
	default:
		close(p.waitCh)
	}
}

// Calls returns a copy of all recorded lifecycle calls in order.
func (p *ProcessStub) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.calls))
	copy(out, p.calls)
	return out
}

// Started returns whether Start was called.
func (p *ProcessStub) Started() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started
}

// Killed returns whether Kill was called.
func (p *ProcessStub) Killed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

// Waited returns whether Wait was called.
func (p *ProcessStub) Waited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waited
}
