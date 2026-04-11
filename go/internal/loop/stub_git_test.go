package loop

import (
	"context"
	"errors"
	"time"

	"github.com/brokenalarms/ralph/internal/git"
)

type stubGit struct {
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

	OpenPR       int
	OpenPRErr    error
	PRState      git.PRState
	PRStateErr   error
	OpenBranches []string
	PRBase       string
	PRNumber     int
	PRTitle      string
	PRURL        string
	FindPRErr    error
	PRHealthy    bool
	PRHealthMsg  string

	AllPRs            []git.PRInfo
	AllPRsErr         error
	GitHubAvailableValue bool

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

	ResumeResult git.ResumeTaskResult
	ResumeErr    error
	ResumeCalls  int

	ShipFunc       func(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error)
	MergeRetryFunc func(ctx context.Context) (bool, error)

	ActiveReviewers                []git.Reviewer
	DetectReviewersErr             error
	DetectActiveReviewersCalled    bool
	DetectActiveReviewersCallCount int
	PollReviewResult               *git.AutoReview
	PollReviewErr                  error
	PollReviewCalled               bool
	PollReviewLastUsername         string
	PollReviewLastTimeout          time.Duration

	PushAndCreatePRCalls  int
	FlushUnpushedWorkFunc func(ctx context.Context, taskID, taskDesc string, autoMerge bool) (bool, error)

	OnRenameBranch          func(desc, id string)
	OnDetectActiveReviewers func()
}

var _ gitOps = (*stubGit)(nil)

func newStubGit() *stubGit {
	return &stubGit{}
}

func (s *stubGit) GetProjectDir() string     { return s.ProjectDir }
func (s *stubGit) GetWorkDir() string         { return s.WorkDir }
func (s *stubGit) GetWorktreeBranch() string  { return s.WorktreeBranch }
func (s *stubGit) GetPrevBranch() string      { return s.PrevBranch }
func (s *stubGit) IsBranchRenamed() bool      { return s.BranchRenamed }
func (s *stubGit) SetBranchRenamed(v bool)    { s.BranchRenamed = v }
func (s *stubGit) SetLocalTestsPassed(v bool) {}
func (s *stubGit) SetKnownPRNumber(n int)     { s.KnownPRNumber = n }

func (s *stubGit) FindOpenPRForBranch(branch string) (int, error) {
	return s.OpenPR, s.OpenPRErr
}

func (s *stubGit) GetPRState(prNumber int) (git.PRState, error) {
	return s.PRState, s.PRStateErr
}

func (s *stubGit) ListOpenPRBranches() ([]string, error) {
	return s.OpenBranches, nil
}

func (s *stubGit) GetPRBase(prNumber int) string {
	return s.PRBase
}

func (s *stubGit) FindPRForBranch(branch string) (int, string, string, error) {
	return s.PRNumber, s.PRTitle, s.PRURL, s.FindPRErr
}

func (s *stubGit) PRDiffForTask(_ string) string { return "" }

func (s *stubGit) PRChainIsHealthy(prNumber int) (bool, string) {
	if s.PRHealthMsg != "" {
		return s.PRHealthy, s.PRHealthMsg
	}
	return true, ""
}

func (s *stubGit) HeadRev() string                                    { return s.HeadRevValue }
func (s *stubGit) HasDiff() bool                                      { return s.HasDiffValue }
func (s *stubGit) HasUncommittedChanges() bool                        { return s.HasUncommittedValue }
func (s *stubGit) ChangedFiles(_, _ string) []string                  { return s.ChangedFilesValue }
func (s *stubGit) DiffStatRange(_, _ string) string                   { return s.DiffStatValue }
func (s *stubGit) DiffFull(_, _ string) string                        { return s.DiffFullValue }
func (s *stubGit) LogOneline(_, _ string) string                      { return s.LogOnelineValue }
func (s *stubGit) ConflictDiff() string                               { return s.ConflictDiffValue }
func (s *stubGit) RemoteURL() string                                  { return s.RemoteURLValue }

func (s *stubGit) DetectDefaultBranch() string {
	if s.DefaultBranch != "" {
		return s.DefaultBranch
	}
	return "main"
}

func (s *stubGit) RecentChangedFiles(_ int) string { return s.RecentFilesValue }
func (s *stubGit) GetCIFailureLog(_ int) string    { return s.CIFailureLogValue }

func (s *stubGit) SyncWorktreeBase(_ context.Context, _ []string) error {
	return s.EnsureUpToDateErr
}

func (s *stubGit) BranchForTask(_ context.Context, taskID, title string, meta git.BranchTaskMeta) (string, error) {
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

func (s *stubGit) PrepareForNextTask(nextTaskID string) {
	s.PrepareForNextCalls++
	s.PrepareForNextTaskIDs = append(s.PrepareForNextTaskIDs, nextTaskID)
	s.BranchRenamed = false
}

func (s *stubGit) ResetToDefaultBranch() {
	s.ResetCalls++
	s.BranchRenamed = false
}

func (s *stubGit) RenameBranchForTask(taskDesc, taskID string) error {
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

func (s *stubGit) RenameBranchTo(name string) {
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

func (s *stubGit) SetPrevBranch(branch string) {
	s.PrevBranch = branch
	s.SetPrevBranchCalls = append(s.SetPrevBranchCalls, branch)
}

func (s *stubGit) TagTaskStart(taskID string) {
	s.TagsCreated = append(s.TagsCreated, "task/"+taskID+"/start")
}

func (s *stubGit) TagTaskEnd(taskID string) {
	s.TagsCreated = append(s.TagsCreated, "task/"+taskID+"/end")
}

func (s *stubGit) CommitAll(message string) {
	s.CommitMessages = append(s.CommitMessages, message)
}

func (s *stubGit) EnsureUpToDate(_ context.Context) error { return s.EnsureUpToDateErr }
func (s *stubGit) Push(_ context.Context) error            { return s.PushErr }

func (s *stubGit) Ship(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
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

func (s *stubGit) PushAndCreatePR(_ context.Context, _, _, _ string) (int, error) {
	s.PushAndCreatePRCalls++
	return s.PushPRNumber, s.PushPRErr
}

func (s *stubGit) MergeWithRetry(ctx context.Context, _ git.MergeRetryOpts) (bool, error) {
	s.MergeRetryCalls++
	if s.MergeRetryFunc != nil {
		return s.MergeRetryFunc(ctx)
	}
	return s.MergeRetryResult, s.MergeRetryErr
}

func (s *stubGit) FlushUnpushedWork(ctx context.Context, taskID, taskDesc string, autoMerge bool) (bool, error) {
	s.FlushUnpushedCalls++
	s.LastFlushAutoMerge = autoMerge
	if s.FlushUnpushedWorkFunc != nil {
		return s.FlushUnpushedWorkFunc(ctx, taskID, taskDesc, autoMerge)
	}
	return s.FlushMerged, s.FlushErr
}

func (s *stubGit) PostMergeUpdateMain() {
	s.PostMergeUpdateCalls++
}

func (s *stubGit) DetectActiveReviewers() ([]git.Reviewer, error) {
	s.DetectActiveReviewersCalled = true
	s.DetectActiveReviewersCallCount++
	if s.OnDetectActiveReviewers != nil {
		s.OnDetectActiveReviewers()
	}
	return s.ActiveReviewers, s.DetectReviewersErr
}

func (s *stubGit) PollReview(botUsername string, _ int, timeout time.Duration) (*git.AutoReview, error) {
	s.PollReviewCalled = true
	s.PollReviewLastUsername = botUsername
	s.PollReviewLastTimeout = timeout
	return s.PollReviewResult, s.PollReviewErr
}

func (s *stubGit) ResumeTask(_ context.Context, _ git.ResumeTaskMeta, _ git.ResumeTaskOpts) (git.ResumeTaskResult, error) {
	s.ResumeCalls++
	return s.ResumeResult, s.ResumeErr
}

func (s *stubGit) FetchBranch(_ string) error            { return s.FetchBranchErr }
func (s *stubGit) CheckoutRemoteBranch(_ string)         {}
func (s *stubGit) RemoteBranchHasCommits(_ string) bool  { return s.RemoteBranchCommits }
func (s *stubGit) RemoteBranchIsOnMain(_ string) bool    { return s.RemoteBranchOnMain }
func (s *stubGit) BranchIsAheadOfMain(_ string) bool     { return s.BranchAheadOfMain }
func (s *stubGit) BranchIsAncestorOfMain(_ string) bool  { return s.BranchAncestorOfMain }

func (s *stubGit) DeleteRemoteBranchByName(_ string) error {
	s.DeleteBranchCalled = true
	return s.DeleteBranchErr
}

func (s *stubGit) RemoveWorktree() {}

func (s *stubGit) GitHubAvailable() bool {
	return s.GitHubAvailableValue
}

func (s *stubGit) ListAllPRs(workDir string) ([]git.PRInfo, error) {
	return s.AllPRs, s.AllPRsErr
}
