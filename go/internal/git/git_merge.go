package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// CIFixResult describes the outcome of a CI fix attempt.
type CIFixResult int

const (
	// CIFixFailed means the fix agent ran but could not fix the issue.
	CIFixFailed CIFixResult = iota
	// CIFixApplied means the fix agent pushed new commits.
	CIFixApplied
	// CIFixNoCommits means the fix agent ran but made no commits,
	// indicating an infrastructure failure rather than a code issue.
	CIFixNoCommits
)

// resolveBaseBranch returns PrevBranch if set, otherwise the default branch.
// Single source of truth for "what is this branch based on."
func (m *Manager) resolveBaseBranch() string {
	if m.PrevBranch != "" {
		return m.PrevBranch
	}
	return m.detectDefaultBranch()
}

// Push squashes all commits into one and force-pushes the branch.
// Always uses --force-with-lease (safe — only forces if remote matches
// last fetch). Squash ensures stacked PRs cascade cleanly on merge.
func (m *Manager) Push(ctx context.Context) error {
	if m.WorktreeBranch == "" || m.WorkDir == m.ProjectDir {
		return nil
	}

	if m.PrePush != nil {
		if err := m.PrePush(ctx); err != nil {
			return fmt.Errorf("pre-push check failed: %w", err)
		}
	}

	baseBranch := m.resolveBaseBranch()
	_ = m.gitCmdErr(m.WorkDir, "fetch", "origin", baseBranch)
	baseRef := "origin/" + baseBranch
	if !m.refExists(m.WorkDir, baseRef) {
		baseRef = baseBranch
	}

	// Squash only commits ahead of the parent branch tip. Using rev-parse
	// (not merge-base) preserves the ancestry link so each stacked PR is
	// exactly one commit ahead of its parent — GitHub merge is a clean no-op.
	baseSHA := m.gitOutput(m.WorkDir, "rev-parse", baseRef)
	if baseSHA != "" {
		// Verify parent tip is an ancestor of HEAD. If not, the branch
		// diverged from its parent (e.g. parent was squash-pushed since
		// this branch was created) and needs rebasing first.
		if m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", baseSHA, "HEAD") != nil {
			m.Logger.Log("git", "Branch diverged from %s — rebasing before push", baseBranch)
			if err := m.EnsureUpToDate(ctx); err != nil {
				m.Logger.Warn("git", "Rebase before push failed: %v", err)
			}
			// Re-fetch after rebase since origin may have moved.
			_ = m.gitCmdErr(m.WorkDir, "fetch", "origin", baseBranch)
			baseSHA = m.gitOutput(m.WorkDir, "rev-parse", baseRef)
		}
		if baseSHA != "" && m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", baseSHA, "HEAD") == nil {
			headSHA := m.gitOutput(m.WorkDir, "rev-parse", "HEAD")
			if headSHA != baseSHA {
				commitMsg := m.gitOutput(m.WorkDir, "log", "-1", "--format=%s")
				if err := m.SquashToOneCommit(baseSHA, commitMsg); err != nil {
					m.Logger.Warn("git", "Squash: %v", err)
				}
			}
		}
	}

	m.Logger.Log("git", "Pushing %s...", m.WorktreeBranch)
	// Try force-with-lease first (safe update of existing branch).
	// Fall back to regular push for new branches.
	if err := m.gitCmdErrCtx(ctx, m.WorkDir, "push", "--force-with-lease", "-u", "origin", m.WorktreeBranch); err != nil {
		return m.gitCmdErrCtx(ctx, m.WorkDir, "push", "-u", "origin", m.WorktreeBranch)
	}
	return nil
}

// reopenClosedPR finds a closed (not merged) PR for the given branch and
// reopens it. Returns the PR number on success or empty string if no closed
// PR was found or reopen failed.
func reopenClosedPR(gh GitHub, workDir, branch, nwo, repoURL, title, body string, logger Log) (string, error) {
	number, _, _, findErr := gh.FindPR(branch, workDir)
	if findErr != nil || number == "" {
		return "", findErr
	}
	state, stateErr := gh.GetPRState(workDir, number)
	if stateErr != nil || strings.ToUpper(state) != "CLOSED" {
		return "", stateErr
	}

	pr := logging.PRLink(nwo, number)

	if err := gh.ReopenPR(number, repoURL); err != nil {
		logger.Warn("git", "Failed to reopen %s: %v", pr, err)
		return "", err
	}
	logger.Log("git", "Reopened %s for %s", pr, branch)

	if title != "" {
		if err := gh.EditPR(number, repoURL, title, body); err != nil {
			logger.Warn("git", "Failed to update %s: %v", pr, err)
		}
	}
	return number, nil
}

func (m *Manager) reopenClosedPR(gh GitHub, repoURL, title, body string) (string, error) {
	nwo := NWOFromRemote(repoURL)
	return reopenClosedPR(gh, m.WorkDir, m.WorktreeBranch, nwo, repoURL, title, body, m.Logger)
}

// EnsurePROpts configures the CreatePR package function.
type EnsurePROpts struct {
	TaskID     string
	TaskDesc   string
	Body       string
	BaseBranch string
	Logger     Log
}

// CreatePR ensures a PR exists for the given branch. If one is already open,
// updates its title and body. Otherwise creates a new PR targeting BaseBranch.
// Returns the PR number.
func CreatePR(ctx context.Context, gh GitHub, workDir, branch, remoteURL string, opts EnsurePROpts) (string, error) {
	if remoteURL == "" {
		return "", nil
	}
	if !gh.Available() {
		return "", fmt.Errorf("gh CLI not found — cannot create PR")
	}

	nwo := NWOFromRemote(remoteURL)
	title := prTitle(opts.TaskID, opts.TaskDesc, branch)

	// Existing PR — update and return.
	prNumber, _ := gh.FindOpenPR(branch, remoteURL)
	if prNumber != "" {
		pr := logging.PRLink(nwo, prNumber)
		if opts.TaskID != "" {
			if err := gh.EditPR(prNumber, remoteURL, title, opts.Body); err != nil {
				opts.Logger.Warn("git", "Failed to update %s: %v", pr, err)
			}
		}
		opts.Logger.Log("git", "%s already open for %s", pr, branch)
		return prNumber, nil
	}

	// New PR.
	body := opts.Body
	if body == "" {
		body = opts.TaskDesc
	}
	createOpts := CreatePROpts{
		Head:  branch,
		Base:  opts.BaseBranch,
		Title: title,
		Body:  body,
		Repo:  remoteURL,
		Dir:   workDir,
	}
	createErr := gh.CreatePR(createOpts)
	if createErr != nil {
		// Creation may fail if a closed PR exists for this head:base.
		// Try to find and reopen it instead.
		if prNumber, reopenErr := reopenClosedPR(gh, workDir, branch, nwo, remoteURL, title, body, opts.Logger); reopenErr == nil && prNumber != "" {
			return prNumber, nil
		}

		// Reopen failed (e.g. branch diverged from old PR history).
		// The old PR is dead — create a fresh PR via the REST API,
		// bypassing gh pr create's client-side checks.
		if nwo != "" {
			if apiPR, apiErr := gh.CreatePRViaAPI(nwo, createOpts); apiErr == nil && apiPR != "" {
				opts.Logger.Log("git", "Created %s for %s (via API fallback)", logging.PRLink(nwo, apiPR), branch)
				return apiPR, nil
			}
		}

		return "", createErr
	}

	newPR, _ := gh.FindOpenPR(branch, remoteURL)
	if newPR != "" {
		opts.Logger.Log("git", "Created %s for %s", logging.PRLink(nwo, newPR), branch)
	} else {
		opts.Logger.Log("git", "Created PR for %s", branch)
	}
	return newPR, nil
}

// Manager.CreatePR delegates to the package function.
func (m *Manager) CreatePR(ctx context.Context, taskID, taskDesc, body string) (string, error) {
	return CreatePR(ctx, m.gh(), m.WorkDir, m.WorktreeBranch, m.RemoteURL(), EnsurePROpts{
		TaskID:     taskID,
		TaskDesc:   taskDesc,
		Body:       body,
		BaseBranch: m.resolveBaseBranch(),
		Logger:     m.Logger,
	})
}

// ShipOpts configures the Ship pipeline.
type ShipOpts struct {
	TaskID    string
	TaskTitle string
	Body      string
	// Infrastructure — Manager fills these before delegating.
	PushFn                  func(ctx context.Context) error
	HasUncommittedChangesFn func() bool
	CommitAllFn             func(message string)
	BaseBranch              string
	Logger                  Log
}

// ShipResult is the outcome of the Ship pipeline.
type ShipResult struct {
	PRNumber string
	PRURL    string
	PRTitle  string
}

// Ship is the single "get work into a PR" pipeline: auto-commit any
// uncommitted changes, push (squash + rebase + force-push), and create
// or update a PR. Returns the PR number and URL.
func Ship(ctx context.Context, runner Runner, gh GitHub, workDir, branch, remoteURL string, opts ShipOpts) (ShipResult, error) {
	hasChanges := opts.HasUncommittedChangesFn
	if hasChanges == nil {
		r := runner
		if r == nil {
			r = defaultRunner
		}
		hasChanges = func() bool {
			_, err1 := r.Run(context.Background(), workDir, "diff", "--quiet")
			_, err2 := r.Run(context.Background(), workDir, "diff", "--cached", "--quiet")
			return err1 != nil || err2 != nil
		}
	}

	commitAll := opts.CommitAllFn
	if commitAll == nil {
		r := runner
		if r == nil {
			r = defaultRunner
		}
		commitAll = func(msg string) {
			r.Run(context.Background(), workDir, "add", "-A")
			r.Run(context.Background(), workDir, "commit", "-m", msg)
		}
	}

	if hasChanges() {
		msg := "auto-commit agent changes"
		if opts.TaskID != "" {
			msg = fmt.Sprintf("[%s] %s", opts.TaskID, msg)
		}
		commitAll(msg)
	}

	if opts.PushFn != nil {
		if err := opts.PushFn(ctx); err != nil {
			if ctx.Err() != nil {
				return ShipResult{}, ctx.Err()
			}
			if opts.Logger != nil {
				opts.Logger.Warn("git", "Push failed: %v — attempting PR creation anyway", err)
			}
		}
	}

	prNumber, err := CreatePR(ctx, gh, workDir, branch, remoteURL, EnsurePROpts{
		TaskID:     opts.TaskID,
		TaskDesc:   opts.TaskTitle,
		Body:       opts.Body,
		BaseBranch: opts.BaseBranch,
		Logger:     opts.Logger,
	})
	if err != nil {
		return ShipResult{PRNumber: prNumber}, err
	}

	// Look up the PR URL for the external ref.
	var prURL, prTitle string
	if prNumber != "" {
		if _, t, u, findErr := gh.FindPR(branch, workDir); findErr == nil {
			prURL = u
			prTitle = t
		}
		if prURL == "" {
			nwo := NWOFromRemote(remoteURL)
			if nwo != "" {
				prURL = fmt.Sprintf("https://github.com/%s/pull/%s", nwo, prNumber)
			}
		}
	}

	return ShipResult{
		PRNumber: prNumber,
		PRURL:    prURL,
		PRTitle:  prTitle,
	}, nil
}

// Manager.Ship delegates to the package function.
func (m *Manager) Ship(ctx context.Context, opts ShipOpts) (ShipResult, error) {
	opts.PushFn = m.Push
	opts.HasUncommittedChangesFn = m.HasUncommittedChanges
	opts.CommitAllFn = m.CommitAll
	opts.BaseBranch = m.resolveBaseBranch()
	opts.Logger = m.Logger
	return Ship(ctx, m.run(), m.gh(), m.WorkDir, m.WorktreeBranch, m.RemoteURL(), opts)
}

// PushAndCreatePR composes Push and CreatePR. Squashes, force-pushes, then
// ensures a PR exists. Returns the PR number.
func (m *Manager) PushAndCreatePR(ctx context.Context, taskID, taskDesc, body string) (string, error) {
	result, err := m.Ship(ctx, ShipOpts{TaskID: taskID, TaskTitle: taskDesc, Body: body})
	return result.PRNumber, err
}

func prTitle(taskID, taskDesc, fallback string) string {
	title := tasks.StripComponentPrefix(taskDesc)
	if taskID != "" {
		title = "[" + taskID + "] " + title
	}
	if len(title) > 70 {
		title = title[:67] + "..."
	}
	if title == "" {
		title = fallback
	}
	return title
}

func (m *Manager) prTitle(taskID, taskDesc string) string {
	return prTitle(taskID, taskDesc, m.WorktreeBranch)
}

// AutoMergeCurrentBranch rebases onto main, pushes, waits for CI, and
// merges. If main moves between CI passing and the merge attempt, loops
// back to rebase+push again. Returns typed errors (CIFailureError,
// MergeConflictError) that callers can handle.
func (m *Manager) AutoMergeCurrentBranch(ctx context.Context) (bool, error) {
	if m.WorktreeBranch == "" || m.WorkDir == m.ProjectDir {
		return false, nil
	}

	gh := m.gh()
	if !gh.Available() {
		return false, fmt.Errorf("gh CLI not found — cannot auto-merge")
	}

	repoURL := m.gitOutput(m.WorkDir, "remote", "get-url", "origin")
	if repoURL == "" {
		m.Logger.Log("git", "No remote URL — skipping auto-merge")
		return false, nil
	}

	nwo := NWOFromRemote(repoURL)
	prNumber, err := gh.FindOpenPR(m.WorktreeBranch, repoURL)
	if err != nil || prNumber == "" {
		prNumber, err = m.resolveClosedPR(gh, repoURL)
		if errors.Is(err, ErrPRAlreadyMerged) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if prNumber == "" {
			m.Logger.Log("git", "No PR found for %s — skipping auto-merge", m.WorktreeBranch)
			return false, nil
		}
	}
	pr := logging.PRLink(nwo, prNumber)

	defaultBranch := m.detectDefaultBranch()

	prBase, _ := gh.GetPRBase(m.WorkDir, prNumber)
	if prBase != "" && prBase != defaultBranch {
		m.Logger.Log("git", "%s targets %s (not %s) — waiting for base PRs to merge first", pr, prBase, defaultBranch)
		return false, ErrStackedPRWaiting
	}

	m.Logger.Log("git", "%s Auto-merging %s...", logging.BranchTag(defaultBranch), pr)

	// Rebase onto latest main and push so CI runs on the final tree.
	// This avoids the updatePRBranch round-trip and double CI wait.
	if err := m.EnsureUpToDate(ctx); err != nil {
		m.Logger.Warn("git", "Pre-merge rebase failed: %v", err)
	}
	if err := m.Push(ctx); err != nil {
		m.Logger.Warn("git", "Pre-merge push failed: %v", err)
	}

	headSHA := m.gitOutput(m.WorkDir, "rev-parse", "HEAD")
	checks, status, ciErr := m.AwaitCI(ctx, prNumber, repoURL, headSHA)
	if ciErr != nil {
		m.Logger.Warn("ci", "CI polling failed for %s: %v — attempting merge anyway", pr, ciErr)
	}
	if status == CIFailed {
		if m.LocalTestsPassed && m.isInfrastructureFailure(ctx, prNumber) {
			m.Logger.Log("ci", "CI infrastructure failure on %s — local tests passed, bypassing branch protection", pr)
			m.BypassRules = true
			return m.executeMerge(ctx, prNumber, repoURL)
		}
		return false, &CIFailureError{PRNumber: prNumber, Failures: failedChecks(checks)}
	}
	if status == CIPassed {
		m.Logger.Log("ci", "CI passed for %s — merging", pr)
	}

	if m.branchNeedsUpdate() {
		m.Logger.Log("git", "Main moved while CI was running — will rebase and retry")
		return false, &MergeConflictError{PRNumber: prNumber}
	}

	return m.executeMerge(ctx, prNumber, repoURL)
}

// ErrPRAlreadyMerged is returned when the PR for the branch is already merged.
// Callers should skip push and close the bead.
var ErrPRAlreadyMerged = fmt.Errorf("PR already merged")

// resolveClosedPR handles the case where no open PR exists for the branch.
// It checks whether a PR exists in another state (merged or closed). If
// merged, returns ErrPRAlreadyMerged. If closed, reopens and returns the
// PR number so the caller can proceed with the normal merge flow.
func (m *Manager) resolveClosedPR(gh GitHub, repoURL string) (string, error) {
	number, _, _, findErr := gh.FindPR(m.WorktreeBranch, m.WorkDir)
	if findErr != nil || number == "" {
		return "", nil
	}

	state, stateErr := gh.GetPRState(m.WorkDir, number)
	if stateErr != nil {
		return "", nil
	}

	nwo := NWOFromRemote(repoURL)
	pr := logging.PRLink(nwo, number)

	switch strings.ToUpper(state) {
	case "MERGED":
		m.Logger.Log("git", "%s already merged — nothing to do", pr)
		return "", ErrPRAlreadyMerged
	case "CLOSED":
		m.Logger.Log("git", "%s is closed — reopening", pr)
		if err := gh.ReopenPR(number, repoURL); err != nil {
			m.Logger.Warn("git", "Failed to reopen %s: %v — creating new PR", pr, err)
			nwo := NWOFromRemote(repoURL)
			baseBranch := m.resolveBaseBranch()
			opts := CreatePROpts{
				Head: m.WorktreeBranch,
				Base: baseBranch,
				Repo: repoURL,
				Dir:  m.WorkDir,
			}
			if apiPR, apiErr := gh.CreatePRViaAPI(nwo, opts); apiErr == nil && apiPR != "" {
				m.Logger.Log("git", "Created %s for %s (via API fallback)", logging.PRLink(nwo, apiPR), m.WorktreeBranch)
				return apiPR, nil
			}
			return "", nil
		}
		m.Logger.Log("git", "%s reopened", pr)
		return number, nil
	default:
		return "", nil
	}
}

// branchNeedsUpdate checks if the base branch has moved ahead of HEAD since
// the last push. Returns true when origin/<base> is not an ancestor of HEAD,
// meaning main moved while CI was running and the branch must be rebased
// before merging. Uses a local ancestry check to avoid creating merge commits.
func (m *Manager) branchNeedsUpdate() bool {
	baseBranch := m.resolveBaseBranch()
	_ = m.gitCmdErr(m.WorkDir, "fetch", "origin", baseBranch)
	if !m.refExists(m.WorkDir, "origin/"+baseBranch) {
		return false
	}
	return m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", "origin/"+baseBranch, "HEAD") != nil
}

// ExecuteMergeOpts holds all parameters for the executeMerge package function.
type ExecuteMergeOpts struct {
	PRNumber       string
	RepoURL        string
	WorktreeBranch string
	WorkDir        string
	DefaultBranch  string
	MergeOpts      MergeOpts
	// AwaitCI polls CI check status for the PR. Required for the Blocked path.
	AwaitCI func(ctx context.Context, prNumber, repoURL, expectedSHA string) ([]CICheckResult, CIStatus, error)
}

// executeMerge attempts the squash-merge and handles CI-gated retries.
// It is a package function — callers compose it without a Manager receiver.
// Manager.executeMerge delegates here.
func executeMerge(ctx context.Context, gh GitHub, opts ExecuteMergeOpts, logger Log) (bool, error) {
	nwo := NWOFromRemote(opts.RepoURL)
	pr := logging.PRLink(nwo, opts.PRNumber)
	mergeOpts := opts.MergeOpts

	if _, prTitle, _, titleErr := gh.FindPR(opts.WorktreeBranch, opts.WorkDir); titleErr == nil && prTitle != "" {
		mergeOpts.Subject = fmt.Sprintf("%s (#%s)", prTitle, opts.PRNumber)
	}

	result := gh.MergePR(opts.PRNumber, opts.RepoURL, mergeOpts)
	if result.Merged {
		return postMergeLog(nwo, opts.PRNumber, opts.DefaultBranch, logger)
	}

	if result.Conflict {
		if logger != nil {
			logger.Warn("git", "%s has merge conflicts — attempting rebase", pr)
		}
		return false, &MergeConflictError{PRNumber: opts.PRNumber}
	}

	if result.Blocked {
		if logger != nil {
			logger.Log("ci", "%s blocked by branch protection: %s — waiting for CI...", pr, result.Message)
		}
		if opts.AwaitCI == nil {
			return false, fmt.Errorf("auto-merge blocked for PR #%s: %s", opts.PRNumber, result.Message)
		}
		checks, status, waitErr := opts.AwaitCI(ctx, opts.PRNumber, opts.RepoURL, "")
		if waitErr != nil {
			return false, fmt.Errorf("CI polling failed for PR #%s: %w", opts.PRNumber, waitErr)
		}
		if status == CIFailed {
			return false, &CIFailureError{PRNumber: opts.PRNumber, Failures: failedChecks(checks)}
		}
		if status == CIPassed {
			if logger != nil {
				logger.Log("ci", "CI passed for %s — retrying merge", pr)
			}
			retry := gh.MergePR(opts.PRNumber, opts.RepoURL, mergeOpts)
			if retry.Merged {
				return postMergeLog(nwo, opts.PRNumber, opts.DefaultBranch, logger)
			}
			if logger != nil {
				logger.Warn("git", "Merge retry failed for %s: %s", pr, retry.Message)
			}
			return false, fmt.Errorf("merge retry failed for PR #%s after CI passed: %s", opts.PRNumber, retry.Message)
		}
	}

	if logger != nil {
		logger.Warn("git", "Auto-merge failed for %s: %s", pr, result.Message)
	}
	return false, fmt.Errorf("auto-merge failed for PR #%s: %s", opts.PRNumber, result.Message)
}

// postMergeLog logs the merge completion.
func postMergeLog(nwo, prNumber, defaultBranch string, logger Log) (bool, error) {
	if logger != nil {
		logger.Log("git", "%s %s merged", logging.BranchTag(defaultBranch), logging.PRLink(nwo, prNumber))
	}
	return true, nil
}

// Manager.executeMerge delegates to the package-level executeMerge function.
func (m *Manager) executeMerge(ctx context.Context, prNumber, repoURL string) (bool, error) {
	return executeMerge(ctx, m.gh(), ExecuteMergeOpts{
		PRNumber:       prNumber,
		RepoURL:        repoURL,
		WorktreeBranch: m.WorktreeBranch,
		WorkDir:        m.WorkDir,
		DefaultBranch:  m.detectDefaultBranch(),
		MergeOpts:      m.mergeOpts(),
		AwaitCI:        m.AwaitCI,
	}, m.Logger)
}

// GetCIFailureLog retrieves the failed CI run's log output for the given PR.
func (m *Manager) GetCIFailureLog(prNumber string) string {
	return m.gh().GetRunLog(prNumber, m.WorkDir)
}

// mergeOpts returns the merge options for the current Manager configuration.
func (m *Manager) mergeOpts() MergeOpts {
	return MergeOpts{
		DeleteBranch: true,
		Admin:        m.BypassRules,
	}
}

// NWOFromRemote extracts "owner/repo" from a GitHub remote URL.
func NWOFromRemote(remoteURL string) string {
	// Handle SSH: git@github.com:owner/repo.git
	if idx := strings.Index(remoteURL, ":"); strings.HasPrefix(remoteURL, "git@") && idx > 0 {
		nwo := remoteURL[idx+1:]
		nwo = strings.TrimSuffix(nwo, ".git")
		return nwo
	}
	// Handle HTTPS: https://github.com/owner/repo.git
	parts := strings.Split(strings.TrimSuffix(remoteURL, ".git"), "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return ""
}


// DeleteRemoteBranch removes the current branch from the remote. Used to
// clean up after a PR has been merged externally.
func (m *Manager) DeleteRemoteBranch() {
	if m.WorktreeBranch == "" {
		return
	}
	_ = m.gitCmdErr(m.WorkDir, "push", "origin", "--delete", m.WorktreeBranch)
}

// MaxMergeAttempts is the total number of merge attempts including retries
// after conflict resolution and CI fixes.
const MaxMergeAttempts = 4

// MaxInfraRetries is the number of times to retry when CI fails due to
// infrastructure issues (fix agent ran but made no commits).
const MaxInfraRetries = 3

// infraBackoff returns the delay before retrying after an infrastructure failure.
// Uses exponential backoff: 30s, 60s, 120s.
func infraBackoff(attempt int) time.Duration {
	d := 30 * time.Second
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	return d
}

// MergeRetryOpts configures the merge-with-retry pipeline.
type MergeRetryOpts struct {
	// OnCIFailure is called when CI checks fail on the PR. It should attempt
	// to fix the failure (e.g. by spawning a fix agent) and return a result:
	//   CIFixApplied   — fix was pushed, retry merge after waiting for CI
	//   CIFixNoCommits — no commits (infrastructure failure), retry with backoff
	//   CIFixFailed    — agent couldn't fix, stop retrying
	OnCIFailure func(ciErr *CIFailureError) CIFixResult

	// OnConflict is called when automatic rebase cannot resolve merge
	// conflicts (UnresolvedConflictError). It should spawn a conflict
	// resolution agent and return true if the conflict was resolved and
	// force-pushed, ready for a merge retry.
	OnConflict func(conflictErr *UnresolvedConflictError) bool

	// SleepFunc is used for infrastructure backoff delays. Defaults to time.Sleep.
	SleepFunc func(time.Duration)

	// The following fields are filled by Manager.MergeWithRetry before delegating
	// to the package-level MergeWithRetry function. They enable callers to compose
	// the retry pipeline without a Manager receiver.

	// ResolveConflict is called to rebase and force-push when a merge conflict
	// is detected. Defaults to Manager.ResolveConflict when nil.
	ResolveConflict func(ctx context.Context) error

	// AwaitCI polls CI status after a fix agent pushes. Used for CIFixApplied.
	AwaitCI func(ctx context.Context, prNumber, repoURL, sha string) ([]CICheckResult, CIStatus, error)

	// Logger receives progress and warning messages. Logging is skipped when nil.
	Logger Log

	// RemoteURL is the repository remote URL, used for AwaitCI after a fix.
	RemoteURL string

	// HeadSHAFn returns the current HEAD SHA, used for AwaitCI after a fix.
	HeadSHAFn func() string
}

// ResolveConflict rebases onto the default branch and force-pushes to
// resolve PR merge conflicts before the next merge attempt. Returns an
// UnresolvedConflictError if the rebase couldn't resolve the divergence,
// signaling that retrying will not help.
func (m *Manager) ResolveConflict(ctx context.Context) error {
	baseBranch := m.resolveBaseBranch()
	m.Logger.Log("git", "%s Rebasing onto %s to resolve merge conflicts...", logging.BranchTag(baseBranch), baseBranch)
	if err := m.EnsureUpToDate(ctx); err != nil {
		return fmt.Errorf("conflict resolution rebase failed: %w", err)
	}

	// Check if the rebase actually resolved the divergence. If origin/base
	// is still not an ancestor of HEAD, auto-resolve failed and force-pushing
	// would just repeat the same conflict on GitHub.
	if m.refExists(m.WorkDir, "origin/"+baseBranch) {
		if m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", "origin/"+baseBranch, "HEAD") != nil {
			m.Logger.Warn("git", "Rebase did not resolve conflicts with origin/%s — skipping force-push", baseBranch)
			return &UnresolvedConflictError{}
		}
	}

	return m.Push(ctx)
}

// MergeWithRetry is the single merge pipeline: try mergeFunc, detect error
// type, handle it, retry. Conflicts trigger ResolveConflict from opts; CI
// failures delegate to the OnCIFailure callback. Code fix retries share the
// main attempt budget. Infrastructure failures use a separate retry counter
// with exponential backoff.
//
// It is a package function — callers compose it without a Manager receiver.
// Manager.MergeWithRetry delegates here after filling in infrastructure callbacks.
func MergeWithRetry(ctx context.Context, mergeFunc func(context.Context) (bool, error), opts MergeRetryOpts) (bool, error) {
	sleepFn := opts.SleepFunc
	if sleepFn == nil {
		sleepFn = time.Sleep
	}

	infraRetries := 0

	for attempt := 0; attempt < MaxMergeAttempts; attempt++ {
		merged, err := mergeFunc(ctx)
		if err == nil {
			return merged, nil
		}

		if attempt > 0 && opts.Logger != nil {
			opts.Logger.Warn("git", "Merge attempt %d failed: %v", attempt+1, err)
		}

		var conflictErr *MergeConflictError
		if errors.As(err, &conflictErr) {
			if opts.ResolveConflict == nil {
				return false, err
			}
			resolveErr := opts.ResolveConflict(ctx)
			if resolveErr == nil {
				continue
			}
			var unresolved *UnresolvedConflictError
			if errors.As(resolveErr, &unresolved) {
				unresolved.PRNumber = conflictErr.PRNumber
				if opts.OnConflict != nil && opts.OnConflict(unresolved) {
					continue
				}
				return false, unresolved
			}
			return false, resolveErr
		}

		var ciErr *CIFailureError
		if errors.As(err, &ciErr) {
			if opts.OnCIFailure == nil {
				return false, err
			}
			result := opts.OnCIFailure(ciErr)
			switch result {
			case CIFixApplied:
				// Fix was applied and force-pushed. Wait for new CI on the
				// updated HEAD before retrying merge — the old check status
				// is stale after force-push.
				if opts.AwaitCI != nil {
					repoURL := opts.RemoteURL
					fixHeadSHA := ""
					if opts.HeadSHAFn != nil {
						fixHeadSHA = opts.HeadSHAFn()
					}
					_, ciStatus, waitErr := opts.AwaitCI(ctx, ciErr.PRNumber, repoURL, fixHeadSHA)
					if waitErr != nil && opts.Logger != nil {
						opts.Logger.Warn("ci", "CI polling after fix: %v", waitErr)
					}
					if ciStatus == CIFailed && opts.Logger != nil {
						opts.Logger.Warn("ci", "CI still failing after fix — will retry")
					}
				}
				continue
			case CIFixNoCommits:
				// Infrastructure failure — fix agent found no code issue.
				// Retry with backoff instead of giving up.
				if infraRetries >= MaxInfraRetries {
					if opts.Logger != nil {
						opts.Logger.Warn("ci", "Infrastructure retries exhausted (%d) — giving up", MaxInfraRetries)
					}
					return false, err
				}
				delay := infraBackoff(infraRetries)
				if opts.Logger != nil {
					opts.Logger.Log("ci", "CI infrastructure failure — retrying in %s (%d/%d)", delay, infraRetries+1, MaxInfraRetries)
				}
				sleepFn(delay)
				infraRetries++
				// Don't consume the code-fix attempt budget for infra retries.
				attempt--
				continue
			default:
				return false, err
			}
		}

		return false, err
	}
	return false, fmt.Errorf("merge failed after %d attempts", MaxMergeAttempts)
}

// Manager.MergeWithRetry delegates to the package-level MergeWithRetry function
// after filling in infrastructure callbacks from Manager fields.
func (m *Manager) MergeWithRetry(ctx context.Context, opts MergeRetryOpts) (bool, error) {
	if opts.ResolveConflict == nil {
		opts.ResolveConflict = m.ResolveConflict
	}
	if opts.AwaitCI == nil {
		opts.AwaitCI = m.AwaitCI
	}
	if opts.Logger == nil {
		opts.Logger = m.Logger
	}
	if opts.RemoteURL == "" {
		opts.RemoteURL = m.RemoteURL()
	}
	if opts.HeadSHAFn == nil {
		opts.HeadSHAFn = func() string { return m.gitOutput(m.WorkDir, "rev-parse", "HEAD") }
	}
	return MergeWithRetry(ctx, m.AutoMergeCurrentBranch, opts)
}

// FlushUnpushedWork pushes any unpushed commits and optionally merges
// the PR. This is the safety net called before exiting or entering wait mode.
func (m *Manager) FlushUnpushedWork(ctx context.Context, taskID, taskDesc string, autoMerge bool) (merged bool, err error) {
	if _, pushErr := m.PushAndCreatePR(ctx, taskID, taskDesc, ""); pushErr != nil {
		return false, pushErr
	}
	if !autoMerge {
		return false, nil
	}
	merged, err = m.AutoMergeCurrentBranch(ctx)
	if err != nil {
		return false, err
	}
	if merged {
		m.PostMergeUpdateMain()
	}
	return merged, nil
}

// PostMergeUpdateMain fetches and fast-forwards local main in the project
// directory after a PR is merged. Does NOT touch the worktree — the next
// task commits on top of the current HEAD.
func (m *Manager) PostMergeUpdateMain() {
	defaultBranch := m.detectDefaultBranch()
	m.gitCmd(m.ProjectDir, "fetch", "origin", defaultBranch)
	m.gitCmd(m.ProjectDir, "reset", "--hard", "origin/"+defaultBranch)
	m.Logger.Log("git", "Updated local %s to latest origin", defaultBranch)

	// Sync worktree with updated main. If rebase conflicts, reset —
	// the merged work is on main and stale stack commits are expendable.
	if m.gitCmdErr(m.WorkDir, "rebase", "origin/"+defaultBranch) != nil {
		m.gitCmd(m.WorkDir, "rebase", "--abort")
		m.Logger.Warn("git", "Post-merge rebase failed — resetting worktree to origin/%s", defaultBranch)
		m.gitCmd(m.WorkDir, "reset", "--hard", "origin/"+defaultBranch)
		m.BranchRenamed = false
		if m.State != nil {
			_ = m.State.Write("branch_renamed", "false")
		}
	}

	// Delete the stale local task branch. The squash-merge already deleted the
	// remote branch; this removes the local ref so branches don't accumulate.
	if m.WorktreeBranch != "" && m.WorktreeBranch != WipBranchName() {
		oldBranch := m.WorktreeBranch
		currentBranch := m.gitOutput(m.WorkDir, "symbolic-ref", "--short", "HEAD")
		if currentBranch == oldBranch {
			// Worktree is checked out on the branch we're about to delete.
			// Move to ralph/next so git allows the deletion.
			nextBranch := normalizeBranch("next")
			m.gitCmd(m.WorkDir, "checkout", "-b", nextBranch)
			m.WorktreeBranch = nextBranch
			m.BranchRenamed = false
			if m.State != nil {
				_ = m.State.Write("worktree_branch", nextBranch)
				_ = m.State.Write("branch_renamed", "false")
			}
		}
		if err := m.gitCmdErr(m.ProjectDir, "branch", "-D", oldBranch); err == nil {
			m.Logger.Log("git", "Deleted local branch %s", oldBranch)
		}
	}
}
