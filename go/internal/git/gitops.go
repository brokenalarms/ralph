package git

import "context"

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

	// GitHub access.
	GH() GitHub

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
	GetCIFailureLog(prNumber string) string

	// Branch lifecycle.
	PrepareForNextTask()
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
	PushAndCreatePR(ctx context.Context, taskID, taskDesc, body string) (string, error)

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
