package git

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// CICheckResult represents the status of a single CI check.
// State is the provider-reported CI state (e.g. SUCCESS, FAILURE, PENDING, CANCELLED).
// Bucket is the normalized merge-gate bucket: pass, fail, or pending.
type CICheckResult struct {
	Name       string    `json:"name"`
	State      string    `json:"state"`
	Bucket     string    `json:"bucket"`
	IsRequired bool      `json:"isRequired"`
	StartedAt  time.Time `json:"startedAt"`
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

// CIFixExhaustedError is returned by MergeWithRetry when CI fix agents applied
// code changes but CI is still failing after all merge attempts. This signals
// genuine test failures — the loop should leave the task open for manual investigation
// rather than closing it as verified.
type CIFixExhaustedError struct {
	Attempts int
}

func (e *CIFixExhaustedError) Error() string {
	return fmt.Sprintf("CI fix agents exhausted after %d attempts — tests still failing", e.Attempts)
}

// DefaultCIPollInterval is the initial time between CI status checks.
// Each subsequent poll doubles this interval up to MaxCIPollInterval.
const DefaultCIPollInterval = 1 * time.Second

// MaxCIPollInterval caps the exponential backoff so polls don't grow too far apart.
const MaxCIPollInterval = 5 * time.Second

// DefaultCIPollTimeout is the maximum time to wait for CI checks to complete.
// Matches the ci_poll_timeout config-file default.
const DefaultCIPollTimeout = 5 * time.Minute

// DefaultNoCIGracePeriod is how long waitForCI waits for any checks to appear
// before concluding no CI is configured. Repos with CI register checks within
// seconds; only repos with no CI configured consistently return zero checks.
const DefaultNoCIGracePeriod = 30 * time.Second

// ciSleep is the function used to create timer channels in waitForCI.
// Tests override this to avoid real sleeps.
var ciSleep = func(d time.Duration) <-chan time.Time {
	return time.After(d)
}

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
		if c.Bucket == "fail" || c.State == "FAILURE" || c.State == "CANCELLED" {
			return CIFailed
		}
		if c.Bucket == "pending" || c.State == "PENDING" || c.State == "IN_PROGRESS" {
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
		if c.Bucket == "fail" || c.State == "FAILURE" || c.State == "CANCELLED" {
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
		if c.Bucket == "fail" && (!hasRequired || c.IsRequired) {
			failed = append(failed, c)
		}
	}
	return failed
}

// ErrStackedPRWaiting is returned when a PR targets a non-main branch
// and must wait for the base PR to merge first. This is not a failure —
// it's expected stacking behavior and should not count as a merge failure.
var ErrStackedPRWaiting = fmt.Errorf("stacked PR waiting for base to merge")

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

	gracePeriod := r.noCIGracePeriod
	if gracePeriod == 0 {
		gracePeriod = DefaultNoCIGracePeriod
	}

	checks, fetchErr := fetch(prNumber, repoURL)
	if fetchErr != nil || len(checks) == 0 {
		r.logger.Emit(logging.Opts{Domain: logging.CI, Link: logging.PRLinkOpt(nwo, prNumber)}, "CI checks not available yet — waiting...")
		return waitForCI(ctx, fetch, prNumber, repoURL, nwo, DefaultCIPollInterval, timeout, gracePeriod, r.logger)
	}
	status := evaluateChecks(checks)
	if status != CIPending {
		return checks, status, nil
	}
	return waitForCI(ctx, fetch, prNumber, repoURL, nwo, DefaultCIPollInterval, timeout, gracePeriod, r.logger)
}

// waitForCI polls PR checks until they complete or timeout is reached.
// Uses exponential backoff starting at interval, doubling each poll up to
// MaxCIPollInterval. Emits a single in-place log line that grows as polls
// accumulate (e.g. "CI polled 1s..2s..4s"), finalizing it to the log file
// on completion. Emits nothing when CI resolves on the first fetch.
//
// gracePeriod controls how long to wait with zero checks before treating the
// repo as having no CI configured and returning CIPassed. Pass 0 to disable.
func waitForCI(ctx context.Context, fetch CIFetchFunc, prNumber int, repoURL, nwo string, interval, timeout, gracePeriod time.Duration, log Log) ([]CICheckResult, CIStatus, error) {
	deadline := time.Now().Add(timeout)
	prLink := logging.PRLinkOpt(nwo, prNumber)

	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}

	currentInterval := interval
	var polled bool
	var zeroChecksSince time.Time

	emitPoll := func(duration string) {
		if !polled {
			log.Emit(logging.Opts{Domain: logging.CI, Link: prLink, Append: true}, "CI polled %s", duration)
		} else {
			log.Emit(logging.Opts{Append: true}, "..%s", duration)
		}
		polled = true
	}

	finalize := func() {
		if polled {
			log.Emit(logging.Opts{}, "\n")
		}
	}

	for {
		checks, err := fetch(prNumber, repoURL)
		if err != nil {
			if time.Now().After(deadline) {
				finalize()
				return nil, CIPending, fmt.Errorf("CI checks not available within %v: %w", timeout, err)
			}
			emitPoll(formatDuration(currentInterval))
			select {
			case <-done:
				finalize()
				return nil, CIPending, fmt.Errorf("interrupted")
			case <-ciSleep(currentInterval):
			}
			currentInterval = nextBackoff(currentInterval)
			continue
		}

		if len(checks) == 0 && gracePeriod > 0 {
			if zeroChecksSince.IsZero() {
				zeroChecksSince = time.Now()
			} else if time.Since(zeroChecksSince) >= gracePeriod {
				finalize()
				log.Emit(logging.Opts{Domain: logging.CI, Link: prLink}, "No CI checks found after %v — no CI configured", gracePeriod)
				return nil, CIPassed, nil
			}
		} else if len(checks) > 0 {
			zeroChecksSince = time.Time{}
		}

		status := evaluateChecks(checks)
		switch status {
		case CIPassed:
			finalize()
			return checks, CIPassed, nil
		case CIFailed:
			finalize()
			return checks, CIFailed, nil
		}

		if time.Now().After(deadline) {
			finalize()
			return checks, CIPending, fmt.Errorf("CI checks did not complete within %v", timeout)
		}

		emitPoll(formatDuration(currentInterval))
		select {
		case <-done:
			finalize()
			return nil, CIPending, fmt.Errorf("interrupted")
		case <-ciSleep(currentInterval):
		}
		currentInterval = nextBackoff(currentInterval)
	}
}

// nextBackoff doubles the interval, capping at MaxCIPollInterval.
func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > MaxCIPollInterval {
		return MaxCIPollInterval
	}
	return next
}

// formatDuration returns a compact human-readable duration (e.g. "1s", "2s", "15s").
func formatDuration(d time.Duration) string {
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
