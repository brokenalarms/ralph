package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// When ResumeTask returns Handled=false (e.g. branch not ahead of main, already
// resolved via squash-merge), the loop continues to run the agent.
func TestResumeTask_HandledFalseRunsAgent(t *testing.T) {
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

	gm := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/ralph-xg0x-fix-auth",
		RemoteURLValue: "https://github.com/example/repo",
		// ResumeResult default {Handled: false} — loop must run agent
	}

	agentCalled := false
	runner := &stubRunner{
		onRun: func() {
			agentCalled = true
			backend.Remaining = 0
			backend.Completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "Fix auth done"},
	}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm)
	l.runner = runner
	l.cfg.IsOnline = func() bool { return true }
	l.cfg.WaitForInternet = func(context.Context) bool { return true }
	l.cfg.CheckGitHub = func(context.Context) error { return nil }

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gm.ResumeCalls == 0 {
		t.Error("expected ResumeTask to be called")
	}
	if !agentCalled {
		t.Error("agent should run when ResumeTask returns Handled=false")
	}
}
