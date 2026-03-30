package testutil

import (
	"context"
	"sync"
)

// CommandFunc executes a named command with arguments and returns its output.
// This is the generic form of exec.Command wrapping — injectable for testing
// any code that shells out to external processes (gh, tail, bd, etc).
type CommandFunc func(ctx context.Context, name string, args ...string) (stdout string, err error)

// ExecCall records a single command invocation for assertion.
type ExecCall struct {
	Name string
	Args []string
}

// ExecStub records all command invocations and returns canned responses.
// Responses are keyed by command name (e.g. "gh", "tail") with optional
// subcommand specificity ("gh pr list"). Lookup tries the most specific
// key first, falling back to less specific keys.
type ExecStub struct {
	mu        sync.Mutex
	calls     []ExecCall
	responses map[string]ExecResponse
}

// ExecResponse holds canned output for a command stub.
type ExecResponse struct {
	Output string
	Err    error
}

// NewExecStub creates an ExecStub ready for use.
func NewExecStub() *ExecStub {
	return &ExecStub{
		responses: make(map[string]ExecResponse),
	}
}

// On registers a canned response for calls matching the given key.
// Key is "name arg0 arg1 ..." or just "name" for a catch-all.
func (s *ExecStub) On(key string, output string, err error) *ExecStub {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[key] = ExecResponse{Output: output, Err: err}
	return s
}

// Run implements CommandFunc — records the call and returns the canned response.
func (s *ExecStub) Run(_ context.Context, name string, args ...string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, ExecCall{Name: name, Args: args})

	// Build progressively less specific keys: "name arg0 arg1", "name arg0", "name"
	tokens := append([]string{name}, args...)
	for i := len(tokens); i > 0; i-- {
		key := ""
		for j := 0; j < i; j++ {
			if j > 0 {
				key += " "
			}
			key += tokens[j]
		}
		if resp, ok := s.responses[key]; ok {
			return resp.Output, resp.Err
		}
	}

	return "", nil
}

// Calls returns a copy of all recorded invocations.
func (s *ExecStub) Calls() []ExecCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ExecCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// CalledWith returns true if any call matched the given name and arg prefix.
func (s *ExecStub) CalledWith(name string, args ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c.Name != name {
			continue
		}
		if len(c.Args) < len(args) {
			continue
		}
		match := true
		for i, a := range args {
			if c.Args[i] != a {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// CallCount returns the number of calls matching the given command name.
func (s *ExecStub) CallCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.Name == name {
			n++
		}
	}
	return n
}
