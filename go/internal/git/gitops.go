package git

import (
	"context"
	"fmt"
)

// GitOps abstracts the git operations that the execution loop needs.
// Production code uses *Manager; tests use testutil.StubGit.
type GitOps interface {
	// State accessors — replace direct field reads on *Manager.
	GetProjectDir() string
	GetWorkDir() string
	GetWorktreeBranch() string
	GetPrevBranch() string
	IsBranchRenamed() bool
	SetBranchRenamed(v bool)

	// PR operations — delegated to GitHub internally.
	FindOpenPRForBranch(branch string) (int, error)
	GetPRState(prNumber int) (string, error)
	ListOpenPRBranches() ([]string, error)
	GetPRBase(prNumber int) string
	FindPRForBranch(branch string) (number int, title, url string, err error)
	PRChainIsHealthy(prNumber int) (bool, string)
	PRDiffForTask(taskID string) string

	// CI bypass flag.
	SetLocalTestsPassed(v bool)

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
	PrepareForNextTask(nextTaskID string)
	ResetToDefaultBranch()
	RenameBranchForTask(taskDesc, taskID string)
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
}

// Compile-time check that *Manager satisfies GitOps.
var _ GitOps = (*Manager)(nil)

// GetProjectDir returns the project root directory.
func (m *Manager) GetProjectDir() string { return m.ProjectDir }

// GetWorkDir returns the worktree working directory.
func (m *Manager) GetWorkDir() string { return m.WorkDir }

// GetWorktreeBranch returns the current worktree branch name.
func (m *Manager) GetWorktreeBranch() string { return m.WorktreeBranch }

// GetPrevBranch returns the previous branch for stacked PR targeting.
func (m *Manager) GetPrevBranch() string { return m.PrevBranch }

// IsBranchRenamed returns whether the branch has been renamed for the current task.
func (m *Manager) IsBranchRenamed() bool { return m.BranchRenamed }

// SetBranchRenamed sets the branch renamed state.
func (m *Manager) SetBranchRenamed(v bool) { m.BranchRenamed = v }

// FindOpenPRForBranch finds an open PR for the given branch.
func (m *Manager) FindOpenPRForBranch(branch string) (int, error) {
	gh := m.gh()
	if !gh.Available() {
		return 0, nil
	}
	return gh.FindOpenPR(branch, m.RemoteURL())
}

// GetPRState returns the state (OPEN/CLOSED/MERGED) of a PR.
func (m *Manager) GetPRState(prNumber int) (string, error) {
	gh := m.gh()
	if !gh.Available() {
		return "", nil
	}
	return gh.GetPRState(m.WorkDir, prNumber)
}

// ListOpenPRBranches returns branch names that have open PRs.
func (m *Manager) ListOpenPRBranches() ([]string, error) {
	gh := m.gh()
	repoURL := m.RemoteURL()
	if repoURL == "" || !gh.Available() {
		return nil, nil
	}
	return gh.ListOpenPRBranches(repoURL)
}

// GetPRBase returns the base branch of a PR.
func (m *Manager) GetPRBase(prNumber int) string {
	gh := m.gh()
	if !gh.Available() {
		return ""
	}
	base, _ := gh.GetPRBase(m.WorkDir, prNumber)
	return base
}

// FindPRForBranch finds any PR (open or closed) for the given branch.
func (m *Manager) FindPRForBranch(branch string) (int, string, string, error) {
	gh := m.gh()
	if !gh.Available() {
		return 0, "", "", nil
	}
	return gh.FindPR(branch, m.RemoteURL())
}

// PRChainIsHealthy checks that the PR's head branch exists on the remote
// and hasn't been merged into main already.
func (m *Manager) PRChainIsHealthy(prNumber int) (bool, string) {
	gh := m.gh()
	if !gh.Available() {
		return false, "gh CLI not available"
	}
	headBranch, _ := gh.GetPRHead(m.WorkDir, prNumber)
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

// PRDiffForTask searches for a PR matching the task ID and returns its diff.
func (m *Manager) PRDiffForTask(taskID string) string {
	gh := m.gh()
	if !gh.Available() {
		return ""
	}
	prNumber, err := gh.SearchPR(m.WorkDir, taskID)
	if err != nil || prNumber == 0 {
		return ""
	}
	diff, err := gh.PRDiff(m.WorkDir, prNumber)
	if err != nil {
		return ""
	}
	return diff
}
