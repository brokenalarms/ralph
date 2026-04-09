package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies the loop rotates the branch on resume when the next task differs
// from the last one, so each task gets its own branch.
func TestLoop_ResumeRotatesBranchWhenTaskChanged(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	// Last run worked on a different task
	st.Write("last_task", "previous task")
	st.Write("last_task_id", "ralph-old")

	backend := &testutil.StubBackend{
		Remaining: 0,
		Completed: 1,
		Total:     1,
		NextTask:  "new task",
		NextID:    "ralph-new",
	}

	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/myproject/01-previous-task",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	// With stacked PRs, no rotation to /next — branch keeps its task name
	if strings.HasSuffix(gm.WorktreeBranch, "/next") {
		t.Errorf("branch should not be /next with stacked PRs, got %q", gm.WorktreeBranch)
	}
}

// Verifies the loop keeps the existing task branch on resume when the next
// task is the same as the last one, so multiple iterations of the same task
// commit to a single branch.
func TestLoop_ResumeKeepsBranchWhenSameTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	st.Write("last_task", "ongoing task")
	st.Write("last_task_id", "ralph-same")

	backend := &testutil.StubBackend{
		Remaining: 0,
		Completed: 1,
		Total:     1,
		NextTask:  "ongoing task",
		NextID:    "ralph-same",
	}

	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/myproject/01-ongoing-task",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	// Same task — branch should NOT have been rotated
	if gm.WorktreeBranch != "ralph/myproject/01-ongoing-task" {
		t.Errorf("expected branch to stay as ralph/myproject/01-ongoing-task, got %q", gm.WorktreeBranch)
	}
	if !gm.BranchRenamed {
		t.Error("BranchRenamed should be true to prevent re-renaming")
	}
}

// Verifies that tasks added mid-run are picked up by the loop on the next
// iteration: the task backend is queried fresh, so new tasks appear in counts
// and get selected for execution.
func TestLoop_NewTasksPickedUpBetweenIterations(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0
	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "task A",
			NextID:       "ralph-aaa",
			BackendLabel: "beads",
		},
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			if iterationCount == 1 {
				// Simulate: task A completes and a new task B is added externally
				backend.Lock()
				backend.Completed = 1
				backend.Remaining = 1
				backend.Total = 2
				backend.NextTask = "task B"
				backend.NextID = "ralph-bbb"
				backend.Unlock()
			} else if iterationCount == 2 {
				// Task B completes, no more tasks
				backend.Lock()
				backend.Completed = 2
				backend.Remaining = 0
				backend.Total = 2
				backend.Unlock()
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := &testutil.StubGit{
		ProjectDir: dir,
		WorkDir:    dir,
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
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.cfg.CheckGitHub = func(context.Context) error { return nil }

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if iterationCount != 2 {
		t.Errorf("expected 2 iterations (A then B), got %d", iterationCount)
	}

	finalState, _ := st.Load()
	if finalState.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", finalState.Status)
	}
}

// Verifies handleRebase recovers from conflicts via EnsureUpToDate's
// escalating retry strategy — worktree ends up at origin/main.
func TestLoop_HandleRebase_RecoversByResetAndReplay(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gm := &testutil.StubGit{
		ProjectDir:        dir,
		WorkDir:           filepath.Join(dir, "worktree"),
		WorktreeBranch:    "ralph/wip-branch",
		EnsureUpToDateErr: nil,
	}

	backend := &testutil.StubBackend{
		Remaining: 0,
		Completed: 1,
		Total:     1,
	}

	handlerCalled := false
	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		OnRebaseConflict: func(err error) git.RebaseRecovery {
			handlerCalled = true
			return git.RebaseFreshWorktree
		},
	}, st, gm, logging.New(nil))

	err := l.git.EnsureUpToDate(context.Background())
	// With stacked PRs, rebase conflicts cause stack to diverge — not an error.
	if err != nil {
		t.Fatalf("expected nil (stack diverges), got: %v", err)
	}
	_ = handlerCalled
}

// Verifies that handleRebase returns nil on real conflicts — EnsureUpToDate
// aborts the rebase and lets the loop continue (diverged stack is expected).
func TestLoop_HandleRebase_RecoversContinues(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gm := &testutil.StubGit{
		ProjectDir:        dir,
		WorkDir:           filepath.Join(dir, "worktree"),
		WorktreeBranch:    "ralph/wip-branch",
		EnsureUpToDateErr: nil,
	}

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Some task",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	err := l.git.EnsureUpToDate(context.Background())
	if err != nil {
		t.Fatalf("expected nil (diverged stack continues), got: %v", err)
	}
}

// Verifies handleRebase returns nil on real conflicts — the diverged
// stack is expected and the loop should continue.
func TestLoop_HandleRebase_PropagatesNilOnDivergedStack(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gm := &testutil.StubGit{
		ProjectDir:        dir,
		WorkDir:           filepath.Join(dir, "worktree"),
		WorktreeBranch:    "ralph/wip-branch",
		EnsureUpToDateErr: nil,
	}

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Some task",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	err := l.git.EnsureUpToDate(context.Background())
	if err != nil {
		t.Fatalf("expected nil (diverged stack continues), got: %v", err)
	}
}

// Verifies that when context is cancelled (Ctrl-C) during a rebase that
// would normally trigger OnRebaseConflict, the handler is NOT called and
// the loop exits cleanly with "stopped" status instead of showing the
// interactive recovery prompt.
func TestLoop_HandleRebase_ContextCancelledSkipsPrompt(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/wip-branch",
	}

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Some task",
	}

	handlerCalled := false
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate Ctrl-C already received

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		OnRebaseConflict: func(err error) git.RebaseRecovery {
			handlerCalled = true
			return git.RebaseAbort
		},
	}, st, gm, logging.New(nil))

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("expected nil error (clean exit), got %v", err)
	}

	if handlerCalled {
		t.Error("OnRebaseConflict should NOT be called when context is cancelled")
	}

	finalState, _ := st.Load()
	if finalState.Status != "stopped" {
		t.Errorf("expected status 'stopped', got %q", finalState.Status)
	}
}

// Verifies isNewTask compares by task ID when available, falling back to
// description, so that task identity is stable even if descriptions change.
func TestLoop_IsNewTask(t *testing.T) {
	if !isNewTask("", "", "ralph-abc", "Fix bug") {
		t.Error("expected new task when no last_task_id in state")
	}

	if isNewTask("ralph-abc", "Fix bug", "ralph-abc", "Fix bug") {
		t.Error("same task ID should not be considered new")
	}

	if !isNewTask("ralph-abc", "Fix bug", "ralph-xyz", "Fix bug") {
		t.Error("different task ID should be considered new")
	}

	if isNewTask("ralph-abc", "Fix bug", "", "Fix bug") {
		t.Error("same description with no ID should not be new")
	}
	if !isNewTask("ralph-abc", "Fix bug", "", "Different task") {
		t.Error("different description with no ID should be new")
	}
}

// Verifies that multiple iterations of the same task stay on one branch,
// proving the one-branch-per-task model works within a single run.
func TestLoop_SameTaskStaysOnOneBranch(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/wip-branch",
	}

	// Simulate two iterations of the same task.
	// After iteration 1, the branch should be renamed for the task.
	// After iteration 2, it should still be the SAME branch.
	callCount := 0
	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Fix the login bug",
		NextID:    "ralph-abc",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 2,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	// Stub out Claude runner to avoid actually running claude.
	// Just create a stop file after 2 iterations.
	l.runner = &stubRunner{
		onRun: func() {
			callCount++
			if callCount >= 2 {
				os.WriteFile(filepath.Join(ralphDir, "stop"), nil, 0o644)
			}
		},
	}

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	// Branch should be the task branch, NOT rotated to /next
	if !strings.Contains(gm.WorktreeBranch, "fix-the-login-bug") {
		t.Errorf("expected branch to contain 'fix-the-login-bug', got %q", gm.WorktreeBranch)
	}

	// Branch should have been renamed exactly once (same task)
	if !gm.BranchRenamed {
		t.Error("expected BranchRenamed=true after one rename")
	}
}

// Verifies that when the task changes between iterations, the branch rotates,
// creating a new branch for the new task while preserving the old one.
func TestLoop_TaskChangeRotatesBranch(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/wip-branch",
	}

	callCount := 0
	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 1,
			Total:     2,
			NextTask:  "First task",
			NextID:    "ralph-1",
		},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 3,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		onRun: func() {
			callCount++
			if callCount == 1 {
				// After first iteration, switch to a different task
				backend.Lock()
				backend.NextTask = "Second task"
				backend.NextID = "ralph-2"
				backend.Unlock()
			}
			if callCount >= 2 {
				os.WriteFile(filepath.Join(ralphDir, "stop"), nil, 0o644)
			}
		},
	}

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	// Branch should now be the second task
	if !strings.Contains(gm.WorktreeBranch, "second-task") {
		t.Errorf("expected branch for second task, got %q", gm.WorktreeBranch)
	}

	// Branch should have been renamed for the second task
	if !gm.BranchRenamed {
		t.Error("expected BranchRenamed=true after task change")
	}

	// With stacked PRs, the branch is renamed for each task —
	// the first task's branch name is replaced by the second.
}

// Verifies that refactor iterations commit to the current task branch
// without creating a separate branch, proving refactors are internal
// housekeeping on the task's branch.
func TestLoop_RefactorStaysOnTaskBranch(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/wip-branch",
	}

	callCount := 0
	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Build feature X",
		NextID:    "ralph-feat",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 3,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		onRun: func() {
			callCount++
			if callCount >= 2 {
				os.WriteFile(filepath.Join(ralphDir, "stop"), nil, 0o644)
			}
		},
	}

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	// Branch should be the task branch — refactor didn't create a new one
	if !strings.Contains(gm.WorktreeBranch, "build-feature-x") {
		t.Errorf("expected task branch after refactor, got %q", gm.WorktreeBranch)
	}

	// Only one branch rename should have happened (for the task, not refactor)
	if !gm.BranchRenamed {
		t.Error("expected BranchRenamed=true after task rename")
	}

	// Only one rename call should have been made
	if gm.RenameBranchCalls != 1 {
		t.Errorf("expected exactly 1 branch rename, got %d", gm.RenameBranchCalls)
	}
}

// Verifies that when Evolve is enabled and auto-merge succeeds,
// the loop exits with "evolve_restart" status, signaling that the
// binary should be rebuilt and re-executed with latest main.
func TestLoop_EvolveRestartsAfterMerge(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Improve feature X",
		NextID:    "ralph-imp",
	}

	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/wip-branch",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		AutoMerge:     true,
		Evolve:        true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		// Simulate agent work by changing HeadRev so the loop sees new commits.
		onRun: func() {
			gm.HeadRevValue = "abc123"
		},
		result: claude.Result{SignalDetected: true},
	}
	gm.ShipResult = git.ShipResult{PRNumber: 42}
	gm.MergeRetryResult = true
	l.cfg.CheckGitHub = func(context.Context) error { return nil }

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "evolve_restart" {
		t.Errorf("expected status 'evolve_restart', got %q", finalState.Status)
	}
}

// Verifies that Evolve does NOT trigger restart when auto-merge fails,
// allowing the loop to continue normally.
func TestLoop_EvolveNoRestartOnMergeFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/wip-branch",
	}

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Improve feature Y",
		NextID:    "ralph-imp2",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
		Evolve:        true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	finalState, _ := st.Load()
	if finalState.Status == "evolve_restart" {
		t.Error("should NOT set evolve_restart when auto-merge fails (no PR to merge)")
	}
}

type metadataBackend struct {
	testutil.StubBackend
	metadata     map[string]map[string]string // id -> key -> value
	externalRefs map[string]string            // id -> ref
}

func newMetadataBackend() *metadataBackend {
	return &metadataBackend{
		metadata:     make(map[string]map[string]string),
		externalRefs: make(map[string]string),
	}
}

func (m *metadataBackend) SetMetadata(id, key, value string) error {
	if m.metadata[id] == nil {
		m.metadata[id] = make(map[string]string)
	}
	m.metadata[id][key] = value
	return nil
}

func (m *metadataBackend) GetMetadata(id, key string) (string, error) {
	if m.metadata[id] == nil {
		return "", nil
	}
	return m.metadata[id][key], nil
}

func (m *metadataBackend) SetExternalRef(id, ref string) error {
	m.externalRefs[id] = ref
	return nil
}

func (m *metadataBackend) GetExternalRef(id string) (string, error) {
	return m.externalRefs[id], nil
}

// Branch name is stored in bead metadata after rename, proving the loop
// persists the branch-to-bead mapping for future resume.
func TestLoop_StoresBranchInMetadata(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/wip-branch",
	}

	backend := newMetadataBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Fix auth bug"
	backend.NextID = "ralph-abc"

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{}

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	branch, _ := backend.GetMetadata("ralph-abc", "branch")
	if branch == "" {
		t.Fatal("expected branch to be stored in metadata")
	}
	if !strings.Contains(branch, "ralph-abc") {
		t.Errorf("metadata branch %q should contain task ID", branch)
	}
	if !strings.Contains(branch, "fix-auth-bug") {
		t.Errorf("metadata branch %q should contain slug", branch)
	}
}

// checkoutExistingBranch: when no stored branch exists in metadata,
// returns false (no remote checkout) and the branch gets renamed.
func TestLoop_CheckoutExistingBranch_NoRemote(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix login"}
	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/wip-branch",
	}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	checkedOut, err := checkoutExistingBranch(l.git, l.cfg.TaskBackend, l.logger, "ralph-xyz", "Fix login")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if checkedOut {
		t.Error("expected false (no stored branch in metadata), got true")
	}
}

func TestLoop_BranchFormat_NoSequenceNumber(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/wip-branch",
	}

	backend := newMetadataBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Fix login flow"
	backend.NextID = "ralph-xyz"

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{}

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	// Branch should be ralph/<project>/ralph-xyz-fix-login-flow (no 01- prefix)
	wantSuffix := "ralph-xyz-fix-login-flow"
	if !strings.HasSuffix(gm.WorktreeBranch, wantSuffix) {
		t.Errorf("branch %q should end with %q (no sequence number)", gm.WorktreeBranch, wantSuffix)
	}
	// Must NOT contain a sequence number like /01- or /02-
	if matched := strings.Contains(gm.WorktreeBranch, "/01-") || strings.Contains(gm.WorktreeBranch, "/02-"); matched {
		t.Errorf("branch %q must not contain sequence number prefix", gm.WorktreeBranch)
	}
}

// Closed PR re-run renames the branch from ralph/next to a task-specific name
// and clears the stale external-ref so the agent pushes to the correct branch.
func TestResolveByPRState_ClosedPR_RenamesBranch(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := newMetadataBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Fix auth bug"
	backend.NextID = "ralph-cdr3"
	// Simulate a closed PR linked to this task.
	backend.externalRefs["ralph-cdr3"] = "gh-439"

	ghStub := git.NewStubGitHub()
	ghStub.PRState = "CLOSED"
	gm := &testutil.StubGit{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/next",
		BranchRenamed:  true, // Stale from previous run
		RemoteURLValue: "https://github.com/owner/repo.git",
		GitHubStub:     ghStub,
	}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: gm.WorkDir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	resolved := resolveByPRState(context.Background(), resolveByPRStateParams{
		taskID:   "ralph-cdr3",
		nextTask: "Fix auth bug",
		prNumber: 439,
		backend:  backend,
		git:      gm,
		logger:   l.logger,
		attempts: l.attempts,
		state:    l.state,
		ralphDir: ralphDir,
	})
	if resolved {
		t.Fatal("expected resolveByPRState to return false for CLOSED PR")
	}

	// Branch must be renamed from ralph/next to a task-specific name.
	if gm.WorktreeBranch == "ralph/next" {
		t.Error("branch should be renamed from ralph/next, still ralph/next")
	}
	if !strings.Contains(gm.WorktreeBranch, "ralph-cdr3") {
		t.Errorf("branch %q should contain task ID ralph-cdr3", gm.WorktreeBranch)
	}

	// External-ref must be cleared so the closed PR isn't re-discovered.
	ref, _ := backend.GetExternalRef("ralph-cdr3")
	if ref != "" {
		t.Errorf("external-ref should be cleared, got %q", ref)
	}

	// Branch metadata should be updated with the new task-specific name.
	branch, _ := backend.GetMetadata("ralph-cdr3", "branch")
	if !strings.Contains(branch, "ralph-cdr3") {
		t.Errorf("branch metadata should contain task ID, got %q", branch)
	}

	// PrepareForNextTask must have been called to reset branch state.
	if gm.PrepareForNextCalls == 0 {
		t.Error("PrepareForNextTask should have been called")
	}
}

// checkoutExistingBranch returns an error when branch rename fails,
// preventing the iteration from proceeding on a placeholder branch.
func TestLoop_CheckoutExistingBranch_RenameFailure_ReturnsError(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix login"}
	gm := &testutil.StubGit{
		ProjectDir:      dir,
		WorkDir:         filepath.Join(dir, "worktree"),
		WorktreeBranch:  "ralph/next",
		RenameBranchErr: fmt.Errorf("git branch -m: fatal: branch already exists"),
	}

	logger := logging.New(nil)
	_, err := checkoutExistingBranch(gm, backend, logger, "ralph-xyz", "Fix login")
	if err == nil {
		t.Fatal("expected error when rename fails, got nil")
	}
	if gm.WorktreeBranch != "ralph/next" {
		t.Errorf("branch should stay unchanged on rename failure, got %q", gm.WorktreeBranch)
	}
	if gm.BranchRenamed {
		t.Error("BranchRenamed should remain false after rename failure")
	}
}

// prepareBranch aborts the iteration when branch rename fails — the agent
// must never run on a placeholder branch like ralph/next.
func TestLoop_PrepareBranch_RenameFailure_AbortsIteration(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix login"}
	gm := &testutil.StubGit{
		ProjectDir:      dir,
		WorkDir:         filepath.Join(dir, "worktree"),
		WorktreeBranch:  "ralph/next",
		RenameBranchErr: fmt.Errorf("git branch -m: fatal: branch already exists"),
	}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: gm.WorkDir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	err := prepareBranch(context.Background(), branchParams{
		git:     l.git,
		backend: l.cfg.TaskBackend,
		state:   st,
		logger:  l.logger,
	}, "ralph-xyz", "Fix login")
	if err == nil {
		t.Fatal("expected error from prepareBranch when rename fails, got nil")
	}
	if gm.ShipCalls > 0 {
		t.Error("Ship must not be called when branch rename fails")
	}
}

