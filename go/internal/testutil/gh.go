package testutil

import (
	"context"
	"strings"
	"sync"
)

// GHStub stubs gh CLI commands by dispatching on subcommands. It records
// all invocations and returns canned responses for pr list, pr create,
// pr merge, pr checks, and other gh operations.
type GHStub struct {
	mu    sync.Mutex
	calls []GHCall

	PRListOutput   string // output for "gh pr list"
	PRCreateErr    error
	PRMergeOutput  string
	PRMergeErr     error
	PRChecksOutput string
	RunViewOutput  string
}

// GHCall records a single gh CLI invocation.
type GHCall struct {
	Args []string
}

// NewGHStub creates a GHStub with empty defaults.
func NewGHStub() *GHStub {
	return &GHStub{}
}

// Runner returns a CommandFunc that stubs gh CLI invocations.
func (s *GHStub) Runner() func(ctx context.Context, name string, args ...string) (string, error) {
	return func(_ context.Context, name string, args ...string) (string, error) {
		s.mu.Lock()
		defer s.mu.Unlock()

		s.calls = append(s.calls, GHCall{Args: args})

		joined := strings.Join(args, " ")

		switch {
		case strings.HasPrefix(joined, "pr list"):
			return s.PRListOutput, nil
		case strings.HasPrefix(joined, "pr create"):
			return "", s.PRCreateErr
		case strings.HasPrefix(joined, "pr merge"):
			return s.PRMergeOutput, s.PRMergeErr
		case strings.HasPrefix(joined, "pr checks"):
			return s.PRChecksOutput, nil
		case strings.HasPrefix(joined, "run view"):
			return s.RunViewOutput, nil
		}

		return "", nil
	}
}

// Calls returns all recorded gh invocations.
func (s *GHStub) Calls() []GHCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]GHCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// CalledWith returns true if any call's args contain the given substring.
func (s *GHStub) CalledWith(substring string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if strings.Contains(strings.Join(c.Args, " "), substring) {
			return true
		}
	}
	return false
}
