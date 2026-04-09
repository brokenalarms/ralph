package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/attempts"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
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

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "Fix auth bug",
				NextID:       "ralph-xyz",
				BackendLabel: "beads",
			},
		},
	}

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

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
	gm.ShipResult = git.ShipResult{PRNumber: 99}
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 {
		t.Fatalf("expected exactly 1 CloseTask call, got %d", len(backend.ClosedIDs))
	}
	if backend.ClosedIDs[0] != "ralph-xyz" {
		t.Errorf("expected CloseTask for ralph-xyz, got %q", backend.ClosedIDs[0])
	}
}

// Verifies the close reason includes the PR number in "Fixed in PR #N" format,
// making it traceable which PR shipped which fix.
func TestLoop_CloseReasonIncludesPRNumber(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "Fix auth bug",
				NextID:       "ralph-xyz",
				BackendLabel: "beads",
			},
		},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		onRun: func() {
			gm.HeadRevValue = "abc123"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
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
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = runner
	gm.ShipResult = git.ShipResult{PRNumber: 42}
	gm.MergeRetryResult = true
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }
	l.cfg.AutoMerge = true

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.CloseReasons) != 1 {
		t.Fatalf("expected exactly 1 CloseTask call, got %d", len(backend.CloseReasons))
	}
	want := "Fixed in PR #42"
	if !strings.Contains(backend.CloseReasons[0], want) {
		t.Errorf("close reason should contain %q, got %q", want, backend.CloseReasons[0])
	}
}

// Verifies the orchestrator does NOT call CloseTask when verification fails,
// ensuring tasks aren't closed prematurely.
func TestLoop_NoCloseOnVerificationFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "Fix auth bug",
				NextID:       "ralph-xyz",
				BackendLabel: "beads",
			},
		},
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: true},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

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
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return false, "no commits" }

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("expected no CloseTask calls on verification failure, got %d: %v", len(backend.ClosedIDs), backend.ClosedIDs)
	}
}

// Verifies that when an iteration completes without a signal, the attempt
// tracker records it so the next iteration knows what was tried.
func TestLoop_RecordsAttemptAfterIteration(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Total:        1,
			NextTask:     "Fix the auth bug",
			NextID:       "ralph-auth",
			BackendLabel: "beads",
		},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

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

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
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
	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Total:        1,
			NextTask:     "Slow task",
			NextID:       "ralph-slow",
			BackendLabel: "beads",
		},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

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
				backend.Lock()
				backend.Remaining = 0
				backend.Completed = 1
				backend.Unlock()
			}
		},
		result: claude.Result{IdleTimeout: true},
	}

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
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

	backend := &testutil.StubBackend{
		Remaining:    1,
		Total:        1,
		NextTask:     "Done task",
		NextID:       "ralph-done",
		BackendLabel: "beads",
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

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

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	history := l.attempts.Read("ralph-done", "Done task")
	if history != "" {
		t.Errorf("expected attempt history to be cleared after signal, got: %s", history)
	}
}

// Verifies that reflections from previous iterations are included in the
// attempt context fed to the prompt.
func TestLoop_IncludesReflectionInAttemptContext(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")

	// Write a reflection file
	reflDir := filepath.Join(ralphDir, "reflections")
	os.MkdirAll(reflDir, 0o755)
	os.WriteFile(filepath.Join(reflDir, "ralph-abc.md"),
		[]byte("# Fix the bug\n## What was discovered\n- The root cause was X"), 0o644)

	tracker := attempts.New(ralphDir)
	ctx := buildAttemptContext("ralph-abc", "Fix the bug", tracker, ralphDir)
	if !strings.Contains(ctx, "root cause was X") {
		t.Errorf("expected reflection content in attempt context, got: %s", ctx)
	}
	if !strings.Contains(ctx, "### Previous reflection") {
		t.Error("expected '### Previous reflection' header in attempt context")
	}
}

// Verifies that attempt history and reflections are combined when both exist.
func TestLoop_CombinesAttemptsAndReflection(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")

	tracker := attempts.New(ralphDir)
	tracker.Record("ralph-combo", "Combo task", "tried approach A", "", "halted: stagnation")

	// Write a reflection
	reflDir := filepath.Join(ralphDir, "reflections")
	os.MkdirAll(reflDir, 0o755)
	os.WriteFile(filepath.Join(reflDir, "ralph-combo.md"),
		[]byte("# Combo task\n## What was discovered\n- approach A doesn't work"), 0o644)

	ctx := buildAttemptContext("ralph-combo", "Combo task", tracker, ralphDir)
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
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")

	tracker := attempts.New(ralphDir)
	ctx := buildAttemptContext("ralph-new", "Brand new task", tracker, ralphDir)
	if ctx != "" {
		t.Errorf("expected empty attempt context for new task, got: %s", ctx)
	}
}

// Verifies that buildAttemptContext includes reflections from other completed
// tasks, not just the current task. This proves cross-task feed-forward works.
func TestLoop_CrossTaskReflectionsFedForward(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")

	// Write reflections from 2 previously completed tasks
	reflDir := filepath.Join(ralphDir, "reflections")
	os.MkdirAll(reflDir, 0o755)
	os.WriteFile(filepath.Join(reflDir, "ralph-old1.md"),
		[]byte("# Old task 1\n## What would help future iterations\n- Run rebuild-go.sh before tests"), 0o644)
	os.WriteFile(filepath.Join(reflDir, "ralph-old2.md"),
		[]byte("# Old task 2\n## What was discovered\n- Auth middleware needs special handling"), 0o644)

	// Build context for a NEW task (ralph-new) — should include old reflections
	tracker := attempts.New(ralphDir)
	ctx := buildAttemptContext("ralph-new", "Brand new task", tracker, ralphDir)
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
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")

	tracker := attempts.New(ralphDir)
	tracker.Record("ralph-prev", "Previous task", "Halted: stagnation", "", "no code changes for 3 iterations")

	// Build context for the next task — should NOT include cross-task attempts
	ctx := buildAttemptContext("ralph-next", "Next task", tracker, ralphDir)
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
	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        2,
			NextTask:     "first task",
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
				backend.NextTask = "second task"
				backend.NextID = "ralph-bbb"
				backend.Unlock()
			} else if iterationCount == 2 {
				backend.Lock()
				backend.Completed = 2
				backend.Remaining = 0
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

	backend := &testutil.StubBackend{
		Remaining: 0,
		Completed: 1,
		Total:     1,
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
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
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

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "Add dark mode",
			NextID:       "",
			BackendLabel: "beads",
		},
	}

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
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
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.cfg.CheckGitHub = func(context.Context) error { return nil }

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

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "fix session display",
			NextID:       "ralph-re76",
			BackendLabel: "beads",
		},
	}

	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "added session summary before evolve"},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

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
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) {
		return true, ""
	}

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
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

	backend := &testutil.StubBackend{
		Remaining:    1,
		Total:        1,
		NextTask:     "broken task",
		NextID:       "ralph-fail",
		BackendLabel: "beads",
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: true, Summary: "tried to fix it"},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

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
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) {
		return false, "tests failed"
	}

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	tasks := l.SessionTasks()
	if len(tasks) != 0 {
		t.Errorf("expected 0 session tasks on verification failure, got %d", len(tasks))
	}
}

// Proves: after a successful task close, the completed task ID is persisted to
// state.json so ralph-task can verify tasks weren't falsely closed.
func TestLoop_PersistsCompletedTaskToState(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "Fix auth bug",
				NextID:       "ralph-xyz",
				BackendLabel: "beads",
			},
		},
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		onRun: func() {
			gm.HeadRevValue = "abc123"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
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
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = runner
	gm.PRNumber = 42
	l.cfg.OnVerify = func(context.Context, string, string) (bool, string) { return true, "" }

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	tasks, err := st.GetCompletedTasks()
	if err != nil {
		t.Fatalf("GetCompletedTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 completed task in state.json, got %d", len(tasks))
	}
	if tasks[0].ID != "ralph-xyz" {
		t.Errorf("completed task ID = %q, want %q", tasks[0].ID, "ralph-xyz")
	}
}

// Proves: completed_tasks in state.json persists across restarts — tasks from
// previous runs are not cleared on a new run start.
func TestLoop_CompletedTasksPersistAcrossRestarts(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	st.AddCompletedTask("ralph-old", true)

	backend := &testutil.StubBackend{
		Remaining: 0,
		Completed: 1,
		Total:     1,
	}

	gm := &testutil.StubGit{ProjectDir: dir, WorkDir: dir}

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

	l.cfg.CheckGitHub = func(context.Context) error { return nil }
	_ = l.Run(context.Background())

	tasks, err := st.GetCompletedTasks()
	if err != nil {
		t.Fatalf("GetCompletedTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 completed task preserved, got %d", len(tasks))
	}
	if tasks[0].ID != "ralph-old" {
		t.Errorf("completed task ID = %q, want %q (preserved from previous run)", tasks[0].ID, "ralph-old")
	}
}
