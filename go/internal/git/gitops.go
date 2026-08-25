package git

import (
	"context"
	"fmt"
	"time"
)

// Ops is the public API of the git module. git.New(Config) returns Ops.
// External tests build an in-memory Ops via git.NewStub(StubRepoConfig);
// git-package tests construct &repo{} directly within the package.
type Ops interface {
	// Init runs git pre-flight checks and worktree setup. Must be called
	// once after construction, before any task execution.
	Init(ctx context.Context) error

	// InitTask is the worktree-setup entry point for `ralph task` —
	// the interactive triage session. Like Init but installs a
	// per-instance worktree on a unique `ralph/task/YYYYMMDD-NN`
	// branch so it never collides with the loop's `ralph/next` wip
	// branch or other concurrent task managers.
	InitTask(ctx context.Context) error

	// CheckBranchNamespaceSquat checks origin for a bare branch ref
	// squatting the ralph/ branch namespace root (refs/heads/ralph).
	// Returns nil when absent; an error naming the ref and its SHA when
	// present. Callers should run this once at loop startup, before any
	// task selection — such a ref blocks every ralph/* push.
	CheckBranchNamespaceSquat(ctx context.Context) error

	// State accessors — replace direct field reads on *repo.
	GetProjectDir() string
	GetWorkDir() string
	GetWorktreeBranch() string
	GetPrevBranch() string
	IsBranchRenamed() bool
	SetBranchRenamed(v bool)

	// PR operations — delegated to GitHub internally.
	FindOpenPRForBranch(ctx context.Context, branch string) (int, error)
	GetPRState(ctx context.Context, prNumber int) (PRState, error)
	ListOpenPRBranches(ctx context.Context) ([]string, error)
	GetPRBase(ctx context.Context, prNumber int) string
	FindPRForBranch(ctx context.Context, branch string) (number int, title, url string, err error)
	PRChainIsHealthy(ctx context.Context, prNumber int) (bool, string)
	PRDiffForRef(ctx context.Context, externalRef string) string

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
	DiffFromBase() string
	LogOneline(from, to string) string
	ConflictDiff() string
	RemoteURL() string
	DetectDefaultBranch() string
	RecentChangedFiles(n int) string
	GetCIFailureLog(ctx context.Context, prNumber int) string

	// Branch lifecycle.
	SyncWorktreeBase(ctx context.Context, completedBranches []string) error
	BranchForTask(ctx context.Context, taskID, title string, meta BranchTaskMeta) (string, error)
	PrepareForNextTask(nextTaskID, baseRef string)
	ResetToDefaultBranch()
	RenameBranchForTask(taskDesc, taskID string) error
	RenameBranchTo(name string)
	SetPrevBranch(branch string)
	// SetAdoptedStackBranch marks a leftover PR branch (from a prior run) that
	// the user explicitly chose, at loop startup, to continue the stack on top
	// of. setStackHead treats this branch as the stack head even when
	// diverged from main (BranchIsAheadOfMain would otherwise reject it).
	SetAdoptedStackBranch(branch string)

	// Tag operations.
	TagTaskStart(taskID string)
	TagTaskEnd(taskID string)

	// Commit operations.
	CommitAll(message string)
	// EmptyCommit creates a commit even if there are no file changes, to re-trigger CI.
	EmptyCommit(message string)

	// Sync operations.
	EnsureUpToDate(ctx context.Context) error

	// Push operations.
	Push(ctx context.Context) error
	Ship(ctx context.Context, opts ShipOpts) (ShipResult, error)
	PushAndCreatePR(ctx context.Context, taskID, taskDesc, body string) (int, error)

	// DetectActiveReviewers queries the repo's installed GitHub Apps and returns
	// the subset that are in the Known reviewer registry. For Copilot it also
	// checks rulesets to set the correct polling timeout.
	DetectActiveReviewers(ctx context.Context) ([]Reviewer, error)
	// PollReview polls for a review from the given bot username on the given PR.
	// Returns nil without error when timeout expires before a review arrives.
	PollReview(ctx context.Context, botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error)
	// ReplyToAndResolveComments replies to each review comment and resolves its
	// thread via the GitHub GraphQL API. Called after a fix is pushed so review
	// threads are closed in one step alongside the code change. Errors are
	// non-fatal — the fix was already pushed successfully.
	ReplyToAndResolveComments(ctx context.Context, prNumber int, comments []ReviewComment) error

	// ResumeTask checks whether prior work exists for the task (open PR, merged PR,
	// remote branch) and resolves it. The loop passes task metadata extracted from
	// the backend; ResumeTask handles all git-side state resolution and returns
	// a result the loop acts on for bead close, notifications, and metadata updates.
	ResumeTask(ctx context.Context, meta ResumeTaskMeta, opts ResumeTaskOpts) (ResumeTaskResult, error)

	// Merge operations.
	MergeWithRetry(ctx context.Context, opts MergeRetryOpts) (bool, error)
	FlushUnpushedWork(ctx context.Context, taskID, taskDesc string, autoMerge bool) (merged bool, err error)
	PostMergeUpdateMain()

	// MergeStack merges a stack of PRs bottom-up: collects the chain from
	// the given top PR, rebases onto the base branch, then iterates
	// merging with CI waits between each.
	MergeStack(ctx context.Context, opts MergeStackOpts) (MergeStackResult, error)

	// Remote branch operations.
	FetchBranch(branch string) error
	CheckoutRemoteBranch(branch string)
	RemoteBranchHasCommits(branch string) bool
	RemoteBranchIsOnMain(branch string) bool
	BranchIsAheadOfMain(branch string) bool
	BranchHasUnmergedWork(branch string) bool
	BranchIsAncestorOfMain(branch string) bool
	// IsCommitAncestorOf returns true when sha is an ancestor of (or equal to) ref.
	// Used to assert a signal-time commit is still reachable on the branch.
	IsCommitAncestorOf(sha, ref string) bool
	DeleteRemoteBranchByName(branch string) error

	// Worktree lifecycle.
	SetupWorktree(ctx context.Context) error
	RemoveWorktree()
	// RemoveWorktreeForBranch removes the worktree registered for branch (if
	// any) and deletes the local branch, without touching persisted
	// worktree_dir/worktree_branch state. Used for cleanup paths (e.g. `ralph
	// task` exit) that must not disturb a concurrent loop's resume state.
	RemoveWorktreeForBranch(branch string)

	// GitHub availability and PR listing.
	GitHubAvailable() bool
	ListAllPRs(ctx context.Context, workDir string) ([]PRInfo, error)
	ListOpenPRs(ctx context.Context, workDir string) ([]PRInfo, error)
	// PingGitHub verifies that GitHub is reachable. Returns nil when
	// reachable, an error otherwise (including timeout). Callers use this
	// for a startup connectivity check before any task execution.
	PingGitHub(ctx context.Context) error
}

// Compile-time check that *repo satisfies Ops.
var _ Ops = (*repo)(nil)

// GetProjectDir returns the project root directory.
func (r *repo) GetProjectDir() string     { return r.projectDir }
func (r *repo) GetWorkDir() string        { return r.workDir }
func (r *repo) GetWorktreeBranch() string { return r.worktreeBranch }
func (r *repo) GetPrevBranch() string     { return r.prevBranch }
func (r *repo) IsBranchRenamed() bool     { return r.branchRenamed }
func (r *repo) SetBranchRenamed(v bool)   { r.branchRenamed = v }

// FindOpenPRForBranch finds an open PR for the given branch.
func (r *repo) FindOpenPRForBranch(ctx context.Context, branch string) (int, error) {
	gh := r.github
	if !gh.Available() {
		return 0, nil
	}
	return gh.FindOpenPR(ctx, branch, r.RemoteURL())
}

// GetPRState returns the state (OPEN/CLOSED/MERGED) of a PR.
func (r *repo) GetPRState(ctx context.Context, prNumber int) (PRState, error) {
	gh := r.github
	if !gh.Available() {
		return "", nil
	}
	pr, err := gh.GetPR(ctx, NWOFromRemote(r.RemoteURL()), prNumber)
	if err != nil {
		return "", err
	}
	if pr == nil {
		return "", nil
	}
	return pr.State, nil
}

// ListOpenPRBranches returns branch names that have open PRs.
func (r *repo) ListOpenPRBranches(ctx context.Context) ([]string, error) {
	gh := r.github
	repoURL := r.RemoteURL()
	if repoURL == "" || !gh.Available() {
		return nil, nil
	}
	return gh.ListOpenPRBranches(ctx, repoURL)
}

// GetPRBase returns the base branch of a PR.
func (r *repo) GetPRBase(ctx context.Context, prNumber int) string {
	gh := r.github
	if !gh.Available() {
		return ""
	}
	pr, err := gh.GetPR(ctx, NWOFromRemote(r.RemoteURL()), prNumber)
	if err != nil || pr == nil {
		return ""
	}
	return pr.BaseRef
}

// FindPRForBranch finds any PR (open or closed) for the given branch.
func (r *repo) FindPRForBranch(ctx context.Context, branch string) (int, string, string, error) {
	gh := r.github
	if !gh.Available() {
		return 0, "", "", nil
	}
	return gh.FindPR(ctx, branch, r.RemoteURL())
}

// PRChainIsHealthy checks that the PR's head branch exists on the remote
// and hasn't been merged into main already.
func (r *repo) PRChainIsHealthy(ctx context.Context, prNumber int) (bool, string) {
	gh := r.github
	if !gh.Available() {
		return false, "gh CLI not available"
	}
	pr, _ := gh.GetPR(ctx, NWOFromRemote(r.RemoteURL()), prNumber)
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
func (r *repo) DetectActiveReviewers(ctx context.Context) ([]Reviewer, error) {
	gh := r.github
	nwo := NWOFromRemote(r.RemoteURL())
	if nwo == "" {
		return nil, nil
	}
	return gh.DetectActiveReviewers(ctx, nwo)
}

// PollReview polls for a review from the given bot username on the given PR.
func (r *repo) PollReview(ctx context.Context, botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error) {
	gh := r.github
	nwo := NWOFromRemote(r.RemoteURL())
	if nwo == "" {
		return nil, nil
	}
	return gh.PollReview(ctx, nwo, botUsername, prNumber, timeout)
}

// ReplyToAndResolveComments replies to each review comment and resolves its
// thread. Errors from individual reply/resolve calls are logged but do not
// stop processing — the fix was already pushed.
func (r *repo) ReplyToAndResolveComments(ctx context.Context, prNumber int, comments []ReviewComment) error {
	nwo := NWOFromRemote(r.RemoteURL())
	if nwo == "" {
		return nil
	}
	var commentIDs []int
	for _, c := range comments {
		if c.ID != 0 {
			commentIDs = append(commentIDs, c.ID)
		}
	}
	if len(commentIDs) == 0 {
		return nil
	}
	threadIDs, err := r.github.FetchReviewThreadIDs(ctx, nwo, prNumber, commentIDs)
	if err != nil {
		return fmt.Errorf("fetching review thread IDs: %w", err)
	}
	const replyBody = "Addressed — fix committed and pushed."
	for _, c := range comments {
		if c.ID == 0 {
			continue
		}
		if replyErr := r.github.ReplyToReviewComment(ctx, nwo, prNumber, c.ID, replyBody); replyErr != nil {
			fmt.Printf("reply to review comment %d: %v\n", c.ID, replyErr)
		}
		if threadID, ok := threadIDs[c.ID]; ok {
			if resolveErr := r.github.ResolveReviewThread(ctx, threadID); resolveErr != nil {
				fmt.Printf("resolve review thread for comment %d: %v\n", c.ID, resolveErr)
			}
		}
	}
	return nil
}

// PRDiffForRef returns the diff of the PR named by an exact external reference
// (a PR URL like ".../pull/715" or "gh-715" stored on the task by a prior run).
// The ref is a 1:1 pointer to a specific PR — there is no search and no
// possibility of matching a different task's PR. Returns "" when the ref does
// not resolve to a PR number or the diff cannot be fetched.
func (r *repo) PRDiffForRef(ctx context.Context, externalRef string) string {
	gh := r.github
	if !gh.Available() {
		return ""
	}
	prNumber := parsePRNumber(externalRef)
	if prNumber == 0 {
		return ""
	}
	diff, err := gh.PRDiff(ctx, r.RemoteURL(), prNumber)
	if err != nil {
		return ""
	}
	return diff
}

// GitHubAvailable returns true when the gh CLI is available and configured.
func (r *repo) GitHubAvailable() bool {
	return r.github.Available()
}

// ListAllPRs returns all PRs (open and closed) for the repo.
func (r *repo) ListAllPRs(ctx context.Context, workDir string) ([]PRInfo, error) {
	return r.github.ListAllPRs(ctx, workDir)
}

// ListOpenPRs returns only open PRs for the repo.
func (r *repo) ListOpenPRs(ctx context.Context, workDir string) ([]PRInfo, error) {
	return r.github.ListOpenPRs(ctx, workDir)
}

// PingGitHub verifies that GitHub is reachable.
func (r *repo) PingGitHub(ctx context.Context) error {
	return r.github.Ping(ctx)
}
