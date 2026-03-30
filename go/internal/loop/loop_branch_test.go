package loop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
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

	backend := &stubBackend{
		remaining: 0,
		completed: 1,
		total:     1,
		nextTask:  "new task",
		nextID:    "ralph-new",
	}

	// Set up a real git repo as the worktree
	wtDir := filepath.Join(dir, "worktree")
	os.MkdirAll(wtDir, 0o755)
	exec.Command("git", "init", "-b", "main", wtDir).Run()
	exec.Command("git", "-C", wtDir, "commit", "--allow-empty", "-m", "init").Run()
	exec.Command("git", "-C", wtDir, "checkout", "-b", "ralph/myproject/01-previous-task").Run()

	gm := &git.Manager{
		ProjectDir:     dir,
		WorkDir:        wtDir,
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/myproject/01-previous-task",
		ProjectName:    "myproject",
		State:          st,
		Logger:         logging.New(nil),
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    wtDir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

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

	backend := &stubBackend{
		remaining: 0,
		completed: 1,
		total:     1,
		nextTask:  "ongoing task",
		nextID:    "ralph-same",
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/myproject/01-ongoing-task",
		ProjectName:    "myproject",
		State:          st,
		Logger:         logging.New(nil),
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
	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "task A",
		nextID:    "ralph-aaa",
		label:     "beads",
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			if iterationCount == 1 {
				// Simulate: task A completes and a new task B is added externally
				backend.mu.Lock()
				backend.completed = 1
				backend.remaining = 1
				backend.total = 2
				backend.nextTask = "task B"
				backend.nextID = "ralph-bbb"
				backend.mu.Unlock()
			} else if iterationCount == 2 {
				// Task B completes, no more tasks
				backend.mu.Lock()
				backend.completed = 2
				backend.remaining = 0
				backend.total = 2
				backend.mu.Unlock()
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := &git.Manager{
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
	project, bare := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5)

	writeFile(t, project, "shared.txt", "original\n")
	run(t, "git", "-C", project, "commit", "-m", "add shared")
	pushToOrigin(t, project)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	run(t, "git", "-C", gm.WorkDir, "remote", "set-url", "origin", bare)
	run(t, "git", "-C", gm.WorkDir, "fetch", "origin")

	// Create a conflicting situation (squash-merged branch)
	gm.RenameBranchForTask("first task", "")
	writeFile(t, gm.WorkDir, "shared.txt", "step one\n")
	run(t, "git", "-C", gm.WorkDir, "commit", "-m", "first step")
	writeFile(t, gm.WorkDir, "shared.txt", "final\n")
	run(t, "git", "-C", gm.WorkDir, "commit", "-m", "final")

	gm.BranchRenamed = false
	gm.RenameBranchForTask("second task", "")
	writeFile(t, gm.WorkDir, "second.txt", "second\n")
	run(t, "git", "-C", gm.WorkDir, "commit", "-m", "second")

	// Create a real conflict on main (not a clean squash-merge)
	writeFile(t, project, "shared.txt", "completely different main version\n")
	run(t, "git", "-C", project, "commit", "-m", "divergent main change")
	pushToOrigin(t, project)

	backend := &stubBackend{
		remaining: 0,
		completed: 1,
		total:     1,
	}

	handlerCalled := false
	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
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

	err := l.handleRebase(context.Background())
	// With stacked PRs, rebase conflicts cause stack to diverge — not an error.
	if err != nil {
		t.Fatalf("expected nil (stack diverges), got: %v", err)
	}
	_ = handlerCalled
}

// Verifies that handleRebase returns nil on real conflicts — EnsureUpToDate
// aborts the rebase and lets the loop continue (diverged stack is expected).
func TestLoop_HandleRebase_RecoversContinues(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	run(t, "git", "-C", gm.WorkDir, "remote", "set-url", "origin", bare)
	run(t, "git", "-C", gm.WorkDir, "fetch", "origin")

	// Create a real conflict
	writeFile(t, gm.WorkDir, "conflict.txt", "worktree version\n")
	run(t, "git", "-C", gm.WorkDir, "commit", "-m", "worktree change")

	writeFile(t, project, "conflict.txt", "main version\n")
	run(t, "git", "-C", project, "commit", "-m", "main change")
	pushToOrigin(t, project)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Some task",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	err := l.handleRebase(context.Background())
	if err != nil {
		t.Fatalf("expected nil (diverged stack continues), got: %v", err)
	}
}

// Verifies handleRebase returns nil on real conflicts — the diverged
// stack is expected and the loop should continue.
func TestLoop_HandleRebase_PropagatesNilOnDivergedStack(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	run(t, "git", "-C", gm.WorkDir, "remote", "set-url", "origin", bare)
	run(t, "git", "-C", gm.WorkDir, "fetch", "origin")

	// Create a real conflict
	writeFile(t, gm.WorkDir, "conflict.txt", "worktree version\n")
	run(t, "git", "-C", gm.WorkDir, "commit", "-m", "worktree change")

	writeFile(t, project, "conflict.txt", "main version\n")
	run(t, "git", "-C", project, "commit", "-m", "main change")
	pushToOrigin(t, project)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Some task",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	err := l.handleRebase(context.Background())
	if err != nil {
		t.Fatalf("expected nil (diverged stack continues), got: %v", err)
	}
}

// Verifies that when context is cancelled (Ctrl-C) during a rebase that
// would normally trigger OnRebaseConflict, the handler is NOT called and
// the loop exits cleanly with "stopped" status instead of showing the
// interactive recovery prompt.
func TestLoop_HandleRebase_ContextCancelledSkipsPrompt(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	run(t, "git", "-C", gm.WorkDir, "remote", "set-url", "origin", bare)
	run(t, "git", "-C", gm.WorkDir, "fetch", "origin")

	// Create a real conflict so rebase would normally fail and trigger the prompt
	writeFile(t, gm.WorkDir, "conflict.txt", "worktree version\n")
	run(t, "git", "-C", gm.WorkDir, "commit", "-m", "worktree change")

	writeFile(t, project, "conflict.txt", "main version\n")
	run(t, "git", "-C", project, "commit", "-m", "main change")
	pushToOrigin(t, project)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Some task",
	}

	handlerCalled := false
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate Ctrl-C already received

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
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
// isNewTask is a standalone function — takes state.Store directly, no Loop needed.
func TestLoop_IsNewTask(t *testing.T) {
	_, st := setupTestDir(t)

	if !isNewTask(st, "ralph-abc", "Fix bug") {
		t.Error("expected new task when no last_task_id in state")
	}

	st.Write("last_task_id", "ralph-abc")
	st.Write("last_task", "Fix bug")

	if isNewTask(st, "ralph-abc", "Fix bug") {
		t.Error("same task ID should not be considered new")
	}

	if !isNewTask(st, "ralph-xyz", "Fix bug") {
		t.Error("different task ID should be considered new")
	}

	if isNewTask(st, "", "Fix bug") {
		t.Error("same description with no ID should not be new")
	}
	if !isNewTask(st, "", "Different task") {
		t.Error("different description with no ID should be new")
	}
}

// Verifies that multiple iterations of the same task stay on one branch,
// proving the one-branch-per-task model works within a single run.
func TestLoop_SameTaskStaysOnOneBranch(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	// Simulate two iterations of the same task.
	// After iteration 1, the branch should be renamed for the task.
	// After iteration 2, it should still be the SAME branch.
	callCount := 0
	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Fix the login bug",
		nextID:    "ralph-abc",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
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
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	callCount := 0
	backend := &mutableBackend{
		remaining: 1,
		total:     2,
		nextTask:  "First task",
		nextID:    "ralph-1",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
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
				backend.mu.Lock()
				backend.nextTask = "Second task"
				backend.nextID = "ralph-2"
				backend.mu.Unlock()
			}
			if callCount >= 2 {
				os.WriteFile(filepath.Join(ralphDir, "stop"), nil, 0o644)
			}
		},
	}

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
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	callCount := 0
	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Build feature X",
		nextID:    "ralph-feat",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
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

	_ = l.Run(context.Background())

	// Branch should be the task branch — refactor didn't create a new one
	if !strings.Contains(gm.WorktreeBranch, "build-feature-x") {
		t.Errorf("expected task branch after refactor, got %q", gm.WorktreeBranch)
	}

	// Only one branch rename should have happened (for the task, not refactor)
	if !gm.BranchRenamed {
		t.Error("expected BranchRenamed=true after task rename")
	}

	// Only one ralph branch should exist (the task branch)
	branches := gm.ListProjectBranches()
	nonNextBranches := 0
	for _, b := range branches {
		if !strings.HasSuffix(b, "/next") {
			nonNextBranches++
		}
	}
	if nonNextBranches != 1 {
		t.Errorf("expected exactly 1 task branch, got %d: %v", nonNextBranches, branches)
	}
}

// Verifies that when Evolve is enabled and auto-merge succeeds,
// the loop exits with "evolve_restart" status, signaling that the
// binary should be rebuilt and re-executed with latest main.
func TestLoop_EvolveRestartsAfterMerge(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Improve feature X",
		nextID:    "ralph-imp",
	}

	gm := &git.Manager{
		ProjectDir: project,
		WorkDir:    project,
		Logger:     logging.New(nil),
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    project,
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
		// Simulate agent work by creating a commit during the run.
		onRun: func() {
			writeFile(t, project, "feature.go", "package main\n")
			run(t, "git", "-C", project, "commit", "-m", "agent work")
		},
		result: claude.Result{SignalDetected: true},
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "42", nil }
	l.mergeFunc = func(context.Context) (bool, error) { return true, nil }

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
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Improve feature Y",
		nextID:    "ralph-imp2",
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
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

	_ = l.Run(context.Background())

	finalState, _ := st.Load()
	if finalState.Status == "evolve_restart" {
		t.Error("should NOT set evolve_restart when auto-merge fails (no PR to merge)")
	}
}

type metadataBackend struct {
	stubBackend
	metadata map[string]map[string]string // id -> key -> value
}

func newMetadataBackend() *metadataBackend {
	return &metadataBackend{
		metadata: make(map[string]map[string]string),
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

// Branch name is stored in bead metadata after rename, proving the loop
// persists the branch-to-bead mapping for future resume.
func TestLoop_StoresBranchInMetadata(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := newMetadataBackend()
	backend.remaining = 1
	backend.total = 1
	backend.nextTask = "Fix auth bug"
	backend.nextID = "ralph-abc"

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

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

// checkoutExistingBranch: when no stored branch exists in metadata,
// returns false (no remote checkout) and the branch gets renamed.
func TestLoop_CheckoutExistingBranch_NoRemote(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{remaining: 1, total: 1, nextTask: "Fix login"}
	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil)}

	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	checkedOut := l.checkoutExistingBranch("ralph-xyz", "Fix login")
	if checkedOut {
		t.Error("expected false (no stored branch in metadata), got true")
	}
}

func TestLoop_BranchFormat_NoSequenceNumber(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := newMetadataBackend()
	backend.remaining = 1
	backend.total = 1
	backend.nextTask = "Fix login flow"
	backend.nextID = "ralph-xyz"

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{}

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
