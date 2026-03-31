package git

// StubGitHub is a test double for the GitHub interface, shared across all
// packages that need to stub GitHub operations. Configure fields for the
// methods under test; all methods have sensible zero-value defaults.
type StubGitHub struct {
	IsAvailable        bool
	OpenPR             string
	FindPRErr          error
	CreatePRErr        error
	EditPRErr          error
	EditPRTitle        string
	MergeResult        MergeResult
	MergeResults       []MergeResult // sequential results; takes precedence over MergeResult
	OnMerge            func()        // called on each MergePR call
	mergeIdx           int
	UpdateResult       bool
	UpdateErr          error
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
	PRNumber           string
	PRTitle            string
	PRURL              string
	SearchPRNumber     string
	PRDiffOutput       string
	CreatedPR          string
	PRState            string
	PRBase             string
	PRHead             string
	PRHeadSHA          string
	OpenPRBranches     []string
	SearchCalled       bool
	PRDiffCalled       bool
	RunLogValue        string
	ReopenPRErr           error
	ReopenPRCalled        bool
	CreatePRViaAPIResult  string
	CreatePRViaAPIErr     error
	CreatePRViaAPICalled  bool
	JobStepCount          int
}

func (s *StubGitHub) Available() bool { return s.IsAvailable }
func (s *StubGitHub) FindOpenPR(branch, repoURL string) (string, error) {
	return s.OpenPR, s.FindPRErr
}
func (s *StubGitHub) CreatePR(opts CreatePROpts) error {
	if s.CreatePRErr == nil && s.CreatedPR != "" {
		s.OpenPR = s.CreatedPR
	}
	return s.CreatePRErr
}
func (s *StubGitHub) EditPR(prNumber, repoURL, title, body string) error {
	s.EditPRTitle = title
	return s.EditPRErr
}
func (s *StubGitHub) MergePR(prNumber, repoURL string, opts MergeOpts) MergeResult {
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
func (s *StubGitHub) UpdateBranch(dir, nwo, prNumber string) (bool, error) {
	return s.UpdateResult, s.UpdateErr
}
func (s *StubGitHub) ListChecks(prNumber, repoURL string) ([]CICheckResult, error) {
	s.checkCalls++
	if s.ChecksFunc != nil {
		return s.ChecksFunc(s.checkCalls), nil
	}
	return s.Checks, s.ChecksErr
}
func (s *StubGitHub) GetRunLog(prNumber, workDir string) string { return s.RunLogValue }
func (s *StubGitHub) CheckEnforceAdmins(nwo, branch string) (bool, error) {
	s.CheckEnforceCalled = true
	return s.EnforceAdmins, s.EnforceAdminsErr
}
func (s *StubGitHub) PostEnforceAdmins(nwo, branch string) (string, error) {
	s.PostEnforceCalled = true
	return s.PostEnforceOutput, s.PostEnforceErr
}
func (s *StubGitHub) FindPR(branch, workDir string) (string, string, string, error) {
	return s.PRNumber, s.PRTitle, s.PRURL, s.FindPRErr
}
func (s *StubGitHub) SearchPR(workDir, query string) (string, error) {
	s.SearchCalled = true
	return s.SearchPRNumber, nil
}
func (s *StubGitHub) PRDiff(workDir, prNumber string) (string, error) {
	s.PRDiffCalled = true
	return s.PRDiffOutput, nil
}
func (s *StubGitHub) GetPRState(workDir, prNumber string) (string, error) {
	return s.PRState, nil
}
func (s *StubGitHub) GetPRBase(workDir, prNumber string) (string, error) {
	return s.PRBase, nil
}
func (s *StubGitHub) GetPRHead(workDir, prNumber string) (string, error) {
	return s.PRHead, nil
}
func (s *StubGitHub) GetPRHeadSHA(workDir, prNumber string) (string, error) {
	return s.PRHeadSHA, nil
}
func (s *StubGitHub) ListOpenPRBranches(repoURL string) ([]string, error) {
	return s.OpenPRBranches, nil
}
func (s *StubGitHub) ReopenPR(prNumber, repoURL string) error {
	s.ReopenPRCalled = true
	if s.ReopenPRErr == nil {
		s.OpenPR = prNumber
		s.PRState = "OPEN"
	}
	return s.ReopenPRErr
}
func (s *StubGitHub) CreatePRViaAPI(nwo string, opts CreatePROpts) (string, error) {
	s.CreatePRViaAPICalled = true
	if s.CreatePRViaAPIErr == nil && s.CreatePRViaAPIResult != "" {
		s.OpenPR = s.CreatePRViaAPIResult
	}
	return s.CreatePRViaAPIResult, s.CreatePRViaAPIErr
}
func (s *StubGitHub) GetJobStepCount(nwo, prNumber string) (int, error) {
	return s.JobStepCount, nil
}
