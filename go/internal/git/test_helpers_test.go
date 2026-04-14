package git

import (
	"context"
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

// newRepoForTest constructs a *Repo wired to test-supplied dependencies.
// This is the one and only seam for building a real Repo with an injected
// stub gitHub inside internal/git tests. Package-private and _test.go-only:
// unreachable from production code and from tests in other packages.
//
// Callers always supply cfg and gh — these are the two things a test must
// state. Runner and state default to no-op stubs (newStubRunner() returns
// "", nil for any command; newMemState() is an in-memory map). Tests that
// need specific runner or state behavior pass them explicitly via the
// variadic options.
//
// Rationale: passing nil for dependencies is fragile — a code change that
// touches an unexercised path panics instead of failing cleanly. Defaulting
// here means every path produces sensible no-op behavior, and tests that
// care about a particular dependency opt in rather than opting out.
func newRepoForTest(cfg Config, gh gitHub, opts ...repoTestOpt) *Repo {
	tc := repoTestDeps{
		runner: newStubRunner(),
		state:  newMemState(),
	}
	for _, opt := range opts {
		opt(&tc)
	}
	if gh == nil {
		gh = newStubGitHub(StubGitHubConfig{})
	}
	projectDir := cfg.ProjectDir
	if projectDir == "" {
		projectDir = cfg.WorkDir
	}
	return &Repo{
		projectDir:                  projectDir,
		workDir:                     cfg.WorkDir,
		ralphDir:                    cfg.RalphDir,
		baseBranch:                  cfg.BaseBranch,
		resume:                      cfg.Resume,
		logger:                      cfg.Logger,
		compileCheckTimeout:         cfg.CompileCheckTimeout,
		ciPollTimeout:               cfg.CIPollTimeout,
		copilotGatedTimeout:         cfg.CopilotGatedTimeout,
		copilotOpportunisticTimeout: cfg.CopilotOpportunisticTimeout,
		codeRabbitTimeout:           cfg.CodeRabbitTimeout,
		github:                      gh,
		runner:                      tc.runner,
		state:                       tc.state,
		worktreeBranch:              tc.worktreeBranch,
		branchRenamed:               tc.branchRenamed,
		prevBranch:                  tc.prevBranch,
		knownPRNumber:               tc.knownPRNumber,
	}
}

// repoTestDeps holds the optional test dependencies. Accessed only through
// the repoTestOpt functions below.
type repoTestDeps struct {
	runner         Runner
	state          stateStore
	worktreeBranch string
	branchRenamed  bool
	prevBranch     string
	knownPRNumber  int
}

type repoTestOpt func(*repoTestDeps)

// withRunner overrides the default no-op runner. Use when a test needs to
// inspect or control git subprocess invocations.
func withRunner(r Runner) repoTestOpt {
	return func(d *repoTestDeps) { d.runner = r }
}

// withState overrides the default in-memory state store.
func withState(s stateStore) repoTestOpt {
	return func(d *repoTestDeps) { d.state = s }
}

// withWorktreeBranch sets the Repo's worktreeBranch field, which production
// code normally initializes through SetupWorktree / RenameBranchTo. Tests
// use this to pre-seed a specific branch name without running real git.
func withWorktreeBranch(name string) repoTestOpt {
	return func(d *repoTestDeps) { d.worktreeBranch = name }
}

// withBranchRenamed sets the Repo's branchRenamed flag. Production code
// sets this inside RenameBranchForTask after a successful rename.
func withBranchRenamed(v bool) repoTestOpt {
	return func(d *repoTestDeps) { d.branchRenamed = v }
}

// withPrevBranch pre-seeds the Repo's prevBranch field, which production
// populates during stack head detection. Tests use this to exercise
// stacked-PR paths without running real git to derive the stack.
func withPrevBranch(name string) repoTestOpt {
	return func(d *repoTestDeps) { d.prevBranch = name }
}

// withKnownPRNumber pre-seeds the Repo's knownPRNumber, which production
// sets via SetKnownPRNumber after discovering a PR during Ship. Tests use
// this to exercise the "PR already known" fast path.
func withKnownPRNumber(n int) repoTestOpt {
	return func(d *repoTestDeps) { d.knownPRNumber = n }
}

