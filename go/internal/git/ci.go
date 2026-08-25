package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/retry"
)

// CIBucket is the normalized merge-gate verdict for a CI check, computed
// once in mapCheckRun from the provider-reported status/conclusion.
type CIBucket string

const (
	CIBucketPass    CIBucket = "pass"
	CIBucketFail    CIBucket = "fail"
	CIBucketPending CIBucket = "pending"
)

// CICheckResult represents the status of a single CI check.
// State is the provider-reported CI state (e.g. SUCCESS, FAILURE, PENDING, CANCELLED).
// Bucket is the normalized merge-gate verdict: pass, fail, or pending.
type CICheckResult struct {
	Name       string    `json:"name"`
	State      string    `json:"state"`
	Bucket     CIBucket  `json:"bucket"`
	IsRequired bool      `json:"isRequired"`
	StartedAt  time.Time `json:"startedAt"`
}

// Failed reports whether the check resolved to a failing or cancelled outcome.
func (r CICheckResult) Failed() bool {
	return r.Bucket == CIBucketFail
}

// Pending reports whether the check has not yet resolved.
func (r CICheckResult) Pending() bool {
	return r.Bucket == CIBucketPending
}

// CIStatus summarizes the overall state of all CI checks on a PR.
type CIStatus int

const (
	CIPending CIStatus = iota
	CIPassed
	CIFailed
)

// CIFailureError is returned when CI checks fail on a PR.
type CIFailureError struct {
	PRNumber int
	Failures []CICheckResult
}

func (e *CIFailureError) Error() string {
	var names []string
	for _, f := range e.Failures {
		names = append(names, f.Name)
	}
	return fmt.Sprintf("CI checks failed on PR #%d: %s", e.PRNumber, strings.Join(names, ", "))
}

// LocalTestFailureError is returned when the pre-merge local test run fails.
// It stands in for CIFailureError on projects with no GitHub checks: the tree
// is red, so the PR must not merge and the bead must not be closed as
// merge-pending. Typed rather than a formatted string so classifyMergeOutcome
// can demux it into a ShipResult the loop branches on.
type LocalTestFailureError struct {
	PRNumber int
	Reason   string
	Details  string
}

func (e *LocalTestFailureError) Error() string {
	msg := fmt.Sprintf("local tests failed before merge on PR #%d: %s", e.PRNumber, e.Reason)
	if e.Details != "" {
		msg += "\n" + e.Details
	}
	return msg
}

// CIIncompleteError is returned when the CI wait gives up before every check
// resolved — either the check set froze for the no-progress budget or the hard
// cap elapsed. Typed so a caller can tell "CI never finished" apart from "CI
// failed" without matching on the message.
type CIIncompleteError struct {
	PRNumber int
	Waited   time.Duration
}

func (e *CIIncompleteError) Error() string {
	return fmt.Sprintf("CI checks did not complete within %v", e.Waited)
}

// ErrCIInterrupted is returned when the context is cancelled while waiting on
// CI. Nothing is known about the checks — the wait simply stopped.
var ErrCIInterrupted = errors.New("interrupted")

// DefaultCIPollInterval is the initial time between CI status checks.
// Each subsequent poll doubles this interval up to MaxCIPollInterval.
const DefaultCIPollInterval = 1 * time.Second

// MaxCIPollInterval caps the exponential backoff so polls don't grow too far apart.
const MaxCIPollInterval = 30 * time.Second

// DefaultCIPollTimeout is the no-progress budget: how long waitForCI waits
// with the fetched check set observably frozen (no new check, no status
// transition, no completion) before abandoning the wait. Matches the
// ci_poll_timeout config-file default. Reset any time the check state changes,
// so a healthy CI run of any length is waited out — only DefaultCIMaxWait
// bounds the total wait.
const DefaultCIPollTimeout = 5 * time.Minute

// DefaultCIMaxWait is the hard cap on total wait time regardless of progress,
// bounding genuinely hung CI even when checks keep transitioning. Matches the
// ci_max_wait config-file default.
const DefaultCIMaxWait = 45 * time.Minute

// DefaultNoCIGracePeriod is how long waitForCI waits for any checks to appear
// before concluding no CI is configured. Repos with CI register checks within
// seconds; only repos with no CI configured consistently return zero checks.
const DefaultNoCIGracePeriod = 30 * time.Second

// errCIFrozen is returned internally by waitForCI's poll function when the
// fetched check state has not changed for the no-progress budget. It is
// classified as fatal so retry.Retry returns it immediately instead of
// continuing to poll.
var errCIFrozen = errors.New("ci: check state has not changed within the no-progress budget")

// ciSleep is the function used to create timer channels in waitForCI.
// Tests override this to avoid real sleeps.
var ciSleep = func(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// ciNow is the clock waitForCI uses to track elapsed time for the
// no-progress budget and the minute-cadence status log. Tests override this
// to simulate elapsed time deterministically without real sleeps.
var ciNow = time.Now

// evaluateChecks determines the overall CI status from individual check results.
// All checks are blocking — any failure returns CIFailed, any pending check
// returns CIPending. CIPassed is only returned when every check has resolved
// successfully.
func evaluateChecks(checks []CICheckResult) CIStatus {
	if len(checks) == 0 {
		return CIPending
	}

	allResolved := true
	for _, c := range checks {
		if c.Failed() {
			return CIFailed
		}
		if c.Pending() {
			allResolved = false
		}
	}

	if allResolved {
		return CIPassed
	}
	return CIPending
}

// failedChecks returns only the checks that did not succeed.
func failedChecks(checks []CICheckResult) []CICheckResult {
	var failed []CICheckResult
	for _, c := range checks {
		if c.Failed() {
			failed = append(failed, c)
		}
	}
	return failed
}

// isInfrastructureFailure checks the GitHub Actions API to determine if CI
// failed due to infrastructure (billing, runner allocation) rather than actual
// test failures. A job with zero steps executed indicates it never ran.
func (r *repo) isInfrastructureFailure(ctx context.Context, prNumber int) bool {
	nwo := NWOFromRemote(r.RemoteURL())
	if nwo == "" {
		return false
	}
	gh := r.github
	if gh == nil || !gh.Available() {
		return false
	}
	steps, err := gh.GetJobStepCount(ctx, nwo, prNumber)
	if err != nil {
		return false
	}
	return steps == 0
}

// stepTimeoutAnnotation is the failure-level check-run annotation GitHub
// Actions attaches to a job whose step exceeded its timeout-minutes, e.g.
// "The action 'Install system tools' has timed out after 5 minutes." The
// step's own conclusion is a plain "failure" (subsequent steps are
// "skipped"), so the annotation is the only signal that distinguishes a
// timeout from a real test failure.
const stepTimeoutAnnotation = "has timed out after"

// isStepTimeoutFailure reports whether every failed job of the PR's current
// workflow run failed because one of its steps hit its timeout. A failed job
// without such an annotation means real work failed, so the whole run is
// classified as a real failure.
//
// Deliberately separate from isInfrastructureFailure: that classification
// feeds mergeAsInfrastructureFailure, which can admin-merge past branch
// protection. A code-caused hang produces the same timeout annotation as a
// slow runner, so a step timeout must only ever re-trigger CI — never merge.
func (r *repo) isStepTimeoutFailure(ctx context.Context, prNumber int) bool {
	nwo := NWOFromRemote(r.RemoteURL())
	if nwo == "" {
		return false
	}
	gh := r.github
	if gh == nil || !gh.Available() {
		return false
	}
	jobs, err := gh.GetFailedJobAnnotations(ctx, nwo, prNumber)
	if err != nil || len(jobs) == 0 {
		return false
	}
	for _, job := range jobs {
		if !hasStepTimeoutAnnotation(job.Messages) {
			return false
		}
	}
	return true
}

// hasStepTimeoutAnnotation reports whether any of a job's failure-level
// annotation messages is a step-timeout message.
func hasStepTimeoutAnnotation(messages []string) bool {
	for _, m := range messages {
		if strings.Contains(m, stepTimeoutAnnotation) {
			return true
		}
	}
	return false
}

// RequiredFailedChecks returns failed checks that the fix agent should address.
// When IsRequired is populated (branch protection was queried successfully),
// only required failed checks are returned. When no check has IsRequired set
// (branch protection unavailable), all failed checks are treated as required.
func RequiredFailedChecks(checks []CICheckResult) []CICheckResult {
	hasRequired := false
	for _, c := range checks {
		if c.IsRequired {
			hasRequired = true
			break
		}
	}
	var failed []CICheckResult
	for _, c := range checks {
		if c.Failed() && (!hasRequired || c.IsRequired) {
			failed = append(failed, c)
		}
	}
	return failed
}

// ErrStackedPRWaiting is returned when a PR targets a non-main branch
// and must wait for the base PR to merge first. This is not a failure —
// it's expected stacking behavior and should not count as a merge failure.
var ErrStackedPRWaiting = errors.New("stacked PR waiting for base to merge")

// MergeConflictError is returned when a PR cannot be merged due to conflicts.
type MergeConflictError struct {
	PRNumber int
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("PR #%d has merge conflicts with the base branch", e.PRNumber)
}

// UnresolvedConflictError is returned when a merge conflict could not be
// auto-resolved by rebasing. Retrying will not help — the conflict requires
// manual or agent-driven resolution.
type UnresolvedConflictError struct {
	PRNumber int
}

func (e *UnresolvedConflictError) Error() string {
	return fmt.Sprintf("PR #%d has unresolvable merge conflicts — auto-resolve failed", e.PRNumber)
}

// LocalRebaseConflictError is returned when rebasing local worktree commits
// onto the remote base branch aborts due to conflicts. The branch state is
// preserved intact (the rebase was aborted), so callers on the startup and
// branch-setup paths can log and continue — the agent or a later task
// boundary will handle the divergence. It is distinct from
// UnresolvedConflictError, which carries PR-merge semantics.
type LocalRebaseConflictError struct {
	Branch string
	Base   string
}

func (e *LocalRebaseConflictError) Error() string {
	return fmt.Sprintf("local commits on %s could not be rebased onto origin/%s — divergent changes", e.Branch, e.Base)
}

// TransportError wraps a git remote-transport failure (e.g. fetch returning
// exit status 128 because the network is temporarily down or auth failed).
// The loop treats this as recoverable: it skips the current task and continues
// to the next iteration rather than exiting with status=error.
type TransportError struct {
	Op  string // git operation that failed, e.g. "fetch"
	Err error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("transient transport error during git %s: %v", e.Op, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// CIFetchFunc is the signature for fetching PR check status.
type CIFetchFunc func(prNumber int, repoURL string) ([]CICheckResult, error)

// AwaitCI fetches CI check status for a PR and polls until checks resolve.
// When pushedAt is non-zero, filters out checks that started before the push
// so only fresh CI results are evaluated. This prevents stale results from a
// previous push from gating the merge.
func (r *repo) AwaitCI(ctx context.Context, prNumber int, repoURL string, pushedAt time.Time) ([]CICheckResult, CIStatus, error) {
	nwo := NWOFromRemote(repoURL)
	gh := r.github

	fetch := func(prNumber int, repoURL string) ([]CICheckResult, error) {
		return gh.ListChecks(ctx, prNumber, repoURL)
	}

	// When required status checks are configured on the base branch, filter to
	// only those checks. Non-required checks (deploy previews, tag workflows)
	// must not gate merging — only branch-protection-required checks count.
	if requiredChecks, err := gh.GetRequiredChecks(ctx, nwo, r.baseBranch); err == nil && len(requiredChecks) > 0 {
		required := make(map[string]bool, len(requiredChecks))
		for _, c := range requiredChecks {
			required[c] = true
		}
		baseFetch := fetch
		fetch = func(prNumber int, repoURL string) ([]CICheckResult, error) {
			checks, err := baseFetch(prNumber, repoURL)
			if err != nil {
				return nil, err
			}
			var filtered []CICheckResult
			for _, c := range checks {
				if required[c.Name] {
					c.IsRequired = true
					filtered = append(filtered, c)
				}
			}
			return filtered, nil
		}
	}

	if !pushedAt.IsZero() {
		r.logger.Emit(logging.Opts{Domain: logging.CI, Link: logging.PRLinkOpt(nwo, prNumber)}, "Waiting for fresh CI checks (pushed at %s)...", pushedAt.Format("15:04:05"))
		baseFetch := fetch
		fetch = func(prNumber int, repoURL string) ([]CICheckResult, error) {
			checks, err := baseFetch(prNumber, repoURL)
			if err != nil {
				return nil, err
			}
			var fresh []CICheckResult
			for _, c := range checks {
				if !c.StartedAt.IsZero() && c.StartedAt.Before(pushedAt) {
					continue
				}
				fresh = append(fresh, c)
			}
			return fresh, nil
		}
	}

	timeout := r.ciPollTimeout
	if timeout == 0 {
		timeout = DefaultCIPollTimeout
	}

	hardCap := r.ciMaxWait
	if hardCap == 0 {
		hardCap = DefaultCIMaxWait
	}

	gracePeriod := r.noCIGracePeriod
	if gracePeriod == 0 {
		gracePeriod = DefaultNoCIGracePeriod
	}

	stepFetch := func() ([]JobStepStatus, error) {
		return gh.GetRunningJobSteps(ctx, nwo, prNumber)
	}

	checks, fetchErr := fetch(prNumber, repoURL)
	if fetchErr != nil || len(checks) == 0 {
		r.logger.Emit(logging.Opts{Domain: logging.CI, Link: logging.PRLinkOpt(nwo, prNumber)}, "CI checks not available yet — waiting...")
		return waitForCI(ctx, fetch, prNumber, repoURL, nwo, DefaultCIPollInterval, timeout, hardCap, gracePeriod, stepFetch, r.logger)
	}
	status := evaluateChecks(checks)
	if status != CIPending {
		return checks, status, nil
	}
	return waitForCI(ctx, fetch, prNumber, repoURL, nwo, DefaultCIPollInterval, timeout, hardCap, gracePeriod, stepFetch, r.logger)
}

// waitForCI polls PR checks until they complete, the no-progress budget
// (noProgressTimeout) elapses with the fetched check state frozen and no live
// step observed, or the hard cap (hardCap) elapses regardless of progress.
// Uses exponential backoff starting at interval, doubling each poll up to
// MaxCIPollInterval.
//
// The no-progress budget resets any time the fetched check set changes (a
// check appears, transitions, or completes) or stepFetch reports at least one
// in-progress GitHub Actions job step — a live running step is progress even
// when the check's own state (e.g. "PENDING") hasn't moved, which is what a
// single long-running required check looks like for its entire run. So a
// healthy CI run of any length is waited out — hardCap is the only bound
// that applies regardless of progress. While waiting, emits at most one
// status log line per minute (plus one immediately when polling begins)
// reporting elapsed time and a checks snapshot. Emits nothing when CI
// resolves on the first fetch.
//
// gracePeriod controls how long to wait with zero checks before treating the
// repo as having no CI configured and returning CIPassed. Pass 0 to disable.
//
// stepFetch, when non-nil, is called at most once per status-line emission
// (never per poll) to enrich the line with the currently running GitHub
// Actions step per in-progress job. A nil stepFetch, or one that errors,
// falls back to the plain check-name rendering.
func waitForCI(ctx context.Context, fetch CIFetchFunc, prNumber int, repoURL, nwo string, interval, noProgressTimeout, hardCap, gracePeriod time.Duration, stepFetch func() ([]JobStepStatus, error), log Log) ([]CICheckResult, CIStatus, error) {
	prLink := logging.PRLinkOpt(nwo, prNumber)

	var zeroChecksSince time.Time
	var checks []CICheckResult
	status := CIPending
	noCIConfigured := false

	start := ciNow()
	lastChangeAt := start
	var lastState map[string]string
	var lastStatusAt time.Time

	emitStatusLine := func(now time.Time, steps []JobStepStatus) {
		completed, total, inProgress := summarizeCIChecks(checks)
		log.Emit(logging.Opts{Domain: logging.CI, Link: prLink}, "CI %s: %d/%d checks complete, %s",
			formatCIElapsed(now.Sub(start)), completed, total, formatInProgressChecks(inProgress, steps))
		lastStatusAt = now
	}

	fetchAttempt := func() (bool, error) {
		c, fetchErr := fetch(prNumber, repoURL)
		now := ciNow()

		if fetchErr == nil {
			checks = c

			if len(checks) == 0 && gracePeriod > 0 {
				if zeroChecksSince.IsZero() {
					zeroChecksSince = time.Now()
				} else if time.Since(zeroChecksSince) >= gracePeriod {
					status = CIPassed
					noCIConfigured = true
					return true, nil
				}
			} else if len(checks) > 0 {
				zeroChecksSince = time.Time{}
			}

			state := checkStateSnapshot(checks)
			if lastState == nil || !checkStateEqual(state, lastState) {
				lastChangeAt = now
				lastState = state
			}

			status = evaluateChecks(checks)
			if status == CIPassed || status == CIFailed {
				return true, nil
			}
		}

		// stepFetch is polled at the same once-per-minute cadence as the status
		// line (never more often — see the doc comment), so the liveness check
		// below adds no extra calls beyond what already ran to build the log
		// line.
		shouldEmit := lastStatusAt.IsZero() || now.Sub(lastStatusAt) >= time.Minute
		var steps []JobStepStatus
		if fetchErr == nil && shouldEmit && stepFetch != nil {
			if s, err := stepFetch(); err == nil {
				steps = s
			}
		}

		// A running GitHub Actions job step is direct evidence CI is alive even
		// when the fetched check state itself is frozen — e.g. a single
		// required check whose status stays "PENDING"/"in_progress" for the
		// whole duration of a long-running job. Treat it as progress so that
		// case doesn't trip the no-progress timeout.
		if len(steps) > 0 {
			lastChangeAt = now
		}

		// A frozen check state counts as no-progress whether the freeze comes
		// from an unchanging fetch result or from fetch itself repeatedly
		// failing — either way nothing observable about CI (including a live
		// running step) has changed.
		if now.Sub(lastChangeAt) >= noProgressTimeout {
			return false, errCIFrozen
		}

		if fetchErr != nil {
			return false, fetchErr
		}

		if shouldEmit {
			emitStatusLine(now, steps)
		}

		return false, nil
	}

	classify := func(err error) bool {
		return !errors.Is(err, errCIFrozen)
	}

	err := retry.Retry(ctx, retry.BackoffOpts{
		Initial: interval,
		Max:     MaxCIPollInterval,
		Timeout: hardCap,
		Sleep:   ciSleep,
	}, classify, fetchAttempt)

	switch {
	case err == nil && noCIConfigured:
		log.Emit(logging.Opts{Domain: logging.CI, Link: prLink}, "No CI checks found after %v — no CI configured", gracePeriod)
		return checks, CIPassed, nil
	case err == nil:
		return checks, status, nil
	case errors.Is(err, errCIFrozen):
		return checks, status, &CIIncompleteError{PRNumber: prNumber, Waited: noProgressTimeout}
	case errors.Is(err, retry.ErrTimedOut):
		return checks, status, &CIIncompleteError{PRNumber: prNumber, Waited: hardCap}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nil, CIPending, ErrCIInterrupted
	default:
		return nil, CIPending, fmt.Errorf("CI checks not available within %v: %w", noProgressTimeout, err)
	}
}

// checkStateSnapshot builds a per-check name -> state signature so waitForCI
// can detect whether CI made observable progress between two polls: a new
// check appearing, an existing check transitioning status, or a check
// completing all change the signature for that check's name.
func checkStateSnapshot(checks []CICheckResult) map[string]string {
	snapshot := make(map[string]string, len(checks))
	for _, c := range checks {
		snapshot[c.Name] = c.State
	}
	return snapshot
}

// checkStateEqual reports whether two check-state snapshots are identical.
func checkStateEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, state := range a {
		if b[name] != state {
			return false
		}
	}
	return true
}

// summarizeCIChecks splits checks into a completed count, the total count,
// and the names of checks still pending — the snapshot shown in waitForCI's
// minute-cadence status log.
func summarizeCIChecks(checks []CICheckResult) (completed, total int, inProgress []string) {
	total = len(checks)
	for _, c := range checks {
		if c.Pending() {
			inProgress = append(inProgress, c.Name)
		} else {
			completed++
		}
	}
	return completed, total, inProgress
}

// formatInProgressChecks renders the in-progress portion of the
// minute-cadence status line. When steps is empty (stepFetch was nil, or
// the fetch errored), it falls back to the plain "in progress: a, b"
// rendering. Otherwise, each in-progress check is matched by name against
// a job in steps: a match renders "name → step (step i/n)"; a check with
// no matching job (external CI has no steps) renders as its bare name.
func formatInProgressChecks(inProgress []string, steps []JobStepStatus) string {
	if len(steps) == 0 {
		return "in progress: " + strings.Join(inProgress, ", ")
	}
	byJob := make(map[string]JobStepStatus, len(steps))
	for _, s := range steps {
		byJob[s.JobName] = s
	}
	parts := make([]string, len(inProgress))
	for i, name := range inProgress {
		if s, ok := byJob[name]; ok {
			parts[i] = fmt.Sprintf("%s → %s (step %d/%d)", name, s.StepName, s.StepIndex, s.StepTotal)
		} else {
			parts[i] = name
		}
	}
	return strings.Join(parts, ", ")
}

// formatCIElapsed renders elapsed wait time for the status log: seconds
// below a minute, whole minutes at or above a minute (e.g. "45s", "3m").
func formatCIElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}
