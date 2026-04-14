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
func (s *StubGitHub) FindPR(branch, repoURL string) (int, string, string, error) {
	return s.PRNumber, s.PRTitle, s.PRURL, s.FindPRErr
}
func (s *StubGitHub) SearchPR(workDir, query string) (int, error) {
	return s.SearchPRNumber, nil
}
func (s *StubGitHub) PRDiff(repoURL string, prNumber int) (string, error) {
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

// --- New stub API (migration target per docs/specs/stub-interface-rewrite.md) ---
//
// stubGitHub is a true in-memory fake of the gitHub interface. Its behavior
// is fixed: every test runs against the same model. What varies per test is
// the initial state of the world (PRs, Checks, static return values) passed
// in through StubGitHubConfig.
//
// Construction is the one and only configuration seam. The stub's internal
// state is reachable only through gitHub interface methods — the same methods
// production uses — so tests observe "what the SUT did" by querying the
// world, not by reading stub fields.
//
// Added alongside the legacy StubGitHub in this scaffold commit. The final
// commit of the rewrite deletes the legacy type and renames newStubGitHub
// to NewStubGitHub.

// StubPR describes a pull request that exists in the fake's world.
// Unset fields are filled in with deterministic defaults at construction:
// URL → https://github.com/owner/repo/pull/<Number>, HeadSHA → stub-sha-<Number>,
// Base → "main", State → PRStateOpen.
//
// Conflicted and Blocked describe world-state causes, not prescribed
// outcomes — the fake's MergePR derives its return value from these
// properties the same way real GitHub derives its response from the actual
// world. A PR with Conflicted=true cannot be merged until the conflict is
// resolved; a PR with Blocked=true is gated by branch protection.
type StubPR struct {
	Number     int
	Title      string
	URL        string
	Branch     string
	Base       string
	HeadSHA    string
	State      PRState
	Conflicted bool
	Blocked    bool
}

// StubGitHubConfig declares the starting state of the fake's world and any
// static return values. All fields are plain data; none programs per-call
// behavior. For fault injection, set the *Err field for the method that
// should fail — a single value per field, no sequencing.
type StubGitHubConfig struct {
	Available bool

	// The world.
	PRs    []StubPR
	Checks map[int][]CICheckResult

	// Static return values.
	RunLog           string
	Reviewers        []Reviewer
	RequiredChecks   []string
	JobStepCount     int
	PollReviewResult *AutoReview
	FetchThreadIDs   map[int]string
	PRDiffOutput     string
	SearchPRResult   int

	// Fault injection — single error per method.
	CreatePRErr           error
	CreatePRViaAPIErr     error
	EditPRErr             error
	ReopenPRErr           error
	ListChecksErr         error
	FindOpenPRErr         error
	FindPRErr             error
	GetPRErr              error
	ListAllPRsErr         error
	ListOpenPRBranchesErr error
	SearchPRErr           error
	PRDiffErr             error
	GetJobStepCountErr    error
	DetectReviewersErr    error
	PollReviewErr         error
	RequiredChecksErr     error
	ReplyToReviewErr      error
	FetchThreadIDsErr     error
	ResolveThreadErr      error
}

// stubGitHub is the unexported fake. Tests never see this type directly;
// they receive it only through the gitHub interface value returned by
// newStubGitHub.
type stubGitHub struct {
	cfg          StubGitHubConfig
	prs          map[int]*StubPR
	nextPRNumber int
}

// Compile-time check that stubGitHub implements the full gitHub interface.
var _ gitHub = (*stubGitHub)(nil)

// newStubGitHub returns a stubGitHub initialized with cfg's world, typed
// as the gitHub interface so callers cannot reach into its internal state.
// Package-private: external packages configure GitHub behavior through
// StubRepoConfig.GitHub when they need it; internal/git tests use this
// constructor directly.
func newStubGitHub(cfg StubGitHubConfig) gitHub {
	s := &stubGitHub{
		cfg: cfg,
		prs: make(map[int]*StubPR, len(cfg.PRs)),
	}
	maxNum := 0
	for i := range cfg.PRs {
		pr := cfg.PRs[i] // copy
		normalizeStubPR(&pr)
		s.prs[pr.Number] = &pr
		if pr.Number > maxNum {
			maxNum = pr.Number
		}
	}
	s.nextPRNumber = maxNum + 1
	if s.nextPRNumber == 1 {
		// No pre-loaded PRs: start numbering at something that can't collide
		// with a default test assumption.
		s.nextPRNumber = 100
	}
	return s
}

// normalizeStubPR fills in deterministic defaults for unset fields.
func normalizeStubPR(pr *StubPR) {
	if pr.Base == "" {
		pr.Base = "main"
	}
	if pr.State == "" {
		pr.State = PRStateOpen
	}
	if pr.HeadSHA == "" {
		pr.HeadSHA = fmt.Sprintf("stub-sha-%d", pr.Number)
	}
	if pr.URL == "" {
		pr.URL = fmt.Sprintf("https://github.com/owner/repo/pull/%d", pr.Number)
	}
}

func (s *stubGitHub) Available() bool { return s.cfg.Available }

func (s *stubGitHub) FindOpenPR(branch, repoURL string) (int, error) {
	if s.cfg.FindOpenPRErr != nil {
		return 0, s.cfg.FindOpenPRErr
	}
	for _, pr := range s.prs {
		if pr.State == PRStateOpen && pr.Branch == branch {
			return pr.Number, nil
		}
	}
	return 0, nil
}

func (s *stubGitHub) CreatePR(opts CreatePROpts) (int, error) {
	if s.cfg.CreatePRErr != nil {
		return 0, s.cfg.CreatePRErr
	}
	num := s.nextPRNumber
	s.nextPRNumber++
	pr := &StubPR{
		Number: num,
		Title:  opts.Title,
		Branch: opts.Head,
		Base:   opts.Base,
		State:  PRStateOpen,
	}
	normalizeStubPR(pr)
	s.prs[num] = pr
	return num, nil
}

func (s *stubGitHub) MergePR(prNumber int, _ string, _ MergeOpts) MergeResult {
	pr, ok := s.prs[prNumber]
	if !ok {
		return MergeResult{Merged: false, Message: fmt.Sprintf("PR %d not found", prNumber)}
	}
	if pr.State != PRStateOpen {
		return MergeResult{Merged: false, Message: fmt.Sprintf("PR %d not open (state=%s)", prNumber, pr.State)}
	}
	if pr.Conflicted {
		return MergeResult{Conflict: true, Message: fmt.Sprintf("PR %d has merge conflicts", prNumber)}
	}
	if pr.Blocked {
		return MergeResult{Blocked: true, Message: fmt.Sprintf("PR %d blocked by branch protection", prNumber)}
	}
	pr.State = PRStateMerged
	return MergeResult{Merged: true}
}

func (s *stubGitHub) ListChecks(prNumber int, _ string) ([]CICheckResult, error) {
	return s.cfg.Checks[prNumber], s.cfg.ListChecksErr
}

func (s *stubGitHub) EditPR(prNumber int, _, title, _ string) error {
	if s.cfg.EditPRErr != nil {
		return s.cfg.EditPRErr
	}
	if pr, ok := s.prs[prNumber]; ok {
		pr.Title = title
	}
	return nil
}

func (s *stubGitHub) GetRunLog(_ int, _ string) string {
	return s.cfg.RunLog
}

func (s *stubGitHub) FindPR(branch, _ string) (int, string, string, error) {
	if s.cfg.FindPRErr != nil {
		return 0, "", "", s.cfg.FindPRErr
	}
	for _, pr := range s.prs {
		if pr.Branch == branch {
			return pr.Number, pr.Title, pr.URL, nil
		}
	}
	return 0, "", "", nil
}

func (s *stubGitHub) SearchPR(_, _ string) (int, error) {
	return s.cfg.SearchPRResult, s.cfg.SearchPRErr
}

func (s *stubGitHub) PRDiff(_ string, _ int) (string, error) {
	return s.cfg.PRDiffOutput, s.cfg.PRDiffErr
}

func (s *stubGitHub) GetPR(_ string, prNumber int) (*PRDetail, error) {
	if s.cfg.GetPRErr != nil {
		return nil, s.cfg.GetPRErr
	}
	pr, ok := s.prs[prNumber]
	if !ok {
		return nil, nil
	}
	return &PRDetail{
		State:   pr.State,
		BaseRef: pr.Base,
		HeadRef: pr.Branch,
		HeadSHA: pr.HeadSHA,
	}, nil
}

func (s *stubGitHub) ListOpenPRBranches(_ string) ([]string, error) {
	if s.cfg.ListOpenPRBranchesErr != nil {
		return nil, s.cfg.ListOpenPRBranchesErr
	}
	// Collect open PR branches in deterministic (number-ascending) order so
	// tests don't depend on map iteration order.
	nums := make([]int, 0, len(s.prs))
	for n := range s.prs {
		nums = append(nums, n)
	}
	sortInts(nums)
	var branches []string
	for _, n := range nums {
		if s.prs[n].State == PRStateOpen {
			branches = append(branches, s.prs[n].Branch)
		}
	}
	return branches, nil
}

func (s *stubGitHub) ReopenPR(prNumber int, _ string) error {
	if s.cfg.ReopenPRErr != nil {
		return s.cfg.ReopenPRErr
	}
	if pr, ok := s.prs[prNumber]; ok {
		pr.State = PRStateOpen
	}
	return nil
}

func (s *stubGitHub) CreatePRViaAPI(_ string, opts CreatePROpts) (int, error) {
	if s.cfg.CreatePRViaAPIErr != nil {
		return 0, s.cfg.CreatePRViaAPIErr
	}
	return s.CreatePR(opts)
}

func (s *stubGitHub) GetJobStepCount(_ string, _ int) (int, error) {
	return s.cfg.JobStepCount, s.cfg.GetJobStepCountErr
}

func (s *stubGitHub) ListAllPRs(_ string) ([]PRInfo, error) {
	if s.cfg.ListAllPRsErr != nil {
		return nil, s.cfg.ListAllPRsErr
	}
	nums := make([]int, 0, len(s.prs))
	for n := range s.prs {
		nums = append(nums, n)
	}
	sortInts(nums)
	out := make([]PRInfo, 0, len(nums))
	for _, n := range nums {
		pr := s.prs[n]
		out = append(out, PRInfo{Number: pr.Number, Head: pr.Branch, Base: pr.Base, State: pr.State})
	}
	return out, nil
}

func (s *stubGitHub) DetectActiveReviewers(_ string) ([]Reviewer, error) {
	return s.cfg.Reviewers, s.cfg.DetectReviewersErr
}

func (s *stubGitHub) PollReview(_, _ string, _ int, _ time.Duration) (*AutoReview, error) {
	return s.cfg.PollReviewResult, s.cfg.PollReviewErr
}

func (s *stubGitHub) GetRequiredChecks(_, _ string) ([]string, error) {
	return s.cfg.RequiredChecks, s.cfg.RequiredChecksErr
}

func (s *stubGitHub) ReplyToReviewComment(_ string, _, _ int, _ string) error {
	return s.cfg.ReplyToReviewErr
}

func (s *stubGitHub) FetchReviewThreadIDs(_ string, _ int, _ []int) (map[int]string, error) {
	return s.cfg.FetchThreadIDs, s.cfg.FetchThreadIDsErr
}

func (s *stubGitHub) ResolveReviewThread(_ string) error {
	return s.cfg.ResolveThreadErr
}

// sortInts sorts a slice in ascending order. Local helper to avoid importing
// sort in this file purely for one call site.
func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// --- stubRepo: true in-memory fake of Ops for external-package unit tests ---
//
// StubRepoConfig declares the starting world state and prescribed outcomes for
// a stubRepo. All fields are plain data; no callbacks, no sequenced slices.
// Construction is the one and only configuration seam — state mutations after
// construction happen only through Ops interface methods driven by the SUT.
//
// PR-related methods delegate to an inner stubGitHub constructed from
// cfg.GitHub. Git-module-local methods read from cfg/mutable state.
type StubRepoConfig struct {
	// World state — the git-module view of the repo.
	ProjectDir     string
	WorkDir        string
	RalphDir       string
	BaseBranch     string // default "main" if empty
	WorktreeBranch string
	PrevBranch     string
	BranchRenamed  bool
	RemoteURL      string
	HeadRev        string // returned by HeadRev()
	DefaultBranch  string // returned by DetectDefaultBranch(); defaults to BaseBranch
	HasUncommitted bool
	HasDiff        bool
	KnownPRNumber  int

	// Inner GitHub fake config. NewStub builds a stubGitHub from this and
	// delegates PR-related methods to it.
	GitHub StubGitHubConfig

	// Static returns for read-only queries.
	ChangedFiles           []string
	DiffFilesBetweenResult []string
	DiffStat               string
	DiffFullResult         string
	LogOnelineResult       string
	ConflictDiff           string
	RecentChangedFiles     string
	CIFailureLog           string
	ProjectBranches        []string

	// Ship / merge results — prescribed outcomes where deriving from state
	// would require an unreasonable modeling burden.
	Ship               ShipResult
	ShipErr            error
	PushAndCreatePRNum int
	PushAndCreatePRErr error
	MergeRetrySucceeds bool
	MergeRetryErr      error
	FlushMerged        bool
	FlushErr           error
	MergeStackResult   MergeStackResult
	MergeStackErr      error
	ResumeTaskResult   ResumeTaskResult
	ResumeTaskErr      error

	// Per-method fault injection for simple methods.
	InitErr                      error
	SyncWorktreeBaseErr          error
	BranchForTaskResult          string // override for BranchForTask; default derived from taskID
	BranchForTaskErr             error
	EnsureUpToDateErr            error
	PushErr                      error
	FetchBranchErr               error
	DeleteRemoteBranchByNameErr  error
	ReplyToAndResolveCommentsErr error
	RenameBranchForTaskErr       error

	// Remote branch state — map of branch name → property. Missing entries
	// return zero values.
	RemoteBranchHasCommits map[string]bool
	RemoteBranchIsOnMain   map[string]bool
	BranchIsAheadOfMain    map[string]bool
	BranchIsAncestorOfMain map[string]bool

	// PRChainHealthy overrides PRChainIsHealthy output. When PRChainHealthyMsg
	// is non-empty, PRChainIsHealthy returns (PRChainHealthy, PRChainHealthyMsg).
	// Otherwise returns (true, "").
	PRChainHealthy    bool
	PRChainHealthyMsg string

	// PRDiffForTaskResult is returned by PRDiffForTask.
	PRDiffForTaskResult string
}

// stubRepo is an unexported fake of Ops. Tests receive it only through the
// Ops interface via NewStub. All mutable state is unexported.
type stubRepo struct {
	cfg StubRepoConfig
	gh  gitHub

	// Mutable state. Initialized from cfg, mutated by SUT-driven Ops methods
	// so subsequent reads reflect what the SUT did.
	worktreeBranch string
	prevBranch     string
	branchRenamed  bool
	headRev        string
	knownPRNumber  int
	commitSeq      int
}

// Compile-time check that *stubRepo satisfies Ops.
var _ Ops = (*stubRepo)(nil)

// NewStub returns a stubRepo as the Ops interface. External packages use
// this to isolate unit tests from the git module.
func NewStub(cfg StubRepoConfig) Ops {
	if cfg.BaseBranch == "" {
		cfg.BaseBranch = "main"
	}
	if cfg.DefaultBranch == "" {
		cfg.DefaultBranch = cfg.BaseBranch
	}
	return &stubRepo{
		cfg:            cfg,
		gh:             newStubGitHub(cfg.GitHub),
		worktreeBranch: cfg.WorktreeBranch,
		prevBranch:     cfg.PrevBranch,
		branchRenamed:  cfg.BranchRenamed,
		headRev:        cfg.HeadRev,
		knownPRNumber:  cfg.KnownPRNumber,
	}
}

// --- State accessors ---

func (s *stubRepo) GetProjectDir() string    { return s.cfg.ProjectDir }
func (s *stubRepo) GetWorkDir() string       { return s.cfg.WorkDir }
func (s *stubRepo) GetWorktreeBranch() string { return s.worktreeBranch }
func (s *stubRepo) GetPrevBranch() string    { return s.prevBranch }
func (s *stubRepo) IsBranchRenamed() bool    { return s.branchRenamed }
func (s *stubRepo) SetBranchRenamed(v bool)  { s.branchRenamed = v }
func (s *stubRepo) SetKnownPRNumber(n int)   { s.knownPRNumber = n }

// --- PR operations: delegate to inner gh, mirroring *Repo's delegation ---

func (s *stubRepo) FindOpenPRForBranch(branch string) (int, error) {
	if !s.gh.Available() {
		return 0, nil
	}
	return s.gh.FindOpenPR(branch, s.cfg.RemoteURL)
}

func (s *stubRepo) GetPRState(prNumber int) (PRState, error) {
	if !s.gh.Available() {
		return "", nil
	}
	pr, err := s.gh.GetPR(NWOFromRemote(s.cfg.RemoteURL), prNumber)
	if err != nil {
		return "", err
	}
	if pr == nil {
		return "", nil
	}
	return pr.State, nil
}

func (s *stubRepo) ListOpenPRBranches() ([]string, error) {
	if s.cfg.RemoteURL == "" || !s.gh.Available() {
		return nil, nil
	}
	return s.gh.ListOpenPRBranches(s.cfg.RemoteURL)
}

func (s *stubRepo) GetPRBase(prNumber int) string {
	if !s.gh.Available() {
		return ""
	}
	pr, err := s.gh.GetPR(NWOFromRemote(s.cfg.RemoteURL), prNumber)
	if err != nil || pr == nil {
		return ""
	}
	return pr.BaseRef
}

func (s *stubRepo) FindPRForBranch(branch string) (int, string, string, error) {
	if !s.gh.Available() {
		return 0, "", "", nil
	}
	return s.gh.FindPR(branch, s.cfg.RemoteURL)
}

func (s *stubRepo) PRChainIsHealthy(prNumber int) (bool, string) {
	if s.cfg.PRChainHealthyMsg != "" {
		return s.cfg.PRChainHealthy, s.cfg.PRChainHealthyMsg
	}
	return true, ""
}

func (s *stubRepo) PRDiffForTask(taskID string) string {
	return s.cfg.PRDiffForTaskResult
}

// --- Diff and status queries ---

func (s *stubRepo) HeadRev() string                        { return s.headRev }
func (s *stubRepo) HasDiff() bool                          { return s.cfg.HasDiff }
func (s *stubRepo) HasUncommittedChanges() bool            { return s.cfg.HasUncommitted }
func (s *stubRepo) ChangedFiles(_, _ string) []string      { return s.cfg.ChangedFiles }
func (s *stubRepo) DiffFilesBetween(_, _ string) []string  { return s.cfg.DiffFilesBetweenResult }
func (s *stubRepo) DiffStatRange(_, _ string) string       { return s.cfg.DiffStat }
func (s *stubRepo) DiffFull(_, _ string) string            { return s.cfg.DiffFullResult }
func (s *stubRepo) LogOneline(_, _ string) string          { return s.cfg.LogOnelineResult }
func (s *stubRepo) ConflictDiff() string                   { return s.cfg.ConflictDiff }
func (s *stubRepo) RemoteURL() string                      { return s.cfg.RemoteURL }
func (s *stubRepo) DetectDefaultBranch() string            { return s.cfg.DefaultBranch }
func (s *stubRepo) RecentChangedFiles(_ int) string        { return s.cfg.RecentChangedFiles }
func (s *stubRepo) GetCIFailureLog(_ int) string           { return s.cfg.CIFailureLog }

// --- Branch lifecycle ---

func (s *stubRepo) SyncWorktreeBase(_ context.Context, _ []string) error {
	return s.cfg.SyncWorktreeBaseErr
}

// BranchForTask mirrors production's contract: prepare-for-next-task (resets
// branchRenamed), then rename the branch for the task. Returns the resulting
// branch name.
func (s *stubRepo) BranchForTask(_ context.Context, taskID, title string, meta BranchTaskMeta) (string, error) {
	s.PrepareForNextTask(taskID)
	if s.cfg.BranchForTaskErr != nil {
		return "", s.cfg.BranchForTaskErr
	}
	if s.cfg.BranchForTaskResult != "" {
		s.worktreeBranch = s.cfg.BranchForTaskResult
		s.branchRenamed = true
		return s.worktreeBranch, nil
	}
	if meta.Branch != "" {
		s.worktreeBranch = meta.Branch
		s.branchRenamed = true
		return s.worktreeBranch, nil
	}
	if err := s.RenameBranchForTask(title, taskID); err != nil {
		return "", err
	}
	if s.worktreeBranch == "" {
		// Fall back to a deterministic derived name so callers always get
		// something back.
		s.worktreeBranch = "ralph/" + taskID
		s.branchRenamed = true
	}
	return s.worktreeBranch, nil
}

func (s *stubRepo) PrepareForNextTask(_ string) {
	s.branchRenamed = false
	s.prevBranch = s.worktreeBranch
}

func (s *stubRepo) ResetToDefaultBranch() {
	s.branchRenamed = false
}

func (s *stubRepo) RenameBranchForTask(taskDesc, taskID string) error {
	if s.cfg.RenameBranchForTaskErr != nil {
		return s.cfg.RenameBranchForTaskErr
	}
	if s.branchRenamed || s.worktreeBranch == "" || taskDesc == "" {
		return nil
	}
	slug := Slugify(taskDesc)
	if slug == "" {
		return nil
	}
	s.worktreeBranch = BranchName(taskID, slug)
	s.branchRenamed = true
	return nil
}

func (s *stubRepo) RenameBranchTo(name string) {
	if s.branchRenamed || s.worktreeBranch == "" || name == "" {
		return
	}
	s.worktreeBranch = name
	s.branchRenamed = true
}

func (s *stubRepo) SetPrevBranch(branch string) {
	s.prevBranch = branch
}

// --- Tag operations (no-ops in the stub; tags have no observable effect
// outside real git) ---

func (s *stubRepo) TagTaskStart(_ string) {}
func (s *stubRepo) TagTaskEnd(_ string)   {}

// --- Commit operations ---

// CommitAll advances the fake head so subsequent HeadRev calls reflect that
// a commit was made.
func (s *stubRepo) CommitAll(_ string) {
	s.commitSeq++
	s.headRev = fmt.Sprintf("stub-head-%d", s.commitSeq)
}

func (s *stubRepo) RevertFilesToRef(_ []string, _ string) {
	s.commitSeq++
	s.headRev = fmt.Sprintf("stub-head-%d", s.commitSeq)
}

func (s *stubRepo) EmptyCommit(_ string) {
	s.commitSeq++
	s.headRev = fmt.Sprintf("stub-head-%d", s.commitSeq)
}

// --- Sync / push operations ---

func (s *stubRepo) EnsureUpToDate(_ context.Context) error { return s.cfg.EnsureUpToDateErr }
func (s *stubRepo) Push(_ context.Context) error           { return s.cfg.PushErr }

// Ship returns the prescribed outcome. Tests that want observable post-Ship
// state (e.g. a findable PR) configure cfg.GitHub.PRs accordingly.
func (s *stubRepo) Ship(_ context.Context, _ ShipOpts) (ShipResult, error) {
	return s.cfg.Ship, s.cfg.ShipErr
}

func (s *stubRepo) PushAndCreatePR(_ context.Context, _, _, _ string) (int, error) {
	return s.cfg.PushAndCreatePRNum, s.cfg.PushAndCreatePRErr
}

// --- Reviewer / review operations: delegate to gh where possible ---

func (s *stubRepo) DetectActiveReviewers() ([]Reviewer, error) {
	nwo := NWOFromRemote(s.cfg.RemoteURL)
	if nwo == "" {
		return nil, nil
	}
	return s.gh.DetectActiveReviewers(nwo)
}

func (s *stubRepo) PollReview(botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error) {
	nwo := NWOFromRemote(s.cfg.RemoteURL)
	if nwo == "" {
		return nil, nil
	}
	return s.gh.PollReview(nwo, botUsername, prNumber, timeout)
}

func (s *stubRepo) ReplyToAndResolveComments(_ int, _ []ReviewComment) error {
	return s.cfg.ReplyToAndResolveCommentsErr
}

// --- Resume / merge operations ---

func (s *stubRepo) ResumeTask(_ context.Context, _ ResumeTaskMeta, _ ResumeTaskOpts) (ResumeTaskResult, error) {
	return s.cfg.ResumeTaskResult, s.cfg.ResumeTaskErr
}

func (s *stubRepo) MergeWithRetry(_ context.Context, _ MergeRetryOpts) (bool, error) {
	return s.cfg.MergeRetrySucceeds, s.cfg.MergeRetryErr
}

func (s *stubRepo) FlushUnpushedWork(_ context.Context, _, _ string, _ bool) (bool, error) {
	return s.cfg.FlushMerged, s.cfg.FlushErr
}

// PostMergeUpdateMain is a no-op in the stub: production fast-forwards the
// local main branch, but the stub has no local refs to update.
func (s *stubRepo) PostMergeUpdateMain() {}

func (s *stubRepo) MergeStack(_ context.Context, _ MergeStackOpts) (MergeStackResult, error) {
	return s.cfg.MergeStackResult, s.cfg.MergeStackErr
}

// --- Remote branch operations ---

func (s *stubRepo) FetchBranch(_ string) error     { return s.cfg.FetchBranchErr }
func (s *stubRepo) CheckoutRemoteBranch(_ string)  {}

func (s *stubRepo) RemoteBranchHasCommits(branch string) bool {
	return s.cfg.RemoteBranchHasCommits[branch]
}

func (s *stubRepo) RemoteBranchIsOnMain(branch string) bool {
	return s.cfg.RemoteBranchIsOnMain[branch]
}

func (s *stubRepo) BranchIsAheadOfMain(branch string) bool {
	return s.cfg.BranchIsAheadOfMain[branch]
}

func (s *stubRepo) BranchIsAncestorOfMain(branch string) bool {
	return s.cfg.BranchIsAncestorOfMain[branch]
}

func (s *stubRepo) DeleteRemoteBranchByName(_ string) error {
	return s.cfg.DeleteRemoteBranchByNameErr
}

// --- Init / worktree lifecycle ---

func (s *stubRepo) Init(_ context.Context) error { return s.cfg.InitErr }

// RemoveWorktree is a no-op: there is no real worktree to remove.
func (s *stubRepo) RemoveWorktree() {}

// --- GitHub availability and PR listing ---

func (s *stubRepo) GitHubAvailable() bool {
	return s.gh.Available()
}

func (s *stubRepo) ListAllPRs(workDir string) ([]PRInfo, error) {
	return s.gh.ListAllPRs(workDir)
}

func (s *stubRepo) ListProjectBranches() []string {
	return s.cfg.ProjectBranches
}
