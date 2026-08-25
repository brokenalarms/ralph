package loop

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// The phase-2 merge call errored, so doShip must hand the error back on the
// shipOutcome. Without it the caller cannot tell "merge errored" from "merge
// not attempted", which is what let an unclassified failure look like a
// benign not-yet-merged PR.
func TestDoShip_MergeError_PopulatesShipErr(t *testing.T) {
	dir, st := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	mergeErr := errors.New("gh: merge exploded")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		HeadRev:    "after-sha",
		Ship: git.ShipResult{
			PRNumber:     87,
			PRURL:        "https://github.com/owner/repo/pull/87",
			PushedBranch: "ralph/ralph-mergeerr",
		},
		ShipMergeErr: mergeErr,
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
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook:  passingVerifyHook(),
	})

	out := l.doShip(context.Background(), "ralph-mergeerr", "Task whose merge errors", "did the thing", "", dir)

	if !errors.Is(out.shipErr, mergeErr) {
		t.Errorf("shipErr = %v, want the merge error %v", out.shipErr, mergeErr)
	}
	if out.prNumber != 87 {
		t.Errorf("prNumber = %d, want 87 — the PR was created before the merge failed", out.prNumber)
	}
	if out.merged {
		t.Error("merged = true — the merge call errored")
	}
}

// Ship fails with an error that is neither a CI failure nor an unresolved
// conflict, so the outcome carries no classifying flag. The bead MUST stay
// open: closing an unclassified failure as merge-pending is what closed
// cablecar-ce0 and seeded the next two beads onto unmerged PR #87. The
// following iteration for the same bead finds the still-open PR and re-enters
// shipAndFinalize through the prior-attempt path, so the bead retries.
func TestFinalizePR_UnclassifiedShipFailure_LeavesBeadOpen(t *testing.T) {
	dir, st := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend:  testutil.StubBackend{Remaining: 1, Total: 1},
			ExternalRefs: map[string]string{},
		},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        dir,
		HeadRev:        "after-sha",
		RemoteURL:      "https://github.com/owner/repo.git",
		WorktreeBranch: "ralph/ralph-unclassified",
		Ship: git.ShipResult{
			PRNumber:     91,
			PRURL:        "https://github.com/owner/repo/pull/91",
			PushedBranch: "ralph/ralph-unclassified",
		},
		ShipMergeErr: errors.New("gh: unexpected merge state"),
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs: []git.StubPR{{
				Number: 91,
				Branch: "ralph/ralph-unclassified",
				State:  git.PRStateOpen,
			}},
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
		taskID:     "ralph-unclassified",
		nextTask:   "Task whose ship outcome is unclassified",
	}

	out := l.shipAndFinalize(context.Background(), p)

	backend.CloseMu.Lock()
	closed := append([]string(nil), backend.ClosedIDs...)
	backend.CloseMu.Unlock()
	if len(closed) != 0 {
		t.Fatalf("bead must stay open on an unclassified ship failure, got CloseTask for %v", closed)
	}
	backend.SkipMu.Lock()
	skipped := append([]string(nil), backend.SkippedIDs...)
	backend.SkipMu.Unlock()
	if len(skipped) != 0 {
		t.Errorf("bead must stay open, not be skipped, got SkipTask for %v", skipped)
	}
	if out.merged {
		t.Error("merged = true — the PR was never merged")
	}

	var logLine string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "without a classified outcome") {
			logLine = line
			break
		}
	}
	if logLine == "" {
		t.Fatalf("expected a log line reporting the unclassified outcome, got: %s", buf.String())
	}
	if !strings.Contains(logLine, logging.Red) {
		t.Errorf("unclassified-outcome line must be logged at Error level, got: %q", logLine)
	}

	tagged := gm.(git.StubInspector).GetTagTaskEndTasks()
	if len(tagged) == 0 || tagged[len(tagged)-1] != "ralph-unclassified" {
		t.Errorf("TagTaskEnd calls = %v, want the task tagged as ended", tagged)
	}

	// Next iteration for the same bead: no new commits, but the PR from the
	// failed attempt is still open, so completeTask must route back through
	// shipAndFinalize via the prior-attempt path rather than closing or
	// stacking on the branch.
	buf.Reset()
	p.headBefore = "after-sha"
	l.completeTask(context.Background(), p)

	if !strings.Contains(buf.String(), "Found open PR #91 from prior attempt") {
		t.Errorf("next iteration must re-enter shipAndFinalize via the prior-attempt path, got: %s", buf.String())
	}
}
