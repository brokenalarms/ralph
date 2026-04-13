package git

import (
	"context"
	"errors"
	"fmt"
	"time"
)

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

	ReplyToReviewCommentErr   error
	FetchReviewThreadIDsErr   error
	FetchReviewThreadIDsResult map[int]string
	ResolveReviewThreadErr    error
}

// NewStubGitHub returns a StubGitHub with sensible defaults for the common
// case: an available GitHub instance with an open PR against main that merges
// successfully. Override specific fields for non-default scenarios.
func NewStubGitHub() *StubGitHub {
	return &StubGitHub{
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
		return r
	}
	r := s.MergeResult
	// Default to success when no explicit result is configured.
	if !r.Merged && !r.Blocked && !r.Conflict && r.Message == "" {
		r.Merged = true
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
func (s *StubGitHub) GetPR(nwo string, prNumber int) (*PRDetail, error) {
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
func (s *StubGitHub) ListOpenPRBranches(repoURL string) ([]string, error) {
	return s.OpenPRBranches, nil
}
func (s *StubGitHub) ReopenPR(prNumber int, repoURL string) error {
	s.ReopenPRCalled = true
	if s.ReopenPRErr == nil {
		s.OpenPR = prNumber
		s.PRState = PRStateOpen
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
func (s *StubGitHub) DetectActiveReviewers(nwo string) ([]Reviewer, error) {
	return s.ActiveReviewers, s.DetectReviewerErr
}
func (s *StubGitHub) PollReview(nwo string, botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error) {
	return s.AutoReviewResult, s.PollReviewErr
}
func (s *StubGitHub) GetRequiredChecks(nwo, branch string) ([]string, error) {
	return s.RequiredChecks, s.RequiredChecksErr
}
func (s *StubGitHub) ReplyToReviewComment(_ string, _, _ int, _ string) error {
	return s.ReplyToReviewCommentErr
}
func (s *StubGitHub) FetchReviewThreadIDs(_ string, _ int, _ []int) (map[int]string, error) {
	return s.FetchReviewThreadIDsResult, s.FetchReviewThreadIDsErr
}
func (s *StubGitHub) ResolveReviewThread(_ string) error {
	return s.ResolveReviewThreadErr
}

// StubRepo implements git.Ops for testing without spawning real git
// subprocesses. Configure fields to control return values; all methods
// have sensible zero-value defaults.
//
// Use NewStubRepo() to create a StubRepo with a default StubGitHub.
// Access GH to configure GitHub API behavior.
type StubRepo struct {
	ProjectDir     string
	WorkDir        string
	WorktreeBranch string
	PrevBranch     string
	BranchRenamed  bool
	KnownPRNumber  int

	// GH is the GitHub stub — configure it to control PR/CI/merge behavior.
	GH *StubGitHub

	HeadRevValue        string
	HasDiffValue        bool
	HasUncommittedValue bool
	ChangedFilesValue       []string
	DiffFilesBetweenValue   []string
	DiffFilesBetweenFunc    func(from, to string) []string
	RevertedFiles           []string
	RevertedRef             string
	DiffStatValue           string
	DiffFullValue       string
	LogOnelineValue     string
	ConflictDiffValue   string
	RemoteURLValue      string
	DefaultBranch       string
	RecentFilesValue    string
	CIFailureLogValue   string

	EnsureUpToDateErr    error
	PushErr              error
	ShipResult           ShipResult
	ShipErr              error
	PushPRNumber         int
	PushPRErr            error
	MergeRetryResult     bool
	MergeRetryErr        error
	FlushMerged          bool
	FlushErr             error
	FetchBranchErr       error
	DeleteBranchErr      error
	DeleteBranchCalled   bool
	RemoteBranchCommits  bool
	RemoteBranchOnMain   bool
	BranchAheadOfMain    bool
	BranchAncestorOfMain bool

	// PR stub values.
	OpenPR       int
	OpenPRErr    error
	PRState      PRState
	PRStateErr   error
	OpenBranches []string
	PRBase       string
	PRNumber     int
	PRTitle      string
	PRURL        string
	PRHealthy    bool
	PRHealthMsg  string

	// Call tracking.
	CommitMessages        []string
	TagsCreated           []string
	PrepareForNextCalls   int
	PrepareForNextTaskIDs []string
	ResetCalls            int
	PostMergeUpdateCalls  int
	RenameBranchCalls     int
	SetPrevBranchCalls    []string
	MergeRetryCalls       int
	ShipCalls             int
	LastShipOpts          ShipOpts
	FlushUnpushedCalls    int
	LastFlushAutoMerge    bool

	RenameBranchErr error

	// ResumeTask stub fields.
	ResumeResult ResumeTaskResult
	ResumeErr    error
	ResumeCalls  int

	// Optional func overrides for fine-grained test control.
	ShipFunc       func(ctx context.Context, opts ShipOpts) (ShipResult, error)
	MergeRetryFunc func(ctx context.Context) (bool, error)

	ActiveReviewers                []Reviewer
	DetectReviewersErr             error
	DetectActiveReviewersCalled    bool
	DetectActiveReviewersCallCount int
	PollReviewResult               *AutoReview
	PollReviewErr                  error
	PollReviewCalled               bool
	PollReviewLastUsername         string
	PollReviewLastTimeout          time.Duration

	ReplyToAndResolveCommentsCalled   bool
	ReplyToAndResolveCommentsErr      error
	ReplyToAndResolveCommentsPRNumber int
	ReplyToAndResolveCommentsArgs     []ReviewComment

	// HeadRevFunc overrides HeadRevValue when set, enabling tests to return
	// different values across sequential calls (e.g. before/after a commit).
	HeadRevFunc func() string

	PushAndCreatePRCalls  int
	FlushUnpushedWorkFunc func(ctx context.Context, taskID, taskDesc string, autoMerge bool) (bool, error)

	// Optional hooks for call-order recording in integration tests.
	OnRenameBranch          func(desc, id string)
	OnDetectActiveReviewers func()
}

// Compile-time check that *StubRepo satisfies Ops.
var _ Ops = (*StubRepo)(nil)

// NewStubRepo returns a StubRepo with a default StubGitHub. Configure fields
// or stub.GH fields for specific test scenarios.
func NewStubRepo() *StubRepo {
	return &StubRepo{
		GH: NewStubGitHub(),
	}
}



func (s *StubRepo) GetProjectDir() string     { return s.ProjectDir }
func (s *StubRepo) GetWorkDir() string         { return s.WorkDir }
func (s *StubRepo) GetWorktreeBranch() string  { return s.WorktreeBranch }
func (s *StubRepo) GetPrevBranch() string      { return s.PrevBranch }
func (s *StubRepo) IsBranchRenamed() bool      { return s.BranchRenamed }
func (s *StubRepo) SetBranchRenamed(v bool)    { s.BranchRenamed = v }
func (s *StubRepo) SetKnownPRNumber(n int)     { s.KnownPRNumber = n }

func (s *StubRepo) FindOpenPRForBranch(branch string) (int, error) {
	if s.OpenPR != 0 {
		return s.OpenPR, s.OpenPRErr
	}
	if s.GH == nil {
		return 0, nil
	}
	return s.GH.FindOpenPR(branch, s.RemoteURLValue)
}
func (s *StubRepo) GetPRState(prNumber int) (PRState, error) {
	if s.PRState != "" {
		return s.PRState, s.PRStateErr
	}
	if s.GH == nil {
		return "", nil
	}
	pr, err := s.GH.GetPR("", prNumber)
	if err != nil {
		return "", err
	}
	return pr.State, nil
}
func (s *StubRepo) ListOpenPRBranches() ([]string, error) {
	if len(s.OpenBranches) > 0 {
		return s.OpenBranches, nil
	}
	if s.GH == nil {
		return nil, nil
	}
	return s.GH.ListOpenPRBranches(s.RemoteURLValue)
}
func (s *StubRepo) GetPRBase(prNumber int) string {
	if s.PRBase != "" {
		return s.PRBase
	}
	if s.GH == nil {
		return ""
	}
	pr, err := s.GH.GetPR("", prNumber)
	if err != nil {
		return ""
	}
	return pr.BaseRef
}
func (s *StubRepo) FindPRForBranch(branch string) (int, string, string, error) {
	if s.PRNumber != 0 {
		return s.PRNumber, s.PRTitle, s.PRURL, nil
	}
	if s.GH == nil {
		return 0, "", "", nil
	}
	return s.GH.FindPR(branch, "")
}
func (s *StubRepo) PRDiffForTask(_ string) string { return "" }
func (s *StubRepo) PRChainIsHealthy(prNumber int) (bool, string) {
	if s.PRHealthMsg != "" {
		return s.PRHealthy, s.PRHealthMsg
	}
	return true, ""
}

func (s *StubRepo) HeadRev() string {
	if s.HeadRevFunc != nil {
		return s.HeadRevFunc()
	}
	return s.HeadRevValue
}
func (s *StubRepo) HasDiff() bool                                      { return s.HasDiffValue }
func (s *StubRepo) HasUncommittedChanges() bool                        { return s.HasUncommittedValue }
func (s *StubRepo) ChangedFiles(_, _ string) []string                  { return s.ChangedFilesValue }
func (s *StubRepo) DiffFilesBetween(from, to string) []string {
	if s.DiffFilesBetweenFunc != nil {
		return s.DiffFilesBetweenFunc(from, to)
	}
	return s.DiffFilesBetweenValue
}
func (s *StubRepo) DiffStatRange(_, _ string) string                   { return s.DiffStatValue }
func (s *StubRepo) DiffFull(_, _ string) string                        { return s.DiffFullValue }
func (s *StubRepo) LogOneline(_, _ string) string                      { return s.LogOnelineValue }
func (s *StubRepo) ConflictDiff() string                               { return s.ConflictDiffValue }
func (s *StubRepo) RemoteURL() string                                  { return s.RemoteURLValue }
func (s *StubRepo) DetectDefaultBranch() string {
	if s.DefaultBranch != "" {
		return s.DefaultBranch
	}
	return "main"
}
func (s *StubRepo) RecentChangedFiles(_ int) string { return s.RecentFilesValue }
func (s *StubRepo) GetCIFailureLog(_ int) string    { return s.CIFailureLogValue }

func (s *StubRepo) SyncWorktreeBase(_ context.Context, _ []string) error {
	return s.EnsureUpToDateErr
}

func (s *StubRepo) BranchForTask(_ context.Context, taskID, title string, meta BranchTaskMeta) (string, error) {
	s.PrepareForNextTask(taskID)

	s.SetPrevBranch("")
	if len(meta.CompletedBranches) > 0 {
		openBranches, err := s.ListOpenPRBranches()
		if err == nil && len(openBranches) > 0 {
			openSet := make(map[string]bool, len(openBranches))
			for _, b := range openBranches {
				openSet[b] = true
			}
			for i := len(meta.CompletedBranches) - 1; i >= 0; i-- {
				branch := meta.CompletedBranches[i]
				if branch != "" && openSet[branch] && s.BranchIsAheadOfMain(branch) {
					s.SetPrevBranch(branch)
					break
				}
			}
		}
	}

	storedBranch := meta.Branch
	if storedBranch != "" {
		s.RenameBranchTo(storedBranch)
		return s.WorktreeBranch, nil
	}
	if err := s.RenameBranchForTask(title, taskID); err != nil {
		return "", err
	}
	return s.WorktreeBranch, nil
}

func (s *StubRepo) PrepareForNextTask(nextTaskID string) {
	s.PrepareForNextCalls++
	s.PrepareForNextTaskIDs = append(s.PrepareForNextTaskIDs, nextTaskID)
	s.BranchRenamed = false
}

func (s *StubRepo) ResetToDefaultBranch() {
	s.ResetCalls++
	s.BranchRenamed = false
}

func (s *StubRepo) RenameBranchForTask(taskDesc, taskID string) error {
	if s.BranchRenamed || s.WorktreeBranch == "" || taskDesc == "" {
		return nil
	}
	if s.RenameBranchErr != nil {
		return s.RenameBranchErr
	}
	s.RenameBranchCalls++
	if s.OnRenameBranch != nil {
		s.OnRenameBranch(taskDesc, taskID)
	}
	slug := Slugify(taskDesc)
	if slug == "" {
		return nil
	}
	s.WorktreeBranch = BranchName(taskID, slug)
	s.BranchRenamed = true
	return nil
}

func (s *StubRepo) RenameBranchTo(name string) {
	if s.BranchRenamed || s.WorktreeBranch == "" || name == "" {
		return
	}
	if s.WorktreeBranch == name {
		s.BranchRenamed = true
		return
	}
	s.WorktreeBranch = name
	s.BranchRenamed = true
}

func (s *StubRepo) SetPrevBranch(branch string) {
	s.PrevBranch = branch
	s.SetPrevBranchCalls = append(s.SetPrevBranchCalls, branch)
}

func (s *StubRepo) TagTaskStart(taskID string) {
	s.TagsCreated = append(s.TagsCreated, "task/"+taskID+"/start")
}

func (s *StubRepo) TagTaskEnd(taskID string) {
	s.TagsCreated = append(s.TagsCreated, "task/"+taskID+"/end")
}

func (s *StubRepo) CommitAll(message string) {
	s.CommitMessages = append(s.CommitMessages, message)
}

func (s *StubRepo) RevertFilesToRef(files []string, ref string) {
	s.RevertedFiles = files
	s.RevertedRef = ref
	s.CommitMessages = append(s.CommitMessages, "revert-out-of-scope")
}

func (s *StubRepo) EmptyCommit(message string) {
	s.CommitMessages = append(s.CommitMessages, message)
}

func (s *StubRepo) EnsureUpToDate(_ context.Context) error { return s.EnsureUpToDateErr }
func (s *StubRepo) Push(_ context.Context) error            { return s.PushErr }

func (s *StubRepo) Ship(ctx context.Context, opts ShipOpts) (ShipResult, error) {
	s.ShipCalls++
	s.LastShipOpts = opts
	if s.ShipFunc != nil {
		return s.ShipFunc(ctx, opts)
	}
	result := s.ShipResult
	if s.ShipErr != nil {
		return result, s.ShipErr
	}
	if opts.PRNumber != 0 {
		result.PRNumber = opts.PRNumber
		prState, stateErr := s.GetPRState(opts.PRNumber)
		if stateErr != nil {
			return result, stateErr
		}
		switch prState {
		case PRStateMerged:
			result.AlreadyMerged = true
			result.Merged = true
			s.PostMergeUpdateMain()
			return result, nil
		case PRStateClosed:
			result.Closed = true
			return result, nil
		}
	}
	if opts.AutoMerge {
		merged, mergeErr := s.MergeWithRetry(ctx, MergeRetryOpts{})
		if mergeErr != nil {
			var ciErr *CIFailureError
			if errors.As(mergeErr, &ciErr) {
				result.CIFailure = true
				result.CIFailureDetail = ciErr
			}
			return result, nil
		}
		result.Merged = merged
		if merged {
			s.PostMergeUpdateMain()
		}
	}
	return result, nil
}

func (s *StubRepo) PushAndCreatePR(_ context.Context, _, _, _ string) (int, error) {
	s.PushAndCreatePRCalls++
	return s.PushPRNumber, s.PushPRErr
}

func (s *StubRepo) MergeWithRetry(ctx context.Context, _ MergeRetryOpts) (bool, error) {
	s.MergeRetryCalls++
	if s.MergeRetryFunc != nil {
		return s.MergeRetryFunc(ctx)
	}
	return s.MergeRetryResult, s.MergeRetryErr
}

func (s *StubRepo) FlushUnpushedWork(ctx context.Context, taskID, taskDesc string, autoMerge bool) (bool, error) {
	s.FlushUnpushedCalls++
	s.LastFlushAutoMerge = autoMerge
	if s.FlushUnpushedWorkFunc != nil {
		return s.FlushUnpushedWorkFunc(ctx, taskID, taskDesc, autoMerge)
	}
	return s.FlushMerged, s.FlushErr
}

func (s *StubRepo) PostMergeUpdateMain() {
	s.PostMergeUpdateCalls++
}

func (s *StubRepo) DetectActiveReviewers() ([]Reviewer, error) {
	s.DetectActiveReviewersCalled = true
	s.DetectActiveReviewersCallCount++
	if s.OnDetectActiveReviewers != nil {
		s.OnDetectActiveReviewers()
	}
	return s.ActiveReviewers, s.DetectReviewersErr
}

func (s *StubRepo) PollReview(botUsername string, _ int, timeout time.Duration) (*AutoReview, error) {
	s.PollReviewCalled = true
	s.PollReviewLastUsername = botUsername
	s.PollReviewLastTimeout = timeout
	return s.PollReviewResult, s.PollReviewErr
}

func (s *StubRepo) ReplyToAndResolveComments(prNumber int, comments []ReviewComment) error {
	s.ReplyToAndResolveCommentsCalled = true
	s.ReplyToAndResolveCommentsPRNumber = prNumber
	s.ReplyToAndResolveCommentsArgs = comments
	return s.ReplyToAndResolveCommentsErr
}

func (s *StubRepo) ResumeTask(_ context.Context, _ ResumeTaskMeta, _ ResumeTaskOpts) (ResumeTaskResult, error) {
	s.ResumeCalls++
	return s.ResumeResult, s.ResumeErr
}

func (s *StubRepo) FetchBranch(_ string) error            { return s.FetchBranchErr }
func (s *StubRepo) CheckoutRemoteBranch(_ string)         {}
func (s *StubRepo) RemoteBranchHasCommits(_ string) bool  { return s.RemoteBranchCommits }
func (s *StubRepo) RemoteBranchIsOnMain(_ string) bool    { return s.RemoteBranchOnMain }
func (s *StubRepo) BranchIsAheadOfMain(_ string) bool     { return s.BranchAheadOfMain }
func (s *StubRepo) BranchIsAncestorOfMain(_ string) bool  { return s.BranchAncestorOfMain }
func (s *StubRepo) DeleteRemoteBranchByName(_ string) error {
	s.DeleteBranchCalled = true
	return s.DeleteBranchErr
}

func (s *StubRepo) Init(_ context.Context) error { return nil }
func (s *StubRepo) RemoveWorktree() {}
func (s *StubRepo) MergeStack(_ context.Context, _ MergeStackOpts) (MergeStackResult, error) {
	return MergeStackResult{}, nil
}
func (s *StubRepo) ListProjectBranches() []string { return nil }

func (s *StubRepo) GitHubAvailable() bool {
	return s.GH != nil && s.GH.Available()
}

func (s *StubRepo) ListAllPRs(workDir string) ([]PRInfo, error) {
	if s.GH == nil {
		return nil, nil
	}
	return s.GH.ListAllPRs(workDir)
}
