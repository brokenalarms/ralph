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
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies that auto-merge fires once per task and calls PostMergeReset after
// each successful merge, so the next task starts from merged main — not stale
// commits.
func TestLoop_AutoMergeFiresPerTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 1,
			Completed: 0,
			Total:     3,
			NextTask:  "task A",
			NextID:    "ralph-aaa",
		},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			gm.HeadRevValue = fmt.Sprintf("commit%d", iterationCount)
			backend.Lock()
			defer backend.Unlock()
			backend.Completed = iterationCount
			switch iterationCount {
			case 1:
				backend.Remaining = 1
				backend.NextTask = "task B"
				backend.NextID = "ralph-bbb"
			case 2:
				backend.Remaining = 1
				backend.NextTask = "task C"
				backend.NextID = "ralph-ccc"
			default:
				backend.Remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	gm.ShipResult = git.ShipResult{PRNumber: 99}
	gm.MergeRetryResult = true

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 3 {
		t.Errorf("expected 3 iterations, got %d", iterationCount)
	}

	if gm.MergeRetryCalls != 3 {
		t.Errorf("expected auto-merge to fire 3 times (once per task), got %d", gm.MergeRetryCalls)
	}
}

// Verifies that PostMergeUpdateMain is called between tasks and branch setup
// is called for the next task, proving each task starts from merged main
// rather than building on stale commits.
func TestLoop_PostMergeResetResetsWorktree(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 1,
			Completed: 0,
			Total:     2,
			NextTask:  "task A",
			NextID:    "ralph-aaa",
		},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			gm.HeadRevValue = fmt.Sprintf("commit%d", iterationCount)
			backend.Lock()
			defer backend.Unlock()
			if iterationCount == 1 {
				backend.Completed = 1
				backend.Remaining = 1
				backend.NextTask = "task B"
				backend.NextID = "ralph-bbb"
			} else {
				backend.Completed = 2
				backend.Remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	gm.MergeRetryResult = true
	gm.ShipResult = git.ShipResult{PRNumber: 99}
	gm.PRState = "OPEN"

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Fatalf("expected 2 iterations, got %d", iterationCount)
	}

	if gm.PostMergeUpdateCalls == 0 {
		t.Errorf("expected PostMergeUpdateMain to be called at least once after merge")
	}

	if gm.PrepareForNextCalls == 0 {
		t.Errorf("expected branch prep (PrepareForNextCalls) at least once between tasks, got 0")
	}
}

// When completed tasks exist in state.json and the backend returns their
// branch metadata, the next task starts from the stack head (last completed
// task's branch) instead of origin/main — enabling stacked PRs.
func TestLoop_StackHeadBranchesFromLastCompletedTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0
	taskABranch := "ralph-aaa-task-a"

	ghStub := git.NewStubGitHub()
	ghStub.OpenPRBranches = []string{taskABranch}

	gm := &testutil.StubGit{
		ProjectDir:          dir,
		WorkDir:             dir,
		WorktreeBranch:      "main",
		GitHubStub:          ghStub,
		RemoteURLValue:      "https://github.com/example/repo",
		RemoteBranchCommits: true,
		BranchAheadOfMain:   true,
	}

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 1,
			Completed: 0,
			Total:     2,
			NextTask:  "task A",
			NextID:    "ralph-aaa",
		},
		Metadata:     map[string]map[string]string{},
		ExternalRefs: map[string]string{},
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			gm.HeadRevValue = fmt.Sprintf("commit%d", iterationCount)
			backend.Lock()
			defer backend.Unlock()
			if iterationCount == 1 {
				backend.Metadata["ralph-aaa"] = map[string]string{"branch": taskABranch}
				backend.ExternalRefs["ralph-aaa"] = "gh-100"
				st.AddCompletedTask("ralph-aaa", true)

				backend.Completed = 1
				backend.Remaining = 1
				backend.NextTask = "task B"
				backend.NextID = "ralph-bbb"
			} else {
				backend.Completed = 2
				backend.Remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	gm.MergeRetryResult = true

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Fatalf("expected 2 iterations, got %d", iterationCount)
	}

	// Task B should have started from task A's branch (PrevBranch set to taskABranch).
	found := false
	for _, call := range gm.SetPrevBranchCalls {
		if call == taskABranch {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected SetPrevBranch to be called with %q, got calls: %v", taskABranch, gm.SetPrevBranchCalls)
	}
}

// When a completed task's PR is merged (branch deleted from remote),
// the between-tasks transition falls back to the default branch instead
// of failing — RemoteBranchHasCommits returns false, so the branch is skipped.
func TestLoop_StackHeadSkipsMergedPR(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0
	taskABranch := "ralph-aaa-task-a"

	ghStub := git.NewStubGitHub()
	ghStub.OpenPRBranches = []string{taskABranch}

	gm := &testutil.StubGit{
		ProjectDir:          dir,
		WorkDir:             dir,
		WorktreeBranch:      "main",
		GitHubStub:          ghStub,
		RemoteURLValue:      "https://github.com/example/repo",
		RemoteBranchCommits: false, // branch deleted from remote
		BranchAheadOfMain:   false,
	}

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 1,
			Completed: 0,
			Total:     2,
			NextTask:  "task A",
			NextID:    "ralph-aaa",
		},
		Metadata:     map[string]map[string]string{},
		ExternalRefs: map[string]string{},
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			gm.HeadRevValue = fmt.Sprintf("commit%d", iterationCount)
			backend.Lock()
			defer backend.Unlock()
			if iterationCount == 1 {
				backend.Metadata["ralph-aaa"] = map[string]string{"branch": taskABranch}
				backend.ExternalRefs["ralph-aaa"] = "gh-100"
				st.AddCompletedTask("ralph-aaa", true)

				backend.Completed = 1
				backend.Remaining = 1
				backend.NextTask = "task B"
				backend.NextID = "ralph-bbb"
			} else {
				backend.Completed = 2
				backend.Remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	gm.MergeRetryResult = true

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Fatalf("expected 2 iterations, got %d", iterationCount)
	}

	// PrevBranch should not have been set to taskABranch since it was skipped.
	for _, call := range gm.SetPrevBranchCalls {
		if call == taskABranch {
			t.Errorf("expected taskABranch to be skipped (merged PR), but SetPrevBranch was called with %q", call)
		}
	}
}

// Stack head detection skips a branch whose work landed on main even when the
// remote branch still exists (BranchIsAheadOfMain returns false).
func TestLoop_StackHeadSkipsBranchAncestorOfMain(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0
	taskABranch := "ralph-aaa-task-a"

	ghStub := git.NewStubGitHub()
	ghStub.OpenPRBranches = []string{taskABranch}

	gm := &testutil.StubGit{
		ProjectDir:          dir,
		WorkDir:             dir,
		WorktreeBranch:      "main",
		GitHubStub:          ghStub,
		RemoteURLValue:      "https://github.com/example/repo",
		RemoteBranchCommits: true, // branch still exists on remote
		BranchAheadOfMain:   false, // but work is already on main
	}

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 1,
			Completed: 0,
			Total:     2,
			NextTask:  "task A",
			NextID:    "ralph-aaa",
		},
		Metadata: map[string]map[string]string{},
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			gm.HeadRevValue = fmt.Sprintf("commit%d", iterationCount)
			backend.Lock()
			defer backend.Unlock()
			if iterationCount == 1 {
				backend.Metadata["ralph-aaa"] = map[string]string{"branch": taskABranch}
				st.AddCompletedTask("ralph-aaa", true)

				backend.Completed = 1
				backend.Remaining = 1
				backend.NextTask = "task B"
				backend.NextID = "ralph-bbb"
			} else {
				backend.Completed = 2
				backend.Remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	gm.MergeRetryResult = true

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Fatalf("expected 2 iterations, got %d", iterationCount)
	}

	// PrevBranch should not have been set to taskABranch since it's not ahead of main.
	for _, call := range gm.SetPrevBranchCalls {
		if call == taskABranch {
			t.Errorf("expected taskABranch to be skipped (not ahead of main), but SetPrevBranch was called with %q", call)
		}
	}
}

// Verifies the full post-merge branch rename cycle: task A merges →
// PostMergeUpdateMain resets to /next → next iteration renames to thematic
// branch for task B. Proves each successive task gets its own descriptively
// named branch even after the previous one is squash-merged.
func TestLoop_PostMergeRenamesCycleFull(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0
	var branchDuringTaskA, branchDuringTaskB string

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 1,
			Completed: 0,
			Total:     2,
			NextTask:  "Fix tail leak",
			NextID:    "ralph-t1",
		},
	}

	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "main",
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			gm.HeadRevValue = fmt.Sprintf("commit%d", iterationCount)
			switch iterationCount {
			case 1:
				branchDuringTaskA = gm.WorktreeBranch
				backend.Lock()
				backend.Completed = 1
				backend.Remaining = 1
				backend.NextTask = "Add retry logic"
				backend.NextID = "ralph-r2"
				backend.Unlock()
			case 2:
				branchDuringTaskB = gm.WorktreeBranch
				backend.Lock()
				backend.Completed = 2
				backend.Remaining = 0
				backend.Unlock()
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	gm.MergeRetryResult = true

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Fatalf("expected 2 iterations, got %d", iterationCount)
	}

	if !strings.Contains(branchDuringTaskA, "ralph-t1-fix-tail-leak") {
		t.Errorf("task A branch should contain slug, got %q", branchDuringTaskA)
	}
	if !strings.Contains(branchDuringTaskB, "ralph-r2-add-retry-logic") {
		t.Errorf("task B branch should contain slug, got %q", branchDuringTaskB)
	}
	if branchDuringTaskA == branchDuringTaskB {
		t.Errorf("tasks should have different branches, both got %q", branchDuringTaskA)
	}
}

// After a merge, PostMergeUpdateMain already syncs the worktree to main.
// The next iteration must not reset again, so ResetCalls should be at most 1
// (from initRun before any task runs).
func TestLoop_NoDoubleResetAfterMerge(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 1,
			Completed: 0,
			Total:     2,
			NextTask:  "task A",
			NextID:    "ralph-aaa",
		},
	}

	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "main",
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			gm.HeadRevValue = fmt.Sprintf("commit%d", iterationCount)
			backend.Lock()
			defer backend.Unlock()
			if iterationCount == 1 {
				backend.Completed = 1
				backend.Remaining = 1
				backend.NextTask = "task B"
				backend.NextID = "ralph-bbb"
			} else {
				backend.Completed = 2
				backend.Remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	logger := logging.New(nil)
	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logger)
	l.runner = runner
	gm.ShipResult = git.ShipResult{PRNumber: 99}
	gm.MergeRetryResult = true

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	// initRun may produce one reset (before any task runs).
	// After merge, PostMergeUpdateMain handles the reset — a second reset is redundant.
	if gm.ResetCalls > 1 {
		t.Errorf("expected at most 1 reset (from initRun), got %d — next-task path should skip reset after merge", gm.ResetCalls)
	}

	if gm.PostMergeUpdateCalls == 0 {
		t.Errorf("expected PostMergeUpdateMain to be called, got 0")
	}
}

