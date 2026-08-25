package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/component"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/retry"
	"github.com/brokenalarms/ralph/internal/verify"
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

// Merge-path outcomes cross the git package boundary as errors.Is-able
// sentinels: no caller distinguishes them today, but each is wrapped with %w
// plus its interpolated context so a caller can start branching on it without
// parsing the message (docs/specs/architecture.md Principle 7).
var (
	// ErrInvalidBaseBranch means the resolved base is neither the configured
	// base branch nor the active stack parent, so no PR may be created or merged.
	ErrInvalidBaseBranch = errors.New("base branch guard")

	// ErrMergedSHANotAncestor means a squash-merge landed on a lineage that is
	// not reachable from the base branch.
	ErrMergedSHANotAncestor = errors.New("post-merge ancestor check FAILED")

	// ErrGHUnavailable means the gh CLI is not installed, so no GitHub
	// operation that needs it can run.
	ErrGHUnavailable = errors.New("gh CLI not found")

	// ErrAutoMergeFailed means GitHub refused the merge for a reason other than
	// conflicts, branch protection, or failing CI.
	ErrAutoMergeFailed = errors.New("auto-merge failed")

	// ErrMergeAttemptsExhausted means every merge attempt in MergeWithRetry was
	// used without the PR merging.
	ErrMergeAttemptsExhausted = errors.New("merge failed")
)

// CompileCheckError is returned when the pre-push compile check fails. Typed
// so a caller can tell a broken tree apart from a git or network failure
// without matching on the compiler output.
type CompileCheckError struct {
	Reason  string
	Details string
}

func (e *CompileCheckError) Error() string {
	return e.Reason + "\n" + compileCheckSummary(e.Details)
}

// AutoMergeBlockedError is returned when branch protection blocks the merge
// and there is no CI waiter configured to wait the block out.
type AutoMergeBlockedError struct {
	PRNumber int
	Message  string
}

func (e *AutoMergeBlockedError) Error() string {
	return fmt.Sprintf("auto-merge blocked for PR #%d: %s", e.PRNumber, e.Message)
}

// MergeRetryFailedError is returned when the merge retried after CI passed and
// GitHub still refused it — the PR is mergeable in principle but something
// changed underneath.
type MergeRetryFailedError struct {
	PRNumber int
	Message  string
}

func (e *MergeRetryFailedError) Error() string {
	return fmt.Sprintf("merge retry failed for PR #%d after CI passed: %s", e.PRNumber, e.Message)
}

// resolveBaseBranch returns PrevBranch if set, otherwise the default branch.
// Single source of truth for "what is this branch based on."
func (r *repo) resolveBaseBranch() string {
	if r.prevBranch != "" {
		return r.prevBranch
	}
	return r.baseBranch
}

// assertValidBase returns an error when base is not the configured BaseBranch
// and is not the active prevBranch stack parent. Prevents PRs from being
// created or merged against an unexpected branch (the sharpe 2026-05-28 failure
// scenario where a stale origin/HEAD pointer produced base=wrong-branch).
func (r *repo) assertValidBase(base string) error {
	if base == r.baseBranch {
		return nil
	}
	if r.prevBranch != "" && base == r.prevBranch {
		return nil
	}
	return fmt.Errorf("%w: resolved base %q is neither cfg.BaseBranch (%q) nor active stack parent %q — refusing to create/merge PR", ErrInvalidBaseBranch, base, r.baseBranch, r.prevBranch)
}

// assertMergedAncestor verifies that mergedSHA is an ancestor of
// origin/<baseBranch> after a successful squash-merge. A merge that lands on a
// dead lineage (not on the real base) returns a non-nil error so the caller can
// fail the iteration loudly without closing the bead.
func (r *repo) assertMergedAncestor(mergedSHA string) error {
	if mergedSHA == "" {
		return nil
	}
	dir := r.projectDir
	_ = r.gitCmdErr(dir, "fetch", "origin", r.baseBranch)
	ref := "origin/" + r.baseBranch
	if !r.isAncestor(dir, mergedSHA, ref) {
		return fmt.Errorf("%w: merged SHA %s is NOT an ancestor of %s — commits may have landed in a dead lineage; bead left open for manual recovery", ErrMergedSHANotAncestor, mergedSHA, ref)
	}
	return nil
}

// Push squashes all commits into one and force-pushes the branch.
// Always uses --force-with-lease (safe — only forces if remote matches
// last fetch). Squash ensures stacked PRs cascade cleanly on merge.
func (r *repo) Push(ctx context.Context) error {
	if r.worktreeBranch == "" {
		return nil
	}

	// Each push re-evaluates whether the branch nets out to nothing.
	r.pushNetEmpty = false

	if r.compileCheckTimeout > 0 {
		result := verify.CompileCheck(ctx, r.compileCheckTimeout, r.workDir)
		if !result.Passed {
			r.logger.Emit(logging.Opts{Domain: logging.Build, Level: logging.Debug}, "%s", result.Details)
			return &CompileCheckError{Reason: result.Reason, Details: result.Details}
		}
		r.logger.Emit(logging.Opts{Domain: logging.Build}, "Pre-push compile check passed")
	}

	// The stack parent may have merged and been deleted between iteration
	// start and now. Re-query GitHub before squashing against a base that
	// may no longer exist — otherwise CreatePR will fail with base=invalid.
	r.validateStackParent(ctx)

	baseBranch := r.resolveBaseBranch()
	_ = r.gitCmdErr(r.workDir, "fetch", "origin", baseBranch)
	baseRef := "origin/" + baseBranch
	if !r.refExists(r.workDir, baseRef) {
		baseRef = baseBranch
	}

	// Squash only commits ahead of the parent branch tip. Using rev-parse
	// (not merge-base) preserves the ancestry link so each stacked PR is
	// exactly one commit ahead of its parent — GitHub merge is a clean no-op.
	baseSHA := r.gitOutput(r.workDir, "rev-parse", baseRef)
	if baseSHA != "" {
		// Verify parent tip is an ancestor of HEAD. If not, the branch
		// diverged from its parent (e.g. parent was squash-pushed since
		// this branch was created) and needs rebasing first.
		if !r.isAncestor(r.workDir, baseSHA, "HEAD") {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Branch diverged from %s — rebasing before push", baseBranch)
			if err := r.EnsureUpToDate(ctx); err != nil {
				r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Rebase before push failed: %v", err)
			}
			// Re-fetch after rebase since origin may have moved.
			_ = r.gitCmdErr(r.workDir, "fetch", "origin", baseBranch)
			baseSHA = r.gitOutput(r.workDir, "rev-parse", baseRef)
		}
		if baseSHA != "" && r.isAncestor(r.workDir, baseSHA, "HEAD") {
			headSHA := r.gitOutput(r.workDir, "rev-parse", "HEAD")
			if headSHA != baseSHA {
				commitMsg := r.gitOutput(r.workDir, "log", "-1", "--format=%s")
				if err := r.SquashToOneCommit(baseSHA, commitMsg); err != nil {
					if errors.Is(err, ErrNoNetChange) {
						// Not a failure: the branch's commits are intact, they
						// just add up to no change vs base. Push them as they
						// are; shipPR skips PR creation for the no-op.
						r.logger.Emit(logging.Opts{Domain: logging.Git}, "Squash skipped: branch %s has no net change vs %s — pushing commits unsquashed", r.worktreeBranch, baseBranch)
						r.pushNetEmpty = true
					} else {
						r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Squash: %v", err)
					}
				}
			}
		}
	}

	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Pushing %s...", r.worktreeBranch)
	// Try force-with-lease first (safe update of existing branch).
	// Fall back to regular push for new branches.
	if err := r.gitCmdErrCtx(ctx, r.workDir, "push", "--force-with-lease", "-u", "origin", r.worktreeBranch); err != nil {
		return r.gitCmdErrCtx(ctx, r.workDir, "push", "-u", "origin", r.worktreeBranch)
	}
	return nil
}

// compileCheckSummary condenses multi-line compiler output into a single
// line: the first line plus a total line count. The full output is logged
// separately by the caller (at debug level) — callers of Push log the
// returned error verbatim, so embedding the full output here would print it
// again on top of that debug emit.
func compileCheckSummary(details string) string {
	trimmed := strings.TrimRight(details, "\n")
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	return fmt.Sprintf("%s (%d lines total)", lines[0], len(lines))
}

// reopenClosedPR finds a closed (not merged) PR for the given branch and
// reopens it. Returns the PR number on success or 0 if no closed PR was
// found or reopen failed.
func reopenClosedPR(ctx context.Context, gh gitHub, workDir, branch, nwo, repoURL, title, body string, logger Log) (int, error) {
	number, _, _, findErr := gh.FindPR(ctx, branch, repoURL)
	if findErr != nil || number == 0 {
		return 0, findErr
	}
	prDetail, stateErr := gh.GetPR(ctx, nwo, number)
	if stateErr != nil || prDetail == nil || prDetail.State != PRStateClosed {
		return 0, stateErr
	}

	prLink := logging.PRLinkOpt(nwo, number)

	if err := gh.ReopenPR(ctx, number, repoURL); err != nil {
		logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "Failed to reopen: %v", err)
		return 0, err
	}
	logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "Reopened for %s", branch)

	if title != "" {
		if err := gh.EditPR(ctx, number, repoURL, title, body); err != nil {
			logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "Failed to update: %v", err)
		}
	}
	return number, nil
}

func (r *repo) reopenClosedPR(ctx context.Context, gh gitHub, repoURL, title, body string) (int, error) {
	nwo := NWOFromRemote(repoURL)
	return reopenClosedPR(ctx, gh, r.workDir, r.worktreeBranch, nwo, repoURL, title, body, r.logger)
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
func CreatePR(ctx context.Context, gh gitHub, workDir, branch, remoteURL string, opts EnsurePROpts) (int, error) {
	if remoteURL == "" {
		return 0, nil
	}
	if !gh.Available() {
		return 0, fmt.Errorf("%w — cannot create PR", ErrGHUnavailable)
	}

	nwo := NWOFromRemote(remoteURL)
	title := prTitle(opts.TaskID, opts.TaskDesc, branch)
	body := formatPRBody(opts.Description, opts.Acceptance, opts.Summary)

	// Existing PR — update and return.
	prNumber, _ := gh.FindOpenPR(ctx, branch, remoteURL)
	if prNumber != 0 {
		prLink := logging.PRLinkOpt(nwo, prNumber)
		if opts.TaskID != "" {
			if err := gh.EditPR(ctx, prNumber, remoteURL, title, body); err != nil {
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
		repo:  remoteURL,
		Dir:   workDir,
	}
	newPR, createErr := gh.CreatePR(ctx, createOpts)
	if createErr != nil {
		// PR creation failed. Check if the branch already has a PR in any state.
		// A merged PR means the push already delivered commits — return its number.
		// A closed PR can be reopened by the block below.
		if existingPR, _, _, findErr := gh.FindPR(ctx, branch, remoteURL); findErr == nil && existingPR != 0 {
			if prDetail, detailErr := gh.GetPR(ctx, nwo, existingPR); detailErr == nil && prDetail != nil && prDetail.State == PRStateMerged {
				prLink := logging.PRLinkOpt(nwo, existingPR)
				opts.Logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "Merged PR already exists for %s — commits landed", branch)
				return existingPR, nil
			}
		}

		// Creation may fail if a closed PR exists for this head:base.
		// Try to find and reopen it instead.
		if prNumber, reopenErr := reopenClosedPR(ctx, gh, workDir, branch, nwo, remoteURL, title, body, opts.Logger); reopenErr == nil && prNumber != 0 {
			return prNumber, nil
		}

		// Reopen failed (e.g. branch diverged from old PR history).
		// The old PR is dead — create a fresh PR via the REST API,
		// bypassing gh pr create's client-side checks.
		if nwo != "" {
			if apiPR, apiErr := gh.CreatePRViaAPI(ctx, nwo, createOpts); apiErr == nil && apiPR != 0 {
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

// repo.CreatePR delegates to the package function. The summary string is
// passed through formatPRBody as the Summary section; callers needing
// description / acceptance criteria use the package function CreatePR
// directly.
func (r *repo) CreatePR(ctx context.Context, taskID, taskDesc, summary string) (int, error) {
	baseBranch := r.resolveBaseBranch()
	if err := r.assertValidBase(baseBranch); err != nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "%v", err)
		return 0, err
	}
	return CreatePR(ctx, r.github, r.workDir, r.worktreeBranch, r.RemoteURL(), EnsurePROpts{
		TaskID:     taskID,
		TaskDesc:   taskDesc,
		Summary:    summary,
		BaseBranch: baseBranch,
		Logger:     r.logger,
	})
}

// ShipOpts configures the Ship pipeline. All fields are data — no func or
// interface fields. repo.Ship fills infrastructure from its own fields.
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

	// InfrastructureFailure is true when CIFailure is true and the failure is
	// due to infrastructure (billing, runner allocation — zero job steps executed)
	// rather than actual test failures. The loop closes the bead and leaves the
	// PR open for merge when CI infrastructure recovers.
	InfrastructureFailure bool

	// StepTimeoutFailure is true when CIFailure is true and every failed job
	// carries a step-timeout annotation — a setup step that exceeded its
	// timeout-minutes rather than a test that failed. No code change can fix
	// it, so the loop re-triggers CI instead of spawning a fix agent, until
	// its re-trigger budget runs out.
	StepTimeoutFailure bool

	// ReviewFixNeeded is true when a reviewer returned actionable comments.
	// The loop should run tryFixReviewComments and retry Ship.
	ReviewFixNeeded bool

	// PendingReview is the review that needs to be addressed.
	PendingReview *AutoReview

	// PendingReviewer is the bot username whose review needs addressing.
	PendingReviewer string

	// LocalTestDetail carries the pre-merge local test failure when CIFailure
	// is true because the locally-detected suite failed rather than because
	// GitHub checks did. Mutually exclusive with CIFailureDetail — there are
	// no GitHub checks to fix in this case.
	LocalTestDetail *LocalTestFailureError

	// ConflictDetail is set when MergeWithRetry encountered an unresolvable
	// merge conflict. The loop should call tryFixConflict and retry Ship.
	ConflictDetail *UnresolvedConflictError

	// NoNetChange is true when the pushed branch carries no net change
	// against the base branch — either no commits ahead at all, or commits
	// whose combined diff is empty. No PR is created; the caller must
	// classify the bead as a net no-op rather than a PR-creation failure.
	NoNetChange bool

	// PushedBranch is the worktree branch name set after a successful push
	// in shipPR. An empty string means no push occurred (nothing to ship, or
	// push failed). When this is non-empty but PRNumber == 0, the push
	// landed commits on the remote but CreatePR failed — the caller must
	// not close the bead (that would orphan the pushed branch).
	PushedBranch string
}

// shipHooks are the repo operations shipPR needs to prepare and push a
// branch before creating its PR. *repo satisfies shipHooks via its own
// methods; tests supply a local stub. This is the constructor-DI seam for
// shipPR in place of individually injected callback fields.
type shipHooks interface {
	Push(ctx context.Context) error
	HasUncommittedChanges() bool
	CommitAll(message string)
	BranchHasUnmergedWork(branch string) bool
	PushedBranchNetEmpty() bool
}

// shipInfra holds the infrastructure shipPR needs. Separated from ShipOpts
// to keep ShipOpts data-only.
type shipInfra struct {
	hooks  shipHooks
	logger Log
}

// shipPR is the single "get work into a PR" pipeline: auto-commit any
// uncommitted changes, push (squash + rebase + force-push), and create
// or update a PR. Returns the PR number and URL.
func shipPR(ctx context.Context, gh gitHub, workDir, branch, remoteURL string, opts ShipOpts, infra shipInfra) (ShipResult, error) {
	if infra.hooks.HasUncommittedChanges() {
		msg := "auto-commit agent changes"
		if opts.TaskID != "" {
			msg = fmt.Sprintf("[%s] %s", opts.TaskID, msg)
		}
		infra.hooks.CommitAll(msg)
	}

	pushedBranch := ""
	if err := infra.hooks.Push(ctx); err != nil {
		if ctx.Err() != nil {
			return ShipResult{}, ctx.Err()
		}
		return ShipResult{}, fmt.Errorf("push failed: %w", err)
	}
	// Push succeeded — commits are now on the remote branch. Downstream
	// callers use this signal to avoid closing a bead when CreatePR fails
	// (which would orphan the pushed branch).
	pushedBranch = branch

	if !infra.hooks.BranchHasUnmergedWork(branch) {
		if infra.logger != nil {
			infra.logger.Emit(logging.Opts{Domain: logging.Git}, "Ship: branch %s has no commits ahead of main — skipping PR creation", branch)
		}
		return ShipResult{PushedBranch: pushedBranch, NoNetChange: true}, nil
	}

	// Commits exist but add up to nothing (the agent's change was undone, or
	// main already carries it). There is no diff to review, so a PR would be
	// empty — report the no-op instead.
	if infra.hooks.PushedBranchNetEmpty() {
		if infra.logger != nil {
			infra.logger.Emit(logging.Opts{Domain: logging.Git}, "Ship: branch %s has commits but no net change vs main — skipping PR creation", branch)
		}
		return ShipResult{PushedBranch: pushedBranch, NoNetChange: true}, nil
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
		return ShipResult{PRNumber: prNumber, PushedBranch: pushedBranch}, err
	}

	// Look up the PR URL for the external ref.
	var prURL, prTitle string
	if prNumber != 0 {
		if _, t, u, findErr := gh.FindPR(ctx, branch, remoteURL); findErr == nil {
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
		PRNumber:     prNumber,
		PRURL:        prURL,
		PRTitle:      prTitle,
		PushedBranch: pushedBranch,
	}, nil
}

// repo.Ship runs the full push + PR + reviewer poll + merge pipeline.
// ShipOpts carries only data; infrastructure callbacks come from repo fields.
// When opts.PRNumber is non-zero, Ship skips push+PR and proceeds directly to
// reviewer polling and merge using that PR.
func (r *repo) Ship(ctx context.Context, opts ShipOpts) (ShipResult, error) {
	if opts.BaseBranch == "" {
		opts.BaseBranch = r.resolveBaseBranch()
	}

	var result ShipResult
	var err error

	if opts.PRNumber != 0 {
		// Caller identified the PR — check its state before proceeding.
		result = ShipResult{PRNumber: opts.PRNumber}
		prState, stateErr := r.GetPRState(ctx, opts.PRNumber)
		if stateErr != nil {
			return result, fmt.Errorf("get PR state: %w", stateErr)
		}
		switch prState {
		case PRStateMerged:
			result.AlreadyMerged = true
			result.Merged = true
			r.PostMergeUpdateMain()
			return result, nil
		case PRStateClosed:
			result.Closed = true
			return result, nil
		}
		// PRStateOpen — fall through to merge phase.
	} else {
		// Guard 1: assert base is cfg.BaseBranch or active stack parent before
		// any push or PR creation API call.
		if err := r.assertValidBase(opts.BaseBranch); err != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "%v", err)
			return ShipResult{}, err
		}
		infra := shipInfra{
			hooks:  r,
			logger: r.logger,
		}
		result, err = shipPR(ctx, r.github, r.workDir, r.worktreeBranch, r.RemoteURL(), opts, infra)
		if err != nil {
			return result, err
		}
	}

	if !opts.AutoMerge || result.PRNumber == 0 {
		return result, nil
	}

	gh := r.github
	nwo := NWOFromRemote(r.RemoteURL())
	prLink := logging.PRLinkOpt(nwo, result.PRNumber)

	var reviewHandled bool
	result, reviewHandled = r.pollReviewers(ctx, opts, result, prLink)
	if reviewHandled {
		return result, nil
	}

	// Check if stacked (PR targets non-default branch — merge skipped).
	prDetail, _ := gh.GetPR(ctx, nwo, result.PRNumber)
	if prBase, stacked := r.checkStacked(prDetail); stacked {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "targets %s (not %s) — stacked, closing bead", prBase, r.baseBranch)
		result.Stacked = true
		return result, nil
	}

	// Attempt merge — no callbacks. Returns typed errors (CIFailureError,
	// LocalTestFailureError, UnresolvedConflictError) with the data the loop
	// needs; the loop decides whether to spawn a fix agent and calls Ship
	// again to retry.
	r.SetKnownPRNumber(result.PRNumber)
	defer r.SetKnownPRNumber(0)

	merged, mergeErr := r.MergeWithRetry(ctx, MergeRetryOpts{
		Logger: r.logger,
	})
	if mergeErr != nil {
		return r.classifyMergeOutcome(ctx, result, mergeErr)
	}

	result.Merged = merged
	if merged {
		r.PostMergeUpdateMain()
	}
	return result, nil
}

// pollReviewers polls each configured reviewer bot for pending review
// comments. Returns handled=true when a reviewer flagged actionable
// comments — result is populated with the pending review and Ship should
// return it immediately.
func (r *repo) pollReviewers(ctx context.Context, opts ShipOpts, result ShipResult, prLink *logging.Link) (ShipResult, bool) {
	for _, reviewer := range opts.Reviewers {
		if opts.ReviewAddressed[reviewer.BotUsername] {
			continue
		}
		review, pollErr := r.PollReview(ctx, reviewer.BotUsername, result.PRNumber, reviewer.DefaultTimeout)
		if pollErr != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "%s review poll: %v", reviewer.BotUsername, pollErr)
			continue
		}
		if review != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "%s review received (%d comments)", reviewer.BotUsername, len(review.Comments))
			result.ReviewFixNeeded = true
			result.PendingReview = review
			result.PendingReviewer = reviewer.BotUsername
			return result, true
		}
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "No %s review pending — continuing to CI-gated merge (waits for CI, merges only if it passes)", reviewer.BotUsername)
	}
	return result, false
}

// checkStacked reports whether prDetail targets a base branch other than
// the repo's default branch, returning that base for the caller's log
// message. Shared by AutoMergeCurrentBranch (which retries once the base PR
// merges) and Ship (which closes the bead) — each attaches its own outcome
// to the same guard.
func (r *repo) checkStacked(prDetail *PRDetail) (prBase string, stacked bool) {
	if prDetail != nil {
		prBase = prDetail.BaseRef
	}
	return prBase, prBase != "" && prBase != r.baseBranch
}

// classifyMergeOutcome demuxes the error MergeWithRetry returns into the
// ShipResult fields the loop uses to decide whether to spawn a fix agent.
// Returns mergeErr unchanged when it is none of CIFailureError,
// LocalTestFailureError or UnresolvedConflictError, so Ship propagates it as
// a fatal error.
func (r *repo) classifyMergeOutcome(ctx context.Context, result ShipResult, mergeErr error) (ShipResult, error) {
	var ciFailure *CIFailureError
	if errors.As(mergeErr, &ciFailure) {
		result.CIFailure = true
		result.CIFailureDetail = ciFailure
		result.InfrastructureFailure = r.isInfrastructureFailure(ctx, ciFailure.PRNumber)
		if !result.InfrastructureFailure {
			result.StepTimeoutFailure = r.isStepTimeoutFailure(ctx, ciFailure.PRNumber)
		}
		return result, nil
	}
	// A failed local test run has no GitHub checks behind it, so the
	// infrastructure and step-timeout probes have nothing to inspect and are
	// deliberately skipped — the tree is red for a reason a fix agent, not a
	// CI re-trigger, would have to address.
	var localTestErr *LocalTestFailureError
	if errors.As(mergeErr, &localTestErr) {
		result.CIFailure = true
		result.LocalTestDetail = localTestErr
		return result, nil
	}
	var conflictErr *UnresolvedConflictError
	if errors.As(mergeErr, &conflictErr) {
		result.ConflictDetail = conflictErr
		return result, nil
	}
	return result, mergeErr
}

// PushAndCreatePR composes Push and CreatePR. Squashes, force-pushes, then
// ensures a PR exists. Returns the PR number. The summary argument flows
// into the Summary section of the formatted PR body.
func (r *repo) PushAndCreatePR(ctx context.Context, taskID, taskDesc, summary string) (int, error) {
	result, err := r.Ship(ctx, ShipOpts{TaskID: taskID, TaskTitle: taskDesc, Summary: summary})
	return result.PRNumber, err
}

func prTitle(taskID, taskDesc, fallback string) string {
	title := component.StripComponentPrefix(taskDesc)
	if taskID != "" {
		title = "[" + taskID + "] " + title
	}
	if len(title) > 70 {
		title = title[:67] + "..."
	}
	if title == "" {
		title = fallback
	}
	return neutralizeCISkipMarkers(title)
}

func (r *repo) prTitle(taskID, taskDesc string) string {
	return prTitle(taskID, taskDesc, r.worktreeBranch)
}

// runLocalTestsBeforeMerge runs the project test suite when no CI is configured,
// gating the merge on local test results. Returns nil when tests pass or when no
// test command is detected — absence of tests is treated as a pass, not a failure.
func (r *repo) runLocalTestsBeforeMerge(ctx context.Context, prNumber int) error {
	timeout := r.testTimeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	result := verify.RunTests(ctx, timeout, r.configVerify, r.workDir, r.projectDir)
	if result.ScriptMissing {
		r.logger.Emit(logging.Opts{Domain: logging.CI}, "No test command detected — treating as pass")
		return nil
	}
	if !result.Passed {
		return &LocalTestFailureError{PRNumber: prNumber, Reason: result.Reason, Details: result.Details}
	}
	r.logger.Emit(logging.Opts{Domain: logging.CI}, "Local tests passed — proceeding to merge")
	return nil
}

// AutoMergeCurrentBranch rebases onto main, pushes, waits for CI, and
// merges. If main moves between CI passing and the merge attempt, loops
// back to rebase+push again. Returns typed errors (CIFailureError,
// MergeConflictError) that callers can handle.
func (r *repo) AutoMergeCurrentBranch(ctx context.Context) (bool, error) {
	if r.worktreeBranch == "" {
		return false, nil
	}

	gh := r.github
	if !gh.Available() {
		return false, fmt.Errorf("%w — cannot auto-merge", ErrGHUnavailable)
	}

	repoURL := r.gitOutput(r.workDir, "remote", "get-url", "origin")
	if repoURL == "" {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "No remote URL — skipping auto-merge")
		return false, nil
	}

	prNumber, alreadyMerged, err := r.resolvePRForMerge(ctx, gh, repoURL)
	if alreadyMerged {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if prNumber == 0 {
		return false, nil
	}

	nwo := NWOFromRemote(repoURL)
	prLink := logging.PRLinkOpt(nwo, prNumber)
	prDetail, _ := gh.GetPR(ctx, nwo, prNumber)

	if prBase, stacked := r.checkStacked(prDetail); stacked {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "targets %s (not %s) — waiting for base PRs to merge first", prBase, r.baseBranch)
		return false, ErrStackedPRWaiting
	}

	r.logger.Emit(logging.Opts{Domain: logging.Git, Branch: r.baseBranch, Link: prLink}, "Waiting for CI — will merge only if it passes")

	// Fast path: when local HEAD already matches the PR head SHA and CI is already
	// passing, skip the rebase+push cycle — no new tree to test, no push needed.
	// This avoids the no-op push → stale pushedAt filter → infinite poll cycle.
	if prDetail != nil && prDetail.HeadSHA != "" && prDetail.HeadSHA == r.HeadRev() {
		fastChecks, fastStatus, _ := r.AwaitCI(ctx, prNumber, repoURL, time.Time{})
		if fastStatus == CIPassed {
			if len(fastChecks) == 0 {
				if localErr := r.runLocalTestsBeforeMerge(ctx, prNumber); localErr != nil {
					return false, localErr
				}
			}
			r.logger.Emit(logging.Opts{Domain: logging.CI, Link: prLink}, "CI already passing on %s — merging", prDetail.HeadSHA)
			return r.executeMerge(ctx, prNumber, repoURL, false)
		}
	}

	awaitPushedAt := r.rebasePushAndComputeAwaitWindow(ctx, prLink)

	return r.gateOnCI(ctx, prNumber, repoURL, prLink, awaitPushedAt)
}

// resolvePRForMerge resolves the PR number to merge for the current
// worktree branch: the known PR, an open PR lookup, or a closed PR that
// gets reopened. alreadyMerged is true when the PR is already merged —
// the caller reports success without further work. A zero prNumber with a
// nil error means no PR exists for the branch — the caller skips
// auto-merge silently (already logged here).
func (r *repo) resolvePRForMerge(ctx context.Context, gh gitHub, repoURL string) (prNumber int, alreadyMerged bool, err error) {
	if r.knownPRNumber != 0 {
		return r.knownPRNumber, false, nil
	}

	prNumber, err = gh.FindOpenPR(ctx, r.worktreeBranch, repoURL)
	if err == nil && prNumber != 0 {
		return prNumber, false, nil
	}

	prNumber, err = r.resolveClosedPR(ctx, gh, repoURL)
	if errors.Is(err, ErrPRAlreadyMerged) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	if prNumber == 0 {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "No PR found for %s — skipping auto-merge", r.worktreeBranch)
	}
	return prNumber, false, nil
}

// rebasePushAndComputeAwaitWindow rebases onto the latest base branch and
// force-pushes so CI runs on the final tree, then computes the pushedAt
// window AwaitCI should filter checks against. Returns a zero time when
// the push succeeded but was a no-op (SHA unchanged before/after) — no new
// CI run was triggered, so pre-existing checks must not be discarded by
// the pushedAt filter.
func (r *repo) rebasePushAndComputeAwaitWindow(ctx context.Context, prLink *logging.Link) time.Time {
	if err := r.EnsureUpToDate(ctx); err != nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Pre-merge rebase failed: %v", err)
	}
	headBefore := r.HeadRev()
	pushedAt := time.Now()
	pushErr := r.Push(ctx)
	if pushErr != nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Pre-merge push failed: %v", pushErr)
	}
	if pushErr == nil && headBefore != "" && r.HeadRev() == headBefore {
		r.logger.Emit(logging.Opts{Domain: logging.CI, Link: prLink}, "Push was no-op (SHA unchanged) — evaluating existing checks")
		return time.Time{}
	}
	return pushedAt
}

// gateOnCI awaits CI on prNumber and merges once it passes. The three
// CI-infrastructure-failure escape hatches (pre-wait, post-timeout,
// post-failure) all funnel into mergeAsInfrastructureFailure, so the
// admin-override merge call exists in exactly one place.
func (r *repo) gateOnCI(ctx context.Context, prNumber int, repoURL string, prLink *logging.Link, awaitPushedAt time.Time) (bool, error) {
	if r.isInfrastructureFailure(ctx, prNumber) {
		return r.mergeAsInfrastructureFailure(ctx, prNumber, repoURL, prLink, "CI infrastructure failure detected on PR #%d (zero job steps) — skipping CI wait and proceeding to merge")
	}

	checks, status, ciErr := r.AwaitCI(ctx, prNumber, repoURL, awaitPushedAt)
	if ciErr != nil {
		if r.isInfrastructureFailure(ctx, prNumber) {
			return r.mergeAsInfrastructureFailure(ctx, prNumber, repoURL, prLink, "CI timed out on PR #%d and job steps are zero — infrastructure failure, proceeding to merge")
		}
		r.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn, Link: prLink}, "CI did not complete within timeout — leaving PR open")
		return false, ciErr
	}
	if status == CIFailed {
		if r.isInfrastructureFailure(ctx, prNumber) {
			return r.mergeAsInfrastructureFailure(ctx, prNumber, repoURL, prLink, "CI failure on PR #%d is infrastructure-only (zero job steps) — proceeding to merge")
		}
		return false, &CIFailureError{PRNumber: prNumber, Failures: failedChecks(checks)}
	}
	if status == CIPassed {
		if len(checks) == 0 {
			if localErr := r.runLocalTestsBeforeMerge(ctx, prNumber); localErr != nil {
				return false, localErr
			}
		}
		r.logger.Emit(logging.Opts{Domain: logging.CI, Link: prLink}, "CI passed — merging")
	}

	if r.branchNeedsUpdate() {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Main moved while CI was running — will rebase and retry")
		return false, &MergeConflictError{PRNumber: prNumber}
	}

	return r.executeMerge(ctx, prNumber, repoURL, false)
}

// mergeAsInfrastructureFailure logs msg (formatted with prNumber) and
// merges with the admin-override bypass — the single admin-override merge
// path shared by every CI-infrastructure-failure escape hatch in gateOnCI.
func (r *repo) mergeAsInfrastructureFailure(ctx context.Context, prNumber int, repoURL string, prLink *logging.Link, msg string) (bool, error) {
	r.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn, Link: prLink}, msg, prNumber)
	return r.executeMerge(ctx, prNumber, repoURL, true)
}

// ErrPRAlreadyMerged is returned when the PR for the branch is already merged.
// Callers should skip push and close the bead.
var ErrPRAlreadyMerged = errors.New("PR already merged")

// resolveClosedPR handles the case where no open PR exists for the branch.
// It checks whether a PR exists in another state (merged or closed). If
// merged, returns ErrPRAlreadyMerged. If closed, reopens and returns the
// PR number so the caller can proceed with the normal merge flow.
func (r *repo) resolveClosedPR(ctx context.Context, gh gitHub, repoURL string) (int, error) {
	number, _, _, findErr := gh.FindPR(ctx, r.worktreeBranch, repoURL)
	if findErr != nil || number == 0 {
		return 0, nil
	}

	nwo := NWOFromRemote(repoURL)
	prDetail, stateErr := gh.GetPR(ctx, nwo, number)
	if stateErr != nil || prDetail == nil {
		return 0, nil
	}

	prLink := logging.PRLinkOpt(nwo, number)

	switch prDetail.State {
	case PRStateMerged:
		r.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "already merged — nothing to do")
		return 0, ErrPRAlreadyMerged
	case PRStateClosed:
		r.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "is closed — reopening")
		if err := gh.ReopenPR(ctx, number, repoURL); err != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "Failed to reopen: %v — creating new PR", err)
			baseBranch := r.resolveBaseBranch()
			opts := CreatePROpts{
				Head: r.worktreeBranch,
				Base: baseBranch,
				repo: repoURL,
				Dir:  r.workDir,
			}
			if apiPR, apiErr := gh.CreatePRViaAPI(ctx, nwo, opts); apiErr == nil && apiPR != 0 {
				r.logger.Emit(logging.Opts{Domain: logging.Git, Link: logging.PRLinkOpt(nwo, apiPR)}, "Created for %s (via API fallback)", r.worktreeBranch)
				return apiPR, nil
			}
			return 0, nil
		}
		r.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink}, "reopened")
		return number, nil
	default:
		return 0, nil
	}
}

// branchNeedsUpdate checks if the base branch has moved ahead of HEAD since
// the last push. Returns true when origin/<base> is not an ancestor of HEAD,
// meaning main moved while CI was running and the branch must be rebased
// before merging. Uses a local ancestry check to avoid creating merge commits.
func (r *repo) branchNeedsUpdate() bool {
	baseBranch := r.resolveBaseBranch()
	_ = r.gitCmdErr(r.workDir, "fetch", "origin", baseBranch)
	if !r.refExists(r.workDir, "origin/"+baseBranch) {
		return false
	}
	return !r.isAncestor(r.workDir, "origin/"+baseBranch, "HEAD")
}

// ExecuteMergeOpts holds all parameters for the executeMerge package function.
type ExecuteMergeOpts struct {
	PRNumber       int
	RepoURL        string
	WorktreeBranch string
	WorkDir        string
	DefaultBranch  string
	MergeOpts      MergeOpts
	// CI polls CI check status for the PR. Required for the Blocked path.
	CI ciPoller
}

// ciPoller polls CI check status for a PR. *repo satisfies it via AwaitCI;
// package-level merge functions accept it so they compose without a repo
// receiver. This is the constructor-DI seam in place of an injected
// AwaitCI callback field.
type ciPoller interface {
	AwaitCI(ctx context.Context, prNumber int, repoURL string, pushedAt time.Time) ([]CICheckResult, CIStatus, error)
}

// executeMerge attempts the squash-merge and handles CI-gated retries.
// Returns (mergedSHA, merged, err). mergedSHA is the squash-merge commit SHA
// from the GitHub API, populated when merged=true; callers use it for the
// post-merge ancestor check (Guard 2).
// It is a package function — callers compose it without a repo receiver.
// repo.executeMerge delegates here.
func executeMerge(ctx context.Context, gh gitHub, opts ExecuteMergeOpts, logger Log) (string, bool, error) {
	nwo := NWOFromRemote(opts.RepoURL)
	prLink := logging.PRLinkOpt(nwo, opts.PRNumber)
	mergeOpts := opts.MergeOpts

	if _, prTitle, _, titleErr := gh.FindPR(ctx, opts.WorktreeBranch, opts.RepoURL); titleErr == nil && prTitle != "" {
		mergeOpts.Subject = fmt.Sprintf("%s (#%d)", prTitle, opts.PRNumber)
	}

	result := gh.MergePR(ctx, opts.PRNumber, opts.RepoURL, mergeOpts)
	if result.Merged {
		merged, err := postMergeLog(nwo, opts.PRNumber, opts.DefaultBranch, logger)
		return result.MergedSHA, merged, err
	}

	if result.Conflict {
		if logger != nil {
			logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "has merge conflicts — attempting rebase")
		}
		return "", false, &MergeConflictError{PRNumber: opts.PRNumber}
	}

	if result.Blocked {
		if logger != nil {
			logger.Emit(logging.Opts{Domain: logging.CI, Link: prLink}, "blocked by branch protection: %s — waiting for CI...", result.Message)
		}
		if opts.CI == nil {
			return "", false, &AutoMergeBlockedError{PRNumber: opts.PRNumber, Message: result.Message}
		}
		checks, status, waitErr := opts.CI.AwaitCI(ctx, opts.PRNumber, opts.RepoURL, time.Time{})
		if waitErr != nil {
			return "", false, fmt.Errorf("CI polling failed for PR #%d: %w", opts.PRNumber, waitErr)
		}
		if status == CIFailed {
			return "", false, &CIFailureError{PRNumber: opts.PRNumber, Failures: failedChecks(checks)}
		}
		if status == CIPassed {
			if logger != nil {
				logger.Emit(logging.Opts{Domain: logging.CI, Link: prLink}, "CI passed — retrying merge")
			}
			retry := gh.MergePR(ctx, opts.PRNumber, opts.RepoURL, mergeOpts)
			if retry.Merged {
				merged, err := postMergeLog(nwo, opts.PRNumber, opts.DefaultBranch, logger)
				return retry.MergedSHA, merged, err
			}
			if logger != nil {
				logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "Merge retry failed: %s", retry.Message)
			}
			return "", false, &MergeRetryFailedError{PRNumber: opts.PRNumber, Message: retry.Message}
		}
	}

	if logger != nil {
		logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink}, "Auto-merge failed: %s", result.Message)
	}
	return "", false, fmt.Errorf("%w for PR #%d: %s", ErrAutoMergeFailed, opts.PRNumber, result.Message)
}

// postMergeLog logs the merge completion.
func postMergeLog(nwo string, prNumber int, defaultBranch string, logger Log) (bool, error) {
	if logger != nil {
		logger.Emit(logging.Opts{Domain: logging.Git, Branch: defaultBranch, Link: logging.PRLinkOpt(nwo, prNumber)}, "merged")
	}
	return true, nil
}

// repo.executeMerge delegates to the package-level executeMerge function and
// runs the post-merge ancestor check (Guard 2): if the merged SHA is not an
// ancestor of origin/<baseBranch>, the iteration fails loudly without closing
// the bead. When admin is true and adminMergeOnCIInfraFailure is enabled,
// sets Admin:true on MergeOpts to bypass branch protection — used at
// isInfrastructureFailure call sites.
func (r *repo) executeMerge(ctx context.Context, prNumber int, repoURL string, admin bool) (bool, error) {
	opts := r.mergeOpts()
	if admin && r.adminMergeOnCIInfraFailure {
		opts.Admin = true
	}
	mergedSHA, merged, err := executeMerge(ctx, r.github, ExecuteMergeOpts{
		PRNumber:       prNumber,
		RepoURL:        repoURL,
		WorktreeBranch: r.worktreeBranch,
		WorkDir:        r.workDir,
		DefaultBranch:  r.baseBranch,
		MergeOpts:      opts,
		CI:             r,
	}, r.logger)
	if !merged || err != nil {
		return merged, err
	}
	if ancestorErr := r.assertMergedAncestor(mergedSHA); ancestorErr != nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "%v", ancestorErr)
		return false, ancestorErr
	}
	return merged, nil
}

// GetCIFailureLog retrieves the failed CI run's log output for the given PR.
func (r *repo) GetCIFailureLog(ctx context.Context, prNumber int) string {
	return r.github.GetRunLog(ctx, prNumber, r.workDir)
}

// mergeOpts returns the merge options for the current repo configuration.
func (r *repo) mergeOpts() MergeOpts {
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
func (r *repo) DeleteRemoteBranch() {
	if r.worktreeBranch == "" {
		return
	}
	_ = r.gitCmdErr(r.workDir, "push", "origin", "--delete", r.worktreeBranch)
}

// MaxMergeAttempts is the total number of merge attempts including retries
// after conflict resolution.
const MaxMergeAttempts = 4

// conflictResolver rebases and force-pushes to resolve a PR merge conflict.
// *repo satisfies it via ResolveConflict; the package-level MergeWithRetry
// function accepts it so it composes without a repo receiver. This is the
// constructor-DI seam in place of an injected ResolveConflict callback field.
type conflictResolver interface {
	ResolveConflict(ctx context.Context) error
}

// MergeRetryOpts configures the merge-with-retry pipeline.
type MergeRetryOpts struct {
	// Resolver rebases and force-pushes to resolve a merge conflict. Filled
	// by repo.MergeWithRetry with the repo itself (via its ResolveConflict
	// method) when nil, so callers can compose the retry pipeline without a
	// repo receiver.
	Resolver conflictResolver

	// Logger receives progress and warning messages. Logging is skipped when nil.
	Logger Log
}

// ResolveConflict rebases onto the default branch and force-pushes to
// resolve PR merge conflicts before the next merge attempt. Returns an
// UnresolvedConflictError if the rebase couldn't resolve the divergence,
// signaling that retrying will not help.
func (r *repo) ResolveConflict(ctx context.Context) error {
	baseBranch := r.resolveBaseBranch()
	r.logger.Emit(logging.Opts{Domain: logging.Git, Branch: baseBranch}, "Rebasing onto %s to resolve merge conflicts...", baseBranch)
	if err := r.EnsureUpToDate(ctx); err != nil {
		// A local rebase abort in the PR-merge pipeline IS an unresolvable
		// merge conflict — surface it with PR semantics so the caller treats
		// it as an UnresolvedConflictError rather than retrying.
		var localConflict *LocalRebaseConflictError
		if errors.As(err, &localConflict) {
			return &UnresolvedConflictError{}
		}
		// A transient transport failure during conflict resolution is not itself
		// a conflict — continue with stale refs so the caller can decide.
		var transportErr *TransportError
		if errors.As(err, &transportErr) {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Conflict resolution fetch failed (transient) — continuing with stale base: %v", err)
		} else {
			return fmt.Errorf("conflict resolution rebase failed: %w", err)
		}
	}

	// Check if the rebase actually resolved the divergence. If origin/base
	// is still not an ancestor of HEAD, auto-resolve failed and force-pushing
	// would just repeat the same conflict on GitHub.
	if r.refExists(r.workDir, "origin/"+baseBranch) {
		if !r.isAncestor(r.workDir, "origin/"+baseBranch, "HEAD") {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Rebase did not resolve conflicts with origin/%s — skipping force-push", baseBranch)
			return &UnresolvedConflictError{}
		}
	}

	return r.Push(ctx)
}

// MergeWithRetry is the single merge pipeline: try mergeFunc, and when it
// fails with a MergeConflictError, resolve via opts.Resolver and retry.
// Any other error (including CIFailureError) is returned immediately with
// the data the caller needs — the orchestrator (Loop.doShip) decides
// whether to spawn a fix agent and calls Ship again, sequencing the retry
// itself rather than this pipeline calling back into it.
//
// It is a package function — callers compose it without a repo receiver.
// repo.MergeWithRetry delegates here after filling in infrastructure from
// repo fields.
func MergeWithRetry(ctx context.Context, mergeFunc func(context.Context) (bool, error), opts MergeRetryOpts) (bool, error) {
	var merged bool
	calls := 0

	attempt := func() (bool, error) {
		m, err := mergeFunc(ctx)
		calls++
		if err == nil {
			merged = m
			return true, nil
		}

		if calls > 1 && opts.Logger != nil {
			opts.Logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Merge attempt %d failed: %v", calls, err)
		}

		var conflictErr *MergeConflictError
		if errors.As(err, &conflictErr) {
			if opts.Resolver == nil {
				return false, err
			}
			resolveErr := opts.Resolver.ResolveConflict(ctx)
			if resolveErr == nil {
				return false, nil
			}
			var unresolved *UnresolvedConflictError
			if errors.As(resolveErr, &unresolved) {
				unresolved.PRNumber = conflictErr.PRNumber
				return false, unresolved
			}
			return false, resolveErr
		}

		return false, err
	}

	// A resolved conflict is reported to Retry as (false, nil) so it keeps
	// retrying; every other outcome is fatal — classify never treats a
	// returned error as transient itself.
	err := retry.Retry(ctx, retry.BackoffOpts{Schedule: make([]time.Duration, MaxMergeAttempts-1)}, func(error) bool { return false }, attempt)
	if err != nil {
		if errors.Is(err, retry.ErrTimedOut) {
			return false, fmt.Errorf("%w after %d attempts", ErrMergeAttemptsExhausted, MaxMergeAttempts)
		}
		return false, err
	}
	return merged, nil
}

// repo.MergeWithRetry delegates to the package-level MergeWithRetry function
// after filling in infrastructure from repo fields.
func (r *repo) MergeWithRetry(ctx context.Context, opts MergeRetryOpts) (bool, error) {
	if opts.Resolver == nil {
		opts.Resolver = r
	}
	if opts.Logger == nil {
		opts.Logger = r.logger
	}
	return MergeWithRetry(ctx, r.AutoMergeCurrentBranch, opts)
}

// FlushUnpushedWork pushes any unpushed commits and optionally merges
// the PR. This is the safety net called before exiting or entering wait mode.
func (r *repo) FlushUnpushedWork(ctx context.Context, taskID, taskDesc string, autoMerge bool) (merged bool, err error) {
	if r.worktreeBranch == WipBranchName() {
		return false, nil
	}
	// When a PR is already known (set during finalizePR), just push —
	// don't try to create/find the PR again. The PR already exists.
	if r.knownPRNumber != 0 {
		if pushErr := r.Push(ctx); pushErr != nil {
			return false, pushErr
		}
	} else {
		remoteRef := "origin/" + r.worktreeBranch
		if r.refExists(r.workDir, remoteRef) {
			// origin/<branch> exists — bail if HEAD is already there to avoid
			// a spurious API call that would produce a "Problems parsing JSON" 400.
			if !r.hasCommitsAhead(remoteRef, "HEAD") {
				return false, nil
			}
		} else {
			// origin/<branch> absent (e.g. deleted after squash-merge). Bail if
			// HEAD has no commits ahead of origin/main — nothing left to flush.
			defaultBranch := r.baseBranch
			mainRef := "origin/" + defaultBranch
			if !r.hasCommitsAhead(mainRef, "HEAD") {
				return false, nil
			}
		}
		if _, pushErr := r.PushAndCreatePR(ctx, taskID, taskDesc, ""); pushErr != nil {
			return false, pushErr
		}
	}
	if !autoMerge {
		return false, nil
	}
	merged, err = r.AutoMergeCurrentBranch(ctx)
	if err != nil {
		return false, err
	}
	if merged {
		r.PostMergeUpdateMain()
	}
	return merged, nil
}

// PostMergeUpdateMain fetches origin/main and rebases the worktree onto it
// after a PR merge, then moves the worktree to the placeholder branch and
// deletes the stale task branch. It does not modify the ProjectDir checkout.
func (r *repo) PostMergeUpdateMain() {
	defaultBranch := r.baseBranch
	r.gitCmd(r.projectDir, "fetch", "--prune", "origin")

	// Sync worktree with updated main. If rebase conflicts, reset —
	// the merged work is on main and stale stack commits are expendable.
	if r.gitCmdErr(r.workDir, "rebase", "origin/"+defaultBranch) != nil {
		r.gitCmd(r.workDir, "rebase", "--abort")
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Post-merge rebase failed — resetting worktree to origin/%s", defaultBranch)
		r.gitCmd(r.workDir, "reset", "--hard", "origin/"+defaultBranch)
	}

	// The PR for this branch was just squash-merged — the branch is logically
	// merged even if git-ancestry says otherwise (squash rewrites history).
	// Force-delete is safe here and intentional; PrepareForNextTask's
	// conservative delete would spuriously preserve the branch.
	staleTaskBranch := r.worktreeBranch
	r.PrepareForNextTask("", "")
	if staleTaskBranch != "" && staleTaskBranch != WipBranchName() && staleTaskBranch != r.worktreeBranch {
		if err := r.gitCmdErr(r.projectDir, "branch", "-D", staleTaskBranch); err == nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Deleted local branch %s", staleTaskBranch)
		}
	}
}
