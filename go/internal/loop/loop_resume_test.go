package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// When ResumeTask returns ShipFailedAfterPush=true (branch had pushed commits, Ship
// was called to create a PR, and Ship failed), the loop halts with
// status="halted_ship_failed_with_pushed_work" without invoking the agent.
func TestLoop_HaltsWhenShipFailsAfterPushedCommits(t *testing.T) {
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

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/ralph-xg0x-fix-auth",
		RemoteURL:      "https://github.com/example/repo",
		ResumeTaskResult: git.ResumeTaskResult{
			ShipFailedAfterPush: true,
			ShipErr:             fmt.Errorf("pr creation failed: authentication required"),
		},
	})

	agentCalled := false
	runner := &stubRunner{
		onRun: func() { agentCalled = true },
		result: claude.Result{},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Runner:       runner,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if agentCalled {
		t.Error("agent must not be invoked when ShipFailedAfterPush is true")
	}

	status, _ := st.Read("status")
	if status != "halted_ship_failed_with_pushed_work" {
		t.Errorf("expected status halted_ship_failed_with_pushed_work, got %q", status)
	}
}

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

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/ralph-xg0x-fix-auth",
		RemoteURL:      "https://github.com/example/repo",
		// ResumeTaskResult default {Handled: false} — loop must run agent
	})

	agentCalled := false
	runner := &stubRunner{
		onRun: func() {
			agentCalled = true
			backend.Remaining = 0
			backend.Completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "Fix auth done"},
	}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// ResumeTask returning Handled=false is implicit in agentCalled being
	// true — the loop would have skipped the agent if ResumeTask had
	// returned Handled=true or been short-circuited.
	if !agentCalled {
		t.Error("agent should run when ResumeTask returns Handled=false")
	}
}

// When ResumeTask reports an existing open PR (ShipExisting), the loop must
// route it through shipAndFinalize — merging and closing it WITHOUT running the
// agent and WITHOUT bouncing back to task selection. This is the Stage B fix
// for the "CI fixed but PR never merged, loop dropped to wait" stall.
func TestResumeTask_ShipExisting_MergesWithoutAgent(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend:  testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Add footer hint", NextID: "ralph-ship1"},
			ExternalRefs: map[string]string{"ralph-ship1": "gh-42"},
		},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:         dir,
		WorkDir:            dir,
		WorktreeBranch:     "ralph/ralph-ship1-add-footer",
		RemoteURL:          "https://github.com/owner/repo.git",
		DefaultBranch:      "main",
		Ship:               git.ShipResult{PRNumber: 42, Merged: true},
		MergeRetrySucceeds: true,
		ResumeTaskResult:   git.ResumeTaskResult{ShipExisting: true, PRNumber: 42},
		GitHub: git.StubGitHubConfig{
			Available: true,
			PRs:       []git.StubPR{{Number: 42, Base: "main", State: git.PRStateOpen}},
		},
	})

	agentCalled := false
	runner := &stubRunner{
		onRun:  func() { agentCalled = true },
		result: claude.Result{SignalDetected: true, Summary: "should not run"},
	}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})
	l.runner = runner

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if agentCalled {
		t.Error("agent must NOT run for a ShipExisting resume — the open PR ships through shipAndFinalize")
	}
	backend.CloseMu.Lock()
	closedIDs := append([]string{}, backend.ClosedIDs...)
	backend.CloseMu.Unlock()
	if len(closedIDs) != 1 || closedIDs[0] != "ralph-ship1" {
		t.Errorf("expected ralph-ship1 closed after ShipExisting merge, got %v", closedIDs)
	}
}
