package loop

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// stubRunner replaces claude.Runner for tests that need to run the loop
// without actually invoking Claude. The onRun callback is called for each
// Claude invocation (both task iterations and refactor passes).
type stubRunner struct {
	onRun  func()
	result claude.Result
}

func (s *stubRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	if s.onRun != nil {
		s.onRun()
	}
	return s.result, nil
}

func (s *stubRunner) StopStreaming() {}

// stubBackend implements tasks.Backend for testing without shelling out to
// bd or reading plan files. Lets us control exactly how many tasks remain
// and what the next task is.
type stubBackend struct {
	remaining   int
	completed   int
	total       int
	nextTask    string
	nextID      string
	label       string
	description string
}

// mutableBackend is like stubBackend but allows changing the next task
// mid-run to simulate task transitions.
type mutableBackend struct {
	mu          sync.Mutex
	remaining   int
	completed   int
	total       int
	nextTask    string
	nextID      string
	label       string
	description string
}

func (m *mutableBackend) Init() error                          { return nil }
func (m *mutableBackend) HasRemaining() (bool, error)          { m.mu.Lock(); defer m.mu.Unlock(); return m.remaining > 0, nil }
func (m *mutableBackend) CountCompleted() (int, error)         { m.mu.Lock(); defer m.mu.Unlock(); return m.completed, nil }
func (m *mutableBackend) CountRemaining() (int, error)         { m.mu.Lock(); defer m.mu.Unlock(); return m.remaining, nil }
func (m *mutableBackend) CountTotal() (int, error)             { m.mu.Lock(); defer m.mu.Unlock(); return m.total, nil }
func (m *mutableBackend) GetNextTask() (string, error)         { m.mu.Lock(); defer m.mu.Unlock(); return m.nextTask, nil }
func (m *mutableBackend) GetNextTaskID() (string, error)       { m.mu.Lock(); defer m.mu.Unlock(); return m.nextID, nil }
func (m *mutableBackend) GetNextTaskInfo() (string, string, error) { m.mu.Lock(); defer m.mu.Unlock(); return m.nextID, m.nextTask, nil }
func (m *mutableBackend) HasTasks() (bool, error)              { m.mu.Lock(); defer m.mu.Unlock(); return m.total > 0, nil }
func (m *mutableBackend) CloseTask(string, string) error       { return nil }
func (m *mutableBackend) SkipTask(string, string) error        { return nil }
func (m *mutableBackend) ReopenTask(string) error              { return nil }
func (m *mutableBackend) SetState(_, _, _, _ string) error     { return nil }
func (m *mutableBackend) GetState(_, _ string) (string, error) { return "", nil }
func (m *mutableBackend) ExecutionInstructions() (string, error) { return "", nil }
func (m *mutableBackend) GetDescription(_ string) (string, error)  { m.mu.Lock(); defer m.mu.Unlock(); return m.description, nil }
func (m *mutableBackend) GetFullContext(_ string) (string, error)  { return "", nil }
func (m *mutableBackend) ProjectContext() (string, error)          { return "", nil }
func (m *mutableBackend) Label() string {
	if m.label != "" {
		return m.label
	}
	return "checklist"
}

func (s *stubBackend) Init() error                          { return nil }
func (s *stubBackend) HasRemaining() (bool, error)          { return s.remaining > 0, nil }
func (s *stubBackend) CountCompleted() (int, error)         { return s.completed, nil }
func (s *stubBackend) CountRemaining() (int, error)         { return s.remaining, nil }
func (s *stubBackend) CountTotal() (int, error)             { return s.total, nil }
func (s *stubBackend) GetNextTask() (string, error)         { return s.nextTask, nil }
func (s *stubBackend) GetNextTaskID() (string, error)       { return s.nextID, nil }
func (s *stubBackend) GetNextTaskInfo() (string, string, error) { return s.nextID, s.nextTask, nil }
func (s *stubBackend) HasTasks() (bool, error)              { return s.total > 0, nil }
func (s *stubBackend) CloseTask(string, string) error       { return nil }
func (s *stubBackend) SkipTask(string, string) error        { return nil }
func (s *stubBackend) ReopenTask(string) error              { return nil }
func (s *stubBackend) SetState(_, _, _, _ string) error     { return nil }
func (s *stubBackend) GetState(_, _ string) (string, error) { return "", nil }
func (s *stubBackend) ExecutionInstructions() (string, error) { return "", nil }
func (s *stubBackend) GetDescription(_ string) (string, error)  { return s.description, nil }
func (s *stubBackend) GetFullContext(_ string) (string, error)  { return "", nil }
func (s *stubBackend) ProjectContext() (string, error)          { return "", nil }
func (s *stubBackend) Label() string {
	if s.label != "" {
		return s.label
	}
	return "checklist"
}

func setupTestDir(t *testing.T) (string, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5, 0)
	return dir, st
}

// Verifies the loop exits with "completed" when the task backend reports
// no remaining tasks, proving the "all tasks done" exit path works.
func TestLoop_AllTasksComplete(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &stubBackend{
		remaining: 0,
		completed: 3,
		total:     3,
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", finalState.Status)
	}
}

// Verifies the loop exits with "error" when there are zero tasks and it's
// the first iteration, proving the "no tasks found" guard works.
func TestLoop_NoTasksError(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &stubBackend{
		remaining: 0,
		completed: 0,
		total:     0,
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "error" {
		t.Errorf("expected status 'error', got %q", finalState.Status)
	}
}

// Verifies the loop exits with "stopped" when the stop file is present,
// proving the graceful shutdown mechanism works.
func TestLoop_StopFileDetection(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	// Create a stop file before the loop starts.
	os.WriteFile(filepath.Join(ralphDir, "stop"), []byte(""), 0o644)

	backend := &stubBackend{
		remaining: 5,
		total:     5,
		nextTask:  "Some task",
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "stopped" {
		t.Errorf("expected status 'stopped', got %q", finalState.Status)
	}

	// Stop file should be cleaned up.
	if _, err := os.Stat(filepath.Join(ralphDir, "stop")); err == nil {
		t.Error("stop file should have been removed")
	}
}

// Verifies the loop exits with "stopped" when the context is cancelled,
// proving the context-based cancellation path works.
func TestLoop_ContextCancellation(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &stubBackend{
		remaining: 5,
		total:     5,
		nextTask:  "Some task",
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

	logger := logging.New(nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("expected no error on context cancel, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "stopped" {
		t.Errorf("expected status 'stopped', got %q", finalState.Status)
	}
}

// Verifies the loop writes max_iterations to state so users can edit it
// mid-run and the loop picks up the changed value each iteration.
func TestLoop_MaxIterationsFromState(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &stubBackend{
		remaining: 0,
		completed: 1,
		total:     1,
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 10,
		RefactorEvery: 3,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)

	_ = l.Run(context.Background())

	maxIter := st.ReadMaxIterations(0)
	if maxIter != 10 {
		t.Errorf("expected max_iterations=10 in state, got %d", maxIter)
	}

	refEvery := st.ReadRefactorEvery()
	if refEvery != 3 {
		t.Errorf("expected refactor_every=3 in state, got %d", refEvery)
	}
}

// Verifies the stream task file is written with task ID and description,
// proving the tmux pane title integration works correctly.
func TestLoop_UpdateStreamTask(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	l := &Loop{
		cfg: Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}},
	}

	l.updateStreamTask("ralph-abc", "Add feature X")

	data, err := os.ReadFile(filepath.Join(ralphDir, ".stream-task"))
	if err != nil {
		t.Fatalf("expected stream task file, got error: %v", err)
	}
	if string(data) != "ralph-abc: Add feature X" {
		t.Errorf("expected 'ralph-abc: Add feature X', got %q", string(data))
	}

	l.updateStreamTask("", "Add feature Y")
	data, _ = os.ReadFile(filepath.Join(ralphDir, ".stream-task"))
	if string(data) != "Add feature Y" {
		t.Errorf("expected 'Add feature Y', got %q", string(data))
	}
}

// Verifies writeRunBranch persists the current branch name to .run-branch
// so the shell pane-title updater displays the correct branch.
func TestLoop_WriteRunBranch(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	l := &Loop{
		cfg: Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}},
		git: &git.Manager{WorktreeBranch: "ralph/project/01-fix-bug"},
	}

	l.writeRunBranch()

	data, err := os.ReadFile(filepath.Join(ralphDir, ".run-branch"))
	if err != nil {
		t.Fatalf("expected .run-branch file, got error: %v", err)
	}
	if string(data) != "ralph/project/01-fix-bug" {
		t.Errorf("expected 'ralph/project/01-fix-bug', got %q", string(data))
	}
}

// Verifies writeRunBranch defaults to "ralph" when no branch is set.
func TestLoop_WriteRunBranch_Default(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	l := &Loop{
		cfg: Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}},
		git: &git.Manager{},
	}

	l.writeRunBranch()

	data, err := os.ReadFile(filepath.Join(ralphDir, ".run-branch"))
	if err != nil {
		t.Fatalf("expected .run-branch file, got error: %v", err)
	}
	if string(data) != "ralph" {
		t.Errorf("expected 'ralph', got %q", string(data))
	}
}

// Verifies feedback file is read and cleared after consumption.
func TestLoop_FeedbackReadAndClear(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	l := &Loop{
		cfg: Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}},
	}

	feedbackFile := filepath.Join(ralphDir, "feedback")
	os.WriteFile(feedbackFile, []byte("please fix the tests"), 0o644)

	got := l.readFeedback()
	if got != "please fix the tests" {
		t.Errorf("expected feedback content, got %q", got)
	}

	l.clearFeedback()
	if _, err := os.Stat(feedbackFile); err == nil {
		t.Error("feedback file should have been removed after clearing")
	}

	got = l.readFeedback()
	if got != "" {
		t.Errorf("expected empty feedback after clear, got %q", got)
	}
}

// Verifies that buildTaskPrompt includes the bd ID when one is present,
// matching the shell's task prompt format.
func TestLoop_BuildTaskPrompt(t *testing.T) {
	l := &Loop{}

	got := l.buildTaskPrompt("Implement feature X", "ralph-abc")
	if got != "Complete this task (bd id: ralph-abc): Implement feature X" {
		t.Errorf("unexpected prompt with ID: %q", got)
	}

	got = l.buildTaskPrompt("Implement feature X", "")
	if got != "Complete this task: Implement feature X" {
		t.Errorf("unexpected prompt without ID: %q", got)
	}
}

// Verifies that fileLineCount correctly counts newlines in a file.
func TestFileLineCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0o644)
	if got := fileLineCount(path); got != 3 {
		t.Errorf("expected 3 lines, got %d", got)
	}

	if got := fileLineCount(filepath.Join(dir, "nonexistent")); got != 0 {
		t.Errorf("expected 0 for nonexistent file, got %d", got)
	}
}

// Verifies that readLogFrom skips the specified number of lines.
func TestReadLogFrom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	os.WriteFile(path, []byte("line1\nline2\nline3\nline4\n"), 0o644)

	got := readLogFrom(path, 2)
	if got != "line3\nline4\n" {
		t.Errorf("expected 'line3\\nline4\\n', got %q", got)
	}
}

// Verifies the refactor iteration counter increments correctly and only
// triggers a refactor when the threshold is reached.
func TestLoop_MaybeRefactor_CounterIncrement(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	l := &Loop{
		cfg:   Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}},
		state: st,
		logger: logging.New(nil),
	}

	st.Write("iterations_since_refactor", "0")

	// refactorEvery=0 should do nothing.
	err := l.maybeRefactor(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// refactorEvery=5, counter at 2 should just increment.
	st.Write("iterations_since_refactor", "2")
	err = l.maybeRefactor(5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	val, _ := st.Read("iterations_since_refactor")
	n, _ := strconv.Atoi(val)
	if n != 3 {
		t.Errorf("expected counter=3, got %d", n)
	}
}

// Verifies that NoRefactor=true prevents maybeRefactor from running even
// when refactorEvery is set, allowing users to disable refactoring entirely.
func TestLoop_MaybeRefactor_NoRefactorDisables(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	l := &Loop{
		cfg:    Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}, NoRefactor: true},
		state:  st,
		logger: logging.New(nil),
	}

	st.Write("iterations_since_refactor", "10")

	err := l.maybeRefactor(5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	val, _ := st.Read("iterations_since_refactor")
	if val != "10" {
		t.Errorf("counter should not change when NoRefactor=true, got %s", val)
	}
}

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

	// Set up a real git repo as the worktree so RotateBranch can checkout
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

	// Task changed, so the branch should have been rotated to /next
	if gm.WorktreeBranch != "ralph/myproject/next" {
		t.Errorf("expected branch rotated to ralph/myproject/next, got %q", gm.WorktreeBranch)
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
		label:     "checklist",
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

// Verifies that handleRebase with OnRebaseConflict set to RebaseFreshWorktree
// recovers from a squash-merge rebase failure by recreating the worktree.
func TestLoop_HandleRebase_FreshWorktreeRecovery(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5, 0)

	writeFile(t, project, "shared.txt", "original\n")
	run(t, "git", "-C", project, "commit", "-m", "add shared")
	pushToOrigin(t, project)

	gm := &git.Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       st,
		Logger:      logging.New(nil),
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

	gm.RotateBranch()
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

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error after recovery, got %v", err)
	}

	if !handlerCalled {
		t.Error("OnRebaseConflict handler should have been called")
	}

	// Worktree should have been recreated
	if _, err := os.Stat(gm.WorkDir); err != nil {
		t.Error("worktree should exist after recovery")
	}
}

// Verifies that handleRebase with OnRebaseConflict returning RebaseAbort
// propagates the error and halts the loop.
func TestLoop_HandleRebase_AbortHaltsLoop(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5, 0)

	gm := &git.Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       st,
		Logger:      logging.New(nil),
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
			return git.RebaseAbort
		},
	}, st, gm, logging.New(nil))

	err := l.Run(context.Background())
	if err == nil {
		t.Fatal("expected error when rebase is aborted")
	}

	if !handlerCalled {
		t.Error("OnRebaseConflict handler should have been called")
	}

	finalState, _ := st.Load()
	if finalState.Status != "error" {
		t.Errorf("expected status 'error', got %q", finalState.Status)
	}
}

// Verifies that without an OnRebaseConflict handler, rebase failures still
// propagate as errors (backward compatible).
func TestLoop_HandleRebase_NoHandlerPropagatesError(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5, 0)

	gm := &git.Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       st,
		Logger:      logging.New(nil),
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

	err := l.Run(context.Background())
	if err == nil {
		t.Fatal("expected error when no handler and rebase fails")
	}

	finalState, _ := st.Load()
	if finalState.Status != "error" {
		t.Errorf("expected status 'error', got %q", finalState.Status)
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
	st.Init(5, 0)

	gm := &git.Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       st,
		Logger:      logging.New(nil),
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
func TestLoop_IsNewTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	l := &Loop{
		cfg:   Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}},
		state: st,
	}

	// No previous task in state — any task is new
	if !l.isNewTask("ralph-abc", "Fix bug") {
		t.Error("expected new task when no last_task_id in state")
	}

	// Store a task, then compare same ID
	st.Write("last_task_id", "ralph-abc")
	st.Write("last_task", "Fix bug")

	if l.isNewTask("ralph-abc", "Fix bug") {
		t.Error("same task ID should not be considered new")
	}

	// Different ID → new task
	if !l.isNewTask("ralph-xyz", "Fix bug") {
		t.Error("different task ID should be considered new")
	}

	// No ID — falls back to description comparison
	if l.isNewTask("", "Fix bug") {
		t.Error("same description with no ID should not be new")
	}
	if !l.isNewTask("", "Different task") {
		t.Error("different description with no ID should be new")
	}
}

// Verifies that multiple iterations of the same task stay on one branch,
// proving the one-branch-per-task model works within a single run.
func TestLoop_SameTaskStaysOnOneBranch(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5, 0)

	gm := &git.Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       st,
		Logger:      logging.New(nil),
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

	// TaskSeq should be 1 (only one task rename)
	if gm.TaskSeq != 1 {
		t.Errorf("expected TaskSeq=1 (one rename), got %d", gm.TaskSeq)
	}
}

// Verifies that when the task changes between iterations, the branch rotates,
// creating a new branch for the new task while preserving the old one.
func TestLoop_TaskChangeRotatesBranch(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5, 0)

	gm := &git.Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       st,
		Logger:      logging.New(nil),
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

	// TaskSeq should be 2 (two different tasks = two renames)
	if gm.TaskSeq != 2 {
		t.Errorf("expected TaskSeq=2, got %d", gm.TaskSeq)
	}

	// The first task's branch should still exist
	branches := git.ListProjectBranches(project, gm.ProjectName)
	hasFirst := false
	for _, b := range branches {
		if strings.Contains(b, "first-task") {
			hasFirst = true
		}
	}
	if !hasFirst {
		t.Errorf("expected first task branch to be preserved, branches: %v", branches)
	}
}

// Verifies that refactor iterations commit to the current task branch
// without creating a separate branch, proving refactors are internal
// housekeeping on the task's branch.
func TestLoop_RefactorStaysOnTaskBranch(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(5, 0)

	gm := &git.Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       st,
		Logger:      logging.New(nil),
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
		RefactorEvery: 1,
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
	if gm.TaskSeq != 1 {
		t.Errorf("expected TaskSeq=1, got %d", gm.TaskSeq)
	}

	// Only one ralph branch should exist (the task branch)
	branches := git.ListProjectBranches(project, gm.ProjectName)
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
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Improve feature X",
		nextID:    "ralph-imp",
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		WorkDir:        dir,
		Logger: logging.New(nil),
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
		result: claude.Result{SignalDetected: true},
	}
	l.pushPRFunc = func(context.Context, string, string) error { return nil }
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
	st.Init(5, 0)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.Manager{
		ProjectDir:     project,
		RalphDir:       ralphDir,
		UseWorktree:    true,
		State:  st,
		Logger: logging.New(nil),
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

// trackingBackend extends mutableBackend to record CloseTask calls,
// proving the orchestrator closes tasks rather than the agent.
type trackingBackend struct {
	mutableBackend
	closedIDs []string
	closeMu   sync.Mutex
}

func (t *trackingBackend) CloseTask(id string, reason string) error {
	t.closeMu.Lock()
	t.closedIDs = append(t.closedIDs, id)
	t.closeMu.Unlock()
	return nil
}

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
	l.pushPRFunc = func(context.Context, string, string) error { return nil }
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
	l.pushPRFunc = func(context.Context, string, string) error { return nil }
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return false, "no commits" }

	_ = l.Run(context.Background())

	backend.closeMu.Lock()
	defer backend.closeMu.Unlock()
	if len(backend.closedIDs) != 0 {
		t.Errorf("expected no CloseTask calls on verification failure, got %d: %v", len(backend.closedIDs), backend.closedIDs)
	}
}

// --- test helpers ---

// createPromptTemplates creates minimal prompt template files so the loop
// can build prompts without errors.
func createPromptTemplates(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	for _, name := range []string{"shared.md", "internal.md", "reflection.md", "signal.md", "refactor.md", "refactor-style.md", "execution-checklist.md", "execution-bd.md"} {
		os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o644)
	}
}

func initBareRepoWithOrigin(t *testing.T) (projectDir string, bareDir string) {
	t.Helper()
	tmp := t.TempDir()

	bare := filepath.Join(tmp, "bare.git")
	run(t, "git", "init", "--bare", "-b", "main", bare)

	project := filepath.Join(tmp, "project")
	run(t, "git", "clone", bare, project)
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "init")
	run(t, "git", "-C", project, "push", "-u", "origin", "main")
	run(t, "git", "-C", project, "remote", "set-head", "origin", "main")

	return project, bare
}

func pushToOrigin(t *testing.T, projectDir string) {
	t.Helper()
	run(t, "git", "-C", projectDir, "push", "origin", "main", "-q")
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	run(t, "git", "-C", dir, "add", name)
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}

// Verifies that auto-merge fires once per task and calls PostMergeReset after
// each successful merge, so the next task starts from merged main — not stale
// commits.
func TestLoop_AutoMergeFiresPerTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	mergeCount := 0
	iterationCount := 0

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     3,
		nextTask:  "task A",
		nextID:    "ralph-aaa",
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			backend.mu.Lock()
			defer backend.mu.Unlock()
			backend.completed = iterationCount
			switch iterationCount {
			case 1:
				backend.remaining = 1
				backend.nextTask = "task B"
				backend.nextID = "ralph-bbb"
			case 2:
				backend.remaining = 1
				backend.nextTask = "task C"
				backend.nextID = "ralph-ccc"
			default:
				backend.remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
		Logger:     logging.New(nil),
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
	l.pushPRFunc = func(context.Context, string, string) error { return nil }
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCount++
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 3 {
		t.Errorf("expected 3 iterations, got %d", iterationCount)
	}

	if mergeCount != 3 {
		t.Errorf("expected auto-merge to fire 3 times (once per task), got %d", mergeCount)
	}
}

// Verifies that PostMergeReset actually resets the worktree branch to
// origin/main between tasks using a real git worktree, proving each task
// starts from merged main rather than building on stale commits.
func TestLoop_PostMergeResetResetsWorktree(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(10, 0)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       st,
		Logger:      logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	originMain := git.HeadRev(gm.WorkDir)
	iterationCount := 0

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     2,
		nextTask:  "task A",
		nextID:    "ralph-aaa",
	}

	var headAfterMerge string
	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if iterationCount == 1 {
				backend.completed = 1
				backend.remaining = 1
				backend.nextTask = "task B"
				backend.nextID = "ralph-bbb"
			} else {
				headAfterMerge = git.HeadRev(gm.WorkDir)
				backend.completed = 2
				backend.remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.mergeFunc = func(context.Context) (bool, error) {
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Fatalf("expected 2 iterations, got %d", iterationCount)
	}

	if headAfterMerge != originMain {
		t.Errorf("second iteration should start from origin/main (%s), got %s", originMain, headAfterMerge)
	}

	tempBranch := gm.TempBranch()
	if gm.WorktreeBranch != tempBranch {
		t.Errorf("expected branch %q after PostMergeReset, got %q", tempBranch, gm.WorktreeBranch)
	}
}

// Verifies that pushAndCreatePR fires for every completed task when signal
// is detected, regardless of whether auto-merge is enabled. This ensures the
// Go code owns the push/PR lifecycle.
func TestLoop_PushAndCreatePROnSignal(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	pushPRCalls := 0
	iterationCount := 0

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     2,
		nextTask:  "task A",
		nextID:    "ralph-aaa",
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			if iterationCount == 1 {
				backend.mu.Lock()
				backend.completed = 1
				backend.remaining = 1
				backend.nextTask = "task B"
				backend.nextID = "ralph-bbb"
				backend.mu.Unlock()
			} else {
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
		Logger:     logging.New(nil),
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
		AutoMerge:     false,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.pushPRFunc = func(_ context.Context, _, taskDesc string) error {
		pushPRCalls++
		return nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 2 signal-handler pushes (one per task) + 1 safety-net flush before exit.
	// PushAndCreatePR is idempotent, so the flush is harmless.
	if pushPRCalls != 3 {
		t.Errorf("expected pushAndCreatePR called 3 times (2 signal + 1 flush), got %d", pushPRCalls)
	}
}

// Verifies that pushAndCreatePR is NOT called when Claude exits without
// signaling completion (e.g. idle timeout or crash), preventing half-done
// work from being pushed.
func TestLoop_NoPushPRWithoutSignal(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	pushPRCalls := 0

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "some task",
		nextID:    "ralph-xyz",
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: false},
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
		Logger:     logging.New(nil),
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
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.pushPRFunc = func(_ context.Context, _, taskDesc string) error {
		pushPRCalls++
		return nil
	}
	l.mergeFunc = func(context.Context) (bool, error) {
		t.Error("auto-merge should not be called without signal")
		return false, nil
	}

	_ = l.Run(context.Background())

	if pushPRCalls != 0 {
		t.Errorf("pushAndCreatePR should not be called without signal, got %d calls", pushPRCalls)
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
		label:     "checklist",
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
		label:     "checklist",
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
		label:     "checklist",
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
	l.pushPRFunc = func(context.Context, string, string) error { return nil }

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
		TaskBackend:  &stubBackend{label: "checklist"},
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
		TaskBackend:  &stubBackend{label: "checklist"},
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
		TaskBackend:  &stubBackend{label: "checklist"},
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
		TaskBackend:  &stubBackend{label: "checklist"},
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

// Verifies that cross-task attempt entries from halted/killed tasks are fed
// forward so the next task knows what happened.
func TestLoop_CrossTaskAttemptEntriesFedForward(t *testing.T) {
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
		TaskBackend:  &stubBackend{label: "checklist"},
	}, st, gm, logging.New(nil))

	// Record a halt from a different task
	l.attempts.Record("ralph-prev", "Previous task", "Halted: stagnation", "", "no code changes for 3 iterations")

	// Build context for the next task
	ctx := l.buildAttemptContext("ralph-next", "Next task")
	if !strings.Contains(ctx, "ralph-prev") {
		t.Error("expected cross-task attempt entry from ralph-prev")
	}
	if !strings.Contains(ctx, "stagnation") {
		t.Error("expected halt reason in cross-task context")
	}
	if !strings.Contains(ctx, "Recent learnings") {
		t.Error("expected 'Recent learnings' section header")
	}
}

// Verifies that --wait keeps the loop alive when tasks complete, then resumes
// when new tasks appear. Without --wait, the loop would exit immediately.
func TestLoop_WaitResumeOnNewTasks(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &mutableBackend{
		remaining: 0,
		completed: 1,
		total:     1,
		nextTask:  "first task",
		nextID:    "t-1",
		label:     "checklist",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	var (
		callsMu sync.Mutex
		calls   int
	)
	runner := &stubRunner{
		onRun: func() {
			callsMu.Lock()
			calls++
			callsMu.Unlock()
			backend.mu.Lock()
			backend.remaining = 0
			backend.completed++
			backend.mu.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	ctx, cancel := context.WithCancel(context.Background())

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
		Wait:          true,
		WaitInterval:  50 * time.Millisecond,
	}, st, gm, logger)
	l.runner = runner
	l.pushPRFunc = func(context.Context, string, string) error { return nil }

	// After the loop enters wait mode, inject a new task. After the Claude
	// call completes, the loop will re-enter wait mode; cancel the context
	// so the test doesn't hang.
	go func() {
		time.Sleep(200 * time.Millisecond)
		backend.mu.Lock()
		backend.remaining = 1
		backend.total++
		backend.nextTask = "second task"
		backend.nextID = "t-2"
		backend.mu.Unlock()

		for {
			time.Sleep(50 * time.Millisecond)
			callsMu.Lock()
			c := calls
			callsMu.Unlock()
			if c >= 1 {
				time.Sleep(100 * time.Millisecond)
				cancel()
				return
			}
		}
	}()

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	callsMu.Lock()
	finalCalls := calls
	callsMu.Unlock()
	if finalCalls != 1 {
		t.Errorf("expected 1 Claude call (for second task), got %d", finalCalls)
	}
}

// Verifies that --wait exits cleanly when cancelled via context (Ctrl-C).
func TestLoop_WaitExitOnCancel(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &stubBackend{
		remaining: 0,
		completed: 1,
		total:     1,
		label:     "checklist",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Wait:          true,
		WaitInterval:  50 * time.Millisecond,
	}, st, gm, logger)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "stopped" {
		t.Errorf("expected status 'stopped' after cancel, got %q", finalState.Status)
	}
}

// Verifies that --wait exits cleanly when stop file is detected during polling.
func TestLoop_WaitExitOnStopFile(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &stubBackend{
		remaining: 0,
		completed: 1,
		total:     1,
		label:     "checklist",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Wait:          true,
		WaitInterval:  50 * time.Millisecond,
	}, st, gm, logger)

	go func() {
		time.Sleep(150 * time.Millisecond)
		os.WriteFile(filepath.Join(ralphDir, "stop"), nil, 0o644)
	}()

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "stopped" {
		t.Errorf("expected status 'stopped' after stop file, got %q", finalState.Status)
	}
}

// Verifies that without --wait, the loop exits immediately when no tasks remain,
// confirming the default behavior is unchanged.
func TestLoop_NoWaitExitsImmediately(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &stubBackend{
		remaining: 0,
		completed: 2,
		total:     2,
		label:     "checklist",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Wait:          false,
	}, st, gm, logger)

	start := time.Now()
	err := l.Run(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if elapsed > 1*time.Second {
		t.Errorf("loop took %s without --wait, expected immediate exit", elapsed)
	}

	finalState, _ := st.Load()
	if finalState.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", finalState.Status)
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
		label:     "checklist",
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

// Verifies that when a task has no ID (checklist backend), the task title
// is recorded instead, so the plan pane can still show completed items.
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
		label:     "checklist",
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

// Verifies that when verification fails (e.g. tests don't pass), the task
// is NOT closed — it's recorded as a failed attempt so the next iteration
// can retry, preventing ralph from falsely closing beads.
func TestLoop_VerificationFailureBlocksClose(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "fix the bug",
		nextID:    "ralph-bug",
		label:     "checklist",
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: true, Summary: "fixed it"},
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
		return false, "test suite failed"
	}
	l.pushPRFunc = func(context.Context, string, string) error {
		t.Error("push should not be called when verification fails")
		return nil
	}

	_ = l.Run(context.Background())

	// Task should NOT be recorded as completed
	if _, err := os.Stat(filepath.Join(ralphDir, ".completed-tasks")); !os.IsNotExist(err) {
		t.Error("task should not be recorded as completed when verification fails")
	}

	// Attempt should be recorded
	history := l.attempts.Read("ralph-bug", "fix the bug")
	if history == "" {
		t.Error("expected a failed attempt to be recorded")
	}
	if !strings.Contains(history, "verification failed") {
		t.Errorf("attempt should mention verification failure, got: %s", history)
	}
}

// Verifies that when verification passes, the normal completion flow
// proceeds — task is closed, PR is pushed, and completed-tasks is recorded.
func TestLoop_VerificationPassAllowsClose(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	pushCalled := false

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "add feature",
		nextID:    "ralph-feat",
		label:     "checklist",
	}

	runner := &stubRunner{
		onRun: func() {
			backend.mu.Lock()
			backend.completed = 1
			backend.remaining = 0
			backend.mu.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
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
	l.pushPRFunc = func(_ context.Context, _, _ string) error {
		pushCalled = true
		return nil
	}

	_ = l.Run(context.Background())

	if !pushCalled {
		t.Error("push should be called when verification passes")
	}

	data, err := os.ReadFile(filepath.Join(ralphDir, ".completed-tasks"))
	if err != nil {
		t.Fatalf("expected .completed-tasks file: %v", err)
	}
	if !strings.Contains(string(data), "ralph-feat") {
		t.Errorf("expected ralph-feat in completed tasks, got: %s", string(data))
	}
}

// Verifies that the default behavior (no VerifyDir set, no verifyFunc)
// allows tasks to close without verification, preserving backwards
// compatibility for projects that opt out.
func TestLoop_NoVerificationByDefault(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	pushCalled := false

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "simple task",
		nextID:    "ralph-simple",
		label:     "checklist",
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
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		// VerifyDir deliberately not set
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.pushPRFunc = func(_ context.Context, _, _ string) error {
		pushCalled = true
		return nil
	}

	_ = l.Run(context.Background())

	if !pushCalled {
		t.Error("push should be called when no verification is configured")
	}
}

// stateTrackingBackend records SetState calls so tests can verify the
// lifecycle phase transitions (implementing → verified) happen in order.
type stateTrackingBackend struct {
	mutableBackend
	stateCalls []stateCall
}

type stateCall struct {
	id, dimension, value string
}

func (s *stateTrackingBackend) SetState(id, dimension, value, reason string) error {
	s.stateCalls = append(s.stateCalls, stateCall{id, dimension, value})
	return nil
}

// Verifies that the loop sets phase=implementing when starting a task and
// phase=verified after verification passes, ensuring the bd close guard
// will allow the task to be closed.
func TestLoop_LifecycleStates(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stateTrackingBackend{
		mutableBackend: mutableBackend{
			remaining: 1,
			completed: 0,
			total:     1,
			nextTask:  "add lifecycle tracking",
			nextID:    "ralph-lc1",
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
		result: claude.Result{SignalDetected: true, Summary: "done"},
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
	l.pushPRFunc = func(context.Context, string, string) error { return nil }

	_ = l.Run(context.Background())

	if len(backend.stateCalls) < 2 {
		t.Fatalf("expected at least 2 SetState calls, got %d", len(backend.stateCalls))
	}

	first := backend.stateCalls[0]
	if first.id != "ralph-lc1" || first.dimension != "phase" || first.value != "implementing" {
		t.Errorf("first SetState = %+v, want phase=implementing for ralph-lc1", first)
	}

	last := backend.stateCalls[len(backend.stateCalls)-1]
	if last.id != "ralph-lc1" || last.dimension != "phase" || last.value != "verified" {
		t.Errorf("last SetState = %+v, want phase=verified for ralph-lc1", last)
	}
}

// Verifies that phase=verified is NOT set when verification fails,
// ensuring the close guard will reject a premature close.
func TestLoop_LifecycleStates_NoVerifiedOnFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stateTrackingBackend{
		mutableBackend: mutableBackend{
			remaining: 1,
			completed: 0,
			total:     1,
			nextTask:  "broken task",
			nextID:    "ralph-brk",
			label:     "beads",
		},
	}

	runner := &stubRunner{
		result: claude.Result{SignalDetected: true, Summary: "done"},
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

	for _, call := range backend.stateCalls {
		if call.dimension == "phase" && call.value == "verified" {
			t.Error("phase=verified should not be set when verification fails")
		}
	}

	hasImplementing := false
	for _, call := range backend.stateCalls {
		if call.dimension == "phase" && call.value == "implementing" {
			hasImplementing = true
		}
	}
	if !hasImplementing {
		t.Error("phase=implementing should still be set at task start")
	}
}

// When auto-merge returns a CIFailureError, the loop writes the failure
// details to the feedback file so the next iteration can address them.
func TestLoop_CIFailureWritesFeedback(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Fix CI failure",
		nextID:    "ralph-ci1",
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-ci-test",
		State:  st,
		Logger: logging.New(nil),
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
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}
	l.pushPRFunc = func(context.Context, string, string) error { return nil }
	l.mergeFunc = func(context.Context) (bool, error) {
		return false, &git.CIFailureError{
			PRNumber: "99",
			Failures: []git.CICheckResult{
				{Name: "test", State: "FAILURE", Bucket: "fail"},
			},
		}
	}

	// Fix agent returns without signal — the loop should give up gracefully.
	fixAgentSpawned := false
	l.newRunnerFunc = func() claudeRunner {
		fixAgentSpawned = true
		return &stubRunner{result: claude.Result{SignalDetected: false}}
	}

	_ = l.Run(context.Background())

	if !fixAgentSpawned {
		t.Error("expected CI fix agent to be spawned when merge fails due to CI")
	}
}

// When auto-merge returns a MergeConflictError, the loop rebases onto main,
// force-pushes, and retries the merge — resolving PR conflicts automatically.
func TestLoop_MergeConflictTriggersRebaseAndRetry(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Add feature",
		nextID:    "ralph-mc1",
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-conflict-test",
		State:  st,
		Logger: logging.New(nil),
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
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}
	l.pushPRFunc = func(context.Context, string, string) error { return nil }

	// First call returns MergeConflictError, second call succeeds.
	mergeCalls := 0
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		if mergeCalls == 1 {
			return false, &git.MergeConflictError{PRNumber: "77"}
		}
		return true, nil
	}

	forcePushed := false
	l.forcePushFunc = func(context.Context) error {
		forcePushed = true
		return nil
	}

	_ = l.Run(context.Background())

	if mergeCalls < 2 {
		t.Errorf("expected merge to be retried after conflict resolution, got %d calls", mergeCalls)
	}
	if !forcePushed {
		t.Error("expected force-push after rebase to resolve conflicts")
	}
}

// When CI fails after merge, the loop spawns a fix agent via newRunnerFunc,
// re-pushes the fix, and retries the merge — verifying the full CI fix loop.
func TestLoop_CIFailureFixAgentRetriesMerge(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Fix CI failure",
		nextID:    "ralph-ci2",
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-ci-fix",
		State:  st,
		Logger: logging.New(nil),
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
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}

	// Track fix agent invocations via newRunnerFunc.
	fixAgentCalls := 0
	l.newRunnerFunc = func() claudeRunner {
		fixAgentCalls++
		return &stubRunner{result: claude.Result{SignalDetected: true}}
	}

	pushCalls := 0
	l.pushPRFunc = func(_ context.Context, _, _ string) error {
		pushCalls++
		return nil
	}

	// First merge returns CI failure, second merge (after fix) succeeds.
	mergeCalls := 0
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		if mergeCalls == 1 {
			return false, &git.CIFailureError{
				PRNumber: "99",
				Failures: []git.CICheckResult{
					{Name: "test", State: "FAILURE", Bucket: "fail"},
				},
			}
		}
		return true, nil
	}

	_ = l.Run(context.Background())

	if fixAgentCalls == 0 {
		t.Error("expected CI fix agent to be spawned via newRunnerFunc")
	}
	if mergeCalls < 2 {
		t.Errorf("expected merge to be retried after CI fix, got %d calls", mergeCalls)
	}
	// pushCalls: 1 for initial push, 1 for post-fix push
	if pushCalls < 2 {
		t.Errorf("expected push after CI fix, got %d push calls", pushCalls)
	}
}

// When the CI fix agent fails twice (CI keeps failing), the loop gives up
// after the maximum retry count rather than looping forever.
func TestLoop_CIFailureExhaustsRetries(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Retry exhaustion",
		nextID:    "ralph-ci3",
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-ci-exhaust",
		State:  st,
		Logger: logging.New(nil),
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
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}
	l.newRunnerFunc = func() claudeRunner {
		return &stubRunner{result: claude.Result{SignalDetected: true}}
	}
	l.pushPRFunc = func(context.Context, string, string) error { return nil }

	// Every merge attempt returns CI failure.
	mergeCalls := 0
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		return false, &git.CIFailureError{
			PRNumber: "99",
			Failures: []git.CICheckResult{
				{Name: "test", State: "FAILURE", Bucket: "fail"},
			},
		}
	}

	_ = l.Run(context.Background())

	// mergeWithRetry uses a shared retry budget of maxMergeAttempts (4).
	if mergeCalls != maxMergeAttempts {
		t.Errorf("expected %d merge calls (maxMergeAttempts), got %d", maxMergeAttempts, mergeCalls)
	}
}

// mergeWithRetry handles a conflict on the first attempt followed by a CI
// failure on the retry — the shared retry budget handles both error types
// in a single pipeline without the caller needing separate dispatch logic.
func TestLoop_MergeWithRetryHandlesConflictThenCIFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Mixed errors",
		nextID:    "ralph-mixed",
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-mixed",
		State:  st,
		Logger: logging.New(nil),
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
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}
	l.newRunnerFunc = func() claudeRunner {
		return &stubRunner{result: claude.Result{SignalDetected: true}}
	}

	forcePushed := false
	l.forcePushFunc = func(context.Context) error {
		forcePushed = true
		return nil
	}
	l.pushPRFunc = func(context.Context, string, string) error { return nil }
	l.prePushRebaseFunc = func(context.Context) error { return nil }

	// Merge sequence: conflict → (rebase+push) → CI failure → (fix) → success
	mergeCalls := 0
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		switch mergeCalls {
		case 1:
			return false, &git.MergeConflictError{PRNumber: "77"}
		case 2:
			return false, &git.CIFailureError{
				PRNumber: "77",
				Failures: []git.CICheckResult{{Name: "test", State: "FAILURE", Bucket: "fail"}},
			}
		default:
			return true, nil
		}
	}

	_ = l.Run(context.Background())

	if mergeCalls < 3 {
		t.Errorf("expected 3 merge calls (conflict → CI fail → success), got %d", mergeCalls)
	}
	if !forcePushed {
		t.Error("expected force-push after conflict resolution")
	}
}

// Verifies that pre-iteration test results are stored in state.json
// so they persist across restarts and evolve cycles.
func TestLoop_PreIterationTestResultsPersistedInState(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	// Create a Makefile with passing tests so VerifyDir detects a runner
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\ttrue\n"), 0o644)

	backend := &stubBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "Add feature",
		nextID:    "ralph-pre",
		label:     "beads",
	}

	runner := &stubRunner{
		onRun: func() {
			backend.remaining = 0
			backend.completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
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
		VerifyDir:     dir,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }
	l.pushPRFunc = func(context.Context, string, string) error { return nil }

	_ = l.Run(context.Background())

	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.LastTestResult == "" {
		t.Error("expected last_test_result to be set in state after pre-iteration tests")
	}
	if s.LastTestTime == "" {
		t.Error("expected last_test_time to be set in state")
	}
}

// Verifies that {{TEST_STATUS}} placeholder in internal.md gets
// substituted with the orchestrator's pre-iteration test results,
// so the agent knows the test status without running the suite itself.
func TestLoop_TestStatusIncludedInPrompt(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	// Write templates with {{TEST_STATUS}} in internal.md
	for _, name := range []string{"shared.md", "reflection.md", "signal.md", "execution-bd.md"} {
		os.WriteFile(filepath.Join(promptsDir, name), []byte("test"), 0o644)
	}
	os.WriteFile(filepath.Join(promptsDir, "internal.md"),
		[]byte("Assumptions\n{{TEST_STATUS}}\n{{TASK_INSTRUCTIONS}}\n{{ATTEMPT_HISTORY}}"), 0o644)

	// Create Makefile with passing tests
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\ttrue\n"), 0o644)

	var capturedPrompt string
	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Do task",
		nextID:    "ralph-ts",
		label:     "beads",
	}

	runner := &stubRunner{
		onRun: func() {
			backend.remaining = 0
			backend.completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
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
		VerifyDir:     dir,
	}, st, gm, logging.New(nil))

	// Capture the prompt passed to Claude
	l.runner = &stubRunner{
		onRun: func() {
			backend.remaining = 0
			backend.completed = 1
		},
		result: runner.result,
	}
	origRunner := l.runner
	l.runner = &promptCapturingRunner{
		inner: origRunner,
		captured: &capturedPrompt,
	}
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }
	l.pushPRFunc = func(context.Context, string, string) error { return nil }

	_ = l.Run(context.Background())

	if !strings.Contains(capturedPrompt, "tests passing") {
		t.Errorf("prompt should include test status, got: %s", capturedPrompt[:min(200, len(capturedPrompt))])
	}
	// Should not contain the unsubstituted placeholder
	if strings.Contains(capturedPrompt, "{{TEST_STATUS}}") {
		t.Error("prompt should not contain unsubstituted {{TEST_STATUS}} placeholder")
	}
}

// runFixAgent stops the main runner's streaming, creates a new runner via
// newRunnerFunc, passes the standard RunConfig, and returns the result.
// This test verifies all three behaviors in isolation.
func TestLoop_runFixAgent(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	mainRunner := &stubRunner{}
	var capturedCfg claude.RunConfig
	fixRunner := &stubRunner{result: claude.Result{SignalDetected: true, Summary: "fixed"}}

	signals := claude.DefaultSignalPaths(ralphDir)
	l := &Loop{
		cfg: Config{
			Dirs:        workctx.WorkContext{RalphDir: ralphDir},
			IdleTimeout: 30 * time.Second,
		},
		state:   st,
		runner:  mainRunner,
		logger:  logging.New(nil),
		signals: signals,
		newRunnerFunc: func() claudeRunner {
			return &configCapturingRunner{inner: fixRunner, captured: &capturedCfg}
		},
	}

	ctx := context.Background()
	result := l.runFixAgent(ctx, "test failures", "fix the tests", "/work", "/logs/raw.log")

	if !result.SignalDetected {
		t.Error("expected SignalDetected from fix agent result")
	}
	if capturedCfg.Prompt != "fix the tests" {
		t.Errorf("expected prompt %q, got %q", "fix the tests", capturedCfg.Prompt)
	}
	if capturedCfg.WorkDir != "/work" {
		t.Errorf("expected WorkDir %q, got %q", "/work", capturedCfg.WorkDir)
	}
	if capturedCfg.RawLog != "/logs/raw.log" {
		t.Errorf("expected RawLog %q, got %q", "/logs/raw.log", capturedCfg.RawLog)
	}
	if capturedCfg.RalphDir != ralphDir {
		t.Errorf("expected RalphDir %q, got %q", ralphDir, capturedCfg.RalphDir)
	}
	if !capturedCfg.Quiet {
		t.Error("expected Quiet=true for fix agent")
	}
	if capturedCfg.IdleTimeout != 30*time.Second {
		t.Errorf("expected IdleTimeout 30s, got %v", capturedCfg.IdleTimeout)
	}
}

// Verifies that a fix agent's summary is logged when the signal includes
// a descriptive message (not just "done").
func TestLoop_runFixAgent_logsSummary(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	var logBuf bytes.Buffer
	fixRunner := &stubRunner{result: claude.Result{
		SignalDetected: true,
		Summary:        "added missing nil check in parseConfig",
	}}

	signals := claude.DefaultSignalPaths(ralphDir)
	l := &Loop{
		cfg: Config{
			Dirs:        workctx.WorkContext{RalphDir: ralphDir},
			IdleTimeout: 30 * time.Second,
		},
		state:   st,
		runner:  &stubRunner{},
		logger:  logging.NewWithWriter(&logBuf),
		signals: signals,
		newRunnerFunc: func() claudeRunner {
			return fixRunner
		},
	}

	ctx := context.Background()
	l.runFixAgent(ctx, "test failures", "fix the tests", "/work", "/logs/raw.log")

	output := logBuf.String()
	if !strings.Contains(output, "Fix agent (test failures): added missing nil check in parseConfig") {
		t.Errorf("expected fix agent summary in log output, got: %s", output)
	}
}

// configCapturingRunner captures the RunConfig for assertions.
type configCapturingRunner struct {
	inner    claudeRunner
	captured *claude.RunConfig
}

func (c *configCapturingRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	*c.captured = cfg
	return c.inner.Run(cfg)
}

func (c *configCapturingRunner) StopStreaming() {
	c.inner.StopStreaming()
}

// promptCapturingRunner wraps a claude runner to capture the prompt.
type promptCapturingRunner struct {
	inner    claudeRunner
	captured *string
}

func (p *promptCapturingRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	*p.captured = cfg.Prompt
	return p.inner.Run(cfg)
}

func (p *promptCapturingRunner) StopStreaming() {
	p.inner.StopStreaming()
}

// Verifies that the loop rebases onto the base branch before pushing after
// a signal is detected. Without this, direct pushes to main/develop during
// the iteration would be overwritten by the squash-merge.
func TestLoop_PrePushRebaseBeforePush(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.Manager{
		ProjectDir:     dir,
		RalphDir:       ralphDir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/test/01-task",
		UseWorktree:    true,
		State:  st,
		Logger: logging.New(nil),
	}

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Fix the bug",
		nextID:    "ralph-fix1",
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
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	var order []string
	l.prePushRebaseFunc = func(context.Context) error {
		order = append(order, "rebase")
		return nil
	}
	l.pushPRFunc = func(context.Context, string, string) error {
		order = append(order, "push")
		return nil
	}

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true, OnSignalUsed: true},
	}

	l.Run(context.Background())

	if len(order) < 2 {
		t.Fatalf("expected rebase and push to be called, got %v", order)
	}
	if order[0] != "rebase" || order[1] != "push" {
		t.Errorf("expected [rebase, push] order, got %v", order)
	}
}

// Orchestrator status messages ("All tasks complete!", "No tasks found") must
// use the [ralph] prefix, not the task backend label (e.g. [beads]). The task
// label should only appear for actual backend operations.
func TestLoop_OrchestratorMessagesUseRalphPrefix(t *testing.T) {
	tests := []struct {
		name    string
		backend *stubBackend
		want    string // substring expected in log output
	}{
		{
			name: "all tasks complete uses ralph prefix",
			backend: &stubBackend{
				remaining: 0, completed: 3, total: 3, label: "beads",
			},
			want: "[ralph]",
		},
		{
			name: "no tasks error uses ralph prefix",
			backend: &stubBackend{
				remaining: 0, completed: 0, total: 0, label: "beads",
			},
			want: "[ralph]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, st := setupTestDir(t)
			ralphDir := filepath.Join(dir, ".ralph")

			var logBuf strings.Builder
			logger := logging.New(&logBuf)

			gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

			l := New(Config{
				Dirs: workctx.WorkContext{
					ProjectDir: dir,
					WorkDir:    dir,
					RalphDir:   ralphDir,
				},
				MaxIterations: 5,
				CallsPerHour:  80,
				TaskBackend:   tt.backend,
			}, st, gm, logger)

			l.Run(context.Background())

			output := logBuf.String()
			if strings.Contains(output, "[beads]") {
				t.Errorf("orchestrator messages should not use [beads] prefix:\n%s", output)
			}
			if !strings.Contains(output, tt.want) {
				t.Errorf("expected %q in log output:\n%s", tt.want, output)
			}
		})
	}
}

// Verifies that when a task has a description, the log output includes
// the description on a separate line after the task title.
func TestLoop_LogsTaskDescription(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	var logBuf strings.Builder
	logger := logging.New(&logBuf)

	backend := &stubBackend{
		remaining:   1,
		completed:   0,
		total:       1,
		nextTask:    "Fix the auth module",
		nextID:      "ralph-abc",
		label:       "beads",
		description: "Auth tokens are expiring too early due to clock skew",
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
	}, st, gm, logger)

	l.Run(context.Background())

	output := logBuf.String()
	if !strings.Contains(output, "Next task: Fix the auth module (ralph-abc)") {
		t.Errorf("expected task title with bead ID in log output:\n%s", output)
	}
	if !strings.Contains(output, "Auth tokens are expiring too early due to clock skew") {
		t.Errorf("expected task description in log output:\n%s", output)
	}
	if !strings.Contains(output, "ralph-abc → implementing") {
		t.Errorf("expected state transition log with bead ID:\n%s", output)
	}
}

// Verifies that when a task has no description, no extra description line
// is logged — only the task title appears.
func TestLoop_NoDescriptionOmitsLine(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	var logBuf strings.Builder
	logger := logging.New(&logBuf)

	backend := &stubBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "Fix the auth module",
		nextID:    "ralph-abc",
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
	}, st, gm, logger)

	l.Run(context.Background())

	output := logBuf.String()
	if !strings.Contains(output, "Next task: Fix the auth module (ralph-abc)") {
		t.Errorf("expected task title with bead ID in log output:\n%s", output)
	}
	// Count lines containing "description" — there should be none since
	// the backend returns an empty description.
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Description:") {
			t.Errorf("unexpected description line in log output when description is empty:\n%s", output)
			break
		}
	}
}

// When the last task completes and no tasks remain, the loop should still
// push and create a PR before exiting — not silently drop unpushed work.
func TestLoop_FlushesUnpushedWorkBeforeExit(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "last task",
		nextID:    "ralph-last",
		label:     "checklist",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	runner := &stubRunner{
		onRun: func() {
			// Simulate the task completing — no remaining tasks after this.
			backend.mu.Lock()
			backend.remaining = 0
			backend.completed = 1
			backend.mu.Unlock()
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
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		Wait:          false,
	}, st, gm, logger)
	l.runner = runner

	var pushCalls int
	l.pushPRFunc = func(_ context.Context, taskID, taskDesc string) error {
		pushCalls++
		return nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Push must be called: once during signal handling AND once as a flush
	// before exit. PushAndCreatePR is idempotent (returns early when no new
	// commits), so the safety-net call is harmless if the first succeeded.
	if pushCalls < 2 {
		t.Errorf("expected pushPRFunc called at least 2 times (signal + flush), got %d", pushCalls)
	}
}

// When the last task completes and --wait is set, the loop should flush
// unpushed work before entering wait mode, not lose it.
func TestLoop_FlushesUnpushedWorkBeforeWait(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "last task",
		nextID:    "ralph-last",
		label:     "checklist",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	runner := &stubRunner{
		onRun: func() {
			backend.mu.Lock()
			backend.remaining = 0
			backend.completed = 1
			backend.mu.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	ctx, cancel := context.WithCancel(context.Background())

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
		Wait:          true,
		WaitInterval:  50 * time.Millisecond,
	}, st, gm, logger)
	l.runner = runner

	var pushCalls int
	l.pushPRFunc = func(_ context.Context, taskID, taskDesc string) error {
		pushCalls++
		return nil
	}

	// Cancel after entering wait to prevent hanging.
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if pushCalls < 2 {
		t.Errorf("expected pushPRFunc called at least 2 times (signal + flush), got %d", pushCalls)
	}
}

// When AutoMerge is enabled, flushUnpushedWork must squash-merge after
// pushing — same flow as every other task, no special case for last task.
func TestLoop_FlushSquashMergesBeforeExit(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "last task",
		nextID:    "ralph-last",
		label:     "checklist",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	runner := &stubRunner{
		onRun: func() {
			backend.mu.Lock()
			backend.remaining = 0
			backend.completed = 1
			backend.mu.Unlock()
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
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		AutoMerge:     true,
		Wait:          false,
	}, st, gm, logger)
	l.runner = runner

	l.pushPRFunc = func(_ context.Context, _, _ string) error { return nil }

	var mergeCalls int
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mergeCalls == 0 {
		t.Error("expected mergeFunc called during flush before exit, got 0 calls")
	}
}

// When AutoMerge is enabled and --wait is set, flushUnpushedWork must
// squash-merge before entering wait mode.
func TestLoop_FlushSquashMergesBeforeWait(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "last task",
		nextID:    "ralph-last",
		label:     "checklist",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	runner := &stubRunner{
		onRun: func() {
			backend.mu.Lock()
			backend.remaining = 0
			backend.completed = 1
			backend.mu.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	ctx, cancel := context.WithCancel(context.Background())

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
		AutoMerge:     true,
		Wait:          true,
		WaitInterval:  50 * time.Millisecond,
	}, st, gm, logger)
	l.runner = runner

	l.pushPRFunc = func(_ context.Context, _, _ string) error { return nil }

	var mergeCalls int
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		return true, nil
	}

	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mergeCalls == 0 {
		t.Error("expected mergeFunc called during flush before wait, got 0 calls")
	}
}

// When AutoMerge is disabled, flushUnpushedWork must NOT attempt to merge.
func TestLoop_FlushSkipsMergeWhenAutoMergeDisabled(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "last task",
		nextID:    "ralph-last",
		label:     "checklist",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	runner := &stubRunner{
		onRun: func() {
			backend.mu.Lock()
			backend.remaining = 0
			backend.completed = 1
			backend.mu.Unlock()
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
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		AutoMerge:     false,
		Wait:          false,
	}, st, gm, logger)
	l.runner = runner

	l.pushPRFunc = func(_ context.Context, _, _ string) error { return nil }

	var mergeCalls int
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mergeCalls != 0 {
		t.Errorf("expected mergeFunc NOT called when AutoMerge disabled, got %d calls", mergeCalls)
	}
}

// When the signal handler already merged the last task, the flush safety net
// must not merge again — otherwise multi-task runs get an extra merge call.
func TestLoop_FlushSkipsMergeWhenAlreadyMerged(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "last task",
		nextID:    "ralph-last",
		label:     "checklist",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	runner := &stubRunner{
		onRun: func() {
			backend.mu.Lock()
			backend.remaining = 0
			backend.completed = 1
			backend.mu.Unlock()
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
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		AutoMerge:     true,
		Wait:          false,
	}, st, gm, logger)
	l.runner = runner

	l.pushPRFunc = func(_ context.Context, _, _ string) error { return nil }

	var mergeCalls int
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Merge fires once in the signal handler. The flush must not merge again
	// because lastTaskMerged is set.
	if mergeCalls != 1 {
		t.Errorf("expected exactly 1 merge (signal handler only), got %d", mergeCalls)
	}
}

// When the agent exits without a signal, the flush safety net must still
// push and merge so the last task's work is not lost.
func TestLoop_FlushMergesWhenSignalNotDetected(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "last task",
		nextID:    "ralph-last",
		label:     "checklist",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	runner := &stubRunner{
		onRun: func() {
			backend.mu.Lock()
			backend.remaining = 0
			backend.completed = 1
			backend.mu.Unlock()
		},
		result: claude.Result{SignalDetected: false},
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
		AutoMerge:     true,
		Wait:          false,
	}, st, gm, logger)
	l.runner = runner

	l.pushPRFunc = func(_ context.Context, _, _ string) error { return nil }

	var mergeCalls int
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCalls++
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Signal handler didn't fire, so merge only happens during flush.
	if mergeCalls != 1 {
		t.Errorf("expected exactly 1 merge (flush only), got %d", mergeCalls)
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
		label:     "checklist",
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
	l.pushPRFunc = func(_ context.Context, _, _ string) error { return nil }

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
		label:     "checklist",
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
