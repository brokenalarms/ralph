package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/ratelimit"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies that initialize as a package function writes max_iterations to
// state and loads skipped tasks from state into the backend, proving both
// initialization side-effects work when called outside the Loop struct.
func TestInitialize_WritesConfigAndLoadsSkipped(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	st.AddSkippedTask("ralph-skip1")

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 0,
			Completed: 1,
			Total:     1,
		},
	}
	logger := logging.New(nil)
	limiter := ratelimit.New(ralphDir, 80)
	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	err := initialize(context.Background(), initParams{
		limiter: limiter,
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

// Verifies that Loop.Run calls CheckCopilotReviewEnabled at startup and stores
// both the enabled flag and reviewOnPush flag, so the post-signal pipeline knows
// whether to wait for Copilot reviews and how long to poll.
func TestLoop_CopilotReviewEnabledSetAtStartup(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	logger := logging.New(nil)

	gm := &testutil.StubGit{
		ProjectDir:           dir,
		WorkDir:              dir,
		CopilotReviewEnabled: true,
		CopilotReviewOnPush:  true,
	}
	backend := &testutil.StubBackend{Remaining: 0, Completed: 0, Total: 0}
	cfg := Config{
		Dirs:         workctx.WorkContext{ProjectDir: dir, RalphDir: ralphDir},
		MaxIterations: 1,
		TaskBackend:  backend,
		IsOnline:     func() bool { return true },
		WaitForInternet: func(_ context.Context, _ *logging.Logger) bool { return true },
		NewRunner:    func() claudeRunner { return &stubRunner{result: claude.Result{}} },
		QueryFn:      func(_ context.Context, _, _, _ string) (string, error) { return "", nil },
	}

	l := New(cfg, st, gm, logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so the loop exits after initialization
	_ = l.Run(ctx)

	if !l.copilotReviewEnabled {
		t.Error("expected copilotReviewEnabled=true after startup when StubGit.CopilotReviewEnabled=true")
	}
	if !l.copilotReviewOnPush {
		t.Error("expected copilotReviewOnPush=true after startup when StubGit.CopilotReviewOnPush=true")
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
