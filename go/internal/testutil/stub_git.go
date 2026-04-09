package testutil

import (
	"context"
	"errors"
	"time"

	"github.com/brokenalarms/ralph/internal/git"
)

// StubGit implements git.GitOps for testing without spawning real git
// subprocesses. Configure fields to control return values; all methods
// have sensible zero-value defaults. Same pattern as StubBackend.
type StubGit struct {
	ProjectDir     string
	WorkDir        string
	WorktreeBranch string
	PrevBranch     string
	BranchRenamed  bool
	KnownPRNumber  int

	HeadRevValue        string
	HasDiffValue        bool
	HasUncommittedValue bool
	ChangedFilesValue   []string
	DiffStatValue       string
	DiffFullValue       string
	LogOnelineValue     string
	ConflictDiffValue   string
	RemoteURLValue      string
	DefaultBranch       string
	RecentFilesValue    string
	CIFailureLogValue   string

	EnsureUpToDateErr    error
	PushErr              error
	ShipResult           git.ShipResult
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

	GitHubStub git.GitHub

	// PR stub values.
	OpenPR       int
	OpenPRErr    error
	PRState      git.PRState
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
	LastShipOpts          git.ShipOpts
	FlushUnpushedCalls    int
	LastFlushAutoMerge    bool

	RenameBranchErr error

	// Optional func overrides for fine-grained test control.
	ShipFunc        func(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error)
	MergeRetryFunc  func(ctx context.Context) (bool, error)

	ActiveReviewers               []git.Reviewer
	DetectReviewersErr            error
	DetectActiveReviewersCalled   bool
	DetectActiveReviewersCallCount int
	PollReviewResult              *git.AutoReview
	PollReviewErr                 error
	PollReviewCalled              bool
	PollReviewLastUsername        string
	PollReviewLastTimeout         time.Duration

	PushAndCreatePRCalls    int
	FlushUnpushedWorkFunc   func(ctx context.Context, taskID, taskDesc string, autoMerge bool) (bool, error)

	// Optional hooks for call-order recording in integration tests.
	OnRenameBranch          func(desc, id string)
	OnDetectActiveReviewers func()
}

// Compile-time check that StubGit satisfies git.GitOps.
var _ git.GitOps = (*StubGit)(nil)

func (s *StubGit) GetProjectDir() string     { return s.ProjectDir }
func (s *StubGit) GetWorkDir() string         { return s.WorkDir }
func (s *StubGit) GetWorktreeBranch() string  { return s.WorktreeBranch }
func (s *StubGit) GetPrevBranch() string      { return s.PrevBranch }
func (s *StubGit) IsBranchRenamed() bool      { return s.BranchRenamed }
func (s *StubGit) SetBranchRenamed(v bool)    { s.BranchRenamed = v }
func (s *StubGit) SetLocalTestsPassed(v bool) {}
func (s *StubGit) SetKnownPRNumber(n int)     { s.KnownPRNumber = n }

func (s *StubGit) FindOpenPRForBranch(branch string) (int, error) {
	if s.OpenPR != 0 {
		return s.OpenPR, s.OpenPRErr
	}
	if gh, ok := s.GitHubStub.(*git.StubGitHub); ok && gh != nil {
		return gh.FindOpenPR(branch, s.RemoteURLValue)
	}
	return 0, nil
}
func (s *StubGit) GetPRState(prNumber int) (git.PRState, error) {
	if s.PRState != "" {
		return s.PRState, s.PRStateErr
	}
	if gh, ok := s.GitHubStub.(*git.StubGitHub); ok && gh != nil {
		pr, err := gh.GetPR("", prNumber)
		if err != nil {
			return "", err
		}
		return pr.State, nil
	}
	return "", nil
}
func (s *StubGit) ListOpenPRBranches() ([]string, error) {
	if len(s.OpenBranches) > 0 {
		return s.OpenBranches, nil
	}
	if gh, ok := s.GitHubStub.(*git.StubGitHub); ok && gh != nil {
		return gh.ListOpenPRBranches(s.RemoteURLValue)
	}
	return nil, nil
}
func (s *StubGit) GetPRBase(prNumber int) string {
	if s.PRBase != "" {
		return s.PRBase
	}
	if gh, ok := s.GitHubStub.(*git.StubGitHub); ok && gh != nil {
		pr, err := gh.GetPR("", prNumber)
		if err != nil {
			return ""
		}
		return pr.BaseRef
	}
	return ""
}
func (s *StubGit) FindPRForBranch(branch string) (int, string, string, error) {
	if s.PRNumber != 0 {
		return s.PRNumber, s.PRTitle, s.PRURL, nil
	}
	if gh, ok := s.GitHubStub.(*git.StubGitHub); ok && gh != nil {
		return gh.FindPR(branch, "")
	}
	return 0, "", "", nil
}
func (s *StubGit) PRDiffForTask(_ string) string { return "" }
func (s *StubGit) PRChainIsHealthy(prNumber int) (bool, string) {
	if s.PRHealthMsg != "" {
		return s.PRHealthy, s.PRHealthMsg
	}
	// Default: healthy
	return true, ""
}

func (s *StubGit) HeadRev() string                                    { return s.HeadRevValue }
func (s *StubGit) HasDiff() bool                                      { return s.HasDiffValue }
func (s *StubGit) HasUncommittedChanges() bool                        { return s.HasUncommittedValue }
func (s *StubGit) ChangedFiles(_, _ string) []string                  { return s.ChangedFilesValue }
func (s *StubGit) DiffStatRange(_, _ string) string                   { return s.DiffStatValue }
func (s *StubGit) DiffFull(_, _ string) string                        { return s.DiffFullValue }
func (s *StubGit) LogOneline(_, _ string) string                      { return s.LogOnelineValue }
func (s *StubGit) ConflictDiff() string                               { return s.ConflictDiffValue }
func (s *StubGit) RemoteURL() string                                  { return s.RemoteURLValue }
func (s *StubGit) DetectDefaultBranch() string {
	if s.DefaultBranch != "" {
		return s.DefaultBranch
	}
	return "main"
}
func (s *StubGit) RecentChangedFiles(_ int) string                    { return s.RecentFilesValue }
func (s *StubGit) GetCIFailureLog(_ int) string                    { return s.CIFailureLogValue }

func (s *StubGit) PrepareForNextTask(nextTaskID string) {
	s.PrepareForNextCalls++
	s.PrepareForNextTaskIDs = append(s.PrepareForNextTaskIDs, nextTaskID)
	s.BranchRenamed = false
}

func (s *StubGit) ResetToDefaultBranch() {
	s.ResetCalls++
	s.BranchRenamed = false
}

func (s *StubGit) RenameBranchForTask(taskDesc, taskID string) error {
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
	slug := git.Slugify(taskDesc)
	if slug == "" {
		return nil
	}
	s.WorktreeBranch = git.BranchName(taskID, slug)
	s.BranchRenamed = true
	return nil
}

func (s *StubGit) RenameBranchTo(name string) {
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

func (s *StubGit) SetPrevBranch(branch string) {
	s.PrevBranch = branch
	s.SetPrevBranchCalls = append(s.SetPrevBranchCalls, branch)
}

func (s *StubGit) TagTaskStart(taskID string) {
	s.TagsCreated = append(s.TagsCreated, "task/"+taskID+"/start")
}

func (s *StubGit) TagTaskEnd(taskID string) {
	s.TagsCreated = append(s.TagsCreated, "task/"+taskID+"/end")
}

func (s *StubGit) CommitAll(message string) {
	s.CommitMessages = append(s.CommitMessages, message)
}

func (s *StubGit) EnsureUpToDate(_ context.Context) error  { return s.EnsureUpToDateErr }
func (s *StubGit) Push(_ context.Context) error             { return s.PushErr }

func (s *StubGit) Ship(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
	s.ShipCalls++
	s.LastShipOpts = opts
	if s.ShipFunc != nil {
		return s.ShipFunc(ctx, opts)
	}
	result := s.ShipResult
	if s.ShipErr != nil {
		return result, s.ShipErr
	}
	// Mirror real Ship: when PRNumber is set, check PR state first.
	if opts.PRNumber != 0 {
		result.PRNumber = opts.PRNumber
		prState, stateErr := s.GetPRState(opts.PRNumber)
		if stateErr != nil {
			return result, stateErr
		}
		switch prState {
		case git.PRStateMerged:
			result.AlreadyMerged = true
			result.Merged = true
			s.PostMergeUpdateMain()
			return result, nil
		case git.PRStateClosed:
			result.Closed = true
			return result, nil
		}
	}
	if opts.AutoMerge {
		merged, mergeErr := s.MergeWithRetry(ctx, git.MergeRetryOpts{})
		if mergeErr != nil {
			var ciErr *git.CIFailureError
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

func (s *StubGit) PushAndCreatePR(_ context.Context, _, _, _ string) (int, error) {
	s.PushAndCreatePRCalls++
	return s.PushPRNumber, s.PushPRErr
}

func (s *StubGit) MergeWithRetry(ctx context.Context, _ git.MergeRetryOpts) (bool, error) {
	s.MergeRetryCalls++
	if s.MergeRetryFunc != nil {
		return s.MergeRetryFunc(ctx)
	}
	return s.MergeRetryResult, s.MergeRetryErr
}

func (s *StubGit) FlushUnpushedWork(ctx context.Context, taskID, taskDesc string, autoMerge bool) (bool, error) {
	s.FlushUnpushedCalls++
	s.LastFlushAutoMerge = autoMerge
	if s.FlushUnpushedWorkFunc != nil {
		return s.FlushUnpushedWorkFunc(ctx, taskID, taskDesc, autoMerge)
	}
	return s.FlushMerged, s.FlushErr
}

func (s *StubGit) PostMergeUpdateMain() {
	s.PostMergeUpdateCalls++
}

func (s *StubGit) DetectActiveReviewers() ([]git.Reviewer, error) {
	s.DetectActiveReviewersCalled = true
	s.DetectActiveReviewersCallCount++
	if s.OnDetectActiveReviewers != nil {
		s.OnDetectActiveReviewers()
	}
	return s.ActiveReviewers, s.DetectReviewersErr
}

func (s *StubGit) PollReview(botUsername string, _ int, timeout time.Duration) (*git.AutoReview, error) {
	s.PollReviewCalled = true
	s.PollReviewLastUsername = botUsername
	s.PollReviewLastTimeout = timeout
	return s.PollReviewResult, s.PollReviewErr
}

func (s *StubGit) FetchBranch(_ string) error            { return s.FetchBranchErr }
func (s *StubGit) CheckoutRemoteBranch(_ string)         {}
func (s *StubGit) RemoteBranchHasCommits(_ string) bool  { return s.RemoteBranchCommits }
func (s *StubGit) RemoteBranchIsOnMain(_ string) bool    { return s.RemoteBranchOnMain }
func (s *StubGit) BranchIsAheadOfMain(_ string) bool     { return s.BranchAheadOfMain }
func (s *StubGit) BranchIsAncestorOfMain(_ string) bool  { return s.BranchAncestorOfMain }
func (s *StubGit) DeleteRemoteBranchByName(_ string) error {
	s.DeleteBranchCalled = true
	return s.DeleteBranchErr
}
