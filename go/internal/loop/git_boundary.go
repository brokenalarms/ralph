package loop

import (
	"context"
	"time"

	"github.com/brokenalarms/ralph/internal/git"
)

// gitOps is the loop-local interface for git operations. *git.Repo satisfies
// it structurally — no explicit declaration needed on the git side.
// Defined here so the loop holds no exported method-bearing type from the
// git package: only *git.Repo (for construction) and this unexported interface
// (for the loop's runtime dependency) appear in loop code.
type gitOps interface {
	GetProjectDir() string
	GetWorkDir() string
	GetWorktreeBranch() string
	GetPrevBranch() string
	IsBranchRenamed() bool
	SetBranchRenamed(v bool)

	FindOpenPRForBranch(branch string) (int, error)
	GetPRState(prNumber int) (git.PRState, error)
	ListOpenPRBranches() ([]string, error)
	GetPRBase(prNumber int) string
	FindPRForBranch(branch string) (number int, title, url string, err error)
	PRChainIsHealthy(prNumber int) (bool, string)
	PRDiffForTask(taskID string) string

	SetLocalTestsPassed(v bool)
	SetKnownPRNumber(n int)

	HeadRev() string
	HasDiff() bool
	HasUncommittedChanges() bool
	ChangedFiles(headBefore, headAfter string) []string
	DiffStatRange(from, to string) string
	DiffFull(from, to string) string
	LogOneline(from, to string) string
	ConflictDiff() string
	RemoteURL() string
	DetectDefaultBranch() string
	RecentChangedFiles(n int) string
	GetCIFailureLog(prNumber int) string

	SyncWorktreeBase(ctx context.Context, completedBranches []string) error
	BranchForTask(ctx context.Context, taskID, title string, meta git.BranchTaskMeta) (string, error)
	PrepareForNextTask(nextTaskID string)
	ResetToDefaultBranch()
	RenameBranchForTask(taskDesc, taskID string) error
	RenameBranchTo(name string)
	SetPrevBranch(branch string)

	TagTaskStart(taskID string)
	TagTaskEnd(taskID string)

	CommitAll(message string)

	EnsureUpToDate(ctx context.Context) error

	Push(ctx context.Context) error
	Ship(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error)
	PushAndCreatePR(ctx context.Context, taskID, taskDesc, body string) (int, error)

	DetectActiveReviewers() ([]git.Reviewer, error)
	PollReview(botUsername string, prNumber int, timeout time.Duration) (*git.AutoReview, error)

	ResumeTask(ctx context.Context, meta git.ResumeTaskMeta, opts git.ResumeTaskOpts) (git.ResumeTaskResult, error)

	MergeWithRetry(ctx context.Context, opts git.MergeRetryOpts) (bool, error)
	FlushUnpushedWork(ctx context.Context, taskID, taskDesc string, autoMerge bool) (merged bool, err error)
	PostMergeUpdateMain()

	FetchBranch(branch string) error
	CheckoutRemoteBranch(branch string)
	RemoteBranchHasCommits(branch string) bool
	RemoteBranchIsOnMain(branch string) bool
	BranchIsAheadOfMain(branch string) bool
	BranchIsAncestorOfMain(branch string) bool
	DeleteRemoteBranchByName(branch string) error

	RemoveWorktree()

	GitHubAvailable() bool
	ListAllPRs(workDir string) ([]git.PRInfo, error)
}
