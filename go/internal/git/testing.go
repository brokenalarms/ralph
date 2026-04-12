package git

import (
	"fmt"
	"time"
)

// stubGitHub is a test double for the gitHub interface. Configure fields for the
// methods under test; all methods have sensible zero-value defaults.
type stubGitHub struct {
	IsAvailable        bool
	OpenPR             int
	FindPRErr          error
	CreatePRErr        error
	EditPRErr          error
	EditPRTitle        string
	MergeResult        MergeResult
	MergeResults       []MergeResult // sequential results; takes precedence over MergeResult
	OnMerge            func()        // called on each MergePR call
	mergeIdx           int
	Checks             []CICheckResult
	ChecksFunc         func(call int) []CICheckResult // sequential checks; takes precedence
	checkCalls         int
	ChecksErr          error
	MergeCalls         int
	LastMergeOpts      MergeOpts
	EnforceAdmins      bool
	EnforceAdminsErr   error
	PostEnforceOutput  string
	PostEnforceErr     error
	PostEnforceCalled  bool
	CheckEnforceCalled bool
	PRNumber           int
	PRTitle            string
	PRURL              string
	SearchPRNumber     int
	PRDiffOutput       string
	CreatedPR          int
	PRState            PRState
	PRBase             string
	PRHead             string
	HeadSHA    string
	OpenPRBranches     []string
	SearchCalled       bool
	PRDiffCalled       bool
	RunLogValue        string
	ReopenPRErr           error
	ReopenPRCalled        bool
	CreatePRViaAPIResult  int
	CreatePRViaAPIErr     error
	CreatePRViaAPICalled  bool
	JobStepCount          int
	AllPRs                []PRInfo
	AllPRsErr             error
	ActiveReviewers    []Reviewer
	DetectReviewerErr  error
	AutoReviewResult   *AutoReview
	PollReviewErr      error
	RequiredChecks     []string
	RequiredChecksErr  error
}

// newStubGitHub returns a stubGitHub with sensible defaults for the common
// case: an available GitHub instance with an open PR against main that merges
// successfully. Override specific fields for non-default scenarios.
func newStubGitHub() *stubGitHub {
	return &stubGitHub{
		IsAvailable: true,
		OpenPR:      42,
		PRNumber:    42,
		PRHead:      "feature",
		PRTitle:     "stub PR",
		PRURL:       "https://github.com/owner/repo/pull/42",
		PRBase:      "main",
		PRState:     PRStateOpen,
		MergeResult: MergeResult{Merged: true},
	}
}

func (s *stubGitHub) Available() bool { return s.IsAvailable }
func (s *stubGitHub) FindOpenPR(branch, repoURL string) (int, error) {
	return s.OpenPR, s.FindPRErr
}
func (s *stubGitHub) CreatePR(opts CreatePROpts) (int, error) {
	if s.CreatePRErr == nil && s.CreatedPR != 0 {
		s.OpenPR = s.CreatedPR
	}
	return s.CreatedPR, s.CreatePRErr
}
func (s *stubGitHub) EditPR(prNumber int, repoURL, title, body string) error {
	s.EditPRTitle = title
	return s.EditPRErr
}
func (s *stubGitHub) MergePR(prNumber int, repoURL string, opts MergeOpts) MergeResult {
	s.MergeCalls++
	s.LastMergeOpts = opts
	if s.OnMerge != nil {
		s.OnMerge()
	}
	// Sequential results take precedence.
	if s.mergeIdx < len(s.MergeResults) {
		r := s.MergeResults[s.mergeIdx]
		s.mergeIdx++
		return r
	}
	r := s.MergeResult
	// Default to success when no explicit result is configured.
	if !r.Merged && !r.Blocked && !r.Conflict && r.Message == "" {
		r.Merged = true
	}
	return r
}
func (s *stubGitHub) ListChecks(prNumber int, repoURL string) ([]CICheckResult, error) {
	s.checkCalls++
	if s.ChecksFunc != nil {
		return s.ChecksFunc(s.checkCalls), nil
	}
	return s.Checks, s.ChecksErr
}
func (s *stubGitHub) GetRunLog(prNumber int, workDir string) string { return s.RunLogValue }
func (s *stubGitHub) CheckEnforceAdmins(nwo, branch string) (bool, error) {
	s.CheckEnforceCalled = true
	return s.EnforceAdmins, s.EnforceAdminsErr
}
func (s *stubGitHub) PostEnforceAdmins(nwo, branch string) (string, error) {
	s.PostEnforceCalled = true
	return s.PostEnforceOutput, s.PostEnforceErr
}
func (s *stubGitHub) FindPR(branch, repoURL string) (int, string, string, error) {
	return s.PRNumber, s.PRTitle, s.PRURL, s.FindPRErr
}
func (s *stubGitHub) SearchPR(workDir, query string) (int, error) {
	s.SearchCalled = true
	return s.SearchPRNumber, nil
}
func (s *stubGitHub) PRDiff(repoURL string, prNumber int) (string, error) {
	s.PRDiffCalled = true
	return s.PRDiffOutput, nil
}
func (s *stubGitHub) GetPR(nwo string, prNumber int) (*PRDetail, error) {
	headSHA := s.HeadSHA
	if headSHA == "" {
		headSHA = fmt.Sprintf("stub-sha-%d", prNumber)
	}
	return &PRDetail{
		State:   s.PRState,
		BaseRef: s.PRBase,
		HeadRef: s.PRHead,
		HeadSHA: headSHA,
	}, nil
}
func (s *stubGitHub) ListOpenPRBranches(repoURL string) ([]string, error) {
	return s.OpenPRBranches, nil
}
func (s *stubGitHub) ReopenPR(prNumber int, repoURL string) error {
	s.ReopenPRCalled = true
	if s.ReopenPRErr == nil {
		s.OpenPR = prNumber
		s.PRState = PRStateOpen
	}
	return s.ReopenPRErr
}
func (s *stubGitHub) CreatePRViaAPI(nwo string, opts CreatePROpts) (int, error) {
	s.CreatePRViaAPICalled = true
	if s.CreatePRViaAPIErr == nil && s.CreatePRViaAPIResult != 0 {
		s.OpenPR = s.CreatePRViaAPIResult
	}
	return s.CreatePRViaAPIResult, s.CreatePRViaAPIErr
}
func (s *stubGitHub) GetJobStepCount(nwo string, prNumber int) (int, error) {
	return s.JobStepCount, nil
}
func (s *stubGitHub) ListAllPRs(workDir string) ([]PRInfo, error) {
	return s.AllPRs, s.AllPRsErr
}
func (s *stubGitHub) DetectActiveReviewers(nwo string) ([]Reviewer, error) {
	return s.ActiveReviewers, s.DetectReviewerErr
}
func (s *stubGitHub) PollReview(nwo string, botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error) {
	return s.AutoReviewResult, s.PollReviewErr
}
func (s *stubGitHub) GetRequiredChecks(nwo, branch string) ([]string, error) {
	return s.RequiredChecks, s.RequiredChecksErr
}

// NewRepoForTesting returns a *Repo wired to a stubGitHub for tests that need
// real git operations (branch creation, worktrees, etc.) with GitHub stubbed.
// The returned stubGitHub can be configured before use.
//
// cfg supplies the construction-time inputs the test wants on the Repo.
// Any GitHub field on cfg is overridden by the freshly-created stubGitHub.
// The cfg approach preserves the Rule-B no-mutation pattern: tests pass
// Config values rather than mutating fields after construction.
func NewRepoForTesting(cfg Config) (*Repo, *stubGitHub) {
	gh := newStubGitHub()
	cfg.GitHub = gh
	return New(cfg), gh
}
