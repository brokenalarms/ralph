package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/brokenalarms/ralph/internal/logging"
)

// Log is the logging interface used by Runner.
type Log interface {
	Emit(o logging.Opts, format string, args ...any)
	AgentLog(domain logging.Domain, format string, args ...any)
}

// Runner manages Claude process lifecycle: spawning, signal polling, and cleanup.
// It tracks active streaming goroutines (filter and tail) so they can be
// explicitly stopped before a new Run() call to prevent goroutine accumulation.
type Runner struct {
	Logger         Log
	OnTaskDetected OnTaskDetected
	CmdFactory     CmdFactory

	// ProjectDir is the repository root that agent spawns must NEVER chdir
	// into. Run() rejects RunConfig.WorkDir values equal to ProjectDir or
	// empty — the structural defense against "worktree leaked into main"
	// failures where a misconfigured workDir falls back to the project root.
	//
	// May be empty in tests and in direct claude.Runner construction; in
	// production it is set by agent.New() so all paths into Run() are guarded.
	ProjectDir string

	mu         sync.Mutex
	stdinPipe  io.WriteCloser
	filterStop chan struct{}
	filterDone <-chan struct{}
	tailStop   chan struct{}
	tailDone   <-chan struct{}
}

// StopStreaming stops and drains any active filter/tail goroutines. Safe to
// call multiple times or when no goroutines are active. Must be called before
// spawning a new Run() that shares the same raw log or log file to prevent
// duplicate writers.
func (r *Runner) StopStreaming() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.filterStop != nil {
		close(r.filterStop)
		<-r.filterDone
		r.filterStop = nil
		r.filterDone = nil
	}
	if r.tailStop != nil {
		close(r.tailStop)
		if r.tailDone != nil {
			<-r.tailDone
		}
		r.tailStop = nil
		r.tailDone = nil
	}
}

// InjectMessage writes a user message to the running agent's stdin pipe using
// the stream-json input format. Returns an error if no pipe is available (agent
// not running) or the write fails (pipe broken — agent exited).
func (r *Runner) InjectMessage(msg string) error {
	r.mu.Lock()
	pipe := r.stdinPipe
	r.mu.Unlock()
	if pipe == nil {
		return fmt.Errorf("no stdin pipe available")
	}
	payload := UserInputMessage(msg)
	_, err := fmt.Fprintln(pipe, payload)
	return err
}

// UserInputMessage builds a stream-json user message for injection into a
// running Claude process via stdin. The content is JSON-escaped to handle
// multi-line test output, error messages, etc.
func UserInputMessage(content string) string {
	escaped, _ := json.Marshal(content)
	return fmt.Sprintf(`{"type":"user_input_text","content":%s}`, string(escaped))
}
