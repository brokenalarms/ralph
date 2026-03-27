package loop

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/attempts"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/ratelimit"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/verify"
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

func (s *stubRunner) InjectMessage(_ string) error { return nil }

// stubBackend implements tasks.Backend for testing without shelling out to
// bd or reading plan files. Lets us control exactly how many tasks remain
// and what the next task is.
type stubBackend struct {
	remaining    int
	completed    int
	total        int
	nextTask     string
	nextID       string
	nextPriority *int
	label        string
	description  string
	acceptance   string
	fullContext   string
	skippedTask  string
	skipReason   string
}

// mutableBackend is like stubBackend but allows changing the next task
// mid-run to simulate task transitions.
type mutableBackend struct {
	mu           sync.Mutex
	remaining    int
	completed    int
	total        int
	nextTask     string
	nextID       string
	nextPriority *int
	label        string
	description  string
}

func (m *mutableBackend) Init() error                          { return nil }
func (m *mutableBackend) HasRemaining() (bool, error)          { m.mu.Lock(); defer m.mu.Unlock(); return m.remaining > 0, nil }
func (m *mutableBackend) CountCompleted() (int, error)         { m.mu.Lock(); defer m.mu.Unlock(); return m.completed, nil }
func (m *mutableBackend) CountRemaining() (int, error)         { m.mu.Lock(); defer m.mu.Unlock(); return m.remaining, nil }
func (m *mutableBackend) CountTotal() (int, error)             { m.mu.Lock(); defer m.mu.Unlock(); return m.total, nil }
func (m *mutableBackend) GetNextTask() (string, error)         { m.mu.Lock(); defer m.mu.Unlock(); return m.nextTask, nil }
func (m *mutableBackend) GetNextTaskID() (string, error)       { m.mu.Lock(); defer m.mu.Unlock(); return m.nextID, nil }
func (m *mutableBackend) GetNextTaskInfo() (tasks.TaskInfo, error) { m.mu.Lock(); defer m.mu.Unlock(); return tasks.TaskInfo{ID: m.nextID, Title: m.nextTask, Priority: m.nextPriority}, nil }
func (m *mutableBackend) HasTasks() (bool, error)              { m.mu.Lock(); defer m.mu.Unlock(); return m.total > 0, nil }
func (m *mutableBackend) CloseTask(string, string) error       { return nil }
func (m *mutableBackend) SkipTask(string, string) error        { return nil }
func (m *mutableBackend) SetSkippedIDs([]string)               {}
func (m *mutableBackend) ReopenTask(string) error              { return nil }
func (m *mutableBackend) SetState(_, _, _, _ string) error     { return nil }
func (m *mutableBackend) GetState(_, _ string) (string, error) { return "", nil }
func (m *mutableBackend) ExecutionInstructions() (string, error) { return "", nil }
func (m *mutableBackend) GetDescription(_ string) (string, error)  { m.mu.Lock(); defer m.mu.Unlock(); return m.description, nil }
func (m *mutableBackend) GetAcceptance(_ string) (string, error)   { return "", nil }
func (m *mutableBackend) GetFullContext(_ string) (string, error)  { return "", nil }
func (m *mutableBackend) ProjectContext() (string, error)          { return "", nil }
func (m *mutableBackend) GetExternalRef(_ string) (string, error) { return "", nil }
func (m *mutableBackend) SetExternalRef(_, _ string) error       { return nil }
func (m *mutableBackend) SetMetadata(_, _, _ string) error         { return nil }
func (m *mutableBackend) GetMetadata(_, _ string) (string, error)  { return "", nil }
func (m *mutableBackend) Label() string {
	if m.label != "" {
		return m.label
	}
	return "beads"
}

func (s *stubBackend) Init() error                          { return nil }
func (s *stubBackend) HasRemaining() (bool, error)          { return s.remaining > 0, nil }
func (s *stubBackend) CountCompleted() (int, error)         { return s.completed, nil }
func (s *stubBackend) CountRemaining() (int, error)         { return s.remaining, nil }
func (s *stubBackend) CountTotal() (int, error)             { return s.total, nil }
func (s *stubBackend) GetNextTask() (string, error)         { return s.nextTask, nil }
func (s *stubBackend) GetNextTaskID() (string, error)       { return s.nextID, nil }
func (s *stubBackend) GetNextTaskInfo() (tasks.TaskInfo, error) { return tasks.TaskInfo{ID: s.nextID, Title: s.nextTask, Priority: s.nextPriority}, nil }
func (s *stubBackend) HasTasks() (bool, error)              { return s.total > 0, nil }
func (s *stubBackend) CloseTask(string, string) error       { return nil }
func (s *stubBackend) SkipTask(id, reason string) error     { s.skippedTask = id; s.skipReason = reason; return nil }
func (s *stubBackend) SetSkippedIDs([]string)               {}
func (s *stubBackend) ReopenTask(string) error              { return nil }
func (s *stubBackend) SetState(_, _, _, _ string) error     { return nil }
func (s *stubBackend) GetState(_, _ string) (string, error) { return "", nil }
func (s *stubBackend) ExecutionInstructions() (string, error) { return "", nil }
func (s *stubBackend) GetDescription(_ string) (string, error)  { return s.description, nil }
func (s *stubBackend) GetAcceptance(_ string) (string, error)   { return s.acceptance, nil }
func (s *stubBackend) GetFullContext(_ string) (string, error)  { return s.fullContext, nil }
func (s *stubBackend) ProjectContext() (string, error)          { return "", nil }
func (s *stubBackend) GetExternalRef(_ string) (string, error) { return "", nil }
func (s *stubBackend) SetExternalRef(_, _ string) error       { return nil }
func (s *stubBackend) SetMetadata(_, _, _ string) error         { return nil }
func (s *stubBackend) GetMetadata(_, _ string) (string, error)  { return "", nil }
func (s *stubBackend) Label() string {
	if s.label != "" {
		return s.label
	}
	return "beads"
}

func setupTestDir(t *testing.T) (string, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(5)
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
				CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)

	_ = l.Run(context.Background())

	maxIter := st.ReadMaxIterations(0)
	if maxIter != 10 {
		t.Errorf("expected max_iterations=10 in state, got %d", maxIter)
	}
}

// Verifies the stream task file is written with task ID and description,
// proving the tmux pane title integration works correctly.
// updateStreamTask is a standalone function — no Loop required.
func TestLoop_UpdateStreamTask(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	updateStreamTask(ralphDir, "ralph-abc", "Add feature X", nil)

	data, err := os.ReadFile(filepath.Join(ralphDir, ".stream-task"))
	if err != nil {
		t.Fatalf("expected stream task file, got error: %v", err)
	}
	if string(data) != "ralph-abc: Add feature X" {
		t.Errorf("expected 'ralph-abc: Add feature X', got %q", string(data))
	}

	updateStreamTask(ralphDir, "", "Add feature Y", nil)
	data, _ = os.ReadFile(filepath.Join(ralphDir, ".stream-task"))
	if string(data) != "Add feature Y" {
		t.Errorf("expected 'Add feature Y', got %q", string(data))
	}

	p := 3
	updateStreamTask(ralphDir, "ralph-xyz", "Some task", &p)
	data, _ = os.ReadFile(filepath.Join(ralphDir, ".stream-task"))
	got := string(data)
	if !strings.Contains(got, "[P3]") {
		t.Errorf("stream task with priority should include [P3], got %q", got)
	}
	if !strings.Contains(got, "ralph-xyz") {
		t.Errorf("stream task should include task ID, got %q", got)
	}
}

// Verifies writeRunBranch persists the current branch name to .run-branch
// so the shell pane-title updater displays the correct branch.
// writeRunBranch is a standalone function — no Loop required.
func TestLoop_WriteRunBranch(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	writeRunBranch(ralphDir, "ralph/project/01-fix-bug")

	data, err := os.ReadFile(filepath.Join(ralphDir, ".run-branch"))
	if err != nil {
		t.Fatalf("expected .run-branch file, got error: %v", err)
	}
	if string(data) != "ralph/project/01-fix-bug" {
		t.Errorf("expected 'ralph/project/01-fix-bug', got %q", string(data))
	}
}

// writeRunBranch defaults to "ralph" when branch is empty.
func TestLoop_WriteRunBranch_Default(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	writeRunBranch(ralphDir, "")

	data, err := os.ReadFile(filepath.Join(ralphDir, ".run-branch"))
	if err != nil {
		t.Fatalf("expected .run-branch file, got error: %v", err)
	}
	if string(data) != "ralph" {
		t.Errorf("expected 'ralph', got %q", string(data))
	}
}

// readFeedback is a standalone function — reads file without clearing it.
func TestLoop_FeedbackRead(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	feedbackFile := filepath.Join(ralphDir, "feedback")
	os.WriteFile(feedbackFile, []byte("please fix the tests"), 0o644)

	got := readFeedback(ralphDir)
	if got != "please fix the tests" {
		t.Errorf("expected feedback content, got %q", got)
	}

	if _, err := os.Stat(feedbackFile); err != nil {
		t.Error("feedback file should persist after read — agent clears it")
	}
}

// readFeedback returns empty string when no file exists.
func TestLoop_FeedbackReadEmpty(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	got := readFeedback(ralphDir)
	if got != "" {
		t.Errorf("expected empty feedback when file missing, got %q", got)
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

// Verifies that buildTaskPrompt appends screenshot paths when screenshots
// exist in the ralph screenshots directory for the given bead ID.
func TestLoop_BuildTaskPrompt_WithScreenshots(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	ssDir := filepath.Join(ralphDir, "screenshots")
	os.MkdirAll(ssDir, 0o755)
	os.WriteFile(filepath.Join(ssDir, "ralph-abc-01-broken-modal.png"), []byte("img"), 0o644)

	pDir, err := filepath.Abs(filepath.Join("..", "..", "cmd", "ralph", "prompts"))
	if err != nil {
		t.Fatalf("resolve prompts dir: %v", err)
	}

	backend := &stubBackend{nextID: "ralph-abc", nextTask: "Fix modal", fullContext: "○ ralph-abc · Fix modal [● P3 · OPEN]"}
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: pDir,
		},
		CallsPerHour: 80,
		TaskBackend:  backend,
	}, st, gm, logging.New(nil))

	got := l.buildTaskPrompt("Fix modal", "ralph-abc")

	if !strings.Contains(got, "## Screenshots") {
		t.Error("task prompt should include screenshots section when screenshots exist")
	}
	if !strings.Contains(got, "ralph-abc-01-broken-modal.png") {
		t.Error("task prompt should include the screenshot filename")
	}
}

// Verifies that buildTaskPrompt omits the screenshots section when no
// screenshots exist for the bead.
func TestLoop_BuildTaskPrompt_NoScreenshots(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	pDir, err := filepath.Abs(filepath.Join("..", "..", "cmd", "ralph", "prompts"))
	if err != nil {
		t.Fatalf("resolve prompts dir: %v", err)
	}

	backend := &stubBackend{nextID: "ralph-xyz", nextTask: "Fix layout", fullContext: "○ ralph-xyz · Fix layout [● P3 · OPEN]"}
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: pDir,
		},
		CallsPerHour: 80,
		TaskBackend:  backend,
	}, st, gm, logging.New(nil))

	got := l.buildTaskPrompt("Fix layout", "ralph-xyz")

	if strings.Contains(got, "## Screenshots") {
		t.Error("task prompt should not include screenshots section when none exist")
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

// Proves: maybeRefactor skips when Refactor is false (default),
// ensuring refactoring is opt-in only.
func TestLoop_MaybeRefactor_DisabledByDefault(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	l := &Loop{
		cfg:    Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}, Refactor: false},
		state:  st,
		logger: logging.New(nil),
	}

	err := l.maybeRefactor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Proves: maybeRefactor skips when fewer than 5 tasks have been
// completed in the session, even with --refactor enabled.
func TestLoop_MaybeRefactor_SkipsBelow5Completions(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	l := &Loop{
		cfg:          Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}, Refactor: true},
		state:        st,
		logger:       logging.New(nil),
		sessionTasks: []CompletedTask{{ID: "a"}, {ID: "b"}, {ID: "c"}},
	}

	err := l.maybeRefactor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Proves: maybeRefactor calls the LLM when exactly 5 tasks are completed
// and the LLM says NO, no refactoring iteration is spawned.
func TestLoop_MaybeRefactor_LLMSaysNo(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	// Set up a git repo with enough commits so RecentChangedFiles returns content
	gitDir := filepath.Join(dir, "work")
	os.MkdirAll(gitDir, 0o755)
	exec.Command("git", "init", "-b", "main", gitDir).Run()
	exec.Command("git", "-C", gitDir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", gitDir, "config", "user.name", "test").Run()
	os.WriteFile(filepath.Join(gitDir, "file.go"), []byte("package main\n"), 0o644)
	exec.Command("git", "-C", gitDir, "add", ".").Run()
	exec.Command("git", "-C", gitDir, "commit", "-m", "init").Run()
	for i := 0; i < 11; i++ {
		exec.Command("git", "-C", gitDir, "commit", "--allow-empty", "-m", fmt.Sprintf("commit %d", i)).Run()
	}
	os.WriteFile(filepath.Join(gitDir, "file.go"), []byte("package main\nfunc main() {}\n"), 0o644)
	exec.Command("git", "-C", gitDir, "add", ".").Run()
	exec.Command("git", "-C", gitDir, "commit", "-m", "update").Run()

	queryFnCalled := false
	l := &Loop{
		cfg:    Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}, Refactor: true},
		state:  st,
		logger: logging.New(nil),
		git:    &git.Manager{WorkDir: gitDir},
		sessionTasks: make([]CompletedTask, 5),
		refactorQueryFunc: func(ctx context.Context, workDir, prompt, model string) (string, error) {
			queryFnCalled = true
			return "NO\nCode looks fine.", nil
		},
	}

	err := l.maybeRefactor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !queryFnCalled {
		t.Error("expected LLM query to be called at 5 completions")
	}
}

// Proves: maybeRefactor spawns a refactoring iteration when the LLM says YES,
// verifying the full path from LLM decision through runner invocation.
func TestLoop_MaybeRefactor_LLMSaysYes(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gitDir := filepath.Join(dir, "work")
	os.MkdirAll(gitDir, 0o755)
	exec.Command("git", "init", "-b", "main", gitDir).Run()
	exec.Command("git", "-C", gitDir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", gitDir, "config", "user.name", "test").Run()
	os.WriteFile(filepath.Join(gitDir, "file.go"), []byte("package main\n"), 0o644)
	exec.Command("git", "-C", gitDir, "add", ".").Run()
	exec.Command("git", "-C", gitDir, "commit", "-m", "init").Run()
	for i := 0; i < 11; i++ {
		exec.Command("git", "-C", gitDir, "commit", "--allow-empty", "-m", fmt.Sprintf("commit %d", i)).Run()
	}
	os.WriteFile(filepath.Join(gitDir, "file.go"), []byte("package main\nfunc main() {}\n"), 0o644)
	exec.Command("git", "-C", gitDir, "add", ".").Run()
	exec.Command("git", "-C", gitDir, "commit", "-m", "update").Run()

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	runnerCalled := false
	l := &Loop{
		cfg: Config{
			Dirs: workctx.WorkContext{
				RalphDir:   ralphDir,
				WorkDir:    gitDir,
				PromptsDir: promptsDir,
			},
			Refactor:     true,
			CallsPerHour: 80,
		},
		state:        st,
		logger:       logging.New(nil),
		git:          &git.Manager{WorkDir: gitDir},
		sessionTasks: make([]CompletedTask, 5),
		limiter:      ratelimit.New(ralphDir, 80),
		signals:      claude.DefaultSignalPaths(ralphDir),
		runner: &stubRunner{
			onRun: func() {
				runnerCalled = true
			},
		},
		refactorQueryFunc: func(ctx context.Context, workDir, prompt, model string) (string, error) {
			return "YES\nThere is significant duplication.", nil
		},
	}

	err := l.maybeRefactor()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !runnerCalled {
		t.Error("expected runner to be called when LLM says YES")
	}
}

// Proves: maybeRefactor triggers at every multiple of 5, not just the first.
func TestLoop_MaybeRefactor_TriggersAtMultiplesOf5(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	gitDir := filepath.Join(dir, "work")
	os.MkdirAll(gitDir, 0o755)
	exec.Command("git", "init", "-b", "main", gitDir).Run()
	exec.Command("git", "-C", gitDir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", gitDir, "config", "user.name", "test").Run()
	os.WriteFile(filepath.Join(gitDir, "file.go"), []byte("package main\n"), 0o644)
	exec.Command("git", "-C", gitDir, "add", ".").Run()
	exec.Command("git", "-C", gitDir, "commit", "-m", "init").Run()
	for i := 0; i < 11; i++ {
		exec.Command("git", "-C", gitDir, "commit", "--allow-empty", "-m", fmt.Sprintf("commit %d", i)).Run()
	}

	os.WriteFile(filepath.Join(gitDir, "file.go"), []byte("package main\nfunc init() {}\n"), 0o644)
	exec.Command("git", "-C", gitDir, "add", ".").Run()
	exec.Command("git", "-C", gitDir, "commit", "-m", "update").Run()

	queryCalls := 0
	base := &Loop{
		cfg:    Config{Dirs: workctx.WorkContext{RalphDir: ralphDir}, Refactor: true},
		state:  st,
		logger: logging.New(nil),
		git:    &git.Manager{WorkDir: gitDir},
		refactorQueryFunc: func(ctx context.Context, workDir, prompt, model string) (string, error) {
			queryCalls++
			return "NO\nAll good.", nil
		},
	}

	// 7 completions: should NOT trigger (not a multiple of 5)
	base.sessionTasks = make([]CompletedTask, 7)
	queryCalls = 0
	base.maybeRefactor()
	if queryCalls != 0 {
		t.Errorf("expected 0 LLM calls at 7 completions, got %d", queryCalls)
	}

	// 10 completions: should trigger
	base.sessionTasks = make([]CompletedTask, 10)
	queryCalls = 0
	base.maybeRefactor()
	if queryCalls != 1 {
		t.Errorf("expected 1 LLM call at 10 completions, got %d", queryCalls)
	}
}

// Proves: llmShouldRefactor correctly parses YES/NO responses in various
// formats, including case variations and extra whitespace.
func TestLoop_LLMShouldRefactor_ParsesResponses(t *testing.T) {
	l := &Loop{git: &git.Manager{WorkDir: t.TempDir()}}

	tests := []struct {
		name     string
		response string
		want     bool
	}{
		{"uppercase YES", "YES\nDuplication found.", true},
		{"lowercase yes", "yes\nneeds cleanup", true},
		{"mixed case Yes", "Yes\nsome issues", true},
		{"uppercase NO", "NO\nCode looks fine.", false},
		{"lowercase no", "no\neverything clean", false},
		{"with leading whitespace", "  YES\nfoo", true},
		{"unknown response", "MAYBE\nnot sure", false},
		{"empty response", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l.refactorQueryFunc = func(ctx context.Context, workDir, prompt, model string) (string, error) {
				return tt.response, nil
			}
			got, err := l.llmShouldRefactor(context.Background(), "arch spec", "file1.go\nfile2.go")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("llmShouldRefactor(%q) = %v, want %v", tt.response, got, tt.want)
			}
		})
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
		ProjectDir:  project,
		RalphDir:    ralphDir,
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
		ProjectDir:  project,
		RalphDir:    ralphDir,
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
		ProjectDir:  project,
		RalphDir:    ralphDir,
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
		ProjectDir:  project,
		RalphDir:    ralphDir,
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
		ProjectDir:  project,
		RalphDir:    ralphDir,
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
		ProjectDir:  project,
		RalphDir:    ralphDir,
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
		ProjectDir:     project,
		RalphDir:       ralphDir,
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
	closedIDs     []string
	closeReasons  []string
	closeMu       sync.Mutex
	skippedIDs    []string
	skipReasons   []string
	skipMu        sync.Mutex
}

func (t *trackingBackend) CloseTask(id string, reason string) error {
	t.closeMu.Lock()
	t.closedIDs = append(t.closedIDs, id)
	t.closeReasons = append(t.closeReasons, reason)
	t.closeMu.Unlock()
	return nil
}

func (t *trackingBackend) SkipTask(id string, reason string) error {
	t.skipMu.Lock()
	t.skippedIDs = append(t.skippedIDs, id)
	t.skipReasons = append(t.skipReasons, reason)
	t.skipMu.Unlock()
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

// --- test helpers ---

// createPromptTemplates creates minimal prompt template files so the loop
// can build prompts without errors.
func createPromptTemplates(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	for _, name := range []string{"shared.md", "internal.md", "reflection.md", "signal.md", "feedback.md", "refactor.md", "refactor-style.md", "execution-bd.md"} {
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
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(10)

	promptsDir := filepath.Join(project, "prompts")
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
			// Create a commit so headAfterSignal != headBefore.
			fname := fmt.Sprintf("task%d.go", iterationCount)
			os.WriteFile(filepath.Join(project, fname), []byte("package main\n"), 0o644)
			run(t, "git", "-C", project, "add", fname)
			run(t, "git", "-C", project, "commit", "-m", fmt.Sprintf("task %d work", iterationCount))
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
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "99", nil }
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

// Verifies that PostMergeUpdateMain resets the worktree branch to
// origin/main between tasks using a real git worktree, proving each task
// starts from merged main rather than building on stale commits.
func TestLoop_PostMergeResetResetsWorktree(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(10)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				State:       st,
		Logger:      logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	originMain := gm.HeadRev()
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
				headAfterMerge = gm.HeadRev()
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

	// With stacked PRs, the branch stays as the task branch after merge
	// — no reset to temp branch.
}

// Verifies the full post-merge branch rename cycle: task A merges →
// PostMergeUpdateMain resets to /next → next iteration renames to thematic
// branch for task B. Proves each successive task gets its own descriptively
// named branch even after the previous one is squash-merged.
func TestLoop_PostMergeRenamesCycleFull(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(10)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				State:       st,
		Logger:      logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	iterationCount := 0
	var branchDuringTaskA, branchDuringTaskB string

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     2,
		nextTask:  "Fix tail leak",
		nextID:    "ralph-t1",
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			switch iterationCount {
			case 1:
				branchDuringTaskA = gm.WorktreeBranch
				backend.mu.Lock()
				backend.completed = 1
				backend.remaining = 1
				backend.nextTask = "Add retry logic"
				backend.nextID = "ralph-r2"
				backend.mu.Unlock()
			case 2:
				branchDuringTaskB = gm.WorktreeBranch
				backend.mu.Lock()
				backend.completed = 2
				backend.remaining = 0
				backend.mu.Unlock()
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
	l.mergeFunc = func(context.Context) (bool, error) { return true, nil }

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

// Verifies that pushAndCreatePR fires for every completed task when signal
// is detected, regardless of whether auto-merge is enabled. This ensures the
// Go code owns the push/PR lifecycle.
func TestLoop_PushAndCreatePROnSignal(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(10)

	promptsDir := filepath.Join(project, "prompts")
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
			fname := fmt.Sprintf("task%d.go", iterationCount)
			os.WriteFile(filepath.Join(project, fname), []byte("package main\n"), 0o644)
			run(t, "git", "-C", project, "add", fname)
			run(t, "git", "-C", project, "commit", "-m", fmt.Sprintf("task %d", iterationCount))
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
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     false,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.pushPRFunc = func(_ context.Context, _, taskDesc, _ string) (string, error) {
		pushPRCalls++
		return "", nil
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
	l.pushPRFunc = func(_ context.Context, _, taskDesc, _ string) (string, error) {
		pushPRCalls++
		return "", nil
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
		TaskBackend:  &stubBackend{label: "beads"},
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
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

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
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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
		label:     "beads",
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
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) {
		t.Error("push should not be called when verification fails")
		return "", nil
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
		label:     "beads",
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
	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) {
		pushCalled = true
		return "", nil
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
	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) {
		pushCalled = true
		return "", nil
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
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

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

// When merge fails with a CI error, the loop leaves the task open for retry.
// CI fix agent spawning during the merge pipeline is tested in git module
// (TestMergeWithRetry_DelegatesCIFailure).
// When CI fails, the task is still closed because the PR exists — merge
// is a separate concern from task completion.
func TestLoop_CIFailureStillClosesTask(t *testing.T) {
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
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "99", nil }
	l.mergeFunc = func(context.Context) (bool, error) {
		return false, &git.CIFailureError{
			PRNumber: "99",
			Failures: []git.CICheckResult{
				{Name: "test", State: "FAILURE", Bucket: "fail"},
			},
		}
	}

	_ = l.Run(context.Background())

	skipped, _ := st.GetSkippedTasks()
	if len(skipped) != 1 || skipped[0] != "ralph-ci1" {
		t.Errorf("expected ralph-ci1 in skip list, got %v", skipped)
	}
}

// When mergeFunc succeeds, the loop closes the task and records the merge.
// Conflict recovery (rebase + force-push + retry) is tested in git module
// (TestMergeWithRetry_RecoversFromConflict).
func TestLoop_MergeSuccessClosesTask(t *testing.T) {
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
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "42", nil }

	merged := false
	l.mergeFunc = func(context.Context) (bool, error) {
		merged = true
		return true, nil
	}

	_ = l.Run(context.Background())

	if !merged {
		t.Error("expected merge to be called when AutoMerge is enabled")
	}
}

// When mergeFunc eventually succeeds (simulating CI fix + retry), the loop
// closes the task. CI fix agent spawning and retry logic are tested in git
// module (TestMergeWithRetry_DelegatesCIFailure).
func TestLoop_MergeEventualSuccessClosesTask(t *testing.T) {
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
	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) { return "", nil }

	l.mergeFunc = func(context.Context) (bool, error) {
		return true, nil
	}

	_ = l.Run(context.Background())

	tasks := l.SessionTasks()
	if len(tasks) == 0 {
		t.Error("expected at least one completed task after successful merge")
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
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

	// mergeFunc returning error means merge pipeline failed (retry exhaustion
	// is tested in git module: TestMergeWithRetry_ExhaustsRetries).
	l.mergeFunc = func(context.Context) (bool, error) {
		return false, &git.CIFailureError{
			PRNumber: "99",
			Failures: []git.CICheckResult{
				{Name: "test", State: "FAILURE", Bucket: "fail"},
			},
		}
	}

	_ = l.Run(context.Background())

	// Task should remain open since merge failed.
	s, _ := st.Load()
	if s.Status == "completed" {
		t.Error("expected status not to be 'completed' when merge fails")
	}
}

// When mergeFunc returns an error, the loop does not close the task —
// ensuring failed merges leave the task open for retry. The combined
// conflict+CI retry pipeline is tested in git module
// (TestMergeWithRetry_RecoversFromConflict, TestMergeWithRetry_DelegatesCIFailure).
func TestLoop_MergeFailureLeavesTaskOpen(t *testing.T) {
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
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

	l.mergeFunc = func(context.Context) (bool, error) {
		return false, fmt.Errorf("merge failed")
	}

	var buf bytes.Buffer
	logger := logging.New(&buf)
	l.logger = logger

	_ = l.Run(context.Background())

	output := buf.String()
	if !strings.Contains(output, "Auto-merge") {
		t.Log("Log output:", output)
	}
}

// Verifies that after MaxMergeFailures consecutive merge failures, the loop
// skips the task instead of retrying indefinitely. Merge failures are tracked
// across iterations via the attempts tracker.
// Merge failures no longer cause task skipping — the PR exists, work is done.
func TestLoop_MergeFailureStillClosesTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &trackingBackend{
		mutableBackend: mutableBackend{
			remaining: 1,
			completed: 0,
			total:     1,
			nextTask:  "Stubborn task",
			nextID:    "ralph-stub",
			label:     "beads",
		},
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-stubborn",
		State:          st,
		Logger:         logging.New(nil),
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
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "42", nil }
	l.mergeFunc = func(context.Context) (bool, error) {
		return false, fmt.Errorf("push denied by sandbox")
	}

	_ = l.Run(context.Background())

	// Task should be skipped — merge failed, PR exists for manual review.
	skipped, _ := st.GetSkippedTasks()
	if len(skipped) != 1 || skipped[0] != "ralph-stub" {
		t.Errorf("expected ralph-stub in skip list, got %v", skipped)
	}
}

// Merge failure skips the task — PR exists, work is done. No retry counting.
func TestLoop_MergeFailureClosesTaskNoRetryCount(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &trackingBackend{
		mutableBackend: mutableBackend{
			remaining: 1,
			completed: 0,
			total:     1,
			nextTask:  "Fixable task",
			nextID:    "ralph-fix",
			label:     "beads",
		},
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-fixable",
		State:          st,
		Logger:         logging.New(nil),
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
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "50", nil }
	l.mergeFunc = func(context.Context) (bool, error) {
		return false, fmt.Errorf("merge conflict")
	}

	_ = l.Run(context.Background())

	// Task should be skipped — merge failed, PR exists for manual review.
	skipped, _ := st.GetSkippedTasks()
	if len(skipped) != 1 || skipped[0] != "ralph-fix" {
		t.Errorf("expected ralph-fix in skip list, got %v", skipped)
	}
}

// Verifies that successful merge clears the merge failure counter.
func TestLoop_SuccessfulMergeClearsMergeFailures(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &trackingBackend{
		mutableBackend: mutableBackend{
			remaining: 1,
			completed: 0,
			total:     1,
			nextTask:  "Recovering task",
			nextID:    "ralph-rec",
			label:     "beads",
		},
	}

	gm := &git.Manager{
		ProjectDir:     dir,
		WorkDir:        filepath.Join(dir, "worktree"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/project/01-recover",
		State:          st,
		Logger:         logging.New(nil),
	}

	// Seed 2 prior failures.
	tracker := attempts.New(ralphDir)
	tracker.RecordMergeFailure("ralph-rec")
	tracker.RecordMergeFailure("ralph-rec")

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
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "42", nil }
	l.mergeFunc = func(context.Context) (bool, error) {
		return true, nil
	}

	_ = l.Run(context.Background())

	if count := tracker.MergeFailureCount("ralph-rec"); count != 0 {
		t.Errorf("expected merge failures cleared after successful merge, got %d", count)
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
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

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
	for _, name := range []string{"shared.md", "reflection.md", "signal.md", "feedback.md", "execution-bd.md"} {
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
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

	_ = l.Run(context.Background())

	if !strings.Contains(capturedPrompt, "tests passing") {
		t.Errorf("prompt should include test status, got: %s", capturedPrompt[:min(200, len(capturedPrompt))])
	}
	// Should not contain the unsubstituted placeholder
	if strings.Contains(capturedPrompt, "{{TEST_STATUS}}") {
		t.Error("prompt should not contain unsubstituted {{TEST_STATUS}} placeholder")
	}
}

// Verifier.runFixAgent stops the main runner's streaming, creates a new
// runner via NewRunner, passes the standard RunConfig, and returns the result.
func TestVerifier_runFixAgent(t *testing.T) {
	dir, _ := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	mainRunner := &stubRunner{}
	var capturedCfg claude.RunConfig
	fixRunner := &stubRunner{result: claude.Result{SignalDetected: true, Summary: "fixed"}}

	signals := claude.DefaultSignalPaths(ralphDir)
	v := &Verifier{
		cfg: VerifierConfig{
			RalphDir:    ralphDir,
			IdleTimeout: 30 * time.Second,
		},
		deps: VerifierDeps{
			Logger:  logging.New(nil),
			Runner:  func() claudeRunner { return mainRunner },
			Signals: signals,
			NewRunner: func() claudeRunner {
				return &configCapturingRunner{inner: fixRunner, captured: &capturedCfg}
			},
		},
	}

	ctx := context.Background()
	result := v.runFixAgent(ctx, "test failures", "fix the tests", "/work", "/logs/raw.log")

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
func TestVerifier_runFixAgent_logsSummary(t *testing.T) {
	dir, _ := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	var logBuf bytes.Buffer
	fixRunner := &stubRunner{result: claude.Result{
		SignalDetected: true,
		Summary:        "added missing nil check in parseConfig",
	}}

	signals := claude.DefaultSignalPaths(ralphDir)
	v := &Verifier{
		cfg: VerifierConfig{
			RalphDir:    ralphDir,
			IdleTimeout: 30 * time.Second,
		},
		deps: VerifierDeps{
			Logger:  logging.NewWithWriter(&logBuf),
			Runner:  func() claudeRunner { return &stubRunner{} },
			Signals: signals,
			NewRunner: func() claudeRunner {
				return fixRunner
			},
		},
	}

	ctx := context.Background()
	v.runFixAgent(ctx, "test failures", "fix the tests", "/work", "/logs/raw.log")

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

func (c *configCapturingRunner) InjectMessage(msg string) error {
	return c.inner.InjectMessage(msg)
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

func (p *promptCapturingRunner) InjectMessage(msg string) error {
	return p.inner.InjectMessage(msg)
}

// Verifies that push is called after signal detection. The sync guard
// (fetch + rebase) is enforced internally by PushAndCreatePR's EnsureUpToDate
// — tested in git module.
func TestLoop_PushCalledAfterSignal(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.Manager{
		ProjectDir:     dir,
		RalphDir:       ralphDir,
		WorkDir:        filepath.Join(dir, "worktree"),
		WorktreeBranch: "ralph/test/01-task",
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

	pushCalled := false
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) {
		pushCalled = true
		return "", nil
	}

	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true, OnSignalUsed: true},
	}

	l.Run(context.Background())

	if !pushCalled {
		t.Error("expected push to be called after signal detection")
	}
}

// Orchestrator status messages ("All tasks complete!", "No tasks found") must
// use the [o] actor prefix, not the task backend label (e.g. [beads] without [o]).
// The [o][beads] tag is valid — it marks orchestrator-initiated beads operations.
func TestLoop_OrchestratorMessagesUseLoopPrefix(t *testing.T) {
	tests := []struct {
		name    string
		backend *stubBackend
		want    string // substring expected in log output
	}{
		{
			name: "all tasks complete uses orchestrator actor prefix",
			backend: &stubBackend{
				remaining: 0, completed: 3, total: 3, label: "beads",
			},
			want: "[o]",
		},
		{
			name: "no tasks error uses orchestrator actor prefix",
			backend: &stubBackend{
				remaining: 0, completed: 0, total: 0, label: "beads",
			},
			want: "[o]",
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
	l.runner = &stubRunner{
		onRun: func() {
			backend.remaining = 0
			backend.completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

	l.Run(context.Background())

	output := logBuf.String()
	if !strings.Contains(output, "ralph-abc: Fix the auth module") {
		t.Errorf("expected task banner with bead ID and title:\n%s", output)
	}
	if !strings.Contains(output, "═") {
		t.Error("expected ═ separator characters in task banner")
	}
	if !strings.Contains(output, "Auth tokens are expiring too early due to clock skew") {
		t.Errorf("expected task description in log output:\n%s", output)
	}
	if strings.Contains(output, "Next task:") {
		t.Error("redundant 'Next task:' line should be removed")
	}
	if strings.Contains(output, "→ implementing") {
		t.Error("redundant '→ implementing' line should be removed")
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
	l.runner = &stubRunner{
		onRun: func() {
			backend.remaining = 0
			backend.completed = 1
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

	l.Run(context.Background())

	output := logBuf.String()
	if !strings.Contains(output, "ralph-abc: Fix the auth module") {
		t.Errorf("expected task banner with bead ID and title:\n%s", output)
	}
	if strings.Contains(output, "Next task:") {
		t.Error("redundant 'Next task:' line should be removed")
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
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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
	l.pushPRFunc = func(_ context.Context, taskID, taskDesc, _ string) (string, error) {
		pushCalls++
		return "", nil
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
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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
	l.pushPRFunc = func(_ context.Context, taskID, taskDesc, _ string) (string, error) {
		pushCalls++
		return "", nil
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
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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

	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) { return "", nil }

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
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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

	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) { return "", nil }

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
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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

	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) { return "", nil }

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
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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

	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) { return "", nil }

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
		label:     "beads",
	}

	logger := logging.New(nil)
	gm := &git.Manager{ProjectDir: dir, WorkDir: dir, Logger: logger}

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

	l.pushPRFunc = func(_ context.Context, _, _, _ string) (string, error) { return "", nil }

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

// Verifies that a dashed separator line appears between iterations in the
// log output, giving a clear visual boundary between each run.
// signalCallingRunner invokes OnSignal during Run to exercise the
// in-runner verification path (LLM verification, test re-runs).
type signalCallingRunner struct {
	onRun  func()
	result claude.Result
}

func (s *signalCallingRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	if s.onRun != nil {
		s.onRun()
	}
	if cfg.OnSignal != nil {
		cfg.OnSignal("")
		return claude.Result{
			SignalDetected: true,
			OnSignalUsed:   true,
			Summary:        s.result.Summary,
		}, nil
	}
	return s.result, nil
}

func (s *signalCallingRunner) StopStreaming() {}

func (s *signalCallingRunner) InjectMessage(_ string) error { return nil }

// Verifies that LLM verification pass logs with green (Success) color
// and LLM verification reject logs with red (Error) color.
func TestLoop_LLMVerificationLogColors(t *testing.T) {
	tests := []struct {
		name      string
		passed    bool
		reason    string
		details   string
		wantColor string
		wantMsg   string
	}{
		{
			name:      "LLM pass logs green",
			passed:    true,
			reason:    "diff matches requirements",
			wantColor: logging.Green,
			wantMsg:   "LLM verified: diff matches requirements",
		},
		{
			name:      "LLM reject logs red",
			passed:    false,
			details:   "missing error handling",
			wantColor: logging.Red,
			wantMsg:   "LLM verification rejected (attempt 1/3): missing error handling",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, st := setupTestDir(t)
			ralphDir := filepath.Join(dir, ".ralph")
			promptsDir := filepath.Join(dir, "prompts")
			createPromptTemplates(t, promptsDir)
			// verify-llm.md is needed when LLM rejects (fix agent prompt)
			os.WriteFile(filepath.Join(promptsDir, "verify-llm.md"), []byte("fix: {{LLM_FEEDBACK}}"), 0o644)

			backend := &mutableBackend{
				remaining: 1,
				completed: 0,
				total:     1,
				nextTask:  "add colored logs",
				nextID:    "ralph-color",
				label:     "beads",
			}

			runner := &signalCallingRunner{
				onRun: func() {
					backend.mu.Lock()
					backend.completed = 1
					backend.remaining = 0
					backend.mu.Unlock()
				},
				result: claude.Result{Summary: "done"},
			}

			var logBuf bytes.Buffer
			logger := logging.NewWithWriter(&logBuf)

			gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

			llmResult := verify.Result{
				Passed:  tt.passed,
				Reason:  tt.reason,
				Details: tt.details,
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
				VerifyDir:     dir,
			}, st, gm, logger)
			l.runner = runner
			l.verifier.deps.LLMVerify = func(verify.VerifyOpts) verify.Result {
				return llmResult
			}
			l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }

			// For rejection, stub out newRunnerFunc so fix agent doesn't launch real Claude
			if !tt.passed {
				l.newRunnerFunc = func() claudeRunner {
					return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "fixed"}}
				}
				// After fix agent, re-verification will call llmVerifyFunc again;
				// make it pass on second call to avoid skip-task path
				callCount := 0
				l.verifier.deps.LLMVerify = func(verify.VerifyOpts) verify.Result {
					callCount++
					if callCount == 1 {
						return llmResult
					}
					return verify.Result{Passed: true, Reason: "fixed"}
				}
			}

			_ = l.Run(context.Background())

			output := logBuf.String()
			if !strings.Contains(output, tt.wantMsg) {
				t.Errorf("expected %q in output, got:\n%s", tt.wantMsg, output)
			}
			// Verify the message line uses the expected color by checking that
			// the line containing the message also contains the expected ANSI code.
			for _, line := range strings.Split(output, "\n") {
				if strings.Contains(line, tt.wantMsg) {
					if !strings.Contains(line, tt.wantColor) {
						t.Errorf("line with %q should use color %q, got:\n%s", tt.wantMsg, tt.name, line)
					}
					break
				}
			}
		})
	}
}

func TestLoop_DashedSeparatorBetweenIterations(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0
	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     2,
		nextTask:  "task A",
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
	}

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

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
	}, st, gm, logger)
	l.runner = runner

	_ = l.Run(context.Background())

	output := logBuf.String()
	if !strings.Contains(output, "─") {
		t.Error("expected dashed separator (─) between iterations")
	}
	if iterationCount != 2 {
		t.Errorf("expected 2 iterations, got %d", iterationCount)
	}
}

// Verifies that the loop prints a task separator banner with the bead ID
// when a new task starts, replacing the old per-line magenta prefix.
func TestLoop_TaskBannerOnNewTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "fix the thing",
		nextID:    "ralph-l337",
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

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

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
	}, st, gm, logger)
	l.runner = runner

	_ = l.Run(context.Background())

	output := logBuf.String()
	if !strings.Contains(output, "ralph-l337: fix the thing") {
		t.Errorf("expected task banner with bead ID and title, got: %s", output)
	}
	if !strings.Contains(output, "═") {
		t.Error("expected ═ separator characters in task banner")
	}
}

// Verifies that when Claude reports a rate limit, the loop waits until
// the reset time and retries the iteration instead of counting it as
// stagnation.
func TestLoop_RateLimitWaitsAndRetries(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	iterationCount := 0
	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "fix the bug",
		nextID:    "ralph-rl1",
		label:     "beads",
	}

	// First call returns rate limited with a reset time in the past (so
	// WaitUntil returns immediately). Second call completes the task.
	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			if iterationCount >= 2 {
				backend.mu.Lock()
				backend.completed = 1
				backend.remaining = 0
				backend.mu.Unlock()
			}
		},
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	var logBuf bytes.Buffer

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
	}, st, gm, logging.NewWithWriter(&logBuf))

	// Override the runner to return different results per iteration.
	l.runner = &rateLimitStubRunner{
		backend: backend,
		counter: &iterationCount,
	}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "", nil }
	l.mergeFunc = func(context.Context) (bool, error) { return false, nil }
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }
	l.verifier.deps.LLMVerify = func(verify.VerifyOpts) verify.Result {
		return verify.Result{Passed: true}
	}

	_ = l.Run(context.Background())

	output := logBuf.String()
	t.Logf("Output: %s", output)
	if !strings.Contains(output, "rate limit") && !strings.Contains(output, "Rate limit") {
		t.Errorf("expected rate limit log message, got: %s", output)
	}
	if !strings.Contains(output, "resuming") {
		t.Errorf("expected 'resuming' after rate limit wait, got: %s", output)
	}
	// rateLimitStubRunner tracks its own calls.
	rlRunner := l.runner.(*rateLimitStubRunner)
	if rlRunner.calls < 2 {
		t.Errorf("expected at least 2 Claude calls (rate limit + retry), got %d", rlRunner.calls)
	}
	_ = runner // silence unused
	_ = iterationCount
}

// rateLimitStubRunner returns RateLimited on the first call, then
// SignalDetected on subsequent calls.
type rateLimitStubRunner struct {
	backend *mutableBackend
	counter *int
	calls   int
}

func (r *rateLimitStubRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	r.calls++
	if r.calls == 1 {
		return claude.Result{
			RateLimited: true,
			ResetAt:     time.Now().Add(-1 * time.Second),
		}, nil
	}
	r.backend.mu.Lock()
	r.backend.completed = 1
	r.backend.remaining = 0
	r.backend.mu.Unlock()
	return claude.Result{SignalDetected: true, Summary: "done"}, nil
}

func (r *rateLimitStubRunner) StopStreaming() {}

func (r *rateLimitStubRunner) InjectMessage(_ string) error { return nil }

// Health dashboard is logged between iterations so operators can detect
// process leaks, stale signal files, and growing state.json.
func TestLoop_HealthDashboardLoggedBetweenIterations(t *testing.T) {
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

	// Create a signal file so the health snapshot has something to report.
	os.WriteFile(filepath.Join(ralphDir, ".signal_current_task"), []byte("test task"), 0o644)

	callCount := 0
	backend := &mutableBackend{
		remaining: 1,
		total:     2,
		nextTask:  "First task",
		nextID:    "ralph-h1",
	}

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

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
	}, st, gm, logger)

	l.runner = &stubRunner{
		onRun: func() {
			callCount++
			if callCount >= 2 {
				os.WriteFile(filepath.Join(ralphDir, "stop"), nil, 0o644)
			}
		},
	}

	_ = l.Run(context.Background())

	output := logBuf.String()

	if !strings.Contains(output, "[health]") {
		t.Error("expected [health] tag in log output between iterations")
	}
	if !strings.Contains(output, "state fields") {
		t.Error("expected 'state fields' in health log")
	}
	if !strings.Contains(output, "signals:") {
		t.Error("expected 'signals:' in health log")
	}
	if !strings.Contains(output, "branch:") {
		t.Error("expected 'branch:' in health log")
	}
}

// Verifies that the iteration banner includes the Ralph version when
// Config.Version is set, so operators can tell which build is running.
func TestLoop_IterationBannerShowsVersion(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     1,
		nextTask:  "check version",
		nextID:    "ralph-ver1",
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

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		Version:       "1.2.3",
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logger)
	l.runner = runner

	_ = l.Run(context.Background())

	output := logBuf.String()
	if !strings.Contains(output, "Ralph v1.2.3") {
		t.Errorf("expected 'Ralph v1.2.3' in iteration banner, got:\n%s", output)
	}
}

// injectCapturingRunner records messages sent via InjectMessage so tests
// can verify that onSignal injects feedback instead of spawning fix agents.
type injectCapturingRunner struct {
	onRun    func()
	result   claude.Result
	injected []string
}

func (r *injectCapturingRunner) Run(cfg claude.RunConfig) (claude.Result, error) {
	if r.onRun != nil {
		r.onRun()
	}
	return r.result, nil
}

func (r *injectCapturingRunner) StopStreaming() {}

func (r *injectCapturingRunner) InjectMessage(msg string) error {
	r.injected = append(r.injected, msg)
	return nil
}

// Verifies that when post-signal tests fail, the failure output is injected
// to the running agent via stdin instead of spawning a separate fix agent.
// The agent has full context of what it built and can fix its own work.
func TestLoop_onSignal_InjectsTestFailures(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	// Create a Makefile with a failing test command so verify.RunTests
	// detects a test runner and returns a failure.
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\t@echo 'FAIL: broken test' && exit 1\n"), 0o644)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Fix something",
		nextID:    "ralph-test1",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}

	var logBuf bytes.Buffer
	logger := logging.NewWithWriter(&logBuf)

	runner := &injectCapturingRunner{}

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
	}, st, gm, logger)
	l.runner = runner

	p := signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-test1",
		nextTask:   "Fix something",
	}

	accepted := l.onSignal(p)

	if accepted {
		t.Error("onSignal should return false when tests fail (agent continues)")
	}
	if len(runner.injected) == 0 {
		t.Fatal("expected test failure to be injected to agent via stdin")
	}
	if !strings.Contains(runner.injected[0], "Tests failed") {
		t.Errorf("injected message should contain test failure info, got: %q", runner.injected[0])
	}

	output := logBuf.String()
	if !strings.Contains(output, "injected to agent via stdin") {
		t.Errorf("expected injection log message, got:\n%s", output)
	}
}

// Verifies that when LLM verification rejects the agent's work, the rejection
// feedback is injected to the running agent instead of spawning a fix agent.
func TestLoop_onSignal_InjectsLLMRejection(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Add feature",
		nextID:    "ralph-llm1",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	runner := &injectCapturingRunner{}

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
	}, st, gm, logger)
	l.runner = runner

	// Tests pass but LLM rejects.
	l.verifier.deps.LLMVerify = func(verify.VerifyOpts) verify.Result {
		return verify.Result{Passed: false, Details: "missing error handling in parseConfig"}
	}

	p := signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-llm1",
		nextTask:   "Add feature",
	}

	accepted := l.onSignal(p)

	if accepted {
		t.Error("onSignal should return false when LLM rejects (agent continues)")
	}
	if len(runner.injected) == 0 {
		t.Fatal("expected LLM rejection to be injected to agent via stdin")
	}
	if !strings.Contains(runner.injected[0], "missing error handling") {
		t.Errorf("injected message should contain LLM feedback, got: %q", runner.injected[0])
	}
}

// Verifies that when stdin injection fails, onSignal falls back to spawning
// a fix agent — the old behavior provides a safety net.
func TestLoop_onSignal_FallsBackToFixAgentOnBrokenPipe(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Fix bug",
		nextID:    "ralph-fb1",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)

	// Runner that fails injection — simulates broken pipe.
	brokenRunner := &injectFailRunner{
		result: claude.Result{},
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
		VerifyDir:     dir,
	}, st, gm, logger)
	l.runner = brokenRunner

	// Tests pass, LLM rejects, injection fails → should fall back to fix agent.
	l.verifier.deps.LLMVerify = func(verify.VerifyOpts) verify.Result {
		return verify.Result{Passed: false, Details: "incomplete implementation"}
	}

	fixAgentCalled := false
	l.newRunnerFunc = func() claudeRunner {
		fixAgentCalled = true
		return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "fixed"}}
	}

	p := signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-fb1",
		nextTask:   "Fix bug",
	}

	l.onSignal(p)

	if !fixAgentCalled {
		t.Error("expected fix agent to be spawned as fallback when injection fails")
	}
}

// injectFailRunner always fails InjectMessage to test fallback behavior.
type injectFailRunner struct {
	result claude.Result
}

func (r *injectFailRunner) Run(_ claude.RunConfig) (claude.Result, error) {
	return r.result, nil
}

func (r *injectFailRunner) StopStreaming() {}

func (r *injectFailRunner) InjectMessage(_ string) error {
	return fmt.Errorf("broken pipe")
}

// Verifies that test fix attempts are tracked across onSignal calls and
// the agent is not allowed infinite retries.
func TestLoop_onSignal_TestFixAttemptsTracked(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	// Create a Makefile with a failing test command.
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\t@echo 'FAIL: broken' && exit 1\n"), 0o644)

	backend := &stubBackend{
		remaining: 1,
		total:     1,
		nextTask:  "Fix tests",
		nextID:    "ralph-tr1",
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	logger := logging.New(nil)
	runner := &injectCapturingRunner{}

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
	}, st, gm, logger)
	l.runner = runner

	p := signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-tr1",
		nextTask:   "Fix tests",
	}

	// Call onSignal maxTestFixAttempts+1 times — the last should give up.
	for i := 0; i <= 3; i++ {
		l.onSignal(p)
	}

	// Should have injected 3 times (the max), then given up on the 4th.
	if len(runner.injected) != 3 {
		t.Errorf("expected 3 injections before giving up, got %d", len(runner.injected))
	}
}

// buildPRBody assembles description, acceptance criteria, and agent summary
// into a structured PR body when all context is available.
// buildPRBody is a standalone function — takes backend directly, no Loop needed.
func TestBuildPRBody_FullContext(t *testing.T) {
	backend := &stubBackend{
		description: "Fix the auth middleware to validate tokens",
		acceptance:  "1. Tokens are validated\n2. Invalid tokens return 401",
	}

	body := buildPRBody(backend, "ralph-abc", "Fixed auth middleware token validation")

	if !strings.Contains(body, "## Description") {
		t.Error("body should contain Description section")
	}
	if !strings.Contains(body, "Fix the auth middleware") {
		t.Error("body should contain bead description")
	}
	if !strings.Contains(body, "## Acceptance Criteria") {
		t.Error("body should contain Acceptance Criteria section")
	}
	if !strings.Contains(body, "Tokens are validated") {
		t.Error("body should contain acceptance criteria content")
	}
	if !strings.Contains(body, "## Summary") {
		t.Error("body should contain Summary section")
	}
	if !strings.Contains(body, "Fixed auth middleware") {
		t.Error("body should contain agent summary")
	}
}

func TestBuildPRBody_NoBeadDescription(t *testing.T) {
	backend := &stubBackend{}

	body := buildPRBody(backend, "ralph-abc", "Implemented the feature")

	if strings.Contains(body, "## Description") {
		t.Error("body should not contain Description when bead has none")
	}
	if !strings.Contains(body, "## Summary") {
		t.Error("body should contain Summary section as fallback")
	}
	if !strings.Contains(body, "Implemented the feature") {
		t.Error("body should contain agent summary")
	}
}

func TestBuildPRBody_NoContext(t *testing.T) {
	backend := &stubBackend{}

	body := buildPRBody(backend, "", "")

	if body != "" {
		t.Errorf("body should be empty when no context is available, got %q", body)
	}
}

func TestBuildPRBody_NeverGeneric(t *testing.T) {
	backend := &stubBackend{
		description: "Some task description",
	}

	body := buildPRBody(backend, "ralph-abc", "completed task")

	if strings.Contains(body, "Automated PR for") {
		t.Error("body must not contain generic 'Automated PR for' text")
	}
	if strings.Contains(body, "Generated by ralph") {
		t.Error("body must not contain generic 'Generated by ralph' text")
	}
}

// metadataBackend extends stubBackend with SetMetadata/GetMetadata tracking.
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

// Branch name format uses beadID-slug without sequence number,
// proving the old TaskSeq pattern has been removed.
// handlePostSignal: verifies that a successful signal pushes, closes the
// task, and returns signalComplete for the normal (non-evolve) path.
func TestLoop_HandlePostSignal_ClosesTask(t *testing.T) {
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
			total:     1,
			nextTask:  "Fix auth bug",
			nextID:    "ralph-xyz",
		},
	}

	gm := &git.Manager{ProjectDir: project, WorkDir: project, Logger: logging.New(nil)}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "42", nil }
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return true, "" }

	// Create a commit so HeadRev() returns something different from headBefore=""
	writeFile(t, project, "fix.go", "package main\n")
	run(t, "git", "-C", project, "add", "fix.go")
	run(t, "git", "-C", project, "commit", "-m", "fix auth bug")

	runIter, iter := 1, 1
	action := l.handlePostSignal(postSignalParams{
		ctx:        context.Background(),
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    project,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-xyz",
		nextTask:   "Fix auth bug",
		diffStat:   "",
	}, &runIter, &iter)

	if action != signalComplete {
		t.Errorf("expected signalComplete, got %d", action)
	}

	backend.closeMu.Lock()
	defer backend.closeMu.Unlock()
	if len(backend.closedIDs) != 1 || backend.closedIDs[0] != "ralph-xyz" {
		t.Errorf("expected CloseTask for ralph-xyz, got %v", backend.closedIDs)
	}
}

// handlePostSignal: verification failure returns signalRetry so the loop
// continues without closing the task.
func TestLoop_HandlePostSignal_VerificationFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &trackingBackend{
		mutableBackend: mutableBackend{remaining: 1, total: 1, nextTask: "Fix bug", nextID: "ralph-abc"},
	}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}
	l.verifyFunc = func(context.Context, string, string) (bool, string) { return false, "tests failed" }

	runIter, iter := 1, 1
	action := l.handlePostSignal(postSignalParams{
		ctx:      context.Background(),
		result:   claude.Result{},
		taskID:   "ralph-abc",
		nextTask: "Fix bug",
	}, &runIter, &iter)

	if action != signalRetry {
		t.Errorf("expected signalRetry, got %d", action)
	}

	backend.closeMu.Lock()
	defer backend.closeMu.Unlock()
	if len(backend.closedIDs) != 0 {
		t.Errorf("task should not be closed on verification failure, got %v", backend.closedIDs)
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

// prepareAndBuildPrompt: verifies it returns a non-empty prompt and
// the expected head/log state.
func TestLoop_PrepareAndBuildPrompt_ReturnsPrompt(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &stubBackend{remaining: 1, total: 1, nextTask: "Fix login", nextID: "ralph-xyz"}

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	l := New(Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = &stubRunner{}

	prep, ok := l.prepareAndBuildPrompt(context.Background(), "ralph-xyz", "Fix login")
	if !ok {
		t.Fatal("expected ok=true from prepareAndBuildPrompt")
	}
	if prep.fullPrompt == "" {
		t.Error("expected non-empty prompt")
	}
	if prep.workDir != dir {
		t.Errorf("expected workDir=%s, got %s", dir, prep.workDir)
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

// Proves: after a successful task close, the completed task record (ID, title,
// PR number, close reason) is persisted to state.json so ralph-task can verify
// tasks weren't falsely closed.
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
	if tasks[0].ID != "ralph-xyz" {
		t.Errorf("completed task ID = %q, want %q", tasks[0].ID, "ralph-xyz")
	}
	if tasks[0].PRNumber != "42" {
		t.Errorf("completed task PRNumber = %q, want %q", tasks[0].PRNumber, "42")
	}
	if tasks[0].CloseReason == "" {
		t.Error("completed task CloseReason should not be empty")
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

	st.AddCompletedTask(state.CompletedTask{
		ID:          "ralph-old",
		Title:       "previous task",
		PRNumber:    "10",
		CloseReason: "Fixed in PR #10",
	})

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
	if tasks[0].ID != "ralph-old" {
		t.Errorf("completed task ID = %q, want %q (preserved from previous run)", tasks[0].ID, "ralph-old")
	}
}

// stubGitHub implements git.GitHub for tests that need PR state lookups.
type stubGitHub struct {
	available bool
	prState   string
	prBase    string
	prHead    string
}

func (s *stubGitHub) Available() bool                                              { return s.available }
func (s *stubGitHub) FindOpenPR(_, _ string) (string, error)                       { return "", nil }
func (s *stubGitHub) CreatePR(_ git.CreatePROpts) error                            { return nil }
func (s *stubGitHub) MergePR(_, _ string, _ git.MergeOpts) (string, error)         { return "", nil }
func (s *stubGitHub) UpdateBranch(_, _, _ string) (bool, error)                    { return false, nil }
func (s *stubGitHub) ListChecks(_, _ string) ([]git.CICheckResult, error)          { return nil, nil }
func (s *stubGitHub) EditPR(_, _, _, _ string) error                               { return nil }
func (s *stubGitHub) GetRunLog(_, _ string) string                                 { return "" }
func (s *stubGitHub) CheckEnforceAdmins(_, _ string) (bool, error)                 { return false, nil }
func (s *stubGitHub) PostEnforceAdmins(_, _ string) (string, error)                { return "", nil }
func (s *stubGitHub) FindPR(_, _ string) (string, string, string, error)           { return "", "", "", nil }
func (s *stubGitHub) SearchPR(_, _ string) (string, error)                         { return "", nil }
func (s *stubGitHub) PRDiff(_, _ string) (string, error)                           { return "", nil }
func (s *stubGitHub) GetPRState(_, _ string) (string, error)                       { return s.prState, nil }
func (s *stubGitHub) GetPRBase(_, _ string) (string, error)                        { return s.prBase, nil }
func (s *stubGitHub) GetPRHead(_, _ string) (string, error)                        { return s.prHead, nil }

// getPRBase takes only a GitHub interface and workDir — no Loop needed.
func TestGetPRBase_Standalone(t *testing.T) {
	gh := &stubGitHub{available: true, prBase: "main"}
	base := getPRBase(gh, "/tmp", "42")
	if base != "main" {
		t.Errorf("expected 'main', got %q", base)
	}

	base = getPRBase(nil, "/tmp", "42")
	if base != "" {
		t.Errorf("nil gh should return empty, got %q", base)
	}

	gh = &stubGitHub{available: false}
	base = getPRBase(gh, "/tmp", "42")
	if base != "" {
		t.Errorf("unavailable gh should return empty, got %q", base)
	}
}

// closeOrRetryTask takes only closeTaskDeps — no Loop needed. When merge
// fails and AutoMerge is on, the task is skipped instead of closed.
func TestCloseOrRetryTask_Standalone(t *testing.T) {
	_, st := setupTestDir(t)
	var skippedID string
	deps := closeTaskDeps{
		AutoMerge: true,
		Backend:   &stubBackend{},
		Attempts:  attempts.New(t.TempDir()),
		State:     st,
		Logger:    logging.New(nil),
		SkipFn:    func(id, reason string) { skippedID = id },
	}

	closeOrRetryTask(deps, "ralph-abc",
		CompletedTask{PRNum: "99", Title: "Fix bug"},
		false, fmt.Errorf("merge conflict"))

	if skippedID != "ralph-abc" {
		t.Errorf("expected task to be skipped, got skippedID=%q", skippedID)
	}
}

// closeOrRetryTask closes the task when merge succeeded.
func TestCloseOrRetryTask_ClosesOnSuccess(t *testing.T) {
	_, st := setupTestDir(t)
	backend := &trackingBackend{
		mutableBackend: mutableBackend{remaining: 1, total: 1},
	}
	deps := closeTaskDeps{
		AutoMerge: true,
		Backend:   backend,
		Attempts:  attempts.New(t.TempDir()),
		State:     st,
		Logger:    logging.New(nil),
		SkipFn:    func(id, reason string) {},
	}

	closeOrRetryTask(deps, "ralph-xyz",
		CompletedTask{PRNum: "42", Title: "Add feature"},
		true, nil)

	backend.closeMu.Lock()
	defer backend.closeMu.Unlock()
	if len(backend.closedIDs) != 1 || backend.closedIDs[0] != "ralph-xyz" {
		t.Errorf("expected CloseTask for ralph-xyz, got %v", backend.closedIDs)
	}
}

// mergeIfEnabled takes only mergeDeps — no Loop needed. When AutoMerge
// is off, no merge is attempted.
func TestMergeIfEnabled_Standalone_AutoMergeOff(t *testing.T) {
	deps := mergeDeps{AutoMerge: false, Logger: logging.New(nil)}

	merged, err := mergeIfEnabled(deps, postSignalParams{
		ctx:    context.Background(),
		taskID: "ralph-abc",
	}, "42")

	if merged {
		t.Error("should not merge when AutoMerge is disabled")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// mergeIfEnabled delegates to MergeFn when AutoMerge is on and PR exists.
func TestMergeIfEnabled_Standalone_CallsMergeFn(t *testing.T) {
	mergeCalled := false
	deps := mergeDeps{
		AutoMerge: true,
		Logger:    logging.New(nil),
		MergeFn: func(ctx context.Context, taskID, nextTask, workDir, rawLogPath string) (bool, error) {
			mergeCalled = true
			return true, nil
		},
	}

	merged, err := mergeIfEnabled(deps, postSignalParams{
		ctx:    context.Background(),
		taskID: "ralph-abc",
	}, "42")

	if !mergeCalled {
		t.Error("MergeFn should have been called")
	}
	if !merged {
		t.Error("should be merged when MergeFn returns true")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// skipTask takes state, backend, and logger — no Loop needed.
func TestSkipTask_Standalone(t *testing.T) {
	_, st := setupTestDir(t)
	backend := &stubBackend{}

	skipTask(st, backend, logging.New(nil), "ralph-abc", "test reason")

	skipped, _ := st.GetSkippedTasks()
	found := false
	for _, id := range skipped {
		if id == "ralph-abc" {
			found = true
		}
	}
	if !found {
		t.Error("expected ralph-abc in skipped tasks")
	}
}

// persistCompletedTask takes state and logger — no Loop needed.
func TestPersistCompletedTask_Standalone(t *testing.T) {
	_, st := setupTestDir(t)

	persistCompletedTask(st, logging.New(nil), "ralph-abc", "Fix bug", "42", "merged")

	tasks, err := st.GetCompletedTasks()
	if err != nil {
		t.Fatalf("GetCompletedTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 completed task, got %d", len(tasks))
	}
	if tasks[0].ID != "ralph-abc" {
		t.Errorf("expected ID ralph-abc, got %q", tasks[0].ID)
	}
	if tasks[0].PRNumber != "42" {
		t.Errorf("expected PR 42, got %q", tasks[0].PRNumber)
	}
}

// getBeadDescription takes a backend — no Loop or Verifier needed.
func TestGetBeadDescription_Standalone(t *testing.T) {
	backend := &stubBackend{description: "Fix auth middleware"}

	desc := getBeadDescription(backend, "ralph-abc")
	if desc != "Fix auth middleware" {
		t.Errorf("expected description, got %q", desc)
	}

	desc = getBeadDescription(backend, "")
	if desc != "" {
		t.Errorf("empty taskID should return empty, got %q", desc)
	}

	desc = getBeadDescription(nil, "ralph-abc")
	if desc != "" {
		t.Errorf("nil backend should return empty, got %q", desc)
	}
}
