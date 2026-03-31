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

// CreatePR ensures a PR exists for the current branch. If one is already
// open, updates its title and body. Otherwise creates a new PR targeting
// resolveBaseBranch. Returns the PR number.
func (m *Manager) CreatePR(ctx context.Context, taskID, taskDesc, body string) (string, error) {
	repoURL := m.RemoteURL()
	if repoURL == "" {
		return "", nil
	}

	gh := m.gh()
	if !gh.Available() {
		return "", fmt.Errorf("gh CLI not found — cannot create PR")
	}

	nwo := NWOFromRemote(repoURL)
	title := m.prTitle(taskID, taskDesc)

	// Existing PR — update and return.
	prNumber, _ := gh.FindOpenPR(m.WorktreeBranch, repoURL)
	if prNumber != "" {
		pr := logging.PRLink(nwo, prNumber)
		if taskID != "" {
			if err := gh.EditPR(prNumber, repoURL, title, body); err != nil {
				m.Logger.Warn("git", "Failed to update %s: %v", pr, err)
			}
		}
		m.Logger.Log("git", "%s already open for %s", pr, m.WorktreeBranch)
		return prNumber, nil
	}

	// New PR.
	baseBranch := m.resolveBaseBranch()
	if body == "" {
		body = taskDesc
	}
	createOpts := CreatePROpts{
		Head:  m.WorktreeBranch,
		Base:  baseBranch,
		Title: title,
		Body:  body,
		Repo:  repoURL,
		Dir:   m.WorkDir,
	}
	createErr := gh.CreatePR(createOpts)
	if createErr != nil {
		// Creation may fail if a closed PR exists for this head:base.
		// Try to find and reopen it instead.
		if prNumber, reopenErr := m.reopenClosedPR(gh, repoURL, title, body); reopenErr == nil && prNumber != "" {
			return prNumber, nil
		}

		// Reopen failed (e.g. branch diverged from old PR history).
		// The old PR is dead — create a fresh PR via the REST API,
		// bypassing gh pr create's client-side checks.
		nwo := NWOFromRemote(repoURL)
		if nwo != "" {
			if apiPR, apiErr := gh.CreatePRViaAPI(nwo, createOpts); apiErr == nil && apiPR != "" {
				m.Logger.Log("git", "Created %s for %s (via API fallback)", logging.PRLink(nwo, apiPR), m.WorktreeBranch)
				return apiPR, nil
			}
		}

		return "", createErr
	}

	newPR, _ := gh.FindOpenPR(m.WorktreeBranch, repoURL)
	if newPR != "" {
		m.Logger.Log("git", "Created %s for %s", logging.PRLink(nwo, newPR), m.WorktreeBranch)
	} else {
		m.Logger.Log("git", "Created PR for %s", m.WorktreeBranch)
	}
	return newPR, nil
}

// reopenClosedPR finds a closed (not merged) PR for the current branch and
// reopens it. Returns the PR number on success or empty string if no closed
// PR was found or reopen failed.
func (m *Manager) reopenClosedPR(gh GitHub, repoURL, title, body string) (string, error) {
	number, _, _, findErr := gh.FindPR(m.WorktreeBranch, m.WorkDir)
	if findErr != nil || number == "" {
		return "", findErr
	}
	state, stateErr := gh.GetPRState(m.WorkDir, number)
	if stateErr != nil || strings.ToUpper(state) != "CLOSED" {
		return "", stateErr
	}

	nwo := NWOFromRemote(repoURL)
	pr := logging.PRLink(nwo, number)

	if err := gh.ReopenPR(number, repoURL); err != nil {
		m.Logger.Warn("git", "Failed to reopen %s: %v", pr, err)
		return "", err
	}
	m.Logger.Log("git", "Reopened %s for %s", pr, m.WorktreeBranch)

	if title != "" {
		if err := gh.EditPR(number, repoURL, title, body); err != nil {
			m.Logger.Warn("git", "Failed to update %s: %v", pr, err)
		}
	}
	return number, nil
}

// ShipOpts configures the Ship pipeline.
type ShipOpts struct {
	TaskID    string
	TaskTitle string
	Body      string
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
func (m *Manager) Ship(ctx context.Context, opts ShipOpts) (ShipResult, error) {
	// Auto-commit uncommitted agent changes.
	if m.HasUncommittedChanges() {
		msg := "auto-commit agent changes"
		if opts.TaskID != "" {
			msg = fmt.Sprintf("[%s] %s", opts.TaskID, msg)
		}
		m.CommitAll(msg)
	}

	if err := m.Push(ctx); err != nil {
		if ctx.Err() != nil {
			return ShipResult{}, ctx.Err()
		}
		m.Logger.Warn("git", "Push failed: %v — attempting PR creation anyway", err)
	}

	prNumber, err := m.CreatePR(ctx, opts.TaskID, opts.TaskTitle, opts.Body)
	if err != nil {
		return ShipResult{PRNumber: prNumber}, err
	}

	// Look up the PR URL for the external ref.
	var prURL, prTitle string
	if prNumber != "" {
		gh := m.gh()
		if _, t, u, findErr := gh.FindPR(m.WorktreeBranch, m.WorkDir); findErr == nil {
			prURL = u
			prTitle = t
		}
		if prURL == "" {
			nwo := NWOFromRemote(m.RemoteURL())
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

// PushAndCreatePR composes Push and CreatePR. Squashes, force-pushes, then
// ensures a PR exists. Returns the PR number.
func (m *Manager) PushAndCreatePR(ctx context.Context, taskID, taskDesc, body string) (string, error) {
	result, err := m.Ship(ctx, ShipOpts{TaskID: taskID, TaskTitle: taskDesc, Body: body})
	return result.PRNumber, err
}

func (m *Manager) prTitle(taskID, taskDesc string) string {
	title := tasks.StripComponentPrefix(taskDesc)
	if taskID != "" {
		title = "[" + taskID + "] " + title
	}
	if len(title) > 70 {
		title = title[:67] + "..."
	}
	if title == "" {
		title = m.WorktreeBranch
	}
	return title
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

// executeMerge attempts the squash-merge and handles CI-gated retries.
func (m *Manager) executeMerge(ctx context.Context, prNumber, repoURL string) (bool, error) {
	nwo := NWOFromRemote(repoURL)
	pr := logging.PRLink(nwo, prNumber)
	gh := m.gh()
	opts := m.mergeOpts()

	if _, prTitle, _, titleErr := gh.FindPR(m.WorktreeBranch, m.WorkDir); titleErr == nil && prTitle != "" {
		opts.Subject = fmt.Sprintf("%s (#%s)", prTitle, prNumber)
	}

	result := gh.MergePR(prNumber, repoURL, opts)
	if result.Merged {
		return m.postMergeUpdate(nwo, prNumber)
	}

	if result.Conflict {
		m.Logger.Warn("git", "%s has merge conflicts — attempting rebase", pr)
		return false, &MergeConflictError{PRNumber: prNumber}
	}

	if result.Blocked {
		m.Logger.Log("ci", "%s blocked by branch protection: %s — waiting for CI...", pr, result.Message)
		checks, status, waitErr := m.AwaitCI(ctx, prNumber, repoURL, "")
		if waitErr != nil {
			return false, fmt.Errorf("CI polling failed for PR #%s: %w", prNumber, waitErr)
		}
		if status == CIFailed {
			return false, &CIFailureError{PRNumber: prNumber, Failures: failedChecks(checks)}
		}
		if status == CIPassed {
			m.Logger.Log("ci", "CI passed for %s — retrying merge", pr)
			retry := gh.MergePR(prNumber, repoURL, opts)
			if retry.Merged {
				return m.postMergeUpdate(nwo, prNumber)
			}
			m.Logger.Warn("git", "Merge retry failed for %s: %s", pr, retry.Message)
			return false, fmt.Errorf("merge retry failed for PR #%s after CI passed: %s", prNumber, retry.Message)
		}
	}

	m.Logger.Warn("git", "Auto-merge failed for %s: %s", pr, result.Message)
	return false, fmt.Errorf("auto-merge failed for PR #%s: %s", prNumber, result.Message)
}

// GetCIFailureLog retrieves the failed CI run's log output for the given PR.
func (m *Manager) GetCIFailureLog(prNumber string) string {
	return m.gh().GetRunLog(prNumber, m.WorkDir)
}

// postMergeUpdate logs the merge result. PostMergeUpdateMain is NOT called
// here — callers (finalizePR, FlushUnpushedWork) own the post-merge sync
// to avoid double calls when they also need to update main.
func (m *Manager) postMergeUpdate(nwo, prNumber string) (bool, error) {
	defaultBranch := m.detectDefaultBranch()
	m.Logger.Log("git", "%s %s merged", logging.BranchTag(defaultBranch), logging.PRLink(nwo, prNumber))
	return true, nil
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

// MergeWithRetry is the single merge pipeline: try merge, detect error type,
// handle it, retry. Conflicts trigger rebase + force-push; CI failures
// delegate to the OnCIFailure callback. Code fix retries share the main
// attempt budget. Infrastructure failures (no commits) use a separate
// retry counter with exponential backoff.
func (m *Manager) MergeWithRetry(ctx context.Context, opts MergeRetryOpts) (bool, error) {
	sleepFn := opts.SleepFunc
	if sleepFn == nil {
		sleepFn = time.Sleep
	}

	infraRetries := 0

	for attempt := 0; attempt < MaxMergeAttempts; attempt++ {
		merged, err := m.AutoMergeCurrentBranch(ctx)
		if err == nil {
			return merged, nil
		}

		if attempt > 0 {
			m.Logger.Warn("git", "Merge attempt %d failed: %v", attempt+1, err)
		}

		var conflictErr *MergeConflictError
		if errors.As(err, &conflictErr) {
			resolveErr := m.ResolveConflict(ctx)
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
				repoURL := m.RemoteURL()
				fixHeadSHA := m.gitOutput(m.WorkDir, "rev-parse", "HEAD")
				_, ciStatus, waitErr := m.AwaitCI(ctx, ciErr.PRNumber, repoURL, fixHeadSHA)
				if waitErr != nil {
					m.Logger.Warn("ci", "CI polling after fix: %v", waitErr)
				}
				if ciStatus == CIFailed {
					m.Logger.Warn("ci", "CI still failing after fix — will retry")
				}
				continue
			case CIFixNoCommits:
				// Infrastructure failure — fix agent found no code issue.
				// Retry with backoff instead of giving up.
				if infraRetries >= MaxInfraRetries {
					m.Logger.Warn("ci", "Infrastructure retries exhausted (%d) — giving up", MaxInfraRetries)
					return false, err
				}
				delay := infraBackoff(infraRetries)
				m.Logger.Log("ci", "CI infrastructure failure — retrying in %s (%d/%d)", delay, infraRetries+1, MaxInfraRetries)
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
