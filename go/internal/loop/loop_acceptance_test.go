package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// acceptanceProbe is a real acceptance command that appends one byte to a
// marker file each time it runs, so tests can count runs (not just presence)
// and can make the command pass or fail.
type acceptanceProbe struct {
	marker  string
	command string
}

func newAcceptanceProbe(t *testing.T, passes bool) acceptanceProbe {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "acceptance-runs")
	command := fmt.Sprintf("printf x >> %q", marker)
	if !passes {
		command += "; exit 1"
	}
	return acceptanceProbe{marker: marker, command: command}
}

// runs reports how many times the acceptance command executed.
func (a acceptanceProbe) runs(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(a.marker)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("reading acceptance marker: %v", err)
	}
	return len(data)
}

// stubCountdown installs a fake countdown dialog with the given outcome and
// returns a counter of how many times it was shown.
func stubCountdown(t *testing.T, cancelled bool) *int {
	t.Helper()
	shown := 0
	out, err := "button returned:, gave up:true\n", error(nil)
	if cancelled {
		out, err = "execution error: User canceled. (-128)\n", fmt.Errorf("exit status 1")
	}
	prevRunner := notify.SetDialogRunner(func(_ string, _ ...string) (string, error) {
		shown++
		return out, err
	})
	t.Cleanup(func() { notify.SetDialogRunner(prevRunner) })
	prevPath := notify.SetOsascriptPath("/usr/bin/osascript")
	t.Cleanup(func() { notify.SetOsascriptPath(prevPath) })
	return &shown
}

func withAcceptance(command string) func(*Config) {
	return func(cfg *Config) {
		cfg.AcceptanceCommand = command
		cfg.AcceptanceCountdown = time.Second
	}
}

func newAcceptanceBackend() *testutil.TrackingBackend {
	return &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}
}

// The acceptance gate runs BEFORE push/PR creation, not after: when the
// acceptance command fails, nothing reaches the remote. The ship stub is
// primed to return a PR number and a pushed branch, so their absence in state
// and on the bead can only mean doShip never ran.
func TestShipAndFinalize_AcceptanceRunsBeforePushAndPRCreation(t *testing.T) {
	dir, _ := setupTestDir(t)
	stubCountdown(t, false)
	probe := newAcceptanceProbe(t, false)

	backend := newAcceptanceBackend()
	fs := buildFinalizeSetup(t, dir, "ralph-acc1", "Feature needing acceptance", backend, shipResult{
		prNumber:     42,
		prURL:        "https://github.com/owner/repo/pull/42",
		pushedBranch: "ralph/ralph-acc1-feature",
	}, withAcceptance(probe.command))

	out := fs.loop.completeTask(context.Background(), fs.p)

	if probe.runs(t) != 1 {
		t.Fatalf("acceptance command should have run once, ran %d times", probe.runs(t))
	}
	if out.action != signalRetry {
		t.Errorf("acceptance failure must retry the bead, got action %v", out.action)
	}
	if out.prNumber != 0 {
		t.Errorf("no PR may be created when acceptance fails, got PR #%d", out.prNumber)
	}

	branches, err := fs.st.GetPushedBranches()
	if err != nil {
		t.Fatalf("GetPushedBranches: %v", err)
	}
	if len(branches) != 0 {
		t.Errorf("nothing may be pushed when acceptance fails, got %v", branches)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("bead must not be closed when acceptance fails, closed %v", backend.ClosedIDs)
	}
}

// A failed acceptance run routes the bead through the same path as a verify
// failure: the claim is released so the next iteration can pick it back up, and
// the failure is recorded in the attempt history handed to the next agent.
func TestShipAndFinalize_AcceptanceFailure_ReleasesClaimAndRecordsAttempt(t *testing.T) {
	dir, _ := setupTestDir(t)
	stubCountdown(t, false)
	probe := newAcceptanceProbe(t, false)

	backend := newAcceptanceBackend()
	fs := buildFinalizeSetup(t, dir, "ralph-acc2", "Feature needing acceptance", backend, shipResult{
		prNumber: 42,
	}, withAcceptance(probe.command))

	fs.loop.completeTask(context.Background(), fs.p)

	if len(fs.loop.taskAttempts) != 1 {
		t.Fatalf("expected the acceptance failure to be recorded as one attempt, got %d", len(fs.loop.taskAttempts))
	}
	if backend.ReopenedTask != "ralph-acc2" {
		t.Errorf("acceptance failure must release the claim for retry, reopened=%q", backend.ReopenedTask)
	}
}

// After an acceptance failure the next ship attempt re-runs the gate with a
// fresh countdown — the skip is never sticky.
func TestShipAndFinalize_AcceptanceRerunsOnNextShipAttempt(t *testing.T) {
	dir, _ := setupTestDir(t)
	shown := stubCountdown(t, false)
	probe := newAcceptanceProbe(t, false)

	backend := newAcceptanceBackend()
	fs := buildFinalizeSetup(t, dir, "ralph-acc3", "Feature needing acceptance", backend, shipResult{
		prNumber: 42,
	}, withAcceptance(probe.command))

	fs.loop.completeTask(context.Background(), fs.p)
	fs.loop.completeTask(context.Background(), fs.p)

	if probe.runs(t) != 2 {
		t.Errorf("each ship attempt must re-run acceptance, ran %d times", probe.runs(t))
	}
	if *shown != 2 {
		t.Errorf("each ship attempt must show a fresh countdown, shown %d times", *shown)
	}
}

// When the countdown expires with nobody watching, the acceptance command runs
// and the ship proceeds normally. It runs exactly once even though doShip calls
// Ship twice (push/PR then merge) — a machine-seizing suite must not be
// re-triggered by ship's own retry iterations.
func TestShipAndFinalize_CountdownExpiry_RunsAcceptanceOnceThenShips(t *testing.T) {
	dir, _ := setupTestDir(t)
	stubCountdown(t, false)
	probe := newAcceptanceProbe(t, true)

	backend := newAcceptanceBackend()
	fs := buildFinalizeSetup(t, dir, "ralph-acc4", "Feature needing acceptance", backend, shipResult{
		prNumber:     42,
		merged:       true,
		pushedBranch: "ralph/ralph-acc4-feature",
	}, withAcceptance(probe.command))

	out := fs.loop.completeTask(context.Background(), fs.p)

	if probe.runs(t) != 1 {
		t.Errorf("acceptance must run exactly once per ship, ran %d times", probe.runs(t))
	}
	if !out.merged {
		t.Error("ship must proceed after acceptance passes")
	}
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-acc4" {
		t.Errorf("expected CloseTask for ralph-acc4, got %v", backend.ClosedIDs)
	}
}

// Cancelling the countdown skips the acceptance run, and the ship continues
// anyway — but the bead records acceptance_skipped with the unix timestamp of
// the cancel so the task manager can see which ships went acceptance-unverified.
func TestShipAndFinalize_CountdownCancelled_SkipsAcceptanceAndRecordsMetadata(t *testing.T) {
	dir, _ := setupTestDir(t)
	stubCountdown(t, true)
	probe := newAcceptanceProbe(t, true)

	backend := newAcceptanceBackend()
	fs := buildFinalizeSetup(t, dir, "ralph-acc5", "Feature needing acceptance", backend, shipResult{
		prNumber:     42,
		merged:       true,
		pushedBranch: "ralph/ralph-acc5-feature",
	}, withAcceptance(probe.command))

	before := time.Now().Unix()
	out := fs.loop.completeTask(context.Background(), fs.p)
	after := time.Now().Unix()

	if probe.runs(t) != 0 {
		t.Errorf("cancelling the countdown must skip the acceptance command, ran %d times", probe.runs(t))
	}
	if !out.merged {
		t.Error("ship must continue normally after a cancelled acceptance gate")
	}

	raw, err := backend.GetMetadata("ralph-acc5", tasks.MetadataAcceptanceSkipped)
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if raw == "" {
		t.Fatalf("cancelled acceptance must record %s metadata on the bead", tasks.MetadataAcceptanceSkipped)
	}
	ts, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("%s = %q, want a unix timestamp: %v", tasks.MetadataAcceptanceSkipped, raw, err)
	}
	if ts < before || ts > after {
		t.Errorf("%s = %d, want a timestamp within [%d, %d]", tasks.MetadataAcceptanceSkipped, ts, before, after)
	}
}

// A project with no acceptance command configured is entirely unaffected: no
// countdown is shown, nothing is run, and the ship behaves exactly as before.
func TestShipAndFinalize_NoAcceptanceCommand_GateFullyDisabled(t *testing.T) {
	dir, _ := setupTestDir(t)
	shown := stubCountdown(t, true)

	backend := newAcceptanceBackend()
	fs := buildFinalizeSetup(t, dir, "ralph-acc6", "Ordinary feature", backend, shipResult{
		prNumber:     42,
		merged:       true,
		pushedBranch: "ralph/ralph-acc6-feature",
	})

	out := fs.loop.completeTask(context.Background(), fs.p)

	if *shown != 0 {
		t.Errorf("no countdown may be shown when no acceptance command is configured, shown %d times", *shown)
	}
	if !out.merged {
		t.Error("ship must proceed unchanged when the gate is disabled")
	}
	raw, _ := backend.GetMetadata("ralph-acc6", tasks.MetadataAcceptanceSkipped)
	if raw != "" {
		t.Errorf("disabled gate must not record %s metadata, got %q", tasks.MetadataAcceptanceSkipped, raw)
	}
}

// The gate belongs to the ship path only: an iteration that fails verification
// never reaches ship, so a machine-seizing acceptance suite must not run there.
func TestCompleteTask_VerificationFailure_NeverRunsAcceptance(t *testing.T) {
	dir, st := setupTestDir(t)
	shown := stubCountdown(t, false)
	probe := newAcceptanceProbe(t, true)

	backend := newAcceptanceBackend()
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		HeadRev:    "after-sha",
		Ship:       git.ShipResult{PRNumber: 42},
	})
	cfg := Config{
		Dirs:                workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
		AcceptanceCommand:   probe.command,
		AcceptanceCountdown: time.Second,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook:  &stubVerifyHook{passed: false, reason: "tests failed"},
	})

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "before-sha",
		workDir:    dir,
		taskID:     "ralph-acc7",
		nextTask:   "Feature that fails verification",
	})

	if out.action != signalRetry {
		t.Fatalf("expected verification failure to retry, got action %v", out.action)
	}
	if probe.runs(t) != 0 {
		t.Errorf("acceptance must not run when verification fails before ship, ran %d times", probe.runs(t))
	}
	if *shown != 0 {
		t.Errorf("no countdown may be shown outside the ship path, shown %d times", *shown)
	}
}
