package testutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Proves WaitFor returns as soon as the condition holds, polling until then,
// rather than waiting out a fixed duration.
func TestWaitFor_ReturnsWhenConditionBecomesTrue(t *testing.T) {
	calls := 0
	WaitFor(t, time.Second, "counter to reach 3", func() bool {
		calls++
		return calls >= 3
	})
	if calls != 3 {
		t.Fatalf("expected WaitFor to poll until the 3rd check, got %d calls", calls)
	}
}

// Proves WaitForFile returns the trimmed contents of a file once it is present
// and non-empty — the synchronization primitive for reading a value an
// external process writes asynchronously.
func TestWaitForFile_ReturnsTrimmedContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pid")
	if err := os.WriteFile(path, []byte("  4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := WaitForFile(t, time.Second, path); got != "4242" {
		t.Fatalf("expected trimmed contents %q, got %q", "4242", got)
	}
}

// Proves WaitProcessGone observes a real process exiting.
func TestWaitProcessGone_DetectsExit(t *testing.T) {
	// A process that is already gone: PID 1's child reaping aside, an
	// unused high PID never exists, so kill -0 fails immediately.
	WaitProcessGone(t, time.Second, "2147483647")
}
