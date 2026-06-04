package loop

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// TestVerifyGate_RunsInWorktreeNotProjectDir is the regression guard for the
// frozen-VerifyDir bug (a verify gate that ran in the project root instead of
// the live per-task worktree, so a red main poisoned every task and the agent's
// fix in the worktree was never seen). The gate must run in l.git.GetWorkDir().
//
// Setup: the project root (main) is RED — its ralph-verify target fails — while
// the worktree is GREEN. If the gate runs in the worktree (correct), the result
// is "pass". If it regresses to the project root, the result is "fail".
func TestVerifyGate_RunsInWorktreeNotProjectDir(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	// Project root (main) is RED.
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\tfalse\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Worktree is GREEN — the agent's fix lives here.
	workDir := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "Makefile"), []byte("ralph-verify:\n\ttrue\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: workDir})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: workDir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  &testutil.StubBackend{Remaining: 1, Total: 1, Description: "test task"},
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	l.runPreIterationTests(context.Background())

	got, _ := st.Read("last_test_result")
	if got != "pass" {
		t.Fatalf("verify gate must run in the worktree (green), not the project root (red): last_test_result = %q, want %q", got, "pass")
	}
}
