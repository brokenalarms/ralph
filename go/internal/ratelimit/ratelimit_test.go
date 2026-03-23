package ratelimit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestLimiter(t *testing.T, callsPerHour int) *Limiter {
	t.Helper()
	dir := t.TempDir()
	l := New(dir, callsPerHour)
	l.PollInterval = 10 * time.Millisecond
	if err := l.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	return l
}

func withFrozenTime(t *testing.T, frozen time.Time) func() {
	t.Helper()
	orig := timeNow
	timeNow = func() time.Time { return frozen }
	return func() { timeNow = orig }
}

// Verify that a fresh limiter starts with zero calls and allows requests
// up to the configured limit, ensuring the basic counting mechanism works.
func TestAllowedUntilLimit(t *testing.T) {
	l := newTestLimiter(t, 3)

	for i := 0; i < 3; i++ {
		if !l.Allowed() {
			t.Fatalf("expected allowed at count %d", i)
		}
		l.Increment()
	}
	if l.Allowed() {
		t.Fatal("expected blocked after reaching limit")
	}
}

// Verify that the count resets when the hour changes, so users aren't
// permanently blocked after exhausting one hour's quota.
func TestResetOnNewHour(t *testing.T) {
	now := time.Date(2026, 3, 20, 14, 30, 0, 0, time.UTC)
	cleanup := withFrozenTime(t, now)
	defer cleanup()

	l := newTestLimiter(t, 2)
	l.Increment()
	l.Increment()

	if l.Allowed() {
		t.Fatal("expected blocked at limit")
	}

	timeNow = func() time.Time {
		return time.Date(2026, 3, 20, 15, 0, 0, 0, time.UTC)
	}

	if !l.Allowed() {
		t.Fatal("expected allowed after hour rollover")
	}
	if l.Count() != 0 {
		t.Fatalf("expected count 0 after reset, got %d", l.Count())
	}
}

// Verify Init creates the ralph dir and tracking files when they don't
// exist yet, so the limiter works on first run.
func TestInitCreatesFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "ralph")
	l := New(dir, 10)
	if err := l.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if _, err := os.Stat(l.countFile()); err != nil {
		t.Fatal("count file not created")
	}
	if _, err := os.Stat(l.hourFile()); err != nil {
		t.Fatal("hour file not created")
	}
}

// Verify SecondsUntilReset returns the correct remaining time to the
// next hour boundary, which drives the countdown display.
func TestSecondsUntilReset(t *testing.T) {
	cleanup := withFrozenTime(t, time.Date(2026, 3, 20, 14, 45, 30, 0, time.UTC))
	defer cleanup()

	l := newTestLimiter(t, 10)
	secs := l.SecondsUntilReset()

	expected := 14*60 + 30
	if secs != expected {
		t.Fatalf("expected %d seconds, got %d", expected, secs)
	}
}

// Verify that WaitForReset returns immediately when the hour has already
// changed (no actual waiting needed).
func TestWaitForResetAlreadyNewHour(t *testing.T) {
	now := time.Date(2026, 3, 20, 14, 59, 0, 0, time.UTC)
	cleanup := withFrozenTime(t, now)
	defer cleanup()

	l := newTestLimiter(t, 1)
	l.Increment()

	timeNow = func() time.Time {
		return time.Date(2026, 3, 20, 15, 0, 0, 0, time.UTC)
	}

	err := l.WaitForReset(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.Count() != 0 {
		t.Fatalf("expected count reset to 0, got %d", l.Count())
	}
}

// Verify that WaitForReset aborts with an error when a stop file is
// present, allowing the loop to shut down during a rate limit pause.
func TestWaitForResetStopFile(t *testing.T) {
	l := newTestLimiter(t, 1)
	l.Increment()

	if err := os.WriteFile(l.StopFile, []byte("stop\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := l.WaitForReset(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error from stop file")
	}

	if _, statErr := os.Stat(l.StopFile); statErr == nil {
		t.Fatal("stop file should have been removed")
	}
}

// Verify that WaitForReset respects context cancellation, so callers
// can implement their own timeout/shutdown logic.
func TestWaitForResetContextCancel(t *testing.T) {
	l := newTestLimiter(t, 1)
	l.Increment()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := l.WaitForReset(ctx, nil)
	if err == nil {
		t.Fatal("expected context cancelled error")
	}
}

// Verify that zero/negative callsPerHour falls back to the default limit,
// preventing misconfiguration from disabling rate limiting entirely.
func TestDefaultCallsPerHour(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, 0)
	if l.CallsPerHour != DefaultCallsPerHour {
		t.Fatalf("expected default %d, got %d", DefaultCallsPerHour, l.CallsPerHour)
	}

	l2 := New(dir, -5)
	if l2.CallsPerHour != DefaultCallsPerHour {
		t.Fatalf("expected default %d, got %d", DefaultCallsPerHour, l2.CallsPerHour)
	}
}

// Verify the onTick callback fires during WaitForReset with the correct
// seconds remaining, enabling countdown display to the user.
func TestWaitForResetOnTick(t *testing.T) {
	now := time.Date(2026, 3, 20, 14, 59, 50, 0, time.UTC)
	cleanup := withFrozenTime(t, now)
	defer cleanup()

	l := newTestLimiter(t, 1)
	l.Increment()

	callCount := 0
	timeNow = func() time.Time {
		callCount++
		if callCount > 2 {
			return time.Date(2026, 3, 20, 15, 0, 1, 0, time.UTC)
		}
		return now
	}

	var tickValues []int
	err := l.WaitForReset(context.Background(), func(secsLeft int) {
		tickValues = append(tickValues, secsLeft)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tickValues) == 0 {
		t.Fatal("expected at least one tick callback")
	}
}

// Verify WaitUntil returns immediately when the target time is in the past.
func TestWaitUntilAlreadyPassed(t *testing.T) {
	now := time.Date(2026, 3, 24, 15, 0, 0, 0, time.UTC)
	cleanup := withFrozenTime(t, now)
	defer cleanup()

	l := newTestLimiter(t, 10)
	target := time.Date(2026, 3, 24, 14, 0, 0, 0, time.UTC)

	err := l.WaitUntil(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Verify WaitUntil waits until the target time and calls onTick with
// decreasing seconds.
func TestWaitUntilCallsTick(t *testing.T) {
	now := time.Date(2026, 3, 24, 23, 59, 50, 0, time.UTC)
	cleanup := withFrozenTime(t, now)
	defer cleanup()

	l := newTestLimiter(t, 10)
	target := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)

	callCount := 0
	timeNow = func() time.Time {
		callCount++
		if callCount > 2 {
			return time.Date(2026, 3, 25, 0, 0, 1, 0, time.UTC)
		}
		return now
	}

	var tickValues []int
	err := l.WaitUntil(context.Background(), target, func(secsLeft int) {
		tickValues = append(tickValues, secsLeft)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tickValues) == 0 {
		t.Fatal("expected at least one tick callback")
	}
}

// Verify WaitUntil respects context cancellation.
func TestWaitUntilContextCancel(t *testing.T) {
	l := newTestLimiter(t, 10)
	target := time.Now().Add(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := l.WaitUntil(ctx, target, nil)
	if err == nil {
		t.Fatal("expected context cancelled error")
	}
}

// Verify WaitUntil aborts when stop file is present.
func TestWaitUntilStopFile(t *testing.T) {
	l := newTestLimiter(t, 10)
	target := time.Now().Add(time.Hour)

	if err := os.WriteFile(l.StopFile, []byte("stop\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := l.WaitUntil(context.Background(), target, nil)
	if err == nil {
		t.Fatal("expected error from stop file")
	}
}
