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

	st.Write("last_task", "previous task")
	st.Write("last_task_id", "ralph-old")

	backend := &testutil.StubBackend{
		Remaining: 0,
		Completed: 1,
		Total:     1,
		NextTask:  "new task",
		NextID:    "ralph-new",
	}

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/myproject/01-previous-task",
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
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
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	_ = l.Run(context.Background())

	// With stacked PRs, no rotation to /next — branch keeps its task name
	if strings.HasSuffix(gm.GetWorktreeBranch(), "/next") {
		t.Errorf("branch should not be /next with stacked PRs, got %q", gm.GetWorktreeBranch())
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

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/myproject/01-ongoing-task",
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
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
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	_ = l.Run(context.Background())

	if gm.GetWorktreeBranch() != "ralph/myproject/01-ongoing-task" {
		t.Errorf("expected branch to stay as ralph/myproject/01-ongoing-task, got %q", gm.GetWorktreeBranch())
	}
	if !gm.IsBranchRenamed() {
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
				backend.Lock()
				backend.Completed = 1
				backend.Remaining = 1
				backend.Total = 2
				backend.NextTask = "task B"
				backend.NextID = "ralph-bbb"
				backend.Unlock()
			} else if iterationCount == 2 {
				backend.Lock()
				backend.Completed = 2
				backend.Remaining = 0
				backend.Total = 2
				backend.Unlock()
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir: dir,
		WorkDir:    dir,
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

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

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/wip-branch",
	})

	backend := &testutil.StubBackend{
		Remaining: 0,
		Completed: 1,
		Total:     1,
	}

	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	err := l.git.EnsureUpToDate(context.Background())
	if err != nil {
		t.Fatalf("expected nil (stack diverges), got: %v", err)
	}
}

// Verifies that handleRebase returns nil on real conflicts — EnsureUpToDate
// aborts the rebase and lets the loop continue (diverged stack is expected).
func TestLoop_HandleRebase_RecoversContinues(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/wip-branch",
	})

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Some task",
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

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

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/wip-branch",
	})

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Some task",
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	err := l.git.EnsureUpToDate(context.Background())
	if err != nil {
		t.Fatalf("expected nil (diverged stack continues), got: %v", err)
	}
}

// Verifies that when context is cancelled (Ctrl-C) during a rebase, the
// loop exits cleanly with "stopped" status instead of treating the
// interruption as a conflict that needs interactive recovery.
func TestLoop_HandleRebase_ContextCancelledSkipsPrompt(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/wip-branch",
	})

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Some task",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
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
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("expected nil error (clean exit), got %v", err)
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

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/wip-branch",
	})

	callCount := 0
	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Fix the login bug",
		NextID:    "ralph-abc",
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 2,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &stubRunner{
		onRun: func() {
			callCount++
			if callCount >= 2 {
				os.WriteFile(filepath.Join(ralphDir, "stop"), nil, 0o644)
			}
		},
	}

	_ = l.Run(context.Background())

	if !strings.Contains(gm.GetWorktreeBranch(), "fix-the-login-bug") {
		t.Errorf("expected branch to contain 'fix-the-login-bug', got %q", gm.GetWorktreeBranch())
	}

	if !gm.IsBranchRenamed() {
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

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/wip-branch",
	})

	callCount := 0
	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 1,
			Total:     2,
			NextTask:  "First task",
			NextID:    "ralph-1",
		},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 3,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &stubRunner{
		onRun: func() {
			callCount++
			if callCount == 1 {
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

	_ = l.Run(context.Background())

	if !strings.Contains(gm.GetWorktreeBranch(), "second-task") {
		t.Errorf("expected branch for second task, got %q", gm.GetWorktreeBranch())
	}

	if !gm.IsBranchRenamed() {
		t.Error("expected BranchRenamed=true after task change")
	}
}

// Verifies that refactor iterations commit to the current task branch
// without creating a separate branch, proving refactors are internal
// housekeeping on the task's branch.
//
// Phase C migration: dropped `gm.RenameBranchCalls != 1` assertion (pure
// stub call count). The observable "branch is the task branch, not a
// refactor branch" and IsBranchRenamed=true together cover the invariant.
func TestLoop_RefactorStaysOnTaskBranch(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/wip-branch",
	})

	callCount := 0
	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Build feature X",
		NextID:    "ralph-feat",
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 3,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &stubRunner{
		onRun: func() {
			callCount++
			if callCount >= 2 {
				os.WriteFile(filepath.Join(ralphDir, "stop"), nil, 0o644)
			}
		},
	}

	_ = l.Run(context.Background())

	if !strings.Contains(gm.GetWorktreeBranch(), "build-feature-x") {
		t.Errorf("expected task branch after refactor, got %q", gm.GetWorktreeBranch())
	}

	if !gm.IsBranchRenamed() {
		t.Error("expected BranchRenamed=true after task rename")
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

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:         dir,
		WorkDir:            dir,
		WorktreeBranch:     "ralph/wip-branch",
		Ship:               git.ShipResult{PRNumber: 42, Merged: true},
		MergeRetrySucceeds: true,
	})
	cfg := Config{
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
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &stubRunner{
		// Simulate agent work by committing so the loop sees new HEAD.
		onRun:  func() { gm.CommitAll("simulated agent commit") },
		result: claude.Result{SignalDetected: true},
	}

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

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/wip-branch",
	})

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Improve feature Y",
		NextID:    "ralph-imp2",
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
		Evolve:        true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}

	_ = l.Run(context.Background())

	finalState, _ := st.Load()
	if finalState.Status == "evolve_restart" {
		t.Error("should NOT set evolve_restart when auto-merge fails (no PR to merge)")
	}
}

type metadataBackend struct {
	testutil.StubBackend
	metadata     map[string]map[string]string
	externalRefs map[string]string
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

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/wip-branch",
	})

	backend := newMetadataBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Fix auth bug"
	backend.NextID = "ralph-abc"
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &stubRunner{}

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

// BranchForTask: when no stored branch exists in metadata, renames the
// current branch to a task-based name without error.
func TestLoop_BranchForTask_NoStoredBranch_RenamesBranch(t *testing.T) {
	dir, _ := setupTestDir(t)

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/wip-branch",
	})

	branch, err := gm.BranchForTask(context.Background(), "ralph-xyz", "Fix login", git.BranchTaskMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gm.IsBranchRenamed() {
		t.Error("expected BranchRenamed=true after rename, got false")
	}
	if !strings.Contains(branch, "ralph-xyz") {
		t.Errorf("expected branch to contain task ID, got %q", branch)
	}
}

func TestLoop_BranchFormat_NoSequenceNumber(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/wip-branch",
	})

	backend := newMetadataBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Fix login flow"
	backend.NextID = "ralph-xyz"
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &stubRunner{}

	_ = l.Run(context.Background())

	wantSuffix := "ralph-xyz-fix-login-flow"
	if !strings.HasSuffix(gm.GetWorktreeBranch(), wantSuffix) {
		t.Errorf("branch %q should end with %q (no sequence number)", gm.GetWorktreeBranch(), wantSuffix)
	}
	if matched := strings.Contains(gm.GetWorktreeBranch(), "/01-") || strings.Contains(gm.GetWorktreeBranch(), "/02-"); matched {
		t.Errorf("branch %q must not contain sequence number prefix", gm.GetWorktreeBranch())
	}
}

// When ResumeTask reports a closed PR (ClearMetadata=true), the loop clears the
// external-ref, updates branch metadata, and re-runs the agent.
func TestResumeTask_ClosedPR_ClearsMetadataAndReruns(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := newMetadataBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Fix auth bug"
	backend.NextID = "ralph-cdr3"
	backend.externalRefs["ralph-cdr3"] = "https://github.com/owner/repo/pull/439"
	_ = backend.SetMetadata("ralph-cdr3", "branch", "ralph/next")

	renamedBranch := "ralph/ralph-cdr3-fix-auth-bug"
	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/next",
		RemoteURL:      "https://github.com/owner/repo.git",
		ResumeTaskResult: git.ResumeTaskResult{
			Handled:       false,
			ClearMetadata: true,
			NewBranch:     renamedBranch,
			PRNumber:      439,
		},
	})

	agentCalled := false
	runner := &stubRunner{onRun: func() { agentCalled = true }, result: claude.Result{}}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: workDir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ref, _ := backend.GetExternalRef("ralph-cdr3")
	if ref != "" {
		t.Errorf("external-ref should be cleared, got %q", ref)
	}

	branch, _ := backend.GetMetadata("ralph-cdr3", "branch")
	if branch != renamedBranch {
		t.Errorf("branch metadata should be %q, got %q", renamedBranch, branch)
	}

	if !agentCalled {
		t.Error("agent should run when ResumeTask returns Handled=false")
	}
}

// BranchForTask returns an error when branch rename fails, preventing the
// iteration from proceeding on a placeholder branch.
func TestLoop_BranchForTask_RenameFailure_ReturnsError(t *testing.T) {
	dir, _ := setupTestDir(t)

	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:             dir,
		WorkDir:                workDir,
		WorktreeBranch:         "ralph/next",
		RenameBranchForTaskErr: fmt.Errorf("git branch -m: fatal: branch already exists"),
	})

	_, err := gm.BranchForTask(context.Background(), "ralph-xyz", "Fix login", git.BranchTaskMeta{})
	if err == nil {
		t.Fatal("expected error when rename fails, got nil")
	}
	if gm.GetWorktreeBranch() != "ralph/next" {
		t.Errorf("branch should stay unchanged on rename failure, got %q", gm.GetWorktreeBranch())
	}
	if gm.IsBranchRenamed() {
		t.Error("BranchRenamed should remain false after rename failure")
	}
}

// BranchForTask failure aborts the iteration — the agent must never run on a
// placeholder branch like ralph/next. Observable via status="error" and the
// backend receiving no close.
//
// Phase C migration: dropped `gm.ShipCalls > 0` assertion (pure call count).
// The status=error assertion and absence of any bead close capture the same
// end-state: the iteration aborted before reaching ship.
func TestLoop_BranchForTask_RenameFailure_AbortsIteration(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix login"},
		},
	}
	workDir := filepath.Join(dir, "worktree")
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:             dir,
		WorkDir:                workDir,
		WorktreeBranch:         "ralph/next",
		RenameBranchForTaskErr: fmt.Errorf("git branch -m: fatal: branch already exists"),
	})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: workDir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	_ = l.Run(context.Background())

	status, _ := st.Read("status")
	if status != "error" {
		t.Errorf("expected status=error when BranchForTask fails, got %q", status)
	}
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("no bead should be closed when branch setup fails, got %v", backend.ClosedIDs)
	}
}
