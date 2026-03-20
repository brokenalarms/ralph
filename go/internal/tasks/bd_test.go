package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mockBD builds a CommandRunner that dispatches on bd subcommands.
// counts maps status -> count string; total is the bare "count" result.
// inProgress and ready are JSON arrays for list/ready commands.
func mockBD(total string, counts map[string]string, inProgress, ready string) CommandRunner {
	return func(dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "init":
			os.MkdirAll(filepath.Join(dir, ".beads"), 0755)
			return "", nil
		case "count":
			if len(args) >= 3 && args[1] == "--status" {
				if v, ok := counts[args[2]]; ok {
					return v, nil
				}
				return "0", nil
			}
			return total, nil
		case "list":
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "in_progress") && strings.Contains(joined, "--json") {
				return inProgress, nil
			}
			return "[]", nil
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return ready, nil
			}
			return "", nil
		case "show":
			return `[{"status":"in_progress"}]`, nil
		case "close":
			return "closed", nil
		}
		return "", fmt.Errorf("unknown command: %s", args[0])
	}
}

func defaultMock() CommandRunner {
	return mockBD(
		"5",
		map[string]string{"open": "3", "closed": "2", "in_progress": "0"},
		"[]",
		`[{"id":"abc123","title":"Fix the auth module"}]`,
	)
}

func setupBD(t *testing.T, runner CommandRunner) *BD {
	t.Helper()
	dir := t.TempDir()
	return &BD{
		ProjectDir: dir,
		PromptsDir: dir,
		RunBD:      runner,
	}
}

// Proves: BD satisfies the Backend interface at compile time.
var _ Backend = (*BD)(nil)

// Proves: bd backend detects open tasks via count --status.
func TestBD_HasRemaining_WithOpenTasks(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, err := b.HasRemaining()
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected HasRemaining=true when open tasks exist")
	}
}

// Proves: bd backend returns the correct closed count.
func TestBD_CountCompleted(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, _ := b.CountCompleted()
	if got != 2 {
		t.Errorf("CountCompleted = %d, want 2", got)
	}
}

// Proves: bd backend counts open + in_progress as remaining.
func TestBD_CountRemaining(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, _ := b.CountRemaining()
	if got != 3 {
		t.Errorf("CountRemaining = %d, want 3", got)
	}
}

// Proves: bd backend returns correct total count.
func TestBD_CountTotal(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, _ := b.CountTotal()
	if got != 5 {
		t.Errorf("CountTotal = %d, want 5", got)
	}
}

// Proves: bd backend picks the next ready task by title.
func TestBD_GetNextTask_FromReady(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, err := b.GetNextTask()
	if err != nil {
		t.Fatal(err)
	}
	if got != "Fix the auth module" {
		t.Errorf("GetNextTask = %q, want %q", got, "Fix the auth module")
	}
}

// Proves: bd backend returns the task id for prompt inclusion.
func TestBD_GetNextTaskID_FromReady(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, err := b.GetNextTaskID()
	if err != nil {
		t.Fatal(err)
	}
	if got != "abc123" {
		t.Errorf("GetNextTaskID = %q, want %q", got, "abc123")
	}
}

// Proves: in-progress tasks are resumed before picking new ready tasks.
func TestBD_GetNextTask_PrefersInProgress(t *testing.T) {
	runner := mockBD(
		"5",
		map[string]string{"open": "3", "closed": "2", "in_progress": "1"},
		`[{"id":"wip-42","title":"Half-done feature"}]`,
		`[{"id":"abc123","title":"Fix the auth module"}]`,
	)
	b := setupBD(t, runner)
	got, _ := b.GetNextTask()
	if got != "Half-done feature" {
		t.Errorf("GetNextTask = %q, want %q", got, "Half-done feature")
	}
}

// Proves: in-progress task id is returned for prompt inclusion.
func TestBD_GetNextTaskID_PrefersInProgress(t *testing.T) {
	runner := mockBD(
		"5",
		map[string]string{"open": "3", "closed": "2", "in_progress": "1"},
		`[{"id":"wip-42","title":"Half-done feature"}]`,
		`[{"id":"abc123","title":"Fix the auth module"}]`,
	)
	b := setupBD(t, runner)
	got, _ := b.GetNextTaskID()
	if got != "wip-42" {
		t.Errorf("GetNextTaskID = %q, want %q", got, "wip-42")
	}
}

// Proves: Init creates .beads dir and updates .gitignore.
func TestBD_Init_CreatesBeadsAndGitignore(t *testing.T) {
	b := setupBD(t, defaultMock())
	if err := b.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.ProjectDir, ".beads")); err != nil {
		t.Error("expected .beads directory to exist after Init")
	}
	data, err := os.ReadFile(filepath.Join(b.ProjectDir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, ".beads") {
		t.Error("expected .gitignore to contain .beads")
	}
	if !strings.Contains(content, ".dolt") {
		t.Error("expected .gitignore to contain .dolt")
	}
}

// Proves: Init is idempotent — doesn't duplicate .gitignore entries.
func TestBD_Init_IdempotentGitignore(t *testing.T) {
	b := setupBD(t, defaultMock())
	os.WriteFile(filepath.Join(b.ProjectDir, ".gitignore"), []byte(".beads\n.dolt\n"), 0644)
	if err := b.Init(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(b.ProjectDir, ".gitignore"))
	if strings.Count(string(data), ".beads") != 1 {
		t.Error("expected exactly one .beads entry in .gitignore")
	}
	if strings.Count(string(data), ".dolt") != 1 {
		t.Error("expected exactly one .dolt entry in .gitignore")
	}
}

// Proves: Init returns ErrNeedsFallback when bd is completely unavailable.
func TestBD_Init_FallbackOnInitFailure(t *testing.T) {
	failing := func(dir string, args ...string) (string, error) {
		return "", errors.New("bd not found")
	}
	b := setupBD(t, failing)
	err := b.Init()
	if !errors.Is(err, ErrNeedsFallback) {
		t.Errorf("expected ErrNeedsFallback, got %v", err)
	}
}

// Proves: Init returns ErrNeedsFallback when server is unreachable after retry.
func TestBD_Init_FallbackOnUnhealthyServer(t *testing.T) {
	runner := func(dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "init" {
			os.MkdirAll(filepath.Join(dir, ".beads"), 0755)
			return "", nil
		}
		// count always fails — server unreachable
		return "", errors.New("database not found")
	}
	b := setupBD(t, runner)
	err := b.Init()
	if !errors.Is(err, ErrNeedsFallback) {
		t.Errorf("expected ErrNeedsFallback, got %v", err)
	}
}

// Proves: Init retries and succeeds when .beads exists but server reconnects.
func TestBD_Init_RetrySucceeds(t *testing.T) {
	callCount := 0
	runner := func(dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "init":
			os.MkdirAll(filepath.Join(dir, ".beads"), 0755)
			return "", nil
		case "count":
			callCount++
			// First count fails (stale), subsequent succeed after re-init.
			if callCount <= 1 {
				return "", errors.New("stale")
			}
			return "5", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	// Pre-create .beads to simulate a previous run.
	os.MkdirAll(filepath.Join(b.ProjectDir, ".beads"), 0755)
	if err := b.Init(); err != nil {
		t.Errorf("expected Init to succeed after retry, got %v", err)
	}
}

// Proves: HasTasks returns true when total > 0.
func TestBD_HasTasks(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, err := b.HasTasks()
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected HasTasks=true")
	}
}

// Proves: NeedsPlanning is false when tasks exist.
func TestBD_NeedsPlanning_WithTasks(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, _ := b.NeedsPlanning()
	if got {
		t.Error("expected NeedsPlanning=false when tasks exist")
	}
}

// Proves: PlanningSucceeded is true when tasks exist.
func TestBD_PlanningSucceeded_WithTasks(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, _ := b.PlanningSucceeded()
	if !got {
		t.Error("expected PlanningSucceeded=true when tasks exist")
	}
}

// Proves: CloseTask calls bd close only for in_progress tasks.
func TestBD_CloseTask(t *testing.T) {
	closed := false
	runner := func(dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "close" {
			closed = true
			return "closed", nil
		}
		if len(args) > 0 && args[0] == "show" {
			return `[{"status":"in_progress"}]`, nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.CloseTask("abc123", "done"); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Error("expected close to be called for in_progress task")
	}
}

// Proves: CloseTask is a no-op with empty id.
func TestBD_CloseTask_EmptyID(t *testing.T) {
	called := false
	runner := func(dir string, args ...string) (string, error) {
		called = true
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.CloseTask("", "done"); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("expected no bd calls with empty id")
	}
}

// Proves: ExecutionInstructions reads from the prompts directory.
func TestBD_ExecutionInstructions(t *testing.T) {
	b := setupBD(t, defaultMock())
	content := "## Task selection\nRun `bd prime` for workflow context.\n"
	os.WriteFile(filepath.Join(b.PromptsDir, "execution-bd.md"), []byte(content), 0644)
	got, err := b.ExecutionInstructions()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "bd prime") {
		t.Error("expected execution instructions to contain 'bd prime'")
	}
}

// Proves: Label returns "beads" for the bd backend.
func TestBD_Label(t *testing.T) {
	b := &BD{}
	if b.Label() != "beads" {
		t.Errorf("Label = %q, want %q", b.Label(), "beads")
	}
}

// Proves: PlanningInstructions directs to bd for task creation.
func TestBD_PlanningInstructions(t *testing.T) {
	b := &BD{}
	got := b.PlanningInstructions()
	if !strings.Contains(got, "bd") {
		t.Error("expected planning instructions to mention bd")
	}
	if !strings.Contains(got, "create tasks directly in bd") {
		t.Error("expected planning instructions to direct task creation to bd")
	}
}

// Proves: counts return zero when bd commands fail.
func TestBD_Counts_OnError(t *testing.T) {
	failing := func(dir string, args ...string) (string, error) {
		return "", errors.New("fail")
	}
	b := setupBD(t, failing)
	completed, _ := b.CountCompleted()
	remaining, _ := b.CountRemaining()
	total, _ := b.CountTotal()
	if completed != 0 || remaining != 0 || total != 0 {
		t.Errorf("expected all zeros on error, got completed=%d remaining=%d total=%d",
			completed, remaining, total)
	}
}
