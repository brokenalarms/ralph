package git

import (
	"context"
	"fmt"
	"sync"
)

// stubGitHub is a test double for the GitHub interface.
// Compose into tests by embedding or assigning fields for the methods
// under test; all methods have sensible zero-value defaults.
type stubGitHub struct {
	available           bool
	openPR              string
	findPRErr           error
	createPRErr         error
	editPRErr           error
	editPRTitle         string
	mergeOutput         string
	mergeErr            error
	updateResult        bool
	updateErr           error
	checks              []CICheckResult
	checksErr           error
	mergeCalls          int
	mergeOpts           MergeOpts
	enforceAdmins       bool
	enforceAdminsErr    error
	postEnforceOutput   string
	postEnforceErr      error
	postEnforceCalled   bool
	checkEnforceCalled  bool
	prNumber            string
	prTitle             string
	searchPRNumber      string
	prDiff              string
	createdPR           string
}

func (s *stubGitHub) Available() bool { return s.available }
func (s *stubGitHub) FindOpenPR(branch, repoURL string) (string, error) {
	return s.openPR, s.findPRErr
}
func (s *stubGitHub) CreatePR(opts CreatePROpts) error {
	if s.createPRErr == nil && s.createdPR != "" {
		s.openPR = s.createdPR
	}
	return s.createPRErr
}
func (s *stubGitHub) EditPR(prNumber, repoURL, title string) error {
	s.editPRTitle = title
	return s.editPRErr
}
func (s *stubGitHub) MergePR(prNumber, repoURL string, opts MergeOpts) (string, error) {
	s.mergeCalls++
	s.mergeOpts = opts
	return s.mergeOutput, s.mergeErr
}
func (s *stubGitHub) UpdateBranch(dir, nwo, prNumber string) (bool, error) {
	return s.updateResult, s.updateErr
}
func (s *stubGitHub) ListChecks(prNumber, repoURL string) ([]CICheckResult, error) {
	return s.checks, s.checksErr
}
func (s *stubGitHub) GetRunLog(prNumber, workDir string) string {
	return ""
}
func (s *stubGitHub) CheckEnforceAdmins(nwo, branch string) (bool, error) {
	s.checkEnforceCalled = true
	return s.enforceAdmins, s.enforceAdminsErr
}
func (s *stubGitHub) PostEnforceAdmins(nwo, branch string) (string, error) {
	s.postEnforceCalled = true
	return s.postEnforceOutput, s.postEnforceErr
}
func (s *stubGitHub) FindPR(branch, workDir string) (string, string, error) {
	return s.prNumber, s.prTitle, s.findPRErr
}
func (s *stubGitHub) SearchPR(workDir, query string) (string, error) {
	return s.searchPRNumber, nil
}
func (s *stubGitHub) PRDiff(workDir, prNumber string) (string, error) {
	return s.prDiff, nil
}

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
}

type stubResponse struct {
	Output string
	Err    error
}

func newStubRunner() *stubRunner {
	return &stubRunner{
		responses: make(map[string]stubResponse),
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

// discardLog silences log output during tests.
type discardLog struct{}

func (discardLog) Log(string, string, ...any)   {}
func (discardLog) Warn(string, string, ...any)  {}
func (discardLog) Error(string, string, ...any) {}

// capturingGitHub captures CreatePR calls for assertion.
type capturingGitHub struct {
	stubGitHub
	createPR func(CreatePROpts) error
}

func (c *capturingGitHub) CreatePR(opts CreatePROpts) error {
	if c.createPR != nil {
		return c.createPR(opts)
	}
	return nil
}

// runLogGitHub stubs GetRunLog with a configurable return value.
type runLogGitHub struct {
	stubGitHub
	log string
}

func (r *runLogGitHub) GetRunLog(prNumber, workDir string) string {
	return r.log
}

// stubManager creates a Manager wired to stubs for both git commands and
// GitHub operations. The Manager's dirs are set to the given directory.
func stubManager(dir string, runner *stubRunner, gh *stubGitHub) *Manager {
	if runner == nil {
		runner = newStubRunner()
	}
	if gh == nil {
		gh = &stubGitHub{}
	}
	return &Manager{
		ProjectDir: dir,
		WorkDir:    dir,
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
