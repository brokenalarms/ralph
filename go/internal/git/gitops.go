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
func (m *Repo) GetProjectDir() string { return m.ProjectDir }

// GetRalphDir returns the ralph state directory (typically <project>/.ralph).
func (m *Repo) GetRalphDir() string { return m.ralphDir }

// GetWorkDir returns the worktree working directory.
func (m *Repo) GetWorkDir() string { return m.WorkDir }

// GetWorktreeBranch returns the current worktree branch name.
func (m *Repo) GetWorktreeBranch() string { return m.WorktreeBranch }

// GetPrevBranch returns the previous branch for stacked PR targeting.
func (m *Repo) GetPrevBranch() string { return m.PrevBranch }

// IsBranchRenamed returns whether the branch has been renamed for the current task.
func (m *Repo) IsBranchRenamed() bool { return m.BranchRenamed }

// SetBranchRenamed sets the branch renamed state.
func (m *Repo) SetBranchRenamed(v bool) { m.BranchRenamed = v }

// FindOpenPRForBranch finds an open PR for the given branch.
func (m *Repo) FindOpenPRForBranch(branch string) (int, error) {
	gh := m.gh()
	if !gh.Available() {
		return 0, nil
	}
	return gh.FindOpenPR(branch, m.RemoteURL())
}

// GetPRState returns the state (OPEN/CLOSED/MERGED) of a PR.
func (m *Repo) GetPRState(prNumber int) (PRState, error) {
	gh := m.gh()
	if !gh.Available() {
		return "", nil
	}
	pr, err := gh.GetPR(NWOFromRemote(m.RemoteURL()), prNumber)
	if err != nil {
		return "", err
	}
	return pr.State, nil
}

// ListOpenPRBranches returns branch names that have open PRs.
func (m *Repo) ListOpenPRBranches() ([]string, error) {
	gh := m.gh()
	repoURL := m.RemoteURL()
	if repoURL == "" || !gh.Available() {
		return nil, nil
	}
	return gh.ListOpenPRBranches(repoURL)
}

// GetPRBase returns the base branch of a PR.
func (m *Repo) GetPRBase(prNumber int) string {
	gh := m.gh()
	if !gh.Available() {
		return ""
	}
	pr, err := gh.GetPR(NWOFromRemote(m.RemoteURL()), prNumber)
	if err != nil {
		return ""
	}
	return pr.BaseRef
}

// FindPRForBranch finds any PR (open or closed) for the given branch.
func (m *Repo) FindPRForBranch(branch string) (int, string, string, error) {
	gh := m.gh()
	if !gh.Available() {
		return 0, "", "", nil
	}
	return gh.FindPR(branch, m.RemoteURL())
}

// PRChainIsHealthy checks that the PR's head branch exists on the remote
// and hasn't been merged into main already.
func (m *Repo) PRChainIsHealthy(prNumber int) (bool, string) {
	gh := m.gh()
	if !gh.Available() {
		return false, "gh CLI not available"
	}
	pr, _ := gh.GetPR(NWOFromRemote(m.RemoteURL()), prNumber)
	headBranch := ""
	if pr != nil {
		headBranch = pr.HeadRef
	}
	if headBranch == "" {
		return false, fmt.Sprintf("PR #%d has no head branch", prNumber)
	}
	_ = m.FetchBranch(headBranch)
	if !m.RemoteBranchHasCommits(headBranch) {
		return false, fmt.Sprintf("branch %s missing from remote", headBranch)
	}
	if m.BranchIsAncestorOfMain(headBranch) {
		return false, fmt.Sprintf("branch %s already merged into main", headBranch)
	}
	return true, ""
}

// DetectActiveReviewers queries the repo's installed GitHub Apps and returns
// the subset matching the Known reviewer registry.
func (m *Repo) DetectActiveReviewers() ([]Reviewer, error) {
	gh := m.gh()
	nwo := NWOFromRemote(m.RemoteURL())
	if nwo == "" {
		return nil, nil
	}
	return gh.DetectActiveReviewers(nwo)
}

// PollReview polls for a review from the given bot username on the given PR.
func (m *Repo) PollReview(botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error) {
	gh := m.gh()
	nwo := NWOFromRemote(m.RemoteURL())
	if nwo == "" {
		return nil, nil
	}
	return gh.PollReview(nwo, botUsername, prNumber, timeout)
}

// PRDiffForTask searches for a PR matching the task ID and returns its diff.
func (m *Repo) PRDiffForTask(taskID string) string {
	gh := m.gh()
	if !gh.Available() {
		return ""
	}
	prNumber, err := gh.SearchPR(m.WorkDir, taskID)
	if err != nil || prNumber == 0 {
		return ""
	}
	diff, err := gh.PRDiff(m.RemoteURL(), prNumber)
	if err != nil {
		return ""
	}
	return diff
}

// GitHubAvailable returns true when the gh CLI is available and configured.
func (m *Repo) GitHubAvailable() bool {
	return m.gh().Available()
}

// ListAllPRs returns all PRs (open and closed) for the repo.
func (m *Repo) ListAllPRs(workDir string) ([]PRInfo, error) {
	return m.gh().ListAllPRs(workDir)
}
