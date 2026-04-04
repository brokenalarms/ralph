package git

// StubGitHub is a test double for the GitHub interface, shared across all
// packages that need to stub GitHub operations. Configure fields for the
// methods under test; all methods have sensible zero-value defaults.
type StubGitHub struct {
	IsAvailable        bool
	OpenPR             int
	FindPRErr          error
	CreatePRErr        error
	EditPRErr          error
	EditPRTitle        string
	MergeResult        MergeResult
	MergeResults       []MergeResult // sequential results; takes precedence over MergeResult
	AdminMergeResult   *MergeResult  // returned when Admin=true and REST would return Blocked
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
	CreatePRViaAPIResult  int
	CreatePRViaAPIErr     error
	CreatePRViaAPICalled  bool
	JobStepCount          int
	AllPRs                []PRInfo
	AllPRsErr             error
}

func (s *StubGitHub) Available() bool { return s.IsAvailable }
func (s *StubGitHub) FindOpenPR(branch, repoURL string) (int, error) {
	return s.OpenPR, s.FindPRErr
}
func (s *StubGitHub) CreatePR(opts CreatePROpts) (int, error) {
	if s.CreatePRErr == nil && s.CreatedPR != 0 {
		s.OpenPR = s.CreatedPR
	}
	return s.CreatedPR, s.CreatePRErr
}
func (s *StubGitHub) EditPR(prNumber int, repoURL, title, body string) error {
	s.EditPRTitle = title
	return s.EditPRErr
}
func (s *StubGitHub) MergePR(prNumber int, repoURL string, opts MergeOpts) MergeResult {
	s.MergeCalls++
	s.LastMergeOpts = opts
	if s.OnMerge != nil {
		s.OnMerge()
	}
	// Sequential results take precedence.
	if s.mergeIdx < len(s.MergeResults) {
		r := s.MergeResults[s.mergeIdx]
		s.mergeIdx++
		// Admin fallback: when Admin=true and REST would return Blocked, use
		// the next sequential result (the admin attempt outcome).
		if r.Blocked && opts.Admin && s.mergeIdx < len(s.MergeResults) {
			admin := s.MergeResults[s.mergeIdx]
			s.mergeIdx++
			return admin
		}
		return r
	}
	r := s.MergeResult
	// Default to success when no explicit result is configured.
	if !r.Merged && !r.Blocked && !r.Conflict && r.Message == "" {
		r.Merged = true
	}
	// Admin fallback: mirrors ghCLI.MergePR behaviour — when Admin=true and
	// the REST result would be Blocked, fall back to an admin attempt.
	if r.Blocked && opts.Admin {
		if s.AdminMergeResult != nil {
			return *s.AdminMergeResult
		}
		return MergeResult{Merged: true, Message: "merged (admin)"}
	}
	return r
}
func (s *StubGitHub) ListChecks(prNumber int, repoURL string) ([]CICheckResult, error) {
	s.checkCalls++
	if s.ChecksFunc != nil {
		return s.ChecksFunc(s.checkCalls), nil
	}
	return s.Checks, s.ChecksErr
}
func (s *StubGitHub) GetRunLog(prNumber int, workDir string) string { return s.RunLogValue }
func (s *StubGitHub) CheckEnforceAdmins(nwo, branch string) (bool, error) {
	s.CheckEnforceCalled = true
	return s.EnforceAdmins, s.EnforceAdminsErr
}
func (s *StubGitHub) PostEnforceAdmins(nwo, branch string) (string, error) {
	s.PostEnforceCalled = true
	return s.PostEnforceOutput, s.PostEnforceErr
}
func (s *StubGitHub) FindPR(branch, repoURL string) (int, string, string, error) {
	return s.PRNumber, s.PRTitle, s.PRURL, s.FindPRErr
}
func (s *StubGitHub) SearchPR(workDir, query string) (int, error) {
	s.SearchCalled = true
	return s.SearchPRNumber, nil
}
func (s *StubGitHub) PRDiff(repoURL string, prNumber int) (string, error) {
	s.PRDiffCalled = true
	return s.PRDiffOutput, nil
}
func (s *StubGitHub) GetPRState(workDir string, prNumber int) (string, error) {
	return s.PRState, nil
}
func (s *StubGitHub) GetPRBase(workDir string, prNumber int) (string, error) {
	return s.PRBase, nil
}
func (s *StubGitHub) GetPRHead(workDir string, prNumber int) (string, error) {
	return s.PRHead, nil
}
func (s *StubGitHub) GetPRHeadSHA(workDir string, prNumber int) (string, error) {
	return s.PRHeadSHA, nil
}
func (s *StubGitHub) ListOpenPRBranches(repoURL string) ([]string, error) {
	return s.OpenPRBranches, nil
}
func (s *StubGitHub) ReopenPR(prNumber int, repoURL string) error {
	s.ReopenPRCalled = true
	if s.ReopenPRErr == nil {
		s.OpenPR = prNumber
		s.PRState = "OPEN"
	}
	return s.ReopenPRErr
}
func (s *StubGitHub) CreatePRViaAPI(nwo string, opts CreatePROpts) (int, error) {
	s.CreatePRViaAPICalled = true
	if s.CreatePRViaAPIErr == nil && s.CreatePRViaAPIResult != 0 {
		s.OpenPR = s.CreatePRViaAPIResult
	}
	return s.CreatePRViaAPIResult, s.CreatePRViaAPIErr
}
func (s *StubGitHub) GetJobStepCount(nwo string, prNumber int) (int, error) {
	return s.JobStepCount, nil
}
func (s *StubGitHub) ListAllPRs(workDir string) ([]PRInfo, error) {
	return s.AllPRs, s.AllPRsErr
}
