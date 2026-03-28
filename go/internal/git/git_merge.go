package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// PushAndCreatePR pushes the current branch to remote and creates a PR if
// none exists. For stacked PRs, the first PR in a session targets main and
// subsequent PRs target the previous task's branch. Returns the PR number
// (e.g. "42") so the caller can link it to the task backend.
func (m *Manager) PushAndCreatePR(ctx context.Context, taskID, taskDesc, body string) (string, error) {
	if m.WorktreeBranch == "" || m.WorkDir == m.ProjectDir {
		return "", nil
	}

	defaultBranch := m.detectDefaultBranch()
	_ = m.gitCmdErrCtx(ctx, m.WorkDir, "fetch", "origin", defaultBranch)
	if m.gitCmdErr(m.WorkDir, "rebase", "origin/"+defaultBranch) != nil {
		m.gitCmd(m.WorkDir, "rebase", "--abort")
		m.Logger.Warn("git", "Pre-push rebase failed — pushing without rebase, GitHub will handle conflicts")
	}

	repoURL := m.gitOutput(m.WorkDir, "remote", "get-url", "origin")
	if repoURL == "" {
		return "", nil
	}

	revCount := m.gitOutput(m.WorkDir, "rev-list", "--count", "origin/"+defaultBranch+"..HEAD")
	if revCount == "" || revCount == "0" {
		m.Logger.Log("git", "No new commits to push")
		return "", nil
	}

	gh := m.gh()
	if !gh.Available() {
		return "", fmt.Errorf("gh CLI not found — cannot create PR")
	}

	prNumber, _ := gh.FindOpenPR(m.WorktreeBranch, repoURL)
	if prNumber != "" {
		if taskID != "" {
			title := m.prTitle(taskID, taskDesc)
			if err := gh.EditPR(prNumber, repoURL, title, body); err != nil {
				m.Logger.Warn("git", "Failed to update PR #%s: %v", prNumber, err)
			}
		}
		m.Logger.Log("git", "PR #%s already open for %s", prNumber, m.WorktreeBranch)
		return prNumber, nil
	}

	m.Logger.Log("git", "Pushing %s...", m.WorktreeBranch)
	if err := m.gitCmdErrCtx(ctx, m.WorkDir, "push", "-u", "origin", m.WorktreeBranch); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Push failed — remote branch may already have the work.
		// Don't force-push. Just continue to PR creation using the remote branch.
		m.Logger.Log("git", "Push rejected — remote branch exists, continuing to PR creation")
	}

	// Stacked PRs: target previous branch if one exists, else main.
	baseBranch := defaultBranch
	if m.PrevBranch != "" {
		baseBranch = m.PrevBranch
		m.Logger.Log("git", "Stacked PR: targeting %s (previous task branch)", baseBranch)
	}

	// Check there are commits between the base and HEAD.
	_ = m.gitCmdErr(m.WorkDir, "fetch", "origin", baseBranch)
	baseRef := "origin/" + baseBranch
	if !m.refExists(m.WorkDir, baseRef) {
		baseRef = baseBranch
	}
	diffCount := m.gitOutput(m.WorkDir, "rev-list", "--count", baseRef+"..HEAD")
	if (diffCount == "" || diffCount == "0") && baseBranch != defaultBranch {
		// Stacked base has no diff — fall back to main.
		m.Logger.Log("git", "No commits between %s and HEAD — falling back to %s", baseRef, defaultBranch)
		baseBranch = defaultBranch
		baseRef = "origin/" + defaultBranch
		diffCount = m.gitOutput(m.WorkDir, "rev-list", "--count", baseRef+"..HEAD")
	}
	if diffCount == "" || diffCount == "0" {
		m.Logger.Log("git", "No commits between %s and HEAD — skipping PR", baseRef)
		return "", nil
	}

	title := m.prTitle(taskID, taskDesc)
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
		nwo := NWOFromRemote(repoURL)
		m.Logger.Log("git", "Created %s for %s", logging.PRLink(nwo, newPR), m.WorktreeBranch)
	} else {
		m.Logger.Log("git", "Created PR for %s", m.WorktreeBranch)
	}
	return newPR, nil
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

// AutoMergeCurrentBranch squash-merges the PR for the current branch into main.
// Returns (true, nil) when a PR was merged, (false, nil) when no PR exists or
// no action was needed, and (false, err) on failure. Returns typed errors
// (CIFailureError, MergeConflictError) that callers can handle.
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

	prNumber, err := gh.FindOpenPR(m.WorktreeBranch, repoURL)
	if err != nil || prNumber == "" {
		m.Logger.Log("git", "No open PR found for %s — skipping auto-merge", m.WorktreeBranch)
		return false, nil
	}

	defaultBranch := m.detectDefaultBranch()

	prBase, _ := gh.GetPRBase(m.WorkDir, prNumber)
	if prBase != "" && prBase != defaultBranch {
		m.Logger.Log("git", "PR #%s targets %s (not %s) — waiting for base PRs to merge first", prNumber, prBase, defaultBranch)
		return false, ErrStackedPRWaiting
	}

	m.Logger.Log("git", "%s Auto-merging PR #%s...", logging.BranchTag(defaultBranch), prNumber)

	checks, status, ciErr := m.AwaitCI(ctx, prNumber, repoURL)
	if ciErr != nil {
		m.Logger.Warn("ci", "CI polling failed for PR #%s: %v — attempting merge anyway", prNumber, ciErr)
	}
	if status == CIFailed {
		return false, &CIFailureError{PRNumber: prNumber, Failures: failedChecks(checks)}
	}
	if status == CIPassed {
		m.Logger.Log("ci", "CI passed for PR #%s — merging", prNumber)
	}

	if updateErr := m.updatePRBranch(ctx, prNumber, repoURL); updateErr != nil {
		return false, updateErr
	}

	return m.executeMerge(ctx, prNumber, repoURL)
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
	m.Logger.Log("git", "Updated PR #%s branch with latest base", prNumber)
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
	gh := m.gh()
	opts := m.mergeOpts()

	if _, prTitle, _, titleErr := gh.FindPR(m.WorktreeBranch, m.WorkDir); titleErr == nil && prTitle != "" {
		opts.Subject = fmt.Sprintf("%s (#%s)", prTitle, prNumber)
	}

	mergeOutput, mergeErr := gh.MergePR(prNumber, repoURL, opts)
	if mergeErr == nil {
		return m.postMergeUpdate(prNumber)
	}

	if isMergeConflictError(mergeOutput) {
		m.Logger.Warn("git", "PR #%s has merge conflicts — attempting rebase", prNumber)
		return false, &MergeConflictError{PRNumber: prNumber}
	}

	if isCIGatedError(mergeOutput) {
		m.Logger.Log("ci", "PR #%s blocked by branch protection — waiting for CI...", prNumber)
		checks, status, waitErr := m.AwaitCI(ctx, prNumber, repoURL)
		if waitErr != nil {
			return false, fmt.Errorf("CI polling failed for PR #%s: %w", prNumber, waitErr)
		}
		if status == CIFailed {
			return false, &CIFailureError{PRNumber: prNumber, Failures: failedChecks(checks)}
		}
		if status == CIPassed {
			m.Logger.Log("ci", "CI passed for PR #%s — retrying merge", prNumber)
			retryOutput, retryErr := gh.MergePR(prNumber, repoURL, opts)
			if retryErr == nil {
				return m.postMergeUpdate(prNumber)
			}
			m.Logger.Warn("git", "Merge retry failed for PR #%s: %s", prNumber, retryOutput)
			return false, fmt.Errorf("merge retry failed for PR #%s after CI passed", prNumber)
		}
	}

	m.Logger.Warn("git", "Auto-merge failed for PR #%s: %s", prNumber, mergeOutput)
	return false, fmt.Errorf("auto-merge failed for PR #%s", prNumber)
}

// GetCIFailureLog retrieves the failed CI run's log output for the given PR.
func (m *Manager) GetCIFailureLog(prNumber string) string {
	return m.gh().GetRunLog(prNumber, m.WorkDir)
}

// postMergeUpdate logs the merge result. PostMergeUpdateMain is NOT called
// here — callers (finalizePR, FlushUnpushedWork) own the post-merge sync
// to avoid double calls when they also need to update main.
func (m *Manager) postMergeUpdate(prNumber string) (bool, error) {
	defaultBranch := m.detectDefaultBranch()
	m.Logger.Log("git", "%s PR #%s merged", logging.BranchTag(defaultBranch), prNumber)
	return true, nil
}

// mergeOpts returns the merge options for the current Manager configuration.
func (m *Manager) mergeOpts() MergeOpts {
	return MergeOpts{
		DeleteBranch: true,
		Admin:        m.MergeAdmin,
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

// ForcePush pushes the current branch to the remote with --force-with-lease,
// which is needed after rebasing to resolve merge conflicts on a PR.
func (m *Manager) ForcePush(ctx context.Context) error {
	if m.WorktreeBranch == "" {
		return nil
	}
	m.Logger.Log("git", "Force-pushing %s...", m.WorktreeBranch)
	return m.gitCmdErrCtx(ctx, m.WorkDir, "push", "--force-with-lease", "origin", m.WorktreeBranch)
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
}

// ResolveConflict rebases onto the default branch and force-pushes to
// resolve PR merge conflicts before the next merge attempt. Returns an
// UnresolvedConflictError if the rebase couldn't resolve the divergence,
// signaling that retrying will not help.
func (m *Manager) ResolveConflict(ctx context.Context) error {
	defaultBranch := m.detectDefaultBranch()
	baseBranch := defaultBranch
	if m.PrevBranch != "" {
		baseBranch = m.PrevBranch
	}
	m.Logger.Log("git", "%s Rebasing onto %s to resolve merge conflicts...", logging.BranchTag(defaultBranch), baseBranch)
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

	m.Logger.Log("git", "%s Force-pushing rebased branch...", logging.BranchTag(defaultBranch))
	return m.ForcePush(ctx)
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
				return false, unresolved
			}
			return false, resolveErr
		}

		var ciErr *CIFailureError
		if errors.As(err, &ciErr) {
			if opts.OnCIFailure != nil && opts.OnCIFailure(ciErr) {
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
	m.Logger.Log("git", "Reset worktree to latest %s", defaultBranch)

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
