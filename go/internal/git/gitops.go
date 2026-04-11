package git

import (
	"context"
	"fmt"
	"time"
)

// Ops abstracts the git operations that the execution loop needs.
// Production code uses *Repo; tests use *StubRepo.
type Ops interface {
	// State accessors — replace direct field reads on *Repo.
	GetProjectDir() string
	GetWorkDir() string
	GetWorktreeBranch() string
	GetPrevBranch() string
	IsBranchRenamed() bool
	SetBranchRenamed(v bool)

	// PR operations — delegated to GitHub internally.
	FindOpenPRForBranch(branch string) (int, error)
	GetPRState(prNumber int) (PRState, error)
	ListOpenPRBranches() ([]string, error)
	GetPRBase(prNumber int) string
	FindPRForBranch(branch string) (number int, title, url string, err error)
	PRChainIsHealthy(prNumber int) (bool, string)
	PRDiffForTask(taskID string) string

	// CI bypass flag.
	SetLocalTestsPassed(v bool)

	// SetKnownPRNumber stores a known PR number so merge/PR operations
	// skip the FindOpenPR lookup.
	SetKnownPRNumber(n int)

	// Diff and status queries.
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

	// Branch lifecycle.
	SyncWorktreeBase(ctx context.Context, completedBranches []string) error
	BranchForTask(ctx context.Context, taskID, title string, meta BranchTaskMeta) (string, error)
	PrepareForNextTask(nextTaskID string)
	ResetToDefaultBranch()
	RenameBranchForTask(taskDesc, taskID string) error
	RenameBranchTo(name string)
	SetPrevBranch(branch string)

	// Tag operations.
	TagTaskStart(taskID string)
	TagTaskEnd(taskID string)

	// Commit operations.
	CommitAll(message string)

	// Sync operations.
	EnsureUpToDate(ctx context.Context) error

	// Push operations.
	Push(ctx context.Context) error
	Ship(ctx context.Context, opts ShipOpts) (ShipResult, error)
	PushAndCreatePR(ctx context.Context, taskID, taskDesc, body string) (int, error)

	// DetectActiveReviewers queries the repo's installed GitHub Apps and returns
	// the subset that are in the Known reviewer registry. For Copilot it also
	// checks rulesets to set the correct polling timeout.
	DetectActiveReviewers() ([]Reviewer, error)
	// PollReview polls for a review from the given bot username on the given PR.
	// Returns nil without error when timeout expires before a review arrives.
	PollReview(botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error)

	// ResumeTask checks whether prior work exists for the task (open PR, merged PR,
	// remote branch) and resolves it. The loop passes task metadata extracted from
	// the backend; ResumeTask handles all git-side state resolution and returns
	// a result the loop acts on for bead close, notifications, and metadata updates.
	ResumeTask(ctx context.Context, meta ResumeTaskMeta, opts ResumeTaskOpts) (ResumeTaskResult, error)

	// Merge operations.
	MergeWithRetry(ctx context.Context, opts MergeRetryOpts) (bool, error)
	FlushUnpushedWork(ctx context.Context, taskID, taskDesc string, autoMerge bool) (merged bool, err error)
	PostMergeUpdateMain()

	// Remote branch operations.
	FetchBranch(branch string) error
	CheckoutRemoteBranch(branch string)
	RemoteBranchHasCommits(branch string) bool
	RemoteBranchIsOnMain(branch string) bool
	BranchIsAheadOfMain(branch string) bool
	BranchIsAncestorOfMain(branch string) bool
	DeleteRemoteBranchByName(branch string) error

	// Worktree lifecycle.
	RemoveWorktree()

	// GitHub availability and PR listing — used by cmd/ralph/merge without
	// exposing the GitHub interface type outside this package.
	GitHubAvailable() bool
	ListAllPRs(workDir string) ([]PRInfo, error)
}

// Compile-time check that *Repo satisfies Ops.
var _ Ops = (*Repo)(nil)

// GetProjectDir returns the project root directory.
func (r *Repo) GetProjectDir() string { return r.ProjectDir }

// GetRalphDir returns the ralph state directory (typically <project>/.ralph).
func (r *Repo) GetRalphDir() string { return r.ralphDir }

// GetWorkDir returns the worktree working directory.
func (r *Repo) GetWorkDir() string { return r.WorkDir }

// GetWorktreeBranch returns the current worktree branch name.
func (r *Repo) GetWorktreeBranch() string { return r.WorktreeBranch }

// GetPrevBranch returns the previous branch for stacked PR targeting.
func (r *Repo) GetPrevBranch() string { return r.PrevBranch }

// IsBranchRenamed returns whether the branch has been renamed for the current task.
func (r *Repo) IsBranchRenamed() bool { return r.BranchRenamed }

// SetBranchRenamed sets the branch renamed state.
func (r *Repo) SetBranchRenamed(v bool) { r.BranchRenamed = v }

// FindOpenPRForBranch finds an open PR for the given branch.
func (r *Repo) FindOpenPRForBranch(branch string) (int, error) {
	gh := r.gh()
	if !gh.Available() {
		return 0, nil
	}
	return gh.FindOpenPR(branch, r.RemoteURL())
}

// GetPRState returns the state (OPEN/CLOSED/MERGED) of a PR.
func (r *Repo) GetPRState(prNumber int) (PRState, error) {
	gh := r.gh()
	if !gh.Available() {
		return "", nil
	}
	pr, err := gh.GetPR(NWOFromRemote(r.RemoteURL()), prNumber)
	if err != nil {
		return "", err
	}
	return pr.State, nil
}

// ListOpenPRBranches returns branch names that have open PRs.
func (r *Repo) ListOpenPRBranches() ([]string, error) {
	gh := r.gh()
	repoURL := r.RemoteURL()
	if repoURL == "" || !gh.Available() {
		return nil, nil
	}
	return gh.ListOpenPRBranches(repoURL)
}

// GetPRBase returns the base branch of a PR.
func (r *Repo) GetPRBase(prNumber int) string {
	gh := r.gh()
	if !gh.Available() {
		return ""
	}
	pr, err := gh.GetPR(NWOFromRemote(r.RemoteURL()), prNumber)
	if err != nil {
		return ""
	}
	return pr.BaseRef
}

// FindPRForBranch finds any PR (open or closed) for the given branch.
func (r *Repo) FindPRForBranch(branch string) (int, string, string, error) {
	gh := r.gh()
	if !gh.Available() {
		return 0, "", "", nil
	}
	return gh.FindPR(branch, r.RemoteURL())
}

// PRChainIsHealthy checks that the PR's head branch exists on the remote
// and hasn't been merged into main already.
func (r *Repo) PRChainIsHealthy(prNumber int) (bool, string) {
	gh := r.gh()
	if !gh.Available() {
		return false, "gh CLI not available"
	}
	pr, _ := gh.GetPR(NWOFromRemote(r.RemoteURL()), prNumber)
	headBranch := ""
	if pr != nil {
		headBranch = pr.HeadRef
	}
	if headBranch == "" {
		return false, fmt.Sprintf("PR #%d has no head branch", prNumber)
	}
	_ = r.FetchBranch(headBranch)
	if !r.RemoteBranchHasCommits(headBranch) {
		return false, fmt.Sprintf("branch %s missing from remote", headBranch)
	}
	if r.BranchIsAncestorOfMain(headBranch) {
		return false, fmt.Sprintf("branch %s already merged into main", headBranch)
	}
	return true, ""
}

// DetectActiveReviewers queries the repo's installed GitHub Apps and returns
// the subset matching the Known reviewer registry.
func (r *Repo) DetectActiveReviewers() ([]Reviewer, error) {
	gh := r.gh()
	nwo := NWOFromRemote(r.RemoteURL())
	if nwo == "" {
		return nil, nil
	}
	return gh.DetectActiveReviewers(nwo)
}

// PollReview polls for a review from the given bot username on the given PR.
func (r *Repo) PollReview(botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error) {
	gh := r.gh()
	nwo := NWOFromRemote(r.RemoteURL())
	if nwo == "" {
		return nil, nil
	}
	return gh.PollReview(nwo, botUsername, prNumber, timeout)
}

// PRDiffForTask searches for a PR matching the task ID and returns its diff.
func (r *Repo) PRDiffForTask(taskID string) string {
	gh := r.gh()
	if !gh.Available() {
		return ""
	}
	prNumber, err := gh.SearchPR(r.WorkDir, taskID)
	if err != nil || prNumber == 0 {
		return ""
	}
	diff, err := gh.PRDiff(r.RemoteURL(), prNumber)
	if err != nil {
		return ""
	}
	return diff
}

// GitHubAvailable returns true when the gh CLI is available and configured.
func (r *Repo) GitHubAvailable() bool {
	return r.gh().Available()
}

// ListAllPRs returns all PRs (open and closed) for the repo.
func (r *Repo) ListAllPRs(workDir string) ([]PRInfo, error) {
	return r.gh().ListAllPRs(workDir)
}
