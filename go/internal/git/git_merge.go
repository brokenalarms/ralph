package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
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
			baseSHA = m.gitOutput(m.WorkDir, "rev-parse", baseRef)
		}
		if baseSHA != "" {
			commitMsg := m.gitOutput(m.WorkDir, "log", "-1", "--format=%s")
			if err := m.SquashToOneCommit(baseSHA, commitMsg); err != nil {
				m.Logger.Warn("git", "Squash: %v", err)
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
	if err := gh.CreatePR(CreatePROpts{
		Head:  m.WorktreeBranch,
		Base:  baseBranch,
		Title: title,
		Body:  body,
		Repo:  repoURL,
		Dir:   m.WorkDir,
	}); err != nil {
		return "", err
	}

	newPR, _ := gh.FindOpenPR(m.WorktreeBranch, repoURL)
	if newPR != "" {
		m.Logger.Log("git", "Created %s for %s", logging.PRLink(nwo, newPR), m.WorktreeBranch)
	} else {
		m.Logger.Log("git", "Created PR for %s", m.WorktreeBranch)
	}
	return newPR, nil
}

// PushAndCreatePR composes Push and CreatePR. Squashes, force-pushes, then
// ensures a PR exists. Returns the PR number.
func (m *Manager) PushAndCreatePR(ctx context.Context, taskID, taskDesc, body string) (string, error) {
	if err := m.Push(ctx); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		m.Logger.Warn("git", "Push failed: %v — attempting PR creation anyway", err)
	}
	return m.CreatePR(ctx, taskID, taskDesc, body)
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
		m.Logger.Log("git", "No open PR found for %s — skipping auto-merge", m.WorktreeBranch)
		return false, nil
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

	checks, status, ciErr := m.AwaitCI(ctx, prNumber, repoURL)
	if ciErr != nil {
		m.Logger.Warn("ci", "CI polling failed for %s: %v — attempting merge anyway", pr, ciErr)
	}
	if status == CIFailed {
		return false, &CIFailureError{PRNumber: prNumber, Failures: failedChecks(checks)}
	}
	if status == CIPassed {
		m.Logger.Log("ci", "CI passed for %s — merging", pr)
	}

	// Check if main moved while CI was running. If the branch needs
	// updating, return a merge conflict error so MergeWithRetry loops
	// back through rebase+push+CI.
	if m.branchNeedsUpdate(prNumber, repoURL) {
		m.Logger.Log("git", "Main moved while CI was running — will rebase and retry")
		return false, &MergeConflictError{PRNumber: prNumber}
	}

	return m.executeMerge(ctx, prNumber, repoURL)
}

// branchNeedsUpdate checks if the PR branch is behind the base branch.
func (m *Manager) branchNeedsUpdate(prNumber, repoURL string) bool {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return false
	}
	gh := m.gh()
	updated, err := gh.UpdateBranch(m.WorkDir, nwo, prNumber)
	if err != nil {
		return true
	}
	return updated
}

// updatePRBranch updates the PR branch with the latest base branch commits.
// If the branch was updated, waits for CI on the new HEAD. Returns a
// CIFailureError when CI fails after the update.
func (m *Manager) updatePRBranch(ctx context.Context, prNumber, repoURL string) error {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return nil
	}
	gh := m.gh()
	updated, updateErr := gh.UpdateBranch(m.WorkDir, nwo, prNumber)
	if updateErr != nil {
		m.Logger.Warn("git", "PR branch update: %v", updateErr)
		return nil
	}
	if !updated {
		return nil
	}
	m.Logger.Log("git", "Updated %s branch with latest base", logging.PRLink(nwo, prNumber))
	checks, status, err := m.AwaitCI(ctx, prNumber, repoURL)
	if err != nil {
		m.Logger.Warn("ci", "CI polling after branch update: %v — attempting merge anyway", err)
		return nil
	}
	if status == CIFailed {
		return &CIFailureError{PRNumber: prNumber, Failures: failedChecks(checks)}
	}
	return nil
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

	mergeOutput, mergeErr := gh.MergePR(prNumber, repoURL, opts)
	if mergeErr == nil {
		return m.postMergeUpdate(nwo, prNumber)
	}

	if isMergeConflictError(mergeOutput) {
		m.Logger.Warn("git", "%s has merge conflicts — attempting rebase", pr)
		return false, &MergeConflictError{PRNumber: prNumber}
	}

	if isCIGatedError(mergeOutput) {
		m.Logger.Log("ci", "%s blocked by branch protection — waiting for CI...", pr)
		checks, status, waitErr := m.AwaitCI(ctx, prNumber, repoURL)
		if waitErr != nil {
			return false, fmt.Errorf("CI polling failed for PR #%s: %w", prNumber, waitErr)
		}
		if status == CIFailed {
			return false, &CIFailureError{PRNumber: prNumber, Failures: failedChecks(checks)}
		}
		if status == CIPassed {
			m.Logger.Log("ci", "CI passed for %s — retrying merge", pr)
			retryOutput, retryErr := gh.MergePR(prNumber, repoURL, opts)
			if retryErr == nil {
				return m.postMergeUpdate(nwo, prNumber)
			}
			m.Logger.Warn("git", "Merge retry failed for %s: %s", pr, retryOutput)
			return false, fmt.Errorf("merge retry failed for PR #%s after CI passed", prNumber)
		}
	}

	m.Logger.Warn("git", "Auto-merge failed for %s: %s", pr, mergeOutput)
	return false, fmt.Errorf("auto-merge failed for PR #%s", prNumber)
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

// ForcePush is an alias for Push — both squash and force-push.
// Kept for backward compatibility with callers that expect the name.
func (m *Manager) ForcePush(ctx context.Context) error {
	return m.Push(ctx)
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

// MergeRetryOpts configures the merge-with-retry pipeline.
type MergeRetryOpts struct {
	// OnCIFailure is called when CI checks fail on the PR. It should attempt
	// to fix the failure (e.g. by spawning a fix agent) and return true if
	// the fix was applied and a retry should be attempted.
	OnCIFailure func(ciErr *CIFailureError) bool

	// OnConflict is called when automatic rebase cannot resolve merge
	// conflicts (UnresolvedConflictError). It should spawn a conflict
	// resolution agent and return true if the conflict was resolved and
	// force-pushed, ready for a merge retry.
	OnConflict func(conflictErr *UnresolvedConflictError) bool
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
// delegate to the OnCIFailure callback. Both share a single retry budget.
func (m *Manager) MergeWithRetry(ctx context.Context, opts MergeRetryOpts) (bool, error) {
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
			if opts.OnCIFailure != nil && opts.OnCIFailure(ciErr) {
				// Fix was applied and force-pushed. Wait for new CI on the
				// updated HEAD before retrying merge — the old check status
				// is stale after force-push.
				repoURL := m.RemoteURL()
				_, ciStatus, waitErr := m.AwaitCI(ctx, ciErr.PRNumber, repoURL)
				if waitErr != nil {
					m.Logger.Warn("ci", "CI polling after fix: %v", waitErr)
				}
				if ciStatus == CIFailed {
					m.Logger.Warn("ci", "CI still failing after fix — will retry")
				}
				continue
			}
			return false, err
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
}
