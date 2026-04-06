package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// resumeViaPR skips PR creation when a remote branch exists with commits but
// is not ahead of main — the work was already resolved (e.g. squash-merged via
// parent) and attempting Ship would produce a 422 with no commits to push.
func TestResumeViaPR_SkipsShipWhenBranchNotAheadOfMain(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := newMetadataBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Fix auth"
	backend.NextID = "ralph-xg0x"
	_ = backend.SetMetadata("ralph-xg0x", "branch", "ralph/ralph-xg0x-fix-auth")

	gm := &testutil.StubGit{
		ProjectDir:          dir,
		WorkDir:             filepath.Join(dir, "worktree"),
		WorktreeBranch:      "ralph/ralph-xg0x-fix-auth",
		RemoteURLValue:      "https://github.com/example/repo",
		RemoteBranchCommits: true,  // branch has commits not in main
		RemoteBranchOnMain:  true,  // branch is on main lineage (passes diverge check)
		BranchAheadOfMain:   false, // but NOT strictly ahead (already resolved)
	}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: gm.WorkDir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	result := resumeViaPR(context.Background(), resumeViaPRParams{
		taskID:   "ralph-xg0x",
		nextTask: "Fix auth",
		backend:  backend,
		git:      gm,
		logger:   l.logger,
		attempts: l.attempts,
		state:    l.state,
		ralphDir: ralphDir,
	})

	if result {
		t.Error("resumeViaPR should return false when branch is not ahead of main")
	}
	if gm.ShipCalls > 0 {
		t.Errorf("Ship must not be called when branch has no commits ahead of main, got %d calls", gm.ShipCalls)
	}
	if !gm.DeleteBranchCalled {
		t.Error("stale remote branch should be deleted when not ahead of main")
	}
}
