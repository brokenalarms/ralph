package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Retry returns promptly with ctx.Err() when ctx is cancelled mid-poll,
// instead of blocking until the backoff schedule or timeout is exhausted —
// this is the fix for PollReview's latent "ignores ctx cancellation" bug.
func TestRetry_CancelledContextStopsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	sawUnblockedSleep := make(chan struct{})
	fn := func() (bool, error) {
		calls++
		if calls == 1 {
			// Cancel only after the first attempt has run, proving Retry
			// checks cancellation during the wait rather than up front.
			go cancel()
		}
		return false, nil
	}

	done := make(chan error, 1)
	go func() {
		done <- Retry(ctx, BackoffOpts{
			Initial: time.Hour,
			Max:     time.Hour,
		}, nil, fn)
		close(sawUnblockedSleep)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Retry did not return after ctx cancellation — it kept waiting out the hour-long backoff")
	}
	<-sawUnblockedSleep

	if calls != 1 {
		t.Errorf("expected exactly 1 call to fn before cancellation stopped retrying, got %d", calls)
	}
}

// Retry stops immediately on a fatal error (per classify) without waiting
// out any backoff delay or calling fn again.
func TestRetry_FatalErrorStopsImmediately(t *testing.T) {
	fatal := errors.New("fatal")
	calls := 0
	fn := func() (bool, error) {
		calls++
		return false, fatal
	}
	classify := func(error) bool { return false } // never transient

	err := Retry(context.Background(), BackoffOpts{
		Schedule: []time.Duration{time.Hour, time.Hour},
	}, classify, fn)

	if !errors.Is(err, fatal) {
		t.Fatalf("expected fatal error to propagate, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 call for a fatal error, got %d", calls)
	}
}

// Retry retries a transient error according to classify and succeeds once
// fn reports done, proving the transient path keeps going instead of
// aborting.
func TestRetry_TransientErrorRetriesUntilDone(t *testing.T) {
	transient := errors.New("transient")
	calls := 0
	fn := func() (bool, error) {
		calls++
		if calls < 3 {
			return false, transient
		}
		return true, nil
	}
	classify := func(err error) bool { return errors.Is(err, transient) }

	err := Retry(context.Background(), BackoffOpts{
		Schedule: []time.Duration{0, 0, 0},
	}, classify, fn)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

// Retry's explicit-schedule mode gives up with ErrTimedOut once the
// schedule is exhausted and fn never reports done or a fatal error.
func TestRetry_ScheduleExhaustedReturnsErrTimedOut(t *testing.T) {
	calls := 0
	fn := func() (bool, error) {
		calls++
		return false, nil
	}

	err := Retry(context.Background(), BackoffOpts{
		Schedule: []time.Duration{0, 0},
	}, nil, fn)

	if !errors.Is(err, ErrTimedOut) {
		t.Fatalf("expected ErrTimedOut, got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls (1 initial + 2 scheduled retries), got %d", calls)
	}
}
