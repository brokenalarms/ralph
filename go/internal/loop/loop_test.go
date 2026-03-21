package loop

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
)

// stubBackend implements tasks.Backend for testing without shelling out to
// bd or reading plan files. Lets us control exactly how many tasks remain
// and what the next task is.
type stubBackend struct {
	remaining int
	completed int
	total     int
	nextTask  string
	nextID    string
	label     string
}

func (s *stubBackend) Init() error                          { return nil }
func (s *stubBackend) HasRemaining() (bool, error)          { return s.remaining > 0, nil }
func (s *stubBackend) CountCompleted() (int, error)         { return s.completed, nil }
func (s *stubBackend) CountRemaining() (int, error)         { return s.remaining, nil }
func (s *stubBackend) CountTotal() (int, error)             { return s.total, nil }
func (s *stubBackend) GetNextTask() (string, error)         { return s.nextTask, nil }
func (s *stubBackend) GetNextTaskID() (string, error)       { return s.nextID, nil }
func (s *stubBackend) HasTasks() (bool, error)              { return s.total > 0, nil }
func (s *stubBackend) NeedsPlanning() (bool, error)         { return false, nil }
func (s *stubBackend) PlanningSucceeded() (bool, error)     { return true, nil }
func (s *stubBackend) CloseTask(string, string) error       { return nil }
func (s *stubBackend) SkipTask(string, string) error        { return nil }
func (s *stubBackend) ExecutionInstructions() (string, error) { return "", nil }
func (s *stubBackend) PlanningInstructions() string         { return "" }
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
		ProjectDir:    dir,
		WorkDir:       dir,
		RalphDir:      ralphDir,
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
		ProjectDir:    dir,
		WorkDir:       dir,
		RalphDir:      ralphDir,
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
		ProjectDir:    dir,
		WorkDir:       dir,
		RalphDir:      ralphDir,
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
		ProjectDir:    dir,
		WorkDir:       dir,
		RalphDir:      ralphDir,
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
		ProjectDir:    dir,
		WorkDir:       dir,
		RalphDir:      ralphDir,
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
		cfg: Config{RalphDir: ralphDir},
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

// Verifies feedback file is read and cleared after consumption.
func TestLoop_FeedbackReadAndClear(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	l := &Loop{
		cfg: Config{RalphDir: ralphDir},
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
		cfg:   Config{RalphDir: ralphDir},
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

// Verifies the loop rotates the branch on resume when the current branch is
// already named for a task, preventing RenameBranchForTask from overwriting
// the previous task's branch ref.
func TestLoop_ResumeRotatesBranchWhenAlreadyNamed(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	backend := &stubBackend{
		remaining: 0,
		completed: 1,
		total:     1,
	}

	// Simulate a resumed worktree where the branch is already a task branch
	gm := &git.Manager{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/myproject/01-previous-task",
		ProjectName:    "myproject",
		State:          st,
		Logger:         logging.New(nil),
	}

	l := New(Config{
		ProjectDir:    dir,
		WorkDir:       gm.WorkDir,
		RalphDir:      ralphDir,
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))

	// Run exits immediately (no remaining tasks) but should have rotated first
	_ = l.Run(context.Background())

	// After rotation, the branch should be the /next temp branch (rotation
	// happens before task selection, and since there are no tasks, no rename)
	if gm.WorktreeBranch != "ralph/myproject/next" {
		t.Errorf("expected branch to be rotated to ralph/myproject/next, got %q", gm.WorktreeBranch)
	}
}
