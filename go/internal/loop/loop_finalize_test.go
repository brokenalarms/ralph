package loop

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verifier"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// shipResult holds the values returned by a ship stub for finalize tests.
type shipResult struct {
	prNumber        int
	prURL           string
	merged          bool
	ciFailure       bool
	stacked         bool
	ciInfraFailure  bool                 // InfrastructureFailure flag on ShipResult
	ciFailureDetail *git.CIFailureError  // populated when the loop needs to route through tryFixCI
	pushedBranch    string               // PushedBranch flag: non-empty when Phase 1 push succeeded on this branch
	noNetChange     bool                 // NoNetChange flag: branch pushed but its diff vs main is empty
	conflictDetail  *git.UnresolvedConflictError // populated when the loop needs to route through tryFixConflict
}

// finalizeSetup bundles a Loop and pre-built params for finalize tests.
type finalizeSetup struct {
	loop *Loop
	gm   git.Ops
	st   *state.Store
	p    completeTaskParams
}

// buildFinalizeSetup constructs a Loop with a static-world stub where Ship
// returns a single ShipResult carrying the prescribed outcome. doShip calls
// Ship twice (phase 1 push+PR, phase 2 merge); the static stub returns the
// same result for both calls, and the loop's phase-2 logic only inspects
// fields (PRNumber, Merged, CIFailure, Stacked) whose values describe the
// end state and apply equally to both calls.
func buildFinalizeSetup(t *testing.T, dir, taskID, nextTask string, backend *testutil.TrackingBackend, ship shipResult) finalizeSetup {
	t.Helper()
	_, st := setupTestDir(t)
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		HeadRev:    "after-sha",
		Ship: git.ShipResult{
			PRNumber:              ship.prNumber,
			PRURL:                 ship.prURL,
			Merged:                ship.merged,
			CIFailure:             ship.ciFailure,
			Stacked:               ship.stacked,
			InfrastructureFailure: ship.ciInfraFailure,
			CIFailureDetail:       ship.ciFailureDetail,
			PushedBranch:          ship.pushedBranch,
			NoNetChange:           ship.noNetChange,
			ConflictDetail:        ship.conflictDetail,
		},
	})
	autoMerge := ship.merged || ship.ciFailure || ship.stacked || ship.conflictDetail != nil
	cfg := Config{
		AutoMerge: autoMerge,
		Dirs:      workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook: passingVerifyHook(),
	})
	return finalizeSetup{
		loop: l,
		gm:   gm,
		st:   st,
		p: completeTaskParams{
			result:     claude.Result{SignalDetected: true, OnSignalUsed: true},
			headBefore: "before-sha",
			workDir:    dir,
			taskID:     taskID,
			nextTask:   nextTask,
		},
	}
}

// doShip returns a single shipOutcome struct (not a multi-value tuple) whose
// fields carry the PR number, merge state, and pushed branch through to the
// caller.
func TestDoShip_ReturnsShipOutcomeStruct(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-shipout", "Add feature", backend, shipResult{
		prNumber:     7,
		prURL:        "https://example.com/pr/7",
		merged:       true,
		pushedBranch: "ralph/ralph-shipout",
	})

	out := fs.loop.doShip(context.Background(), "ralph-shipout", "Add feature", "did the thing", "", dir)

	if out.prNumber != 7 {
		t.Errorf("prNumber = %d, want 7", out.prNumber)
	}
	if out.prResultURL != "https://example.com/pr/7" {
		t.Errorf("prResultURL = %q, want the stubbed PR URL", out.prResultURL)
	}
	if !out.merged {
		t.Error("merged = false, want true")
	}
	if out.pushedBranch != "ralph/ralph-shipout" {
		t.Errorf("pushedBranch = %q, want the stubbed branch", out.pushedBranch)
	}
	if out.shipErr != nil {
		t.Errorf("shipErr = %v, want nil", out.shipErr)
	}
}

// completeTask closes the bead when AutoMerge is off (ship returns merged=false).
func TestFinalizePR_NoAutoMerge_ClosesTask(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-abc", "Fix auth bug", backend, shipResult{
		prNumber: 42,
		merged:   false,
	})

	out := fs.loop.completeTask(context.Background(), fs.p)

	if out.merged {
		t.Error("should not merge when AutoMerge is disabled")
	}
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-abc" {
		t.Errorf("expected CloseTask for ralph-abc, got %v", backend.ClosedIDs)
	}
}

// completeTask merges and closes when ship returns merged=true.
func TestFinalizePR_AutoMerge_MergesAndCloses(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-xyz", "Add feature", backend, shipResult{
		prNumber: 42,
		merged:   true,
	})

	out := fs.loop.completeTask(context.Background(), fs.p)

	if !out.merged {
		t.Error("should be merged when ship returns merged=true")
	}
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-xyz" {
		t.Errorf("expected CloseTask for ralph-xyz, got %v", backend.ClosedIDs)
	}
}

// completeTask closes the bead (not skips) when merge fails transiently —
// no unresolved conflict, work is verified, and the branch is findable by
// stack head detection for the next task. Contrast with
// TestFinalizePR_UnresolvableConflict_SkipsInsteadOfCloses: only an
// unresolvable conflict downgrades the close to a skip.
func TestFinalizePR_MergeFailure_ClosesTask(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-abc", "Fix bug", backend, shipResult{
		prNumber: 99,
		merged:   false,
		prURL:    "https://github.com/owner/repo/pull/99",
	})

	out := fs.loop.completeTask(context.Background(), fs.p)

	if out.merged {
		t.Error("should not be merged on failure")
	}
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-abc" {
		t.Errorf("expected CloseTask for ralph-abc, got %v", backend.ClosedIDs)
	}
	if len(backend.CloseReasons) == 0 || !strings.Contains(backend.CloseReasons[0], "merge pending") {
		t.Errorf("close reason should indicate merge pending, got %v", backend.CloseReasons)
	}
}

// An unresolvable merge conflict — auto-rebase failed and the conflict fix
// agent could not resolve it — skips the bead with merge_failed instead of
// closing it as merge-pending. Closing would abandon the PR to rot into
// deeper conflict (tabi PR #1524) and advance dependents onto a main that
// lacks this bead's changes; the skip surfaces the PR to triage.
func TestFinalizePR_UnresolvableConflict_SkipsInsteadOfCloses(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-abc", "Fix bug", backend, shipResult{
		prNumber:       99,
		merged:         false,
		prURL:          "https://github.com/owner/repo/pull/99",
		conflictDetail: &git.UnresolvedConflictError{},
	})

	out := fs.loop.completeTask(context.Background(), fs.p)

	if out.merged {
		t.Error("should not be merged on unresolvable conflict")
	}
	backend.CloseMu.Lock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("CloseTask must not be called for an unresolvable conflict, got %v", backend.ClosedIDs)
	}
	backend.CloseMu.Unlock()
	backend.SkipMu.Lock()
	defer backend.SkipMu.Unlock()
	if len(backend.SkippedIDs) != 1 || backend.SkippedIDs[0] != "ralph-abc" {
		t.Fatalf("expected SkipTask for ralph-abc, got %v", backend.SkippedIDs)
	}
	if backend.SkipReasons[0] != string(tasks.SkipMergeFailed) {
		t.Errorf("expected skip reason %q, got %q", tasks.SkipMergeFailed, backend.SkipReasons[0])
	}
	if !strings.Contains(backend.SkipDetails[0], "pull/99") {
		t.Errorf("skip detail should carry the PR reference, got %q", backend.SkipDetails[0])
	}
}

// When CI fails, completeTask leaves the task open for manual investigation.
func TestFinalizePR_CIFixExhausted_TaskStaysOpen(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-abc", "Fix failing tests", backend, shipResult{
		prNumber:  99,
		ciFailure: true,
	})

	out := fs.loop.completeTask(context.Background(), fs.p)

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("CloseTask must not be called when CI failure, got %v", backend.ClosedIDs)
	}
	if out.ct != nil {
		t.Errorf("CI failure must not return CompletedTask (would prevent retry), got %+v", out.ct)
	}
}

// CIFailure from ship leaves the task open.
func TestFinalizePR_CIFailure_TaskStaysOpen(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-abc", "Fix failing tests", backend, shipResult{
		prNumber:  99,
		ciFailure: true,
	})

	out := fs.loop.completeTask(context.Background(), fs.p)

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("CloseTask must not be called when CI fails, got %v", backend.ClosedIDs)
	}
	if out.ct != nil {
		t.Errorf("CI failure must not return CompletedTask (would prevent retry), got %+v", out.ct)
	}
}

// When merge fails, the task still appears in completed_tasks with merged=false
// so stack head detection can find the unmerged branch for the next task.
func TestFinalizePR_MergeFailure_AppearsInCompletedTasksWithMergedFalse(t *testing.T) {
	dir, st := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		HeadRev:    "after-sha",
		Ship:       git.ShipResult{PRNumber: 99, Merged: false},
	})
	cfg := Config{
		AutoMerge: false,
		Dirs:      workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook: passingVerifyHook(),
	})

	p := completeTaskParams{
		result:     claude.Result{SignalDetected: true, OnSignalUsed: true},
		headBefore: "before-sha",
		workDir:    dir,
		taskID:     "ralph-abc",
		nextTask:   "Fix bug",
	}

	l.completeTask(context.Background(), p)

	tasks, err := st.GetCompletedTasks()
	if err != nil {
		t.Fatalf("GetCompletedTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 entry in completed_tasks, got %d", len(tasks))
	}
	if tasks[0].ID != "ralph-abc" {
		t.Errorf("completed task ID = %q, want ralph-abc", tasks[0].ID)
	}
	if tasks[0].Merged {
		t.Error("merged should be false for unmerged PR")
	}
}

// When ship returns merged=true, completeTask closes the bead immediately.
func TestFinalizePR_AlreadyMerged_ClosesImmediately(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-xyz", "Add feature", backend, shipResult{
		prNumber: 42,
		merged:   true,
	})

	out := fs.loop.completeTask(context.Background(), fs.p)

	if !out.merged {
		t.Error("should report merged when ship returns merged=true")
	}
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-xyz" {
		t.Errorf("expected CloseTask for ralph-xyz, got %v", backend.ClosedIDs)
	}
}

// completeTask uses PR URL in close reason when available.
func TestFinalizePR_UsesURLInCloseReason(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-abc", "Fix login", backend, shipResult{
		prNumber: 55,
		prURL:    "https://github.com/owner/repo/pull/55",
		merged:   false,
	})

	fs.loop.completeTask(context.Background(), fs.p)

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.CloseReasons) == 0 {
		t.Fatal("expected a close reason")
	}
	if !strings.Contains(backend.CloseReasons[0], "https://github.com/owner/repo/pull/55") {
		t.Errorf("close reason should contain PR URL, got %q", backend.CloseReasons[0])
	}
}

// CloseTask failure after merge skips the task so the loop doesn't retry it.
func TestFinalizePR_CloseTaskFailure_SkipsTask(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		CloseErr:       fmt.Errorf("exit status 1"),
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-abc", "Fix auth bug", backend, shipResult{
		prNumber: 42,
		merged:   true,
	})

	out := fs.loop.completeTask(context.Background(), fs.p)

	if !out.merged {
		t.Error("PR should still report merged")
	}

	backend.SkipMu.Lock()
	defer backend.SkipMu.Unlock()
	found := false
	for _, id := range backend.SkippedIDs {
		if id == "ralph-abc" {
			found = true
		}
	}
	if !found {
		t.Errorf("task should be skipped after CloseTask failure, skipped=%v", backend.SkippedIDs)
	}

	for i, id := range backend.SkippedIDs {
		if id == "ralph-abc" {
			if !strings.Contains(backend.SkipReasons[i], "close_failed") {
				t.Errorf("skip reason should contain 'close_failed', got %q", backend.SkipReasons[i])
			}
		}
	}
}

// Dependency-blocked CloseTask failure includes blocker IDs in skip reason.
func TestFinalizePR_DependencyBlockedClose_SkipsWithBlockerIDs(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		CloseErr:       fmt.Errorf("exit status 1: cannot close ralph-abc: blocked by open issues [ralph-dep1] (use --force to override)"),
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-abc", "Fix auth bug", backend, shipResult{
		prNumber: 42,
		merged:   true,
	})

	fs.loop.completeTask(context.Background(), fs.p)

	backend.SkipMu.Lock()
	defer backend.SkipMu.Unlock()
	found := false
	for i, id := range backend.SkippedIDs {
		if id == "ralph-abc" {
			found = true
			if backend.SkipReasons[i] != string(tasks.SkipDependencyBlocked) {
				t.Errorf("skip reason should be %q, got %q", tasks.SkipDependencyBlocked, backend.SkipReasons[i])
			}
			if backend.SkipDetails[i] != "ralph-dep1" {
				t.Errorf("skip detail should contain blocker ID, got %q", backend.SkipDetails[i])
			}
		}
	}
	if !found {
		t.Errorf("task should be skipped, skipped=%v", backend.SkippedIDs)
	}
}

// Ship reports CI infrastructure failure (zero job steps): work is verified
// locally, CI never actually ran. completeTask must close the bead with a
// "merge pending CI recovery" reason and MUST NOT spawn the CI fix agent —
// there is no test failure for the agent to address.
func TestFinalizePR_CIInfraFailure_ClosesBeadAndSkipsFixAgent(t *testing.T) {
	dir, st := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	// Spy runner factory: fails the test if the verifier is ever asked to
	// spawn a fix agent. In the infra-failure path this factory must not be
	// invoked — the short-circuit returns before tryFixCI runs.
	runnerInvoked := false
	failingRunner := &stubRunner{onRun: func() { runnerInvoked = true }}
	stubs := verifierTestStubs{
		newRunner:     func() verifier.Runner { return failingRunner },
		queryResponse: "NO: stub",
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		HeadRev:    "after-sha",
		Ship: git.ShipResult{
			PRNumber:              77,
			PRURL:                 "https://github.com/owner/repo/pull/77",
			Merged:                false,
			CIFailure:             true,
			InfrastructureFailure: true,
			CIFailureDetail:       &git.CIFailureError{PRNumber: 77},
		},
	})
	cfg := Config{
		AutoMerge: true,
		Dirs:      workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger, stubs),
		VerifyHook:  passingVerifyHook(),
	})

	p := completeTaskParams{
		result:     claude.Result{SignalDetected: true, OnSignalUsed: true},
		headBefore: "before-sha",
		workDir:    dir,
		taskID:     "ralph-infra",
		nextTask:   "Task whose PR hits CI infra failure",
	}

	out := l.completeTask(context.Background(), p)

	if runnerInvoked {
		t.Fatal("CI fix agent runner must not be invoked on infrastructure failure")
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-infra" {
		t.Fatalf("expected CloseTask for ralph-infra, got %v", backend.ClosedIDs)
	}
	if len(backend.CloseReasons) == 0 || !strings.Contains(backend.CloseReasons[0], "CI recovery") {
		t.Errorf("close reason should mention CI recovery, got %q", backend.CloseReasons)
	}
	if out.ct == nil {
		t.Error("infra failure should return a CompletedTask so the loop advances")
	}
	if out.merged {
		t.Error("infra failure must not report merged — PR is left open")
	}
}

// Stacked PR (ship returns stacked=true) closes the bead without merging.
func TestFinalizePR_StackedPR_ClosesWithoutMerge(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-stk1", "Stacked task", backend, shipResult{
		prNumber: 88,
		merged:   false,
		stacked:  true,
	})

	out := fs.loop.completeTask(context.Background(), fs.p)

	if out.merged {
		t.Error("merge should not be called for stacked PR")
	}
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) == 0 || backend.ClosedIDs[0] != "ralph-stk1" {
		t.Errorf("stacked PR should still close the task, got %v", backend.ClosedIDs)
	}
}

// When Phase 1's push succeeded but CreatePR failed, ship returns prNumber=0
// and a non-empty pushedBranch. completeTask must SKIP (not close) the bead —
// closing would orphan the pushed remote branch with no way for triage to
// find it. The skip reason must carry the branch name in a machine-parseable
// "pr_creation_failed:<branch>" form so downstream tooling can locate the
// orphan.
func TestFinalizePR_PushSucceededPRCreationFailed_SkipsWithBranch(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-orph", "Task whose PR fails to create", backend, shipResult{
		prNumber:     0,
		pushedBranch: "ralph/ralph-orph-task-whose-pr-fails",
	})

	out := fs.loop.completeTask(context.Background(), fs.p)

	backend.CloseMu.Lock()
	if len(backend.ClosedIDs) != 0 {
		backend.CloseMu.Unlock()
		t.Fatalf("CloseTask must not be called when push succeeded but CreatePR failed — that would orphan the branch. got %v", backend.ClosedIDs)
	}
	backend.CloseMu.Unlock()

	backend.SkipMu.Lock()
	defer backend.SkipMu.Unlock()
	found := false
	for i, id := range backend.SkippedIDs {
		if id != "ralph-orph" {
			continue
		}
		found = true
		reason := backend.SkipReasons[i]
		if reason != string(tasks.SkipPRCreationFailed) {
			t.Errorf("skip reason must be %q, got %q", tasks.SkipPRCreationFailed, reason)
		}
		if backend.SkipDetails[i] != "ralph/ralph-orph-task-whose-pr-fails" {
			t.Errorf("skip detail must carry pushed branch name, got %q", backend.SkipDetails[i])
		}
	}
	if !found {
		t.Errorf("bead ralph-orph must be skipped when push succeeded but CreatePR failed, skipped=%v", backend.SkippedIDs)
	}

	if out.ct != nil {
		t.Errorf("pushed-but-no-PR path must not return a CompletedTask (nothing to record), got %+v", out.ct)
	}
	if out.merged {
		t.Error("pushed-but-no-PR path must not report merged")
	}
}

// Loop.skipTask reassigns the bead via SkipTask and tracks it in sessionSkippedIDs.
func TestSkipTask_ReassignsBeadAndTracksInSession(t *testing.T) {
	dir, st := setupTestDir(t)
	backend := &testutil.StubBackend{}
	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	l.skipTask("ralph-xyz", "merge_failed", "")

	if backend.SkippedTask != "ralph-xyz" {
		t.Errorf("expected backend.SkippedTask=ralph-xyz, got %q", backend.SkippedTask)
	}
	if backend.SkipReason != "merge_failed" {
		t.Errorf("expected backend.SkipReason=merge_failed, got %q", backend.SkipReason)
	}
	if !l.sessionSkippedIDs["ralph-xyz"] {
		t.Error("expected ralph-xyz to be tracked in sessionSkippedIDs")
	}
}

// Loop.skipTask is a no-op with empty ID.
func TestSkipTask_EmptyID(t *testing.T) {
	dir, st := setupTestDir(t)
	backend := &testutil.StubBackend{}
	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	l.skipTask("", "reason", "")

	if backend.SkippedTask != "" {
		t.Error("expected no skip with empty ID")
	}
}

// isActionableComment classifies comments correctly.
func TestIsActionableComment(t *testing.T) {
	tests := []struct {
		name       string
		comment    git.ReviewComment
		actionable bool
	}{
		{
			name:       "suggestion block",
			comment:    git.ReviewComment{Path: "foo.go", Line: 1, Body: "```suggestion\nfixed code\n```"},
			actionable: true,
		},
		{
			name:       "nil check keyword",
			comment:    git.ReviewComment{Path: "foo.go", Line: 5, Body: "Missing nil check before using ptr"},
			actionable: true,
		},
		{
			name:       "bug keyword",
			comment:    git.ReviewComment{Path: "bar.go", Line: 10, Body: "This is a bug — value may overflow"},
			actionable: true,
		},
		{
			name:       "code block on file comment",
			comment:    git.ReviewComment{Path: "bar.go", Line: 3, Body: "Consider:\n```go\nreturn err\n```"},
			actionable: true,
		},
		{
			name:       "informational summary",
			comment:    git.ReviewComment{Path: "bar.go", Line: 1, Body: "This PR adds caching support to the auth layer."},
			actionable: false,
		},
		{
			name:       "empty body",
			comment:    git.ReviewComment{Path: "foo.go", Line: 1, Body: ""},
			actionable: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isActionableComment(tc.comment)
			if got != tc.actionable {
				t.Errorf("isActionableComment(%q) = %v, want %v", tc.comment.Body, got, tc.actionable)
			}
		})
	}
}

// formatReviewContext structures comments with file paths, line numbers, and reviewer name.
func TestFormatReviewContext(t *testing.T) {
	comments := []git.ReviewComment{
		{Path: "pkg/auth.go", Line: 42, Body: "Missing nil check"},
		{Path: "pkg/db.go", Line: 17, Body: "```suggestion\nreturn nil, err\n```"},
	}

	result := formatReviewContext("copilot-pull-request-reviewer", 99, comments)

	if !strings.Contains(result, "PR #99") {
		t.Error("context should mention PR number")
	}
	if !strings.Contains(result, "copilot-pull-request-reviewer") {
		t.Error("context should mention reviewer name")
	}
	if !strings.Contains(result, "pkg/auth.go:42") {
		t.Error("context should contain file:line for first comment")
	}
	if !strings.Contains(result, "Missing nil check") {
		t.Error("context should contain first comment body")
	}
	if !strings.Contains(result, "pkg/db.go:17") {
		t.Error("context should contain file:line for second comment")
	}
}

// closeOrSkip is the single close-or-skip helper shared by shipAndFinalize's
// three close sites and the no-PR near-variant. On a successful CloseTask it
// must persist the completion (with the given merged flag) and clear the
// current task.
func TestCloseOrSkip_Success_PersistsCompletionAndClearsCurrentTask(t *testing.T) {
	dir, st := setupTestDir(t)
	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}
	cfg := Config{Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: dir}}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})
	st.BeginIteration("ralph-close1", "Fix bug", 1)

	if ok := l.closeOrSkip("ralph-close1", "Fixed in PR #9", true); !ok {
		t.Fatal("expected closeOrSkip to report success")
	}

	backend.CloseMu.Lock()
	closed := append([]string(nil), backend.ClosedIDs...)
	backend.CloseMu.Unlock()
	if len(closed) != 1 || closed[0] != "ralph-close1" {
		t.Errorf("expected CloseTask(ralph-close1), got %v", closed)
	}

	completed, err := st.GetCompletedTasks()
	if err != nil {
		t.Fatalf("GetCompletedTasks: %v", err)
	}
	if len(completed) != 1 || completed[0].ID != "ralph-close1" || !completed[0].Merged {
		t.Errorf("expected one persisted completed task with merged=true, got %+v", completed)
	}

	loaded, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CurrentTaskID != "" {
		t.Errorf("expected CurrentTaskID cleared, got %q", loaded.CurrentTaskID)
	}
}

// When CloseTask fails because the bead is blocked by an open dependency,
// closeOrSkip must skip the task (rather than leaving it stuck in_progress)
// with the dependency-blocked reason and the blocker IDs as the detail.
func TestCloseOrSkip_DependencyBlockedFailure_SkipsWithBlockerDetail(t *testing.T) {
	dir, st := setupTestDir(t)
	backend := &testutil.TrackingBackend{
		CloseErr:       fmt.Errorf("exit status 1: cannot close ralph-close2: blocked by open issues [ralph-dep9] (use --force to override)"),
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}
	cfg := Config{Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: dir}}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	if ok := l.closeOrSkip("ralph-close2", "Fixed in PR #9", true); ok {
		t.Fatal("expected closeOrSkip to report failure")
	}

	backend.SkipMu.Lock()
	defer backend.SkipMu.Unlock()
	found := false
	for i, id := range backend.SkippedIDs {
		if id == "ralph-close2" {
			found = true
			if backend.SkipReasons[i] != string(tasks.SkipDependencyBlocked) {
				t.Errorf("skip reason = %q, want %q", backend.SkipReasons[i], tasks.SkipDependencyBlocked)
			}
			if backend.SkipDetails[i] != "ralph-dep9" {
				t.Errorf("skip detail = %q, want ralph-dep9", backend.SkipDetails[i])
			}
		}
	}
	if !found {
		t.Errorf("expected ralph-close2 to be skipped, skipped=%v", backend.SkippedIDs)
	}
}

// A branch whose commits net out to no change against main produces no PR,
// but the push did land. Before the fix that case fell into the
// pr_creation_failed path, reporting a net no-op as a PR-creation infra
// failure and inviting triage to retry it. It must be skipped as
// no_net_change instead, still carrying the branch name for triage.
func TestFinalizePR_NetEmptyBranch_SkipsAsNoNetChangeNotPRFailure(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	fs := buildFinalizeSetup(t, dir, "ralph-noop", "Task whose diff nets out empty", backend, shipResult{
		prNumber:     0,
		pushedBranch: "ralph/ralph-noop-net-empty",
		noNetChange:  true,
	})

	fs.loop.completeTask(context.Background(), fs.p)

	backend.SkipMu.Lock()
	defer backend.SkipMu.Unlock()
	found := false
	for i, id := range backend.SkippedIDs {
		if id != "ralph-noop" {
			continue
		}
		found = true
		if reason := backend.SkipReasons[i]; reason != string(tasks.SkipNoNetChange) {
			t.Errorf("skip reason = %q, want %q — a net no-op must not be reported as a PR-creation failure", reason, tasks.SkipNoNetChange)
		}
		if backend.SkipDetails[i] != "ralph/ralph-noop-net-empty" {
			t.Errorf("skip detail = %q, want the pushed branch name", backend.SkipDetails[i])
		}
	}
	if !found {
		t.Errorf("bead ralph-noop must be skipped for a net-empty branch, skipped=%v", backend.SkippedIDs)
	}
}

// The pre-merge local test run failed (no GitHub CI configured), so ship
// reports a CI failure carrying LocalTestDetail. The bead MUST stay open: on
// cablecar PR #87 the untyped failure was closed as "merge pending" and the
// next two beads were stacked on the unmerged, red PR while main stayed put.
// The log must name local tests (not GitHub CI), and the red branch must not
// be offered as a stack parent to the following iteration — that iteration
// starts from origin/main instead.
func TestFinalizePR_LocalTestFailure_LeavesBeadOpenAndOutOfStack(t *testing.T) {
	dir, st := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		HeadRev:    "after-sha",
		Ship: git.ShipResult{
			PRNumber:     87,
			PRURL:        "https://github.com/owner/repo/pull/87",
			Merged:       false,
			CIFailure:    true,
			PushedBranch: "ralph/ralph-localfail",
			LocalTestDetail: &git.LocalTestFailureError{
				PRNumber: 87,
				Reason:   "test suite failed: exit status 1",
				Details:  "--- FAIL: TestConnectDeadline",
			},
		},
	})
	cfg := Config{
		AutoMerge: true,
		Dirs:      workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
	}
	var buf bytes.Buffer
	logger := logging.New(&buf)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook:  passingVerifyHook(),
	})

	p := completeTaskParams{
		result:     claude.Result{SignalDetected: true, OnSignalUsed: true},
		headBefore: "before-sha",
		workDir:    dir,
		taskID:     "ralph-localfail",
		nextTask:   "Task whose pre-merge local tests fail",
	}

	out := l.shipAndFinalize(context.Background(), p)

	backend.CloseMu.Lock()
	closed := append([]string(nil), backend.ClosedIDs...)
	reasons := append([]string(nil), backend.CloseReasons...)
	backend.CloseMu.Unlock()

	if len(closed) != 0 {
		t.Fatalf("bead must stay open on a failed pre-merge local test run, got CloseTask for %v", closed)
	}
	for _, r := range reasons {
		if strings.Contains(r, "merge pending") {
			t.Errorf("bead was closed as merge-pending: %q", r)
		}
	}
	if out.merged {
		t.Error("merged = true — the PR was never merged")
	}

	output := buf.String()
	if !strings.Contains(output, "Local tests failing") {
		t.Errorf("expected the log to name local tests as the failure, got: %s", output)
	}

	// The following iteration must not stack on the red branch: it is absent
	// from the stack-parent candidates BranchForTask is given, so stack head
	// detection resolves nothing and the worktree branches off origin/main.
	next := taskContext{id: "ralph-nextbead", title: "Following task", changed: true}
	if outcome := l.setupBranchForTask(context.Background(), next); outcome != branchProceed {
		t.Fatalf("branch setup for the following task = %v, want branchProceed", outcome)
	}
	parents := gm.(git.StubInspector).GetBranchForTaskStackParents()
	for _, b := range parents {
		if b == "ralph/ralph-localfail" {
			t.Errorf("following iteration offered the red branch as a stack parent: %v", parents)
		}
	}
}
