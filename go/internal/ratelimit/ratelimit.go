package ratelimit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var timeNow = time.Now

const DefaultCallsPerHour = 80

type Limiter struct {
	CallsPerHour int
	RalphDir     string
	StopFile     string
	PollInterval time.Duration
}

func New(ralphDir string, callsPerHour int) *Limiter {
	if callsPerHour <= 0 {
		callsPerHour = DefaultCallsPerHour
	}
	return &Limiter{
		CallsPerHour: callsPerHour,
		RalphDir:     ralphDir,
		StopFile:     filepath.Join(ralphDir, "stop"),
		PollInterval: 10 * time.Second,
	}
}

func (l *Limiter) countFile() string {
	return filepath.Join(l.RalphDir, ".call_count")
}

func (l *Limiter) hourFile() string {
	return filepath.Join(l.RalphDir, ".call_hour")
}

func (l *Limiter) currentHour() string {
	return timeNow().Format("2006010215")
}

func (l *Limiter) readHour() string {
	data, err := os.ReadFile(l.hourFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (l *Limiter) readCount() int {
	data, err := os.ReadFile(l.countFile())
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return n
}

func (l *Limiter) writeHour(hour string) error {
	return os.WriteFile(l.hourFile(), []byte(hour+"\n"), 0o644)
}

func (l *Limiter) writeCount(count int) error {
	return os.WriteFile(l.countFile(), []byte(strconv.Itoa(count)+"\n"), 0o644)
}

func (l *Limiter) resetIfNewHour() {
	if l.readHour() != l.currentHour() {
		l.writeCount(0)
		l.writeHour(l.currentHour())
	}
}

// Init sets up call tracking files, resetting the count if the hour has changed.
func (l *Limiter) Init() error {
	if err := os.MkdirAll(l.RalphDir, 0o755); err != nil {
		return err
	}
	l.resetIfNewHour()
	return nil
}

// Allowed returns true if the current call count is below the hourly limit.
func (l *Limiter) Allowed() bool {
	l.resetIfNewHour()
	return l.readCount() < l.CallsPerHour
}

// Increment bumps the call count by one.
func (l *Limiter) Increment() {
	l.writeCount(l.readCount() + 1)
}

// Count returns the current call count for this hour.
func (l *Limiter) Count() int {
	return l.readCount()
}

// SecondsUntilReset returns the number of seconds until the top of the next hour.
func (l *Limiter) SecondsUntilReset() int {
	now := timeNow()
	nextHour := now.Truncate(time.Hour).Add(time.Hour)
	return int(nextHour.Sub(now).Seconds())
}

// WaitForReset blocks until the rate limit resets at the top of the next hour.
// It polls every 10 seconds and checks for a stop file. Returns an error if
// the stop file is detected or the context is cancelled. The onTick callback
// is called each poll interval with the seconds remaining, allowing callers
// to display a countdown.
func (l *Limiter) WaitForReset(ctx context.Context, onTick func(secondsLeft int)) error {
	storedHour := l.readHour()
	if storedHour != l.currentHour() {
		l.writeCount(0)
		l.writeHour(l.currentHour())
		return nil
	}

	for {
		if _, err := os.Stat(l.StopFile); err == nil {
			os.Remove(l.StopFile)
			return fmt.Errorf("stop file detected during rate limit wait")
		}

		if l.currentHour() != storedHour {
			break
		}

		secsLeft := l.SecondsUntilReset()
		if onTick != nil {
			onTick(secsLeft)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(l.PollInterval):
		}
	}

	l.writeCount(0)
	l.writeHour(l.currentHour())
	return nil
}
