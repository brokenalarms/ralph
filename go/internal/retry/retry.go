// Package retry provides a single generic retry/poll primitive so callers
// stop hand-rolling "repeat fn until done, honoring ctx and a backoff
// schedule" loops. It owns backoff sleeping, ctx cancellation, and
// transient-vs-fatal error classification; callers own the operation being
// retried and any domain-specific state.
package retry

import (
	"context"
	"errors"
	"time"
)

// ErrTimedOut is returned by Retry when the exponential-backoff Timeout
// elapses (or the explicit Schedule is exhausted) without fn reporting done
// and without a fatal error to surface instead.
var ErrTimedOut = errors.New("retry: timed out waiting for condition")

// BackoffOpts configures Retry's delay schedule between attempts. Set
// Schedule for explicit-schedule mode, or Initial/Max/Timeout for
// exponential-backoff mode — the two are mutually exclusive; a non-nil
// Schedule always takes precedence.
type BackoffOpts struct {
	// Schedule, when non-nil, gives the exact delay before each retry
	// attempt (explicit-schedule mode). Retry makes len(Schedule)+1 attempts
	// total before giving up.
	Schedule []time.Duration

	// Initial and Max configure exponential-backoff mode (used when
	// Schedule is nil): the delay starts at Initial and doubles after each
	// attempt, capped at Max. Set Initial == Max for a fixed-interval retry.
	Initial time.Duration
	Max     time.Duration

	// Timeout bounds the overall time Retry may spend polling, measured
	// from the first call to fn. Zero means no deadline. Only meaningful in
	// exponential-backoff mode — explicit-schedule mode always stops once
	// the schedule is exhausted.
	Timeout time.Duration

	// Sleep creates the timer channel Retry waits on for each delay.
	// Defaults to time.After; tests override it to avoid real sleeps.
	Sleep func(time.Duration) <-chan time.Time

	// OnRetry, when set, is called once per incomplete attempt — after fn
	// returns not-done and before Retry sleeps — with the delay about to be
	// waited. Callers use it to log progress.
	OnRetry func(attempt int, delay time.Duration)
}

// Classify reports whether err is transient (Retry should keep going) or
// fatal (Retry should return it immediately, without waiting or retrying
// further). A nil Classify passed to Retry treats every non-nil error as
// transient.
type Classify func(err error) bool

// Retry calls fn until it reports done, returns a fatal error (per
// classify), the backoff schedule/timeout is exhausted, or ctx is
// cancelled. It centralizes the sleep-with-backoff-honoring-ctx structure
// so callers only need to supply the operation and its error classification.
func Retry(ctx context.Context, opts BackoffOpts, classify Classify, fn func() (done bool, err error)) error {
	sleep := opts.Sleep
	if sleep == nil {
		sleep = func(d time.Duration) <-chan time.Time { return time.After(d) }
	}
	if classify == nil {
		classify = func(error) bool { return true }
	}

	var cancelled <-chan struct{}
	if ctx != nil {
		cancelled = ctx.Done()
	}

	var deadline time.Time
	if opts.Schedule == nil && opts.Timeout > 0 {
		deadline = time.Now().Add(opts.Timeout)
	}

	delay := opts.Initial
	attempt := 0
	for {
		done, err := fn()
		if done {
			return nil
		}
		if err != nil && !classify(err) {
			return err
		}

		var wait time.Duration
		if opts.Schedule != nil {
			if attempt >= len(opts.Schedule) {
				if err != nil {
					return err
				}
				return ErrTimedOut
			}
			wait = opts.Schedule[attempt]
		} else {
			wait = delay
			if opts.Max > 0 && wait > opts.Max {
				wait = opts.Max
			}
			if !deadline.IsZero() {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					if err != nil {
						return err
					}
					return ErrTimedOut
				}
				if wait > remaining {
					wait = remaining
				}
			}
		}

		if opts.OnRetry != nil {
			opts.OnRetry(attempt, wait)
		}

		select {
		case <-cancelled:
			return ctx.Err()
		case <-sleep(wait):
		}

		if opts.Schedule == nil {
			delay *= 2
			if opts.Max > 0 && delay > opts.Max {
				delay = opts.Max
			}
		}
		attempt++
	}
}

// Wait pauses for d, honoring ctx cancellation. It is the single-delay
// building block Retry uses internally; callers that need to wait out one
// backoff step inside a larger state machine (rather than looping fn
// through Retry) call it directly instead of hand-rolling a ctx/timer
// select.
func Wait(ctx context.Context, d time.Duration, sleep func(time.Duration) <-chan time.Time) error {
	if sleep == nil {
		sleep = func(d time.Duration) <-chan time.Time { return time.After(d) }
	}
	var cancelled <-chan struct{}
	if ctx != nil {
		cancelled = ctx.Done()
	}
	select {
	case <-cancelled:
		return ctx.Err()
	case <-sleep(d):
		return nil
	}
}
