package tasks

import (
	"context"
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
	return func(_ context.Context, dir string, args ...string) (string, error) {
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
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "--json") {
				return `[{"description":"Fix the auth flow","status":"in_progress"}]`, nil
			}
			return "○ abc123 · Fix auth   [● P1 · OPEN]\nOwner: user\n\nDESCRIPTION\nFix the auth flow\n\nNOTES\nCheck middleware\n\nLABELS: orchestrator", nil
		case "close":
			return "closed", nil
		case "update":
			return "", nil
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

// Proves: in-progress tasks are resumed when priority is equal to ready tasks.
func TestBD_GetNextTask_PrefersInProgressAtSamePriority(t *testing.T) {
	runner := mockBD(
		"5",
		map[string]string{"open": "3", "closed": "2", "in_progress": "1"},
		`[{"id":"wip-42","title":"Half-done feature","priority":2}]`,
		`[{"id":"abc123","title":"Fix the auth module","priority":2}]`,
	)
	b := setupBD(t, runner)
	got, _ := b.GetNextTask()
	if got != "Half-done feature" {
		t.Errorf("GetNextTask = %q, want %q", got, "Half-done feature")
	}
}

// Proves: in-progress task id is returned when priorities are equal.
func TestBD_GetNextTaskID_PrefersInProgressAtSamePriority(t *testing.T) {
	runner := mockBD(
		"5",
		map[string]string{"open": "3", "closed": "2", "in_progress": "1"},
		`[{"id":"wip-42","title":"Half-done feature","priority":2}]`,
		`[{"id":"abc123","title":"Fix the auth module","priority":2}]`,
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
	failing := func(_ context.Context, dir string, args ...string) (string, error) {
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
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
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
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
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

// Proves: CloseTask calls bd close for verified tasks.
func TestBD_CloseTask(t *testing.T) {
	closed := false
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "close" {
			closed = true
			return "closed", nil
		}
		if len(args) > 0 && args[0] == "state" {
			return "verified", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.CloseTask("abc123", "done"); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Error("expected close to be called for verified task")
	}
}

// Proves: CloseTask is a no-op with empty id.
func TestBD_CloseTask_EmptyID(t *testing.T) {
	called := false
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
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

// Proves: GetNextTask falls back to ready queue when no in-progress tasks.
func TestBD_GetNextTask_FallsBackToReady(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, _ := b.GetNextTask()
	if got != "Fix the auth module" {
		t.Errorf("GetNextTask = %q, want %q", got, "Fix the auth module")
	}
}

// Proves: bd skip_task closes the task with a blocked reason.
func TestBD_SkipTask_ClosesWithBlockedReason(t *testing.T) {
	var closeArgs []string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "close" {
			closeArgs = args
			return "closed", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.SkipTask("abc123", "stuck_loop"); err != nil {
		t.Fatal(err)
	}
	if len(closeArgs) == 0 {
		t.Fatal("expected close to be called")
	}
	joined := strings.Join(closeArgs, " ")
	if !strings.Contains(joined, "blocked: stuck_loop") {
		t.Errorf("expected blocked reason in close args, got: %v", closeArgs)
	}
}

// Proves: bd SkipTask is a no-op with empty id.
func TestBD_SkipTask_EmptyID(t *testing.T) {
	called := false
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		called = true
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.SkipTask("", "reason"); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("expected no bd calls with empty id")
	}
}

// Proves: bd execution instructions contain "bd prime" and prohibit agent close.
func TestBD_ExecutionInstructions_Content(t *testing.T) {
	b := setupBD(t, defaultMock())
	content := "Run `bd prime` for workflow context.\nDo NOT run `bd close` — the orchestrator closes your assigned task automatically.\n"
	os.WriteFile(filepath.Join(b.PromptsDir, "execution-bd.md"), []byte(content), 0644)
	got, err := b.ExecutionInstructions()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "bd prime") {
		t.Error("expected execution instructions to contain 'bd prime'")
	}
	if !strings.Contains(got, "orchestrator closes") {
		t.Error("expected execution instructions to tell agent that orchestrator closes tasks")
	}
}

// Proves: Checklist.ProjectContext returns empty string since checklists have no beads.
func TestChecklist_ProjectContext_ReturnsEmpty(t *testing.T) {
	c := &Checklist{PlanFile: "/tmp/plan.md"}
	got, err := c.ProjectContext()
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty project context for checklist, got %q", got)
	}
}

// Proves: Checklist Init is a no-op (does not create .beads).
func TestChecklist_Init_IsNoOp(t *testing.T) {
	dir := t.TempDir()
	c := &Checklist{PlanFile: filepath.Join(dir, "plan.md")}
	if err := c.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".beads")); !os.IsNotExist(err) {
		t.Error("expected .beads to not exist after checklist Init")
	}
}

// Proves: resolveBD finds bd via exec.LookPath when it's on PATH.
func TestBD_ResolveBD_FindsOnPath(t *testing.T) {
	b := &BD{}
	err := b.resolveBD()
	if err != nil {
		t.Skipf("bd not installed on test host: %v", err)
	}
	if b.bdPath == "" {
		t.Error("expected bdPath to be set after resolveBD")
	}
	if !filepath.IsAbs(b.bdPath) {
		t.Errorf("expected absolute path, got %q", b.bdPath)
	}
}

// Proves: resolveBD is idempotent — doesn't re-resolve when bdPath is set.
func TestBD_ResolveBD_Idempotent(t *testing.T) {
	b := &BD{bdPath: "/fake/bd"}
	if err := b.resolveBD(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.bdPath != "/fake/bd" {
		t.Errorf("expected bdPath to remain /fake/bd, got %q", b.bdPath)
	}
}

// Proves: Init skips resolveBD when a mock runner is injected.
func TestBD_Init_SkipsResolveWithMockRunner(t *testing.T) {
	b := setupBD(t, defaultMock())
	if err := b.Init(); err != nil {
		t.Fatalf("Init with mock runner should not fail: %v", err)
	}
	if b.bdPath != "" {
		t.Error("expected bdPath to remain empty with mock runner")
	}
}

// Proves: Init returns ErrNeedsFallback when bd binary is not found.
func TestBD_Init_FallbackWhenBinaryNotFound(t *testing.T) {
	b := &BD{
		ProjectDir: t.TempDir(),
		PromptsDir: t.TempDir(),
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	err := b.Init()
	if !errors.Is(err, ErrNeedsFallback) {
		t.Errorf("expected ErrNeedsFallback, got %v", err)
	}
}

// Proves: GetNextTaskInfo returns both id and title from a single bd query,
// ensuring the loop gets a consistent task identity without race conditions.
func TestBD_GetNextTaskInfo_ReturnsConsistentPair(t *testing.T) {
	callCount := 0
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "list":
			callCount++
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "in_progress") && strings.Contains(joined, "--json") {
				return "[]", nil
			}
			return "[]", nil
		case "ready":
			callCount++
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[{"id":"task-42","title":"Implement login"}]`, nil
			}
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	id, title, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if id != "task-42" {
		t.Errorf("id = %q, want %q", id, "task-42")
	}
	if title != "Implement login" {
		t.Errorf("title = %q, want %q", title, "Implement login")
	}
	if callCount != 2 {
		t.Errorf("expected 2 bd calls (list + ready), got %d", callCount)
	}
}

// Proves: GetNextTaskInfo prefers in-progress when priorities are equal.
func TestBD_GetNextTaskInfo_PrefersInProgressAtSamePriority(t *testing.T) {
	runner := mockBD(
		"3",
		map[string]string{"open": "1", "closed": "1", "in_progress": "1"},
		`[{"id":"wip-99","title":"Resume this","priority":2}]`,
		`[{"id":"new-1","title":"Start fresh","priority":2}]`,
	)
	b := setupBD(t, runner)
	id, title, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if id != "wip-99" || title != "Resume this" {
		t.Errorf("expected wip-99/Resume this, got %s/%s", id, title)
	}
}

// Proves: a higher-priority ready task preempts a lower-priority in-progress
// task, and the in-progress task is reopened via bd update.
func TestBD_GetNextTask_HigherPriorityReadyPreempts(t *testing.T) {
	var reopenedID string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "list":
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "in_progress") && strings.Contains(joined, "--json") {
				return `[{"id":"wip-1","title":"P3 feature","priority":3}]`, nil
			}
			return "[]", nil
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[{"id":"hot-1","title":"P0 critical bug","priority":0}]`, nil
			}
			return "", nil
		case "update":
			reopenedID = args[1]
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	got, _ := b.GetNextTask()
	if got != "P0 critical bug" {
		t.Errorf("GetNextTask = %q, want %q", got, "P0 critical bug")
	}
	if reopenedID != "wip-1" {
		t.Errorf("expected in-progress task wip-1 to be reopened, got %q", reopenedID)
	}
}

// Proves: a lower-priority ready task does not preempt a higher-priority
// in-progress task.
func TestBD_GetNextTask_LowerPriorityReadyDoesNotPreempt(t *testing.T) {
	runner := mockBD(
		"5",
		map[string]string{"open": "1", "closed": "2", "in_progress": "1"},
		`[{"id":"wip-1","title":"P1 important","priority":1}]`,
		`[{"id":"new-1","title":"P3 backlog","priority":3}]`,
	)
	b := setupBD(t, runner)
	got, _ := b.GetNextTask()
	if got != "P1 important" {
		t.Errorf("GetNextTask = %q, want %q", got, "P1 important")
	}
}

// Proves: when in-progress task has no explicit priority, default (2) is used
// for comparison.
func TestBD_GetNextTask_DefaultPriorityComparison(t *testing.T) {
	runner := mockBD(
		"5",
		map[string]string{"open": "1", "closed": "2", "in_progress": "1"},
		`[{"id":"wip-1","title":"No priority set"}]`,
		`[{"id":"hot-1","title":"P0 urgent","priority":0}]`,
	)
	b := setupBD(t, runner)
	got, _ := b.GetNextTask()
	if got != "P0 urgent" {
		t.Errorf("GetNextTask = %q, want %q", got, "P0 urgent")
	}
}

// Proves: SetState calls bd set-state with the correct dimension=value format.
func TestBD_SetState(t *testing.T) {
	var capturedArgs []string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "set-state" {
			capturedArgs = args
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.SetState("task-1", "phase", "implementing", "starting work"); err != nil {
		t.Fatal(err)
	}
	if len(capturedArgs) == 0 {
		t.Fatal("expected set-state to be called")
	}
	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "phase=implementing") {
		t.Errorf("expected phase=implementing in args, got: %v", capturedArgs)
	}
	if !strings.Contains(joined, "--reason") {
		t.Errorf("expected --reason flag in args, got: %v", capturedArgs)
	}
}

// Proves: SetState is a no-op with empty id.
func TestBD_SetState_EmptyID(t *testing.T) {
	called := false
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		called = true
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.SetState("", "phase", "implementing", ""); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("expected no bd calls with empty id")
	}
}

// Proves: GetState queries bd state and returns the dimension value.
func TestBD_GetState(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "state" && args[2] == "phase" {
			return "implementing", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	got, err := b.GetState("task-1", "phase")
	if err != nil {
		t.Fatal(err)
	}
	if got != "implementing" {
		t.Errorf("GetState = %q, want %q", got, "implementing")
	}
}

// Proves: GetState returns empty string with empty id.
func TestBD_GetState_EmptyID(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, err := b.GetState("", "phase")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("GetState = %q, want empty string", got)
	}
}

// Proves: CloseTask is rejected when phase is not "verified".
func TestBD_CloseTask_RejectsUnverified(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "state" {
			return "implementing", nil
		}
		if len(args) > 0 && args[0] == "close" {
			t.Error("close should not be called when phase is not verified")
			return "closed", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	err := b.CloseTask("task-1", "done")
	if err == nil {
		t.Error("expected error when closing unverified task")
	}
	if !strings.Contains(err.Error(), "phase") {
		t.Errorf("expected phase-related error, got: %v", err)
	}
}

// Proves: CloseTask succeeds when phase is "verified".
func TestBD_CloseTask_AllowsVerified(t *testing.T) {
	closed := false
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "state" {
			return "verified", nil
		}
		if len(args) > 0 && args[0] == "close" {
			closed = true
			return "closed", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.CloseTask("task-1", "done"); err != nil {
		t.Fatalf("expected close to succeed for verified task, got: %v", err)
	}
	if !closed {
		t.Error("expected close to be called for verified task")
	}
}

// Proves: CloseTask with empty phase (state not set) is rejected.
func TestBD_CloseTask_RejectsEmptyPhase(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "state" {
			return "", fmt.Errorf("no state set")
		}
		return "", nil
	}
	b := setupBD(t, runner)
	err := b.CloseTask("task-1", "done")
	if err == nil {
		t.Error("expected error when phase state is not set")
	}
}

// Proves: ProjectContext assembles open beads, recently closed beads,
// project directory, and config into a single string for prompt injection.
func TestBD_ProjectContext_AssemblesAllSections(t *testing.T) {
	openList := "○ task-1 [● P1] - Fix auth\n○ task-2 [● P2] - Add tests"
	closedList := "✓ task-0 ● P1 - Bootstrap project"
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "list --flat":
			return openList, nil
		case strings.Contains(joined, "list") && strings.Contains(joined, "closed"):
			return closedList, nil
		case joined == "prime":
			return "# bd prime output", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)

	// Write a ralph.toml so config is included.
	os.WriteFile(filepath.Join(b.ProjectDir, "ralph.toml"), []byte("max_iterations = 10\n"), 0644)

	got, err := b.ProjectContext()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "task-1") {
		t.Error("expected open beads in project context")
	}
	if !strings.Contains(got, "task-0") {
		t.Error("expected recently closed beads in project context")
	}
	if !strings.Contains(got, b.ProjectDir) {
		t.Error("expected project directory in project context")
	}
	if !strings.Contains(got, "max_iterations") {
		t.Error("expected ralph config in project context")
	}
	if !strings.Contains(got, "bd prime output") {
		t.Error("expected bd prime output in project context")
	}
}

// Proves: ProjectContext gracefully handles missing ralph.toml.
func TestBD_ProjectContext_NoConfig(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "list --flat":
			return "○ task-1 - Something", nil
		case strings.Contains(joined, "list") && strings.Contains(joined, "closed"):
			return "", nil
		case joined == "prime":
			return "# prime", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	got, err := b.ProjectContext()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "task-1") {
		t.Error("expected open beads even without config")
	}
}

// Proves: ProjectContext returns empty when all bd commands fail.
func TestBD_ProjectContext_AllCommandsFail(t *testing.T) {
	failing := func(_ context.Context, dir string, args ...string) (string, error) {
		return "", errors.New("fail")
	}
	b := setupBD(t, failing)
	got, err := b.ProjectContext()
	if err != nil {
		t.Fatal(err)
	}
	// Should still include project directory at minimum.
	if !strings.Contains(got, b.ProjectDir) {
		t.Error("expected project directory even when bd commands fail")
	}
}

// Proves: GetFullContext returns the complete human-readable bead output
// including notes, labels, and dependencies so the agent gets full context.
func TestBD_GetFullContext(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, err := b.GetFullContext("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "NOTES") {
		t.Error("full context should include NOTES section")
	}
	if !strings.Contains(got, "LABELS: orchestrator") {
		t.Error("full context should include LABELS")
	}
	if !strings.Contains(got, "Check middleware") {
		t.Error("full context should include note content")
	}
}

// Proves: GetFullContext returns empty string for empty ID.
func TestBD_GetFullContext_EmptyID(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, err := b.GetFullContext("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty string for empty ID, got %q", got)
	}
}

// Proves: GetDescription extracts just the description field from JSON.
func TestBD_GetDescription(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, err := b.GetDescription("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Fix the auth flow" {
		t.Errorf("GetDescription = %q, want %q", got, "Fix the auth flow")
	}
}

// Proves: counts return zero when bd commands fail.
func TestBD_Counts_OnError(t *testing.T) {
	failing := func(_ context.Context, dir string, args ...string) (string, error) {
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
