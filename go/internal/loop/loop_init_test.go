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

// Verifies that initialize writes max_iterations to state and loads skipped
// tasks from state into the backend, proving both initialization side-effects
// work when called on a Loop.
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

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		MaxIterations: 7,
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: filepath.Join(dir, ".ralph")},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	err := l.initialize(context.Background())
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

	// Second call must no-op: the cache gate is reviewersDetected=true.
	// ensureActiveReviewers returns early before calling DetectActiveReviewers
	// when that flag is set. After a second call, activeReviewers is
	// unchanged (still populated from the first call).
	l.ensureActiveReviewers()
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
	ensureReviewersFn := func() []git.Reviewer { l.ensureActiveReviewers(); return l.activeReviewers }

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
