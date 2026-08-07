package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A zero-exit acceptance command passes, and the Result names the command that
// ran so the loop can log what it gated on.
func TestRunAcceptance_Success(t *testing.T) {
	dir := t.TempDir()

	res := RunAcceptance(context.Background(), "exit 0", dir, 30*time.Second)

	if !res.Passed {
		t.Errorf("expected pass, got %+v", res)
	}
	if res.Command != "exit 0" {
		t.Errorf("Command = %q, want the configured acceptance command", res.Command)
	}
}

// A non-zero exit fails the gate and carries the command's output as details,
// so the next agent sees why acceptance rejected the work.
func TestRunAcceptance_Failure_CarriesOutput(t *testing.T) {
	dir := t.TempDir()

	res := RunAcceptance(context.Background(), "echo safari-suite-crashed; exit 3", dir, 30*time.Second)

	if res.Passed {
		t.Fatal("expected failure for a non-zero exit")
	}
	if !strings.Contains(res.Details, "safari-suite-crashed") {
		t.Errorf("Details should carry the command output, got %q", res.Details)
	}
}

// The acceptance command runs in the given worktree directory, not wherever the
// loop process happens to be — a suite driven from the wrong tree would verify
// the wrong code.
func TestRunAcceptance_RunsInGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran-here")

	res := RunAcceptance(context.Background(), "touch ran-here", dir, 30*time.Second)

	if !res.Passed {
		t.Fatalf("expected pass, got %+v", res)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("command did not run in the given directory: %v", err)
	}
}

// A hung acceptance suite must not stall the loop forever — it is killed at the
// timeout and reported as a failure.
func TestRunAcceptance_Timeout_Fails(t *testing.T) {
	dir := t.TempDir()

	res := RunAcceptance(context.Background(), "sleep 30", dir, 100*time.Millisecond)

	if res.Passed {
		t.Fatal("expected a timed-out acceptance command to fail")
	}
	if !strings.Contains(res.Reason, "timed out") {
		t.Errorf("Reason should say the command timed out, got %q", res.Reason)
	}
}

// An empty command passes without running a shell at all — that is how a
// project with no [acceptance] table keeps its ship path unchanged.
func TestRunAcceptance_EmptyCommand_PassesWithoutRunning(t *testing.T) {
	dir := t.TempDir()

	res := RunAcceptance(context.Background(), "   ", dir, 30*time.Second)

	if !res.Passed {
		t.Errorf("empty command must pass (gate disabled), got %+v", res)
	}
	if res.Command != "" {
		t.Errorf("no command should be reported as run, got %q", res.Command)
	}
}
