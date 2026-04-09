package loop

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies that initialize as a package function writes max_iterations to
// state and loads skipped tasks from state into the backend, proving both
// initialization side-effects work when called outside the Loop struct.
func TestInitialize_WritesConfigAndLoadsSkipped(t *testing.T) {
	dir, st := setupTestDir(t)

	st.AddSkippedTask("ralph-skip1")

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 0,
			Completed: 1,
			Total:     1,
		},
	}
	logger := logging.New(nil)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	err := initialize(context.Background(), initParams{
		maxIter: 7,
		state:   st,
		backend: backend,
		logger:  logger,
		git:     gm,
	})
	if err != nil {
		t.Fatalf("initialize returned error: %v", err)
	}

	// initialize must call state.WriteConfig(maxIter).
	if got := st.ReadMaxIterations(0); got != 7 {
		t.Errorf("expected max_iterations=7 in state, got %d", got)
	}

	// initialize must load skipped tasks from state into the backend.
	backend.Lock()
	skipped := backend.LastSkippedIDs
	backend.Unlock()
	if len(skipped) == 0 {
		t.Error("expected SetSkippedIDs to be called with skipped tasks from state")
	}
	last := skipped[len(skipped)-1]
	if len(last) == 0 || last[0] != "ralph-skip1" {
		t.Errorf("expected ['ralph-skip1'] in last SetSkippedIDs call, got %v", last)
	}
}

// Verifies that Loop.Run does NOT call DetectActiveReviewers at startup when no
// tasks are available — reviewer detection is deferred until a task actually runs.
func TestLoop_ActiveReviewersNotDetectedAtStartup(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	logger := logging.New(nil)

	gm := &testutil.StubGit{
		ProjectDir: dir,
		WorkDir:    dir,
		ActiveReviewers: []git.Reviewer{
			{AppSlug: "copilot-code-review", BotUsername: "copilot-pull-request-reviewer", DefaultTimeout: 120 * time.Second, ReviewOnPush: true},
		},
	}
	backend := &testutil.StubBackend{Remaining: 0, Completed: 0, Total: 0}
	cfg := Config{
		Dirs:            workctx.WorkContext{ProjectDir: dir, RalphDir: ralphDir},
		MaxIterations:   1,
		TaskBackend:     backend,
		IsOnline:        func() bool { return true },
		WaitForInternet: func(_ context.Context, _ *logging.Logger) bool { return true },
		NewRunner:       func() claudeRunner { return &stubRunner{result: claude.Result{}} },
		QueryFn:         func(_ context.Context, _, _, _ string) (string, error) { return "", nil },
	}

	l := New(cfg, st, gm, logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately — no tasks will run
	_ = l.Run(ctx)

	// With no tasks, DetectActiveReviewers must not be called at startup.
	if gm.DetectActiveReviewersCalled {
		t.Error("DetectActiveReviewers should not be called at startup when no tasks are available")
	}
	if l.reviewersDetected {
		t.Error("reviewersDetected should be false when no tasks ran")
	}
}

// Verifies that ensureActiveReviewers populates activeReviewers on first call
// and caches the result — DetectActiveReviewers is not called a second time.
func TestLoop_EnsureActiveReviewers_LazyInitAndCache(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	logger := logging.New(nil)

	reviewer := git.Reviewer{AppSlug: "copilot-code-review", BotUsername: "copilot-pull-request-reviewer", DefaultTimeout: 120 * time.Second, ReviewOnPush: true}
	gm := &testutil.StubGit{
		ProjectDir:      dir,
		WorkDir:         dir,
		ActiveReviewers: []git.Reviewer{reviewer},
	}
	cfg := Config{
		Dirs:        workctx.WorkContext{ProjectDir: dir, RalphDir: ralphDir},
		TaskBackend: &testutil.StubBackend{},
	}
	l := New(cfg, st, gm, logger)

	// Before first call: no reviewers cached, detection not run.
	if l.reviewersDetected {
		t.Fatal("reviewersDetected should be false before first ensureActiveReviewers call")
	}

	l.ensureActiveReviewers()

	if !l.reviewersDetected {
		t.Error("reviewersDetected should be true after first call")
	}
	if len(l.activeReviewers) != 1 {
		t.Fatalf("expected 1 reviewer after lazy init, got %d", len(l.activeReviewers))
	}
	if l.activeReviewers[0].BotUsername != "copilot-pull-request-reviewer" {
		t.Errorf("expected copilot bot username, got %q", l.activeReviewers[0].BotUsername)
	}

	// Second call must not call DetectActiveReviewers again.
	prevCount := gm.DetectActiveReviewersCallCount
	l.ensureActiveReviewers()
	if gm.DetectActiveReviewersCallCount != prevCount {
		t.Error("DetectActiveReviewers called more than once — cache not working")
	}
}

// Verifies that the ensureReviewersFn closure passed to completeTaskParams
// triggers DetectActiveReviewers when called (as it is during finalizePR),
// proving detection is deferred to the finalize phase rather than happening
// before the agent runs.
func TestLoop_ReviewersDetectedViaEnsureReviewersFn(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	logger := logging.New(nil)

	reviewer := git.Reviewer{AppSlug: "copilot-code-review", BotUsername: "copilot-pull-request-reviewer", DefaultTimeout: 120 * time.Second, ReviewOnPush: true}
	gm := &testutil.StubGit{
		ProjectDir:      dir,
		WorkDir:         dir,
		ActiveReviewers: []git.Reviewer{reviewer},
	}
	cfg := Config{
		Dirs:        workctx.WorkContext{ProjectDir: dir, RalphDir: ralphDir},
		TaskBackend: &testutil.StubBackend{},
	}
	l := New(cfg, st, gm, logger)

	// Build the same ensureReviewersFn closure that loop.go passes to completeTaskParams.
	ensureReviewersFn := func() []git.Reviewer { l.ensureActiveReviewers(); return l.activeReviewers }

	// Before the closure is called, DetectActiveReviewers must not have run.
	if gm.DetectActiveReviewersCalled {
		t.Fatal("DetectActiveReviewers should not be called before ensureReviewersFn is invoked")
	}

	// Calling the closure (as finalizePRFn does) must trigger detection.
	reviewers := ensureReviewersFn()

	if !gm.DetectActiveReviewersCalled {
		t.Error("DetectActiveReviewers should be called when ensureReviewersFn is invoked")
	}
	if len(reviewers) != 1 || reviewers[0].BotUsername != "copilot-pull-request-reviewer" {
		t.Errorf("ensureReviewersFn should return detected reviewers, got %v", reviewers)
	}

	// Second call must not re-detect (cache is preserved).
	prevCount := gm.DetectActiveReviewersCallCount
	_ = ensureReviewersFn()
	if gm.DetectActiveReviewersCallCount != prevCount {
		t.Error("DetectActiveReviewers called more than once — cache not preserved")
	}
}

// Verifies that initWorktree as a package function is a no-op when the git
// context has no worktree branch (i.e. running in the project dir directly),
// proving the guard condition works via the package function signature.
func TestInitWorktree_NoopWhenNoWorktreeBranch(t *testing.T) {
	dir, st := setupTestDir(t)
	logger := logging.New(nil)
	backend := &testutil.StubBackend{}
	// StubGit with WorkDir == ProjectDir means no worktree → early return.
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	err := initWorktree(context.Background(), initWorktreeParams{
		git:     gm,
		dirs:    workctx.WorkContext{ProjectDir: dir, WorkDir: dir},
		backend: backend,
		state:   st,
		logger:  logger,
	})
	if err != nil {
		t.Fatalf("initWorktree returned error for no-worktree case: %v", err)
	}
}
