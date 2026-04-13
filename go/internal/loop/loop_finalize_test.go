package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// shipResult holds the values returned by a ship stub for finalize tests.
type shipResult struct {
	prNumber  int
	prURL     string
	merged    bool
	ciFailure bool
	stacked   bool
}

// finalizeSetup bundles a Loop and pre-built params for finalize tests.
type finalizeSetup struct {
	loop *Loop
	gm   *git.StubRepo
	st   *state.Store
	p    completeTaskParams
}

// buildFinalizeSetup constructs a Loop with StubGit configured to return ship,
// and a data-only completeTaskParams with headBefore="before-sha" (ensuring
// the "new commits" path is taken since HeadRev returns "after-sha").
func buildFinalizeSetup(t *testing.T, dir, taskID, nextTask string, backend *testutil.TrackingBackend, ship shipResult) finalizeSetup {
	t.Helper()
	_, st := setupTestDir(t)
	gm := &git.StubRepo{
		ProjectDir:   dir,
		WorkDir:      dir,
		HeadRevValue: "after-sha",
		PRState:      git.PRStateOpen,
	}
	gm.ShipFunc = func(_ context.Context, opts git.ShipOpts) (git.ShipResult, error) {
		if opts.PRNumber == 0 {
			// Phase 1: push + PR creation
			return git.ShipResult{PRNumber: ship.prNumber, PRURL: ship.prURL}, nil
		}
		// Phase 2: merge
		return git.ShipResult{
			PRNumber:  opts.PRNumber,
			PRURL:     ship.prURL,
			Merged:    ship.merged,
			CIFailure: ship.ciFailure,
			Stacked:   ship.stacked,
		}, nil
	}
	autoMerge := ship.merged || ship.ciFailure || ship.stacked
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

// completeTask closes the bead (not skips) when merge fails — work is verified
// and the branch is findable by stack head detection for the next task.
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

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, HeadRevValue: "after-sha"}
	gm.ShipFunc = func(_ context.Context, opts git.ShipOpts) (git.ShipResult, error) {
		if opts.PRNumber == 0 {
			return git.ShipResult{PRNumber: 99}, nil
		}
		return git.ShipResult{PRNumber: opts.PRNumber, Merged: false}, nil
	}
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
			if !strings.Contains(backend.SkipReasons[i], "dependency_blocked_by:ralph-dep1") {
				t.Errorf("skip reason should contain blocker ID, got %q", backend.SkipReasons[i])
			}
		}
	}
	if !found {
		t.Errorf("task should be skipped, skipped=%v", backend.SkippedIDs)
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

// Loop.skipTask sets status to open in backend and persists to state.json.
func TestSkipTask_SetsOpenAndPersistsToState(t *testing.T) {
	dir, st := setupTestDir(t)
	backend := &testutil.StubBackend{}
	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         &git.StubRepo{ProjectDir: dir, WorkDir: dir},
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	l.skipTask("ralph-xyz", "merge_failed")

	if backend.SkippedTask != "ralph-xyz" {
		t.Errorf("expected backend.SkippedTask=ralph-xyz, got %q", backend.SkippedTask)
	}
	if backend.SkipReason != "merge_failed" {
		t.Errorf("expected backend.SkipReason=merge_failed, got %q", backend.SkipReason)
	}
	skipped, err := st.GetSkippedTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != "ralph-xyz" {
		t.Errorf("expected [ralph-xyz] in state.json skipped_tasks, got %v", skipped)
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
		Git:         &git.StubRepo{ProjectDir: dir, WorkDir: dir},
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	l.skipTask("", "reason")

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
