package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies the orchestrator closes the assigned task after signal detection
// and verification pass, preventing agents from needing to call bd close
// directly (which could close tasks they aren't assigned to).
func TestLoop_OrchestratorClosesTaskAfterSignal(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &trackingBackend{
		mutableBackend: mutableBackend{
			remaining: 1,
			completed: 0,
			total:     1,
			nextTask:  "Fix auth bug",
			nextID:    "ralph-xyz",
			label:     "beads",
		},
	}

	runner := &stubRunner{
		onRun: func() {
			backend.mu.Lock()
			backend.completed = 1
			backend.remaining = 0
			backend.mu.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = runner
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "99", nil }
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }

	_ = l.Run(context.Background())

	backend.closeMu.Lock()
	defer backend.closeMu.Unlock()
	if len(backend.closedIDs) != 1 {
		t.Fatalf("expected exactly 1 CloseTask call, got %d", len(backend.closedIDs))
	}
	if backend.closedIDs[0] != "ralph-xyz" {
		t.Errorf("expected CloseTask for ralph-xyz, got %q", backend.closedIDs[0])
	}
}

// Verifies the close reason includes the PR number in "Fixed in PR #N" format,
// making it traceable which PR shipped which fix.
func TestLoop_CloseReasonIncludesPRNumber(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &trackingBackend{
		mutableBackend: mutableBackend{
			remaining: 1,
			completed: 0,
			total:     1,
			nextTask:  "Fix auth bug",
			nextID:    "ralph-xyz",
			label:     "beads",
		},
	}

	runner := &stubRunner{
		onRun: func() {
			writeFile(t, project, "fix.go", "package main\n")
			run(t, "git", "-C", project, "commit", "-m", "fix auth bug")
			backend.mu.Lock()
			backend.completed = 1
			backend.remaining = 0
			backend.mu.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil)}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    project,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = runner
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }
	l.findPRInfoFunc = func(string) (string, string) { return "42", "Fix auth bug" }

	_ = l.Run(context.Background())

	backend.closeMu.Lock()
	defer backend.closeMu.Unlock()
	if len(backend.closeReasons) != 1 {
		t.Fatalf("expected exactly 1 CloseTask call, got %d", len(backend.closeReasons))
	}
	want := "Fixed in PR #42"
	if !strings.Contains(backend.closeReasons[0], want) {
		t.Errorf("close reason should contain %q, got %q", want, backend.closeReasons[0])
	}
}

// Verifies the orchestrator does NOT call CloseTask when verification fails,
// ensuring tasks aren't closed prematurely.
func TestLoop_NoCloseOnVerificationFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &trackingBackend{
		mutableBackend: mutableBackend{
			remaining: 1,
			completed: 0,
			total:     1,
			nextTask:  "Fix auth bug",
			nextID:    "ralph-xyz",
			label:     "beads",
		},
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: true},
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = runner
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return false, "no commits" }

	_ = l.Run(context.Background())

	backend.closeMu.Lock()
	defer backend.closeMu.Unlock()
	if len(backend.closedIDs) != 0 {
		t.Errorf("expected no CloseTask calls on verification failure, got %d: %v", len(backend.closedIDs), backend.closedIDs)
	}
}

// Verifies that when an iteration completes without a signal, the attempt
// tracker records it so the next iteration knows what was tried.
func TestLoop_RecordsAttemptAfterIteration(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &mutableBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Fix the auth bug",
		nextID:    "ralph-auth",
		label:     "beads",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{}

	_ = l.Run(context.Background())

	history := l.attempts.Read("ralph-auth", "Fix the auth bug")
	if !strings.Contains(history, "### Attempt 1") {
		t.Error("expected attempt 1 to be recorded after iteration")
	}
}

// Verifies that when an idle timeout occurs, the attempt tracker records it
// with timeout-specific guidance so the next iteration can adjust its approach.
func TestLoop_RecordsAttemptOnIdleTimeout(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	callCount := 0
	backend := &mutableBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Slow task",
		nextID:    "ralph-slow",
		label:     "beads",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 2,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		onRun: func() {
			callCount++
			if callCount >= 2 {
				// Stop after the retry so we don't loop forever
				backend.mu.Lock()
				backend.remaining = 0
				backend.completed = 1
				backend.mu.Unlock()
			}
		},
		result: claude.Result{IdleTimeout: true},
	}

	_ = l.Run(context.Background())

	history := l.attempts.Read("ralph-slow", "Slow task")
	if !strings.Contains(history, "idle_timeout") {
		t.Errorf("expected idle_timeout in attempt history, got: %s", history)
	}
}

// Verifies that when a task completes via signal, the attempt history is
// cleared so re-attempts start fresh if the task reappears.
func TestLoop_ClearsAttemptsOnSignalCompletion(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Done task",
		nextID:    "ralph-done",
		label:     "beads",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	// Seed an existing attempt
	l.attempts.Record("ralph-done", "Done task", "first try failed", "", "continue")

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true, Summary: "task completed"},
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

	_ = l.Run(context.Background())

	history := l.attempts.Read("ralph-done", "Done task")
	if history != "" {
		t.Errorf("expected attempt history to be cleared after signal, got: %s", history)
	}
}

// Verifies that reflections from previous iterations are included in the
// attempt context fed to the prompt.
func TestLoop_IncludesReflectionInAttemptContext(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		CallsPerHour: 80,
		TaskBackend:  &stubBackend{label: "beads"},
	}, st, gm, logging.New(nil))

	// Write a reflection file
	reflDir := filepath.Join(ralphDir, "reflections")
	os.MkdirAll(reflDir, 0o755)
	os.WriteFile(filepath.Join(reflDir, "ralph-abc.md"),
		[]byte("# Fix the bug\n## What was discovered\n- The root cause was X"), 0o644)

	ctx := l.buildAttemptContext("ralph-abc", "Fix the bug")
	if !strings.Contains(ctx, "root cause was X") {
		t.Errorf("expected reflection content in attempt context, got: %s", ctx)
	}
	if !strings.Contains(ctx, "### Previous reflection") {
		t.Error("expected '### Previous reflection' header in attempt context")
	}
}

// Verifies that attempt history and reflections are combined when both exist.
func TestLoop_CombinesAttemptsAndReflection(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		CallsPerHour: 80,
		TaskBackend:  &stubBackend{label: "beads"},
	}, st, gm, logging.New(nil))

	// Record an attempt
	l.attempts.Record("ralph-combo", "Combo task", "tried approach A", "", "halted: stagnation")

	// Write a reflection
	reflDir := filepath.Join(ralphDir, "reflections")
	os.MkdirAll(reflDir, 0o755)
	os.WriteFile(filepath.Join(reflDir, "ralph-combo.md"),
		[]byte("# Combo task\n## What was discovered\n- approach A doesn't work"), 0o644)

	ctx := l.buildAttemptContext("ralph-combo", "Combo task")
	if !strings.Contains(ctx, "### Attempt 1") {
		t.Error("expected attempt history in combined context")
	}
	if !strings.Contains(ctx, "### Previous reflection") {
		t.Error("expected reflection in combined context")
	}
}

// Verifies that buildAttemptContext returns empty string when no prior
// context exists, so the prompt doesn't get polluted with empty sections.
func TestLoop_EmptyAttemptContextForNewTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		CallsPerHour: 80,
		TaskBackend:  &stubBackend{label: "beads"},
	}, st, gm, logging.New(nil))

	ctx := l.buildAttemptContext("ralph-new", "Brand new task")
	if ctx != "" {
		t.Errorf("expected empty attempt context for new task, got: %s", ctx)
	}
}

// Verifies that buildAttemptContext includes reflections from other completed
// tasks, not just the current task. This proves cross-task feed-forward works.
func TestLoop_CrossTaskReflectionsFedForward(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		CallsPerHour: 80,
		TaskBackend:  &stubBackend{label: "beads"},
	}, st, gm, logging.New(nil))

	// Write reflections from 2 previously completed tasks
	reflDir := filepath.Join(ralphDir, "reflections")
	os.MkdirAll(reflDir, 0o755)
	os.WriteFile(filepath.Join(reflDir, "ralph-old1.md"),
		[]byte("# Old task 1\n## What would help future iterations\n- Run rebuild-go.sh before tests"), 0o644)
	os.WriteFile(filepath.Join(reflDir, "ralph-old2.md"),
		[]byte("# Old task 2\n## What was discovered\n- Auth middleware needs special handling"), 0o644)

	// Build context for a NEW task (ralph-new) — should include old reflections
	ctx := l.buildAttemptContext("ralph-new", "Brand new task")
	if !strings.Contains(ctx, "rebuild-go.sh") {
		t.Error("expected cross-task reflection from ralph-old1")
	}
	if !strings.Contains(ctx, "Auth middleware") {
		t.Error("expected cross-task reflection from ralph-old2")
	}
	if !strings.Contains(ctx, "Recent learnings from previous tasks") {
		t.Error("expected 'Recent learnings' section header")
	}
}

// Verifies that cross-task attempt entries are NOT included in the prompt.
// Only reflections (distilled insights) should cross task boundaries.
func TestLoop_CrossTaskAttemptEntriesExcluded(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		CallsPerHour: 80,
		TaskBackend:  &stubBackend{label: "beads"},
	}, st, gm, logging.New(nil))

	// Record a halt from a different task
	l.attempts.Record("ralph-prev", "Previous task", "Halted: stagnation", "", "no code changes for 3 iterations")

	// Build context for the next task — should NOT include cross-task attempts
	ctx := l.buildAttemptContext("ralph-next", "Next task")
	if strings.Contains(ctx, "ralph-prev") {
		t.Error("cross-task attempt entries should not appear in prompt")
	}
	if strings.Contains(ctx, "stagnation") {
		t.Error("cross-task halt reasons should not appear in prompt")
	}
	if strings.Contains(ctx, "Recent attempt outcomes") {
		t.Error("'Recent attempt outcomes' section should not exist")
	}
}

// Verifies that completed task IDs are written to .completed-tasks when
// tasks finish with a signal, so the plan pane can show which tasks were
// completed in the current run.
func TestLoop_RecordsCompletedTasks(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0
	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     2,
		nextTask:  "first task",
		nextID:    "ralph-aaa",
		label:     "beads",
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			if iterationCount == 1 {
				backend.mu.Lock()
				backend.completed = 1
				backend.remaining = 1
				backend.nextTask = "second task"
				backend.nextID = "ralph-bbb"
				backend.mu.Unlock()
			} else if iterationCount == 2 {
				backend.mu.Lock()
				backend.completed = 2
				backend.remaining = 0
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

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(ralphDir, ".completed-tasks"))
	if err != nil {
		t.Fatalf("expected .completed-tasks file, got error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 completed tasks, got %d: %v", len(lines), lines)
	}
	if lines[0] != "ralph-aaa" {
		t.Errorf("first completed task = %q, want %q", lines[0], "ralph-aaa")
	}
	if lines[1] != "ralph-bbb" {
		t.Errorf("second completed task = %q, want %q", lines[1], "ralph-bbb")
	}
}

// Verifies that .completed-tasks is cleared at the start of each run so
// only tasks from the current run appear, not historical completions.
func TestLoop_ClearsCompletedTasksOnStart(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	os.WriteFile(filepath.Join(ralphDir, ".completed-tasks"), []byte("ralph-old\n"), 0o644)

	backend := &stubBackend{
		remaining: 0,
		completed: 1,
		total:     1,
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
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	_ = l.Run(context.Background())

	if _, err := os.Stat(filepath.Join(ralphDir, ".completed-tasks")); !os.IsNotExist(err) {
		t.Error(".completed-tasks should be removed at run start when no tasks complete")
	}
}

// Verifies that when a task has no ID, the task title is recorded instead,
// so the plan pane can still show completed items.
func TestLoop_RecordsCompletedTaskTitle_WhenNoID(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "Add dark mode",
		nextID:    "",
		label:     "beads",
	}

	runner := &stubRunner{
		onRun: func() {
			backend.mu.Lock()
			backend.completed = 1
			backend.remaining = 0
			backend.mu.Unlock()
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
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(ralphDir, ".completed-tasks"))
	if err != nil {
		t.Fatalf("expected .completed-tasks file: %v", err)
	}

	got := strings.TrimSpace(string(data))
	if got != "Add dark mode" {
		t.Errorf("completed task = %q, want %q", got, "Add dark mode")
	}
}

// Verifies that SessionTasks() captures the bead ID, title, and agent summary
// for each task completed via signal detection, so the session summary can
// display what was accomplished before evolve restart or exit.
func TestLoop_SessionTasksRecordsCompletedWork(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "fix session display",
		nextID:    "ralph-re76",
		label:     "beads",
	}

	runner := &stubRunner{
		onRun: func() {
			backend.mu.Lock()
			backend.completed = 1
			backend.remaining = 0
			backend.mu.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "added session summary before evolve"},
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.verifyFunc = func(context.Context, string, string) (bool, string) {
		return true, ""
	}
	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) { return "", nil }

	_ = l.Run(context.Background())

	tasks := l.SessionTasks()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 session task, got %d", len(tasks))
	}
	if tasks[0].ID != "ralph-re76" {
		t.Errorf("expected task ID ralph-re76, got %s", tasks[0].ID)
	}
	if tasks[0].Title != "fix session display" {
		t.Errorf("expected task title 'fix session display', got %s", tasks[0].Title)
	}
	if tasks[0].Summary != "added session summary before evolve" {
		t.Errorf("expected summary 'added session summary before evolve', got %s", tasks[0].Summary)
	}
}

// Verifies that SessionTasks() is empty when verification fails and no task
// is actually completed, preventing false entries in the session summary.
func TestLoop_SessionTasksEmptyOnVerificationFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "broken task",
		nextID:    "ralph-fail",
		label:     "beads",
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: true, Summary: "tried to fix it"},
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.verifyFunc = func(context.Context, string, string) (bool, string) {
		return false, "tests failed"
	}

	_ = l.Run(context.Background())

	tasks := l.SessionTasks()
	if len(tasks) != 0 {
		t.Errorf("expected 0 session tasks on verification failure, got %d", len(tasks))
	}
}

// Proves: after a successful task close, the completed task ID is persisted to
// state.json so ralph-task can verify tasks weren't falsely closed.
func TestLoop_PersistsCompletedTaskToState(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &trackingBackend{
		mutableBackend: mutableBackend{
			remaining: 1,
			completed: 0,
			total:     1,
			nextTask:  "Fix auth bug",
			nextID:    "ralph-xyz",
			label:     "beads",
		},
	}

	runner := &stubRunner{
		onRun: func() {
			writeFile(t, project, "fix.go", "package main\n")
			run(t, "git", "-C", project, "commit", "-m", "fix auth bug")
			backend.mu.Lock()
			backend.completed = 1
			backend.remaining = 0
			backend.mu.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil)}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    project,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = runner
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }
	l.findPRInfoFunc = func(string) (string, string) { return "42", "Fix auth bug" }

	_ = l.Run(context.Background())

	tasks, err := st.GetCompletedTasks()
	if err != nil {
		t.Fatalf("GetCompletedTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 completed task in state.json, got %d", len(tasks))
	}
	if tasks[0] != "ralph-xyz" {
		t.Errorf("completed task ID = %q, want %q", tasks[0], "ralph-xyz")
	}
}

// Proves: completed_tasks in state.json persists across restarts — tasks from
// previous runs are not cleared on a new run start.
func TestLoop_CompletedTasksPersistAcrossRestarts(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)

	st.AddCompletedTask("ralph-old")

	backend := &stubBackend{
		remaining: 0,
		completed: 1,
		total:     1,
	}

	gm := &git.Manager{ProjectDir: project, WorkDir: project}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    project,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	_ = l.Run(context.Background())

	tasks, err := st.GetCompletedTasks()
	if err != nil {
		t.Fatalf("GetCompletedTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 completed task preserved, got %d", len(tasks))
	}
	if tasks[0] != "ralph-old" {
		t.Errorf("completed task ID = %q, want %q (preserved from previous run)", tasks[0], "ralph-old")
	}
}
