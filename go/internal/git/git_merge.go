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
func (m *Repo) resolveBaseBranch() string {
	if m.PrevBranch != "" {
		return m.PrevBranch
	}
	return m.detectDefaultBranch()
}

// Push squashes all commits into one and force-pushes the branch.
// Always uses --force-with-lease (safe — only forces if remote matches
// last fetch). Squash ensures stacked PRs cascade cleanly on merge.
func (m *Repo) Push(ctx context.Context) error {
	if m.WorktreeBranch == "" || m.WorkDir == m.ProjectDir {
		return nil
	}

	if m.prePush != nil {
		if err := m.prePush.PrePush(ctx, m.WorkDir); err != nil {
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
			m.logger.Emit(logging.Opts{Domain: logging.Git}, "Branch diverged from %s — rebasing before push", baseBranch)
			if err := m.EnsureUpToDate(ctx); err != nil {
				m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Rebase before push failed: %v", err)
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
					m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Squash: %v", err)
				}
			}
		}
	}

	m.logger.Emit(logging.Opts{Domain: logging.Git}, "Pushing %s...", m.WorktreeBranch)
	// Try force-with-lease first (safe update of existing branch).
	// Fall back to regular push for new branches.
	if err := m.gitCmdErrCtx(ctx, m.WorkDir, "push", "--force-with-lease", "-u", "origin", m.WorktreeBranch); err != nil {
		return m.gitCmdErrCtx(ctx, m.WorkDir, "push", "-u", "origin", m.WorktreeBranch)
	}
	return nil
}

// reopenClosedPR finds a closed (not merged) PR for the given branch and
// reopens it. Returns the PR number on success or 0 if no closed PR was
// found or reopen failed.
func reopenClosedPR(gh GitHub, workDir, branch, nwo, repoURL, title, body string, logger Log) (int, error) {
	number, _, _, findErr := gh.FindPR(branch, repoURL)
	if findErr != nil || number == 0 {
		return 0, findErr
	}
	prDetail, stateErr := gh.GetPR(nwo, number)
	if stateErr != nil || prDetail == nil || prDetail.State != PRStateClosed {
		return 0, stateErr
	}

	prLink := logging.PRLinkOpt(nwo, number)

	if err := gh.ReopenPR(number, repoURL); err != nil {
		logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "Failed to reopen: %v", err)
		return 0, err
	}
	logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "Reopened for %s", branch)

	if title != "" {
		if err := gh.EditPR(number, repoURL, title, body); err != nil {
			logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "Failed to update: %v", err)
		}
	}
	return number, nil
}

func (m *Repo) reopenClosedPR(gh GitHub, repoURL, title, body string) (int, error) {
	nwo := NWOFromRemote(repoURL)
	return reopenClosedPR(gh, m.WorkDir, m.WorktreeBranch, nwo, repoURL, title, body, m.logger)
}

// EnsurePROpts configures the CreatePR package function. Description,
// Acceptance, and Summary are pure data the caller pre-fetches; CreatePR
// passes them to formatPRBody (in github.go) to build the body.
type EnsurePROpts struct {
	TaskID      string
	TaskDesc    string
	Description string
	Acceptance  string
	Summary     string
	BaseBranch  string
	Logger      Log
}

// CreatePR ensures a PR exists for the given branch. If one is already open,
// updates its title and body. Otherwise creates a new PR targeting BaseBranch.
// Returns the PR number.
func CreatePR(ctx context.Context, gh GitHub, workDir, branch, remoteURL string, opts EnsurePROpts) (int, error) {
	if remoteURL == "" {
		return 0, nil
	}
	if !gh.Available() {
		return 0, fmt.Errorf("gh CLI not found — cannot create PR")
	}

	nwo := NWOFromRemote(remoteURL)
	title := prTitle(opts.TaskID, opts.TaskDesc, branch)
	body := formatPRBody(opts.Description, opts.Acceptance, opts.Summary)

	// Existing PR — update and return.
	prNumber, _ := gh.FindOpenPR(branch, remoteURL)
	if prNumber != 0 {
		prLink := logging.PRLinkOpt(nwo, prNumber)
		if opts.TaskID != "" {
			if err := gh.EditPR(prNumber, remoteURL, title, body); err != nil {
				opts.Logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "Failed to update: %v", err)
			}
		}
		opts.Logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "already open for %s", branch)
		return prNumber, nil
	}

	// New PR — fall back to task description as the body when nothing
	// else is available.
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
	newPR, createErr := gh.CreatePR(createOpts)
	if createErr != nil {
		// PR creation failed. Check if the branch already has a PR in any state.
		// A merged PR means the push already delivered commits — return its number.
		// A closed PR can be reopened by the block below.
		if existingPR, _, _, findErr := gh.FindPR(branch, remoteURL); findErr == nil && existingPR != 0 {
			if prDetail, detailErr := gh.GetPR(nwo, existingPR); detailErr == nil && prDetail != nil && prDetail.State == PRStateMerged {
				prLink := logging.PRLinkOpt(nwo, existingPR)
				opts.Logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "Merged PR already exists for %s — commits landed", branch)
				return existingPR, nil
			}
		}

		// Creation may fail if a closed PR exists for this head:base.
		// Try to find and reopen it instead.
		if prNumber, reopenErr := reopenClosedPR(gh, workDir, branch, nwo, remoteURL, title, body, opts.Logger); reopenErr == nil && prNumber != 0 {
			return prNumber, nil
		}

		// Reopen failed (e.g. branch diverged from old PR history).
		// The old PR is dead — create a fresh PR via the REST API,
		// bypassing gh pr create's client-side checks.
		if nwo != "" {
			if apiPR, apiErr := gh.CreatePRViaAPI(nwo, createOpts); apiErr == nil && apiPR != 0 {
				opts.Logger.Emit(logging.Opts{Domain: logging.Git, Link: logging.PRLinkOpt(nwo, apiPR)}, "Created for %s (via API fallback)", branch)
				return apiPR, nil
			}
		}

		return 0, createErr
	}

	if newPR != 0 {
		opts.Logger.Emit(logging.Opts{Domain: logging.Git, Link: logging.PRLinkOpt(nwo, newPR)}, "Created for %s", branch)
	}
	return newPR, nil
}

// Repo.CreatePR delegates to the package function. The summary string is
// passed through formatPRBody as the Summary section; callers needing
// description / acceptance criteria use the package function CreatePR
// directly.
func (m *Repo) CreatePR(ctx context.Context, taskID, taskDesc, summary string) (int, error) {
	return CreatePR(ctx, m.gh(), m.WorkDir, m.WorktreeBranch, m.RemoteURL(), EnsurePROpts{
		TaskID:     taskID,
		TaskDesc:   taskDesc,
		Summary:    summary,
		BaseBranch: m.resolveBaseBranch(),
		Logger:     m.logger,
	})
}

// ShipOpts configures the Ship pipeline. All fields are data — no func or
// interface fields. Repo.Ship fills infrastructure from its own fields.
type ShipOpts struct {
	TaskID    string
	TaskTitle string

	// Description, Acceptance, Summary are pure data (strings) that the
	// caller pre-fetches from whatever task backend it uses. git/github
	// formats them into the PR body via formatPRBody — the orchestrator
	// never knows what "## Description" means, only that the data fields
	// exist.
	Description string
	Acceptance  string
	Summary     string

	BaseBranch string

	// PRNumber, when non-zero, tells Ship to skip push+PR creation and proceed
	// directly to the merge phase using the identified PR.
	PRNumber int

	// AutoMerge instructs Ship to proceed through reviewer polling and merge
	// after the PR is created/updated. When false, Ship returns after push+PR.
	AutoMerge bool

	// Reviewers lists automated code reviewers to poll before merging.
	Reviewers []Reviewer

	// ReviewAddressed maps bot username → true when that reviewer's feedback
	// was already addressed in a previous call, skipping re-poll.
	ReviewAddressed map[string]bool
}

// ShipResult is the outcome of the Ship pipeline.
type ShipResult struct {
	PRNumber int
	PRURL    string
	PRTitle  string

	// Merged is true when the PR was squash-merged successfully.
	Merged bool

	// AlreadyMerged is true when Ship discovered the PR was already merged
	// before it took any action. The caller can close the task immediately.
	AlreadyMerged bool

	// Closed is true when Ship discovered the PR is closed (not merged).
	// The caller should clear stale refs and re-run the agent.
	Closed bool

	// Stacked is true when the PR targets a non-default branch (merge skipped,
	// task should be closed and the loop continues).
	Stacked bool

	// CIFailure is true when merge was attempted but CI is failing. The loop
	// may call a fix agent and retry Ship.
	CIFailure bool

	// CIFailureDetail carries the CI failure error for use by tryFixCI.
	// Populated when CIFailure is true.
	CIFailureDetail *CIFailureError

	// ReviewFixNeeded is true when a reviewer returned actionable comments.
	// The loop should run tryFixReviewComments and retry Ship.
	ReviewFixNeeded bool

	// PendingReview is the review that needs to be addressed.
	PendingReview *AutoReview

	// PendingReviewer is the bot username whose review needs addressing.
	PendingReviewer string
}

// shipInfra holds the infrastructure callbacks used by shipPR. These are
// separated from ShipOpts to keep ShipOpts data-only.
type shipInfra struct {
	push              func(context.Context) error
	hasUncommitted    func() bool
	commitAll         func(string)
	branchAheadOfMain func(string) bool
	logger            Log
}

// shipPR is the single "get work into a PR" pipeline: auto-commit any
// uncommitted changes, push (squash + rebase + force-push), and create
// or update a PR. Returns the PR number and URL.
func shipPR(ctx context.Context, runner Runner, gh GitHub, workDir, branch, remoteURL string, opts ShipOpts, infra shipInfra) (ShipResult, error) {
	hasChanges := infra.hasUncommitted
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

	commitAll := infra.commitAll
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

	if infra.push != nil {
		if err := infra.push(ctx); err != nil {
			if ctx.Err() != nil {
				return ShipResult{}, ctx.Err()
			}
			if infra.logger != nil {
				infra.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Push failed: %v", err)
			}
			return ShipResult{}, fmt.Errorf("push failed: %w", err)
		}
	}

	if infra.branchAheadOfMain != nil && !infra.branchAheadOfMain(branch) {
		if infra.logger != nil {
			infra.logger.Emit(logging.Opts{Domain: logging.Git}, "Ship: branch %s is not ahead of main — skipping PR creation", branch)
		}
		return ShipResult{}, nil
	}

	baseBranch := opts.BaseBranch
	prNumber, err := CreatePR(ctx, gh, workDir, branch, remoteURL, EnsurePROpts{
		TaskID:      opts.TaskID,
		TaskDesc:    opts.TaskTitle,
		Description: opts.Description,
		Acceptance:  opts.Acceptance,
		Summary:     opts.Summary,
		BaseBranch:  baseBranch,
		Logger:      infra.logger,
	})
	if err != nil {
		return ShipResult{PRNumber: prNumber}, err
	}

	// Look up the PR URL for the external ref.
	var prURL, prTitle string
	if prNumber != 0 {
		if _, t, u, findErr := gh.FindPR(branch, remoteURL); findErr == nil {
			prURL = u
			prTitle = t
		}
		if prURL == "" {
			nwo := NWOFromRemote(remoteURL)
			if nwo != "" {
				prURL = fmt.Sprintf("https://github.com/%s/pull/%d", nwo, prNumber)
			}
		}
	}

	return ShipResult{
		PRNumber: prNumber,
		PRURL:    prURL,
		PRTitle:  prTitle,
	}, nil
}

// Repo.Ship runs the full push + PR + reviewer poll + merge pipeline.
// ShipOpts carries only data; infrastructure callbacks come from Repo fields.
// When opts.PRNumber is non-zero, Ship skips push+PR and proceeds directly to
// reviewer polling and merge using that PR.
func (m *Repo) Ship(ctx context.Context, opts ShipOpts) (ShipResult, error) {
	if opts.BaseBranch == "" {
		opts.BaseBranch = m.resolveBaseBranch()
	}

	var result ShipResult
	var err error

	if opts.PRNumber != 0 {
		// Caller identified the PR — check its state before proceeding.
		result = ShipResult{PRNumber: opts.PRNumber}
		prState, stateErr := m.GetPRState(opts.PRNumber)
		if stateErr != nil {
			return result, fmt.Errorf("get PR state: %w", stateErr)
		}
		switch prState {
		case PRStateMerged:
			result.AlreadyMerged = true
			result.Merged = true
			m.PostMergeUpdateMain()
			return result, nil
		case PRStateClosed:
			result.Closed = true
			return result, nil
		}
		// PRStateOpen — fall through to merge phase.
	} else {
		infra := shipInfra{
			push:              m.Push,
			hasUncommitted:    m.HasUncommittedChanges,
			commitAll:         m.CommitAll,
			branchAheadOfMain: m.BranchIsAheadOfMain,
			logger:            m.logger,
		}
		result, err = shipPR(ctx, m.run(), m.gh(), m.WorkDir, m.WorktreeBranch, m.RemoteURL(), opts, infra)
		if err != nil {
			return result, err
		}
	}

	if !opts.AutoMerge || result.PRNumber == 0 {
		return result, nil
	}

	gh := m.gh()
	nwo := NWOFromRemote(m.RemoteURL())
	repoURL := m.RemoteURL()
	prLink := logging.PRLinkOpt(nwo, result.PRNumber)

	// Reviewer polling.
	for _, reviewer := range opts.Reviewers {
		if opts.ReviewAddressed[reviewer.BotUsername] {
			continue
		}
		review, pollErr := m.PollReview(reviewer.BotUsername, result.PRNumber, reviewer.DefaultTimeout)
		if pollErr != nil {
			m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "%s review poll: %v", reviewer.BotUsername, pollErr)
			continue
		}
		if review != nil {
			m.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "%s review received (%d comments)", reviewer.BotUsername, len(review.Comments))
			result.ReviewFixNeeded = true
			result.PendingReview = review
			result.PendingReviewer = reviewer.BotUsername
			return result, nil
		}
		m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "No %s review arrived within timeout — proceeding to merge", reviewer.BotUsername)
	}

	// Check if stacked (PR targets non-default branch — merge skipped).
	defaultBranch := m.detectDefaultBranch()
	prDetail, _ := gh.GetPR(nwo, result.PRNumber)
	prBase := ""
	if prDetail != nil {
		prBase = prDetail.BaseRef
	}
	if prBase != "" && prBase != defaultBranch {
		m.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "targets %s (not %s) — stacked, closing bead", prBase, defaultBranch)
		result.Stacked = true
		return result, nil
	}

	// Attempt merge (no OnCIFailure/OnConflict — return errors for loop to handle).
	m.SetLocalTestsPassed(true)
	m.SetKnownPRNumber(result.PRNumber)
	defer m.SetKnownPRNumber(0)

	merged, mergeErr := m.MergeWithRetry(ctx, MergeRetryOpts{
		Logger:    m.logger,
		RemoteURL: repoURL,
	})
	if mergeErr != nil {
		var ciExhausted *CIFixExhaustedError
		var ciFailure *CIFailureError
		if errors.As(mergeErr, &ciExhausted) {
			result.CIFailure = true
			return result, nil
		}
		if errors.As(mergeErr, &ciFailure) {
			result.CIFailure = true
			result.CIFailureDetail = ciFailure
			return result, nil
		}
		return result, mergeErr
	}

	result.Merged = merged
	if merged {
		m.PostMergeUpdateMain()
	}
	return result, nil
}

// PushAndCreatePR composes Push and CreatePR. Squashes, force-pushes, then
// ensures a PR exists. Returns the PR number. The summary argument flows
// into the Summary section of the formatted PR body.
func (m *Repo) PushAndCreatePR(ctx context.Context, taskID, taskDesc, summary string) (int, error) {
	result, err := m.Ship(ctx, ShipOpts{TaskID: taskID, TaskTitle: taskDesc, Summary: summary})
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

func (m *Repo) prTitle(taskID, taskDesc string) string {
	return prTitle(taskID, taskDesc, m.WorktreeBranch)
}

// AutoMergeCurrentBranch rebases onto main, pushes, waits for CI, and
// merges. If main moves between CI passing and the merge attempt, loops
// back to rebase+push again. Returns typed errors (CIFailureError,
// MergeConflictError) that callers can handle.
func (m *Repo) AutoMergeCurrentBranch(ctx context.Context) (bool, error) {
	if m.WorktreeBranch == "" || m.WorkDir == m.ProjectDir {
		return false, nil
	}

	gh := m.gh()
	if !gh.Available() {
		return false, fmt.Errorf("gh CLI not found — cannot auto-merge")
	}

	repoURL := m.gitOutput(m.WorkDir, "remote", "get-url", "origin")
	if repoURL == "" {
		m.logger.Emit(logging.Opts{Domain: logging.Git}, "No remote URL — skipping auto-merge")
		return false, nil
	}

	nwo := NWOFromRemote(repoURL)
	prNumber := m.KnownPRNumber
	if prNumber == 0 {
		var err error
		prNumber, err = gh.FindOpenPR(m.WorktreeBranch, repoURL)
		if err != nil || prNumber == 0 {
			prNumber, err = m.resolveClosedPR(gh, repoURL)
			if errors.Is(err, ErrPRAlreadyMerged) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			if prNumber == 0 {
				m.logger.Emit(logging.Opts{Domain: logging.Git}, "No PR found for %s — skipping auto-merge", m.WorktreeBranch)
				return false, nil
			}
		}
	}
	prLink := logging.PRLinkOpt(nwo, prNumber)

	defaultBranch := m.detectDefaultBranch()

	prDetail, _ := gh.GetPR(nwo, prNumber)
	prBase := ""
	if prDetail != nil {
		prBase = prDetail.BaseRef
	}
	if prBase != "" && prBase != defaultBranch {
		m.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "targets %s (not %s) — waiting for base PRs to merge first", prBase, defaultBranch)
		return false, ErrStackedPRWaiting
	}

	m.logger.Emit(logging.Opts{Domain: logging.Git, Branch: defaultBranch, Link: prLink}, "Auto-merging...")

	// Fast path: when local HEAD already matches the PR head SHA and CI is already
	// passing, skip the rebase+push cycle — no new tree to test, no push needed.
	// This avoids the no-op push → stale pushedAt filter → infinite poll cycle.
	if prDetail != nil && prDetail.HeadSHA != "" && prDetail.HeadSHA == m.HeadRev() {
		_, fastStatus, _ := m.AwaitCI(ctx, prNumber, repoURL, time.Time{})
		if fastStatus == CIPassed {
			m.logger.Emit(logging.Opts{Domain: logging.CI, Link: prLink}, "CI already passing on %s — merging", prDetail.HeadSHA)
			return m.executeMerge(ctx, prNumber, repoURL)
		}
	}

	// Rebase onto latest main and push so CI runs on the final tree.
	// This avoids the updatePRBranch round-trip and double CI wait.
	if err := m.EnsureUpToDate(ctx); err != nil {
		m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Pre-merge rebase failed: %v", err)
	}
	headBefore := m.HeadRev()
	pushedAt := time.Now()
	pushErr := m.Push(ctx)
	if pushErr != nil {
		m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Pre-merge push failed: %v", pushErr)
	}
	// When the push succeeds but is a no-op (same SHA before and after), no new
	// CI is triggered. The existing checks are the only relevant results �� skip
	// the pushedAt filter so they are not discarded. Without this, the loop
	// evaluates only post-push checks and misses pre-existing failures.
	awaitPushedAt := pushedAt
	if pushErr == nil && headBefore != "" && m.HeadRev() == headBefore {
		m.logger.Emit(logging.Opts{Domain: logging.CI, Link: prLink}, "Push was no-op (SHA unchanged) — evaluating existing checks")
		awaitPushedAt = time.Time{}
	}

	checks, status, ciErr := m.AwaitCI(ctx, prNumber, repoURL, awaitPushedAt)
	if ciErr != nil {
		m.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn, Link: prLink}, "CI did not complete within timeout — leaving PR open")
		return false, ciErr
	}
	if status == CIFailed {
		return false, &CIFailureError{PRNumber: prNumber, Failures: failedChecks(checks)}
	}
	if status == CIPassed {
		m.logger.Emit(logging.Opts{Domain: logging.CI, Link: prLink}, "CI passed — merging")
	}

	if m.branchNeedsUpdate() {
		m.logger.Emit(logging.Opts{Domain: logging.Git}, "Main moved while CI was running — will rebase and retry")
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
func (m *Repo) resolveClosedPR(gh GitHub, repoURL string) (int, error) {
	number, _, _, findErr := gh.FindPR(m.WorktreeBranch, repoURL)
	if findErr != nil || number == 0 {
		return 0, nil
	}

	nwo := NWOFromRemote(repoURL)
	prDetail, stateErr := gh.GetPR(nwo, number)
	if stateErr != nil || prDetail == nil {
		return 0, nil
	}

	prLink := logging.PRLinkOpt(nwo, number)

	switch prDetail.State {
	case PRStateMerged:
		m.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "already merged — nothing to do")
		return 0, ErrPRAlreadyMerged
	case PRStateClosed:
		m.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "is closed — reopening")
		if err := gh.ReopenPR(number, repoURL); err != nil {
			m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "Failed to reopen: %v — creating new PR", err)
			baseBranch := m.resolveBaseBranch()
			opts := CreatePROpts{
				Head: m.WorktreeBranch,
				Base: baseBranch,
				Repo: repoURL,
				Dir:  m.WorkDir,
			}
			if apiPR, apiErr := gh.CreatePRViaAPI(nwo, opts); apiErr == nil && apiPR != 0 {
				m.logger.Emit(logging.Opts{Domain: logging.Git, Link: logging.PRLinkOpt(nwo, apiPR)}, "Created for %s (via API fallback)", m.WorktreeBranch)
				return apiPR, nil
			}
			return 0, nil
		}
		m.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "reopened")
		return number, nil
	default:
		return 0, nil
	}
}

// branchNeedsUpdate checks if the base branch has moved ahead of HEAD since
// the last push. Returns true when origin/<base> is not an ancestor of HEAD,
// meaning main moved while CI was running and the branch must be rebased
// before merging. Uses a local ancestry check to avoid creating merge commits.
func (m *Repo) branchNeedsUpdate() bool {
	baseBranch := m.resolveBaseBranch()
	_ = m.gitCmdErr(m.WorkDir, "fetch", "origin", baseBranch)
	if !m.refExists(m.WorkDir, "origin/"+baseBranch) {
		return false
	}
	return m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", "origin/"+baseBranch, "HEAD") != nil
}

// ExecuteMergeOpts holds all parameters for the executeMerge package function.
type ExecuteMergeOpts struct {
	PRNumber       int
	RepoURL        string
	WorktreeBranch string
	WorkDir        string
	DefaultBranch  string
	MergeOpts      MergeOpts
	// AwaitCI polls CI check status for the PR. Required for the Blocked path.
	AwaitCI func(ctx context.Context, prNumber int, repoURL string, pushedAt time.Time) ([]CICheckResult, CIStatus, error)
}

// executeMerge attempts the squash-merge and handles CI-gated retries.
// It is a package function — callers compose it without a Repo receiver.
// Repo.executeMerge delegates here.
func executeMerge(ctx context.Context, gh GitHub, opts ExecuteMergeOpts, logger Log) (bool, error) {
	nwo := NWOFromRemote(opts.RepoURL)
	prLink := logging.PRLinkOpt(nwo, opts.PRNumber)
	mergeOpts := opts.MergeOpts

	if _, prTitle, _, titleErr := gh.FindPR(opts.WorktreeBranch, opts.RepoURL); titleErr == nil && prTitle != "" {
		mergeOpts.Subject = fmt.Sprintf("%s (#%d)", prTitle, opts.PRNumber)
	}

	result := gh.MergePR(opts.PRNumber, opts.RepoURL, mergeOpts)
	if result.Merged {
		return postMergeLog(nwo, opts.PRNumber, opts.DefaultBranch, logger)
	}

	if result.Conflict {
		if logger != nil {
			logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "has merge conflicts — attempting rebase")
		}
		return false, &MergeConflictError{PRNumber: opts.PRNumber}
	}

	if result.Blocked {
		if logger != nil {
			logger.Emit(logging.Opts{Domain: logging.CI, Link: prLink}, "blocked by branch protection: %s — waiting for CI...", result.Message)
		}
		if opts.AwaitCI == nil {
			return false, fmt.Errorf("auto-merge blocked for PR #%d: %s", opts.PRNumber, result.Message)
		}
		checks, status, waitErr := opts.AwaitCI(ctx, opts.PRNumber, opts.RepoURL, time.Time{})
		if waitErr != nil {
			return false, fmt.Errorf("CI polling failed for PR #%d: %w", opts.PRNumber, waitErr)
		}
		if status == CIFailed {
			return false, &CIFailureError{PRNumber: opts.PRNumber, Failures: failedChecks(checks)}
		}
		if status == CIPassed {
			if logger != nil {
				logger.Emit(logging.Opts{Domain: logging.CI, Link: prLink}, "CI passed — retrying merge")
			}
			retry := gh.MergePR(opts.PRNumber, opts.RepoURL, mergeOpts)
			if retry.Merged {
				return postMergeLog(nwo, opts.PRNumber, opts.DefaultBranch, logger)
			}
			if logger != nil {
				logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "Merge retry failed: %s", retry.Message)
			}
			return false, fmt.Errorf("merge retry failed for PR #%d after CI passed: %s", opts.PRNumber, retry.Message)
		}
	}

	if logger != nil {
		logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "Auto-merge failed: %s", result.Message)
	}
	return false, fmt.Errorf("auto-merge failed for PR #%d: %s", opts.PRNumber, result.Message)
}

// postMergeLog logs the merge completion.
func postMergeLog(nwo string, prNumber int, defaultBranch string, logger Log) (bool, error) {
	if logger != nil {
		logger.Emit(logging.Opts{Domain: logging.Git, Branch: defaultBranch, Link: logging.PRLinkOpt(nwo, prNumber)}, "merged")
	}
	return true, nil
}

// Repo.executeMerge delegates to the package-level executeMerge function.
func (m *Repo) executeMerge(ctx context.Context, prNumber int, repoURL string) (bool, error) {
	return executeMerge(ctx, m.gh(), ExecuteMergeOpts{
		PRNumber:       prNumber,
		RepoURL:        repoURL,
		WorktreeBranch: m.WorktreeBranch,
		WorkDir:        m.WorkDir,
		DefaultBranch:  m.detectDefaultBranch(),
		MergeOpts:      m.mergeOpts(),
		AwaitCI:        m.AwaitCI,
	}, m.logger)
}

// GetCIFailureLog retrieves the failed CI run's log output for the given PR.
func (m *Repo) GetCIFailureLog(prNumber int) string {
	return m.gh().GetRunLog(prNumber, m.WorkDir)
}

// mergeOpts returns the merge options for the current Repo configuration.
func (m *Repo) mergeOpts() MergeOpts {
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


// DeleteRemoteBranch removes the current branch from the remote. Used to
// clean up after a PR has been merged externally.
func (m *Repo) DeleteRemoteBranch() {
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

	// The following fields are filled by Repo.MergeWithRetry before delegating
	// to the package-level MergeWithRetry function. They enable callers to compose
	// the retry pipeline without a Repo receiver.

	// ResolveConflict is called to rebase and force-push when a merge conflict
	// is detected. Defaults to Repo.ResolveConflict when nil.
	ResolveConflict func(ctx context.Context) error

	// AwaitCI polls CI status after a fix agent pushes. pushedAt filters
	// out stale check results that started before the push.
	AwaitCI func(ctx context.Context, prNumber int, repoURL string, pushedAt time.Time) ([]CICheckResult, CIStatus, error)

	// Logger receives progress and warning messages. Logging is skipped when nil.
	Logger Log

	// RemoteURL is the repository remote URL, used for AwaitCI after a fix.
	RemoteURL string
}

// ResolveConflict rebases onto the default branch and force-pushes to
// resolve PR merge conflicts before the next merge attempt. Returns an
// UnresolvedConflictError if the rebase couldn't resolve the divergence,
// signaling that retrying will not help.
func (m *Repo) ResolveConflict(ctx context.Context) error {
	baseBranch := m.resolveBaseBranch()
	m.logger.Emit(logging.Opts{Domain: logging.Git, Branch: baseBranch}, "Rebasing onto %s to resolve merge conflicts...", baseBranch)
	if err := m.EnsureUpToDate(ctx); err != nil {
		return fmt.Errorf("conflict resolution rebase failed: %w", err)
	}

	// Check if the rebase actually resolved the divergence. If origin/base
	// is still not an ancestor of HEAD, auto-resolve failed and force-pushing
	// would just repeat the same conflict on GitHub.
	if m.refExists(m.WorkDir, "origin/"+baseBranch) {
		if m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", "origin/"+baseBranch, "HEAD") != nil {
			m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Rebase did not resolve conflicts with origin/%s — skipping force-push", baseBranch)
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
// It is a package function — callers compose it without a Repo receiver.
// Repo.MergeWithRetry delegates here after filling in infrastructure callbacks.
func MergeWithRetry(ctx context.Context, mergeFunc func(context.Context) (bool, error), opts MergeRetryOpts) (bool, error) {
	sleepFn := opts.SleepFunc
	if sleepFn == nil {
		sleepFn = time.Sleep
	}

	infraRetries := 0
	ciFixApplied := false

	for attempt := 0; attempt < MaxMergeAttempts; attempt++ {
		merged, err := mergeFunc(ctx)
		if err == nil {
			return merged, nil
		}

		if attempt > 0 && opts.Logger != nil {
			opts.Logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Merge attempt %d failed: %v", attempt+1, err)
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
			pushedAt := time.Now()
			result := opts.OnCIFailure(ciErr)
			switch result {
			case CIFixApplied:
				ciFixApplied = true
				// Fix was applied and force-pushed. Wait for fresh CI
				// checks that started after the push.
				if opts.AwaitCI != nil {
					repoURL := opts.RemoteURL
					_, ciStatus, waitErr := opts.AwaitCI(ctx, ciErr.PRNumber, repoURL, pushedAt)
					if waitErr != nil && opts.Logger != nil {
						opts.Logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn}, "CI polling after fix: %v", waitErr)
					}
					if ciStatus == CIFailed && opts.Logger != nil {
						opts.Logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn}, "CI still failing after fix — will retry")
					}
				}
				continue
			case CIFixNoCommits:
				// Infrastructure failure — fix agent found no code issue.
				// Retry with backoff instead of giving up.
				if infraRetries >= MaxInfraRetries {
					if opts.Logger != nil {
						opts.Logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn}, "Infrastructure retries exhausted (%d) — giving up", MaxInfraRetries)
					}
					return false, err
				}
				delay := infraBackoff(infraRetries)
				if opts.Logger != nil {
					opts.Logger.Emit(logging.Opts{Domain: logging.CI}, "CI infrastructure failure — retrying in %s (%d/%d)", delay, infraRetries+1, MaxInfraRetries)
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
	if ciFixApplied {
		return false, &CIFixExhaustedError{Attempts: MaxMergeAttempts}
	}
	return false, fmt.Errorf("merge failed after %d attempts", MaxMergeAttempts)
}

// Repo.MergeWithRetry delegates to the package-level MergeWithRetry function
// after filling in infrastructure callbacks from Repo fields.
func (m *Repo) MergeWithRetry(ctx context.Context, opts MergeRetryOpts) (bool, error) {
	if opts.ResolveConflict == nil {
		opts.ResolveConflict = m.ResolveConflict
	}
	if opts.AwaitCI == nil {
		opts.AwaitCI = m.AwaitCI
	}
	if opts.Logger == nil {
		opts.Logger = m.logger
	}
	if opts.RemoteURL == "" {
		opts.RemoteURL = m.RemoteURL()
	}
	return MergeWithRetry(ctx, m.AutoMergeCurrentBranch, opts)
}

// FlushUnpushedWork pushes any unpushed commits and optionally merges
// the PR. This is the safety net called before exiting or entering wait mode.
func (m *Repo) FlushUnpushedWork(ctx context.Context, taskID, taskDesc string, autoMerge bool) (merged bool, err error) {
	if m.WorktreeBranch == WipBranchName() {
		return false, nil
	}
	// When a PR is already known (set during finalizePR), just push —
	// don't try to create/find the PR again. The PR already exists.
	if m.KnownPRNumber != 0 {
		if pushErr := m.Push(ctx); pushErr != nil {
			return false, pushErr
		}
	} else {
		remoteRef := "origin/" + m.WorktreeBranch
		if m.refExists(m.WorkDir, remoteRef) {
			// origin/<branch> exists — bail if HEAD is already there to avoid
			// a spurious API call that would produce a "Problems parsing JSON" 400.
			count := strings.TrimSpace(m.gitOutput(m.WorkDir, "rev-list", remoteRef+"..HEAD", "--count"))
			if count == "0" {
				return false, nil
			}
		} else {
			// origin/<branch> absent (e.g. deleted after squash-merge). Bail if
			// HEAD has no commits ahead of origin/main — nothing left to flush.
			defaultBranch := m.detectDefaultBranch()
			mainRef := "origin/" + defaultBranch
			count := strings.TrimSpace(m.gitOutput(m.WorkDir, "rev-list", mainRef+"..HEAD", "--count"))
			if count == "0" {
				return false, nil
			}
		}
		if _, pushErr := m.PushAndCreatePR(ctx, taskID, taskDesc, ""); pushErr != nil {
			return false, pushErr
		}
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

// PostMergeUpdateMain fetches origin/main and rebases the worktree onto it
// after a PR merge, then moves the worktree to the placeholder branch and
// deletes the stale task branch. It does not modify the ProjectDir checkout.
func (m *Repo) PostMergeUpdateMain() {
	defaultBranch := m.detectDefaultBranch()
	m.gitCmd(m.ProjectDir, "fetch", "origin", defaultBranch)

	// Sync worktree with updated main. If rebase conflicts, reset —
	// the merged work is on main and stale stack commits are expendable.
	if m.gitCmdErr(m.WorkDir, "rebase", "origin/"+defaultBranch) != nil {
		m.gitCmd(m.WorkDir, "rebase", "--abort")
		m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Post-merge rebase failed — resetting worktree to origin/%s", defaultBranch)
		m.gitCmd(m.WorkDir, "reset", "--hard", "origin/"+defaultBranch)
	}

	// Move worktree to placeholder branch and delete the stale task branch.
	m.PrepareForNextTask("")
}
