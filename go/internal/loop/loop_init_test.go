package loop

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies that Loop.Run does NOT call DetectActiveReviewers at startup when no
// tasks are available — reviewer detection is deferred until a task actually runs.
func TestLoop_ActiveReviewersNotDetectedAtStartup(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		RemoteURL:  "https://github.com/owner/repo.git",
		GitHub: git.StubGitHubConfig{
			Reviewers: []git.Reviewer{
				{AppSlug: "copilot-code-review", BotUsername: "copilot-pull-request-reviewer", DefaultTimeout: 120 * time.Second, ReviewOnPush: true},
			},
		},
	})
	backend := &testutil.StubBackend{Remaining: 0, Completed: 0, Total: 0}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, RalphDir: ralphDir},
		MaxIterations: 1,
	}

	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		Runner:       &stubRunner{result: claude.Result{}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately — no tasks will run
	_ = l.Run(ctx)

	// DetectActiveReviewers is gated by ensureActiveReviewers, which sets
	// reviewersDetected=true on entry. With no tasks running, that gate is
	// never passed, so reviewersDetected remains false.
	if l.reviewersDetected {
		t.Error("reviewersDetected should be false when no tasks ran")
	}
	if len(l.activeReviewers) != 0 {
		t.Errorf("activeReviewers should be empty when detection did not run, got %d", len(l.activeReviewers))
	}
}

// Verifies that ensureActiveReviewers populates activeReviewers on first call
// and caches the result — DetectActiveReviewers is not called a second time.
func TestLoop_EnsureActiveReviewers_LazyInitAndCache(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	reviewer := git.Reviewer{AppSlug: "copilot-code-review", BotUsername: "copilot-pull-request-reviewer", DefaultTimeout: 120 * time.Second, ReviewOnPush: true}
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		RemoteURL:  "https://github.com/owner/repo.git",
		GitHub:     git.StubGitHubConfig{Reviewers: []git.Reviewer{reviewer}},
	})
	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, RalphDir: ralphDir},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{},
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	// Before first call: no reviewers cached, detection not run.
	if l.reviewersDetected {
		t.Fatal("reviewersDetected should be false before first ensureActiveReviewers call")
	}

	l.ensureActiveReviewers(context.Background())

	if !l.reviewersDetected {
		t.Error("reviewersDetected should be true after first call")
	}
	if len(l.activeReviewers) != 1 {
		t.Fatalf("expected 1 reviewer after lazy init, got %d", len(l.activeReviewers))
	}
	if l.activeReviewers[0].BotUsername != "copilot-pull-request-reviewer" {
		t.Errorf("expected copilot bot username, got %q", l.activeReviewers[0].BotUsername)
	}

	// Second call must no-op: the cache gate is reviewersDetected=true.
	// ensureActiveReviewers returns early before calling DetectActiveReviewers
	// when that flag is set. After a second call, activeReviewers is
	// unchanged (still populated from the first call).
	l.ensureActiveReviewers(context.Background())
	if len(l.activeReviewers) != 1 || l.activeReviewers[0].BotUsername != "copilot-pull-request-reviewer" {
		t.Errorf("activeReviewers changed on second call — cache did not hold: %v", l.activeReviewers)
	}
}

// Verifies that the ensureReviewersFn closure passed to completeTaskParams
// triggers DetectActiveReviewers when called (as it is during finalizePR),
// proving detection is deferred to the finalize phase rather than happening
// before the agent runs.
func TestLoop_ReviewersDetectedViaEnsureReviewersFn(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	reviewer := git.Reviewer{AppSlug: "copilot-code-review", BotUsername: "copilot-pull-request-reviewer", DefaultTimeout: 120 * time.Second, ReviewOnPush: true}
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
		RemoteURL:  "https://github.com/owner/repo.git",
		GitHub:     git.StubGitHubConfig{Reviewers: []git.Reviewer{reviewer}},
	})
	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, RalphDir: ralphDir},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{},
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	// Build the same ensureReviewersFn closure that loop.go passes to completeTaskParams.
	ensureReviewersFn := func() []git.Reviewer { l.ensureActiveReviewers(context.Background()); return l.activeReviewers }

	// Before the closure is called, detection must not have run.
	if l.reviewersDetected {
		t.Fatal("reviewersDetected should be false before ensureReviewersFn is invoked")
	}

	// Calling the closure (as finalizePRFn does) must trigger detection.
	reviewers := ensureReviewersFn()

	if !l.reviewersDetected {
		t.Error("reviewersDetected should be true after ensureReviewersFn is invoked")
	}
	if len(reviewers) != 1 || reviewers[0].BotUsername != "copilot-pull-request-reviewer" {
		t.Errorf("ensureReviewersFn should return detected reviewers, got %v", reviewers)
	}

	// Second call must not re-run detection; observable via activeReviewers
	// remaining unchanged.
	snapshot := append([]git.Reviewer(nil), l.activeReviewers...)
	_ = ensureReviewersFn()
	if len(l.activeReviewers) != len(snapshot) || l.activeReviewers[0].BotUsername != snapshot[0].BotUsername {
		t.Errorf("activeReviewers changed across calls — cache not preserved: %v", l.activeReviewers)
	}
}

// Verifies that initWorktree is a no-op when the git context has no worktree
// branch (i.e. running in the project dir directly), proving the guard
// condition works.
func TestInitWorktree_NoopWhenNoWorktreeBranch(t *testing.T) {
	dir, st := setupTestDir(t)

	backend := &testutil.StubBackend{}
	// Stub with WorkDir == ProjectDir means no worktree → early return.
	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	err := l.initWorktree(context.Background())
	if err != nil {
		t.Fatalf("initWorktree returned error for no-worktree case: %v", err)
	}
}

// Verifies that initWorktree frames a leftover previous-session branch as
// such, rather than emitting a bare "Branch: %s" line that reads as if the
// new run were actively on the old task's branch. At this point in startup
// PrepareForNextTask has not run yet, so GetWorktreeBranch still reports the
// previous session's task branch.
func TestInitWorktree_FramesLeftoverPreviousSessionBranch(t *testing.T) {
	dir, st := setupTestDir(t)

	staleBranch := "ralph/tabi-sk3y-tabi-ui-introduce-generic"
	backend := &testutil.StubBackend{}
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "wt"),
		WorktreeBranch: staleBranch,
	})
	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: filepath.Join(dir, "wt"), RalphDir: filepath.Join(dir, ".ralph")},
	}
	var buf bytes.Buffer
	logger := logging.NewWithWriter(&buf)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	if err := l.initWorktree(context.Background()); err != nil {
		t.Fatalf("initWorktree returned error: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "Branch: "+staleBranch) {
		t.Errorf("expected bare 'Branch: %s' to be replaced with previous-session framing, got log: %s", staleBranch, out)
	}
	wantMsg := "Worktree on previous-session branch " + staleBranch + " — will reset for next task"
	if !strings.Contains(out, wantMsg) {
		t.Errorf("expected log to contain %q, got: %s", wantMsg, out)
	}
}

// Verifies that initWorktree keeps the plain "Branch: %s" form when the
// worktree is already on the WIP placeholder branch — no previous-session
// framing is needed since there is no leftover branch to explain.
func TestInitWorktree_PlainBranchLineWhenOnWipBranch(t *testing.T) {
	dir, st := setupTestDir(t)

	wipBranch := git.WipBranchName()
	backend := &testutil.StubBackend{}
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "wt"),
		WorktreeBranch: wipBranch,
	})
	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: filepath.Join(dir, "wt"), RalphDir: filepath.Join(dir, ".ralph")},
	}
	var buf bytes.Buffer
	logger := logging.NewWithWriter(&buf)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	if err := l.initWorktree(context.Background()); err != nil {
		t.Fatalf("initWorktree returned error: %v", err)
	}

	out := buf.String()
	wantMsg := "Branch: " + wipBranch
	if !strings.Contains(out, wantMsg) {
		t.Errorf("expected log to contain %q, got: %s", wantMsg, out)
	}
	if strings.Contains(out, "previous-session branch") {
		t.Errorf("did not expect previous-session framing when already on WIP branch, got: %s", out)
	}
}

// Loop.Run fails the startup preflight — before any task is selected — when
// origin has a bare branch ref squatting the ralph/ namespace root
// (refs/heads/ralph). Such a ref blocks every ralph/<task> push with a git
// "directory file conflict" rejection, so this must be caught loudly at
// startup rather than surfacing as a cryptic push failure mid-iteration.
func TestLoop_Run_FailsStartupPreflight_WhenBranchNamespaceSquatted(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	squatErr := fmt.Errorf("origin has a branch at refs/heads/ralph (sha deadbeef) squatting the ralph/ branch namespace")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:                   dir,
		WorkDir:                      dir,
		CheckBranchNamespaceSquatErr: squatErr,
	})
	// A task is ready to run — proves the preflight failure, not an empty
	// backlog, is what stops the loop before task selection.
	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextID: "ralph-abc", NextTask: "fix something"}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, RalphDir: ralphDir},
		MaxIterations: 5,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		Runner:       &stubRunner{result: claude.Result{}},
	})

	err := l.Run(context.Background())
	if err == nil {
		t.Fatal("expected Run to return an error when origin has a squatting ralph ref")
	}
	if !strings.Contains(err.Error(), "refs/heads/ralph") {
		t.Errorf("expected error to name the squatting ref, got: %v", err)
	}
	if !strings.Contains(err.Error(), "git push origin --delete ralph") {
		t.Errorf("expected error to give the remediation command, got: %v", err)
	}

	// BranchForTask is only called once task selection and iteration begin —
	// zero calls proves the preflight failure stopped the loop before that.
	if calls := gm.(git.StubInspector).GetBranchForTaskCalls(); calls != 0 {
		t.Errorf("expected no task branch setup after a failed preflight, got %d BranchForTask call(s)", calls)
	}
}
