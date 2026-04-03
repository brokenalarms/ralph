package git

import (
	"context"
	"fmt"
	"sync"

	"github.com/brokenalarms/ralph/internal/logging"
)

// gitCall records a single git command invocation for assertion.
type gitCall struct {
	Dir  string
	Args []string
}

// stubRunner is a test double for the Runner interface that records every
// git command invocation and returns canned responses. Tests configure it
// with the responses map keyed by the first git argument (e.g. "fetch",
// "push", "rev-parse") or a more specific "arg0 arg1" key.
type stubRunner struct {
	mu        sync.Mutex
	calls     []gitCall
	responses map[string]stubResponse
	sequences map[string]*seqState
}

type stubResponse struct {
	Output string
	Err    error
}

// seqState tracks sequential responses for a given key.
type seqState struct {
	responses []stubResponse
	idx       int
}

func newStubRunner() *stubRunner {
	return &stubRunner{
		responses: make(map[string]stubResponse),
		sequences: make(map[string]*seqState),
	}
}

func (s *stubRunner) Run(_ context.Context, dir string, args ...string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, gitCall{Dir: dir, Args: args})

	if len(args) == 0 {
		return "", nil
	}

	// Try progressively less specific keys: "arg0 arg1 arg2", "arg0 arg1", "arg0"
	for i := len(args); i > 0; i-- {
		key := ""
		for j := 0; j < i; j++ {
			if j > 0 {
				key += " "
			}
			key += args[j]
		}
		// Check sequential responses first — they take precedence over static ones.
		if seq, ok := s.sequences[key]; ok && seq.idx < len(seq.responses) {
			resp := seq.responses[seq.idx]
			seq.idx++
			return resp.Output, resp.Err
		}
		if resp, ok := s.responses[key]; ok {
			return resp.Output, resp.Err
		}
	}

	return "", nil
}

// Called returns all recorded calls.
func (s *stubRunner) Called() []gitCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]gitCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// CalledWith returns true if any call matched the given args prefix.
func (s *stubRunner) CalledWith(args ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if matchArgs(c.Args, args) {
			return true
		}
	}
	return false
}

func matchArgs(actual, prefix []string) bool {
	if len(actual) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if actual[i] != p {
			return false
		}
	}
	return true
}

// On registers a canned response for calls matching the given key.
// Key can be a single arg ("fetch") or multiple ("push -u origin").
func (s *stubRunner) On(key string, output string, err error) *stubRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[key] = stubResponse{Output: output, Err: err}
	return s
}

// OnSequence registers sequential responses for calls matching the given key.
// Each call consumes the next response in order. Falls back to the On response
// (or "", nil) once the sequence is exhausted.
func (s *stubRunner) OnSequence(key string, responses []stubResponse) *stubRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequences[key] = &seqState{responses: responses}
	return s
}

// discardLog silences log output during tests.
type discardLog struct{}

func (discardLog) Emit(logging.Opts, string, ...any) {}
func (discardLog) EmitInPlace(logging.Opts, string, ...any) {}
func (discardLog) EmitAppend(string, ...any)                {}
func (discardLog) EmitFinalInPlace()                        {}

// capturingGitHub captures CreatePR calls for assertion.
type capturingGitHub struct {
	StubGitHub
	createPR func(CreatePROpts) error
}

func (c *capturingGitHub) CreatePR(opts CreatePROpts) error {
	if c.createPR != nil {
		return c.createPR(opts)
	}
	return nil
}

// stubManager creates a Manager wired to stubs for both git commands and
// GitHub operations. The Manager's dirs are set to the given directory.
func stubManager(dir string, runner *stubRunner, gh *StubGitHub) *Manager {
	if runner == nil {
		runner = newStubRunner()
	}
	if gh == nil {
		gh = &StubGitHub{}
	}
	return &Manager{
		ProjectDir: dir,
		WorkDir:    dir,
		BaseBranch: "main",
		Runner:     runner,
		GitHub:     gh,
		State:      newMemState(),
		Logger:     discardLog{},
	}
}

// errRunner returns a Runner where every call returns an error.
func errRunner(msg string) Runner {
	return &errRunnerImpl{stubRunner: newStubRunner(), err: fmt.Errorf("%s", msg)}
}

type errRunnerImpl struct {
	*stubRunner
	err error
}

func (e *errRunnerImpl) Run(ctx context.Context, dir string, args ...string) (string, error) {
	e.stubRunner.Run(ctx, dir, args...)
	return "", e.err
}
