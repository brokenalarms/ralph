package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/config"
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
			return `[{"status":"in_progress"}]`, nil
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

// Proves: CountTotal counts only actionable beads (open + in_progress + closed).
func TestBD_CountTotal_ExcludesNonActionable(t *testing.T) {
	// bare "bd count" returns 10, but actionable = 3+1+4 = 8
	runner := mockBD(
		"10",
		map[string]string{"open": "3", "in_progress": "1", "closed": "4"},
		"[]",
		`[{"id":"test-1","title":"Test task"}]`,
	)
	b := setupBD(t, runner)
	got, _ := b.CountTotal()
	if got != 8 {
		t.Errorf("CountTotal = %d, want 8 (open+in_progress+closed)", got)
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

// Proves: in-progress tasks from bd ready are resumed when priority is equal.
func TestBD_GetNextTask_PrefersInProgressAtSamePriority(t *testing.T) {
	runner := mockBD(
		"5",
		map[string]string{"open": "3", "closed": "2", "in_progress": "1"},
		"[]",
		`[{"id":"abc123","title":"Fix the auth module","priority":2,"status":"open"},{"id":"wip-42","title":"Half-done feature","priority":2,"status":"in_progress"}]`,
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
		"[]",
		`[{"id":"abc123","title":"Fix the auth module","priority":2,"status":"open"},{"id":"wip-42","title":"Half-done feature","priority":2,"status":"in_progress"}]`,
	)
	b := setupBD(t, runner)
	got, _ := b.GetNextTaskID()
	if got != "wip-42" {
		t.Errorf("GetNextTaskID = %q, want %q", got, "wip-42")
	}
}

// Proves: Init creates .beads dir and updates .gitignore.
// Proves: Init requires .beads to already exist — never auto-initializes.
func TestBD_Init_RequiresExistingBeads(t *testing.T) {
	b := setupBD(t, defaultMock())
	// No .beads directory — Init should fail.
	err := b.Init()
	if !errors.Is(err, ErrNeedsFallback) {
		t.Errorf("expected ErrNeedsFallback when .beads missing, got %v", err)
	}
}

// Proves: Init creates .gitignore entries when .beads exists and server is healthy.
func TestBD_Init_CreatesGitignore(t *testing.T) {
	b := setupBD(t, defaultMock())
	os.MkdirAll(filepath.Join(b.ProjectDir, ".beads"), 0755)
	if err := b.Init(); err != nil {
		t.Fatal(err)
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
	os.MkdirAll(filepath.Join(b.ProjectDir, ".beads"), 0755)
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

// Proves: Init returns ErrNeedsFallback when .beads is missing.
func TestBD_Init_FallbackOnMissingBeads(t *testing.T) {
	b := setupBD(t, defaultMock())
	// No .beads — should fail without calling bd init.
	err := b.Init()
	if !errors.Is(err, ErrNeedsFallback) {
		t.Errorf("expected ErrNeedsFallback, got %v", err)
	}
}

// Proves: Init returns ErrNeedsFallback when server is unreachable (no retry).
func TestBD_Init_FallbackOnUnhealthyServer(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		// count always fails — server unreachable
		return "", errors.New("database not found")
	}
	b := setupBD(t, runner)
	os.MkdirAll(filepath.Join(b.ProjectDir, ".beads"), 0755)
	err := b.Init()
	if !errors.Is(err, ErrNeedsFallback) {
		t.Errorf("expected ErrNeedsFallback, got %v", err)
	}
}

// Proves: Init never calls bd init even when server is unhealthy.
func TestBD_Init_NeverCallsInit(t *testing.T) {
	initCalled := false
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "init" {
			initCalled = true
			return "", nil
		}
		// count fails — server stale
		return "", errors.New("stale")
	}
	b := setupBD(t, runner)
	os.MkdirAll(filepath.Join(b.ProjectDir, ".beads"), 0755)
	_ = b.Init() // will fail, that's expected
	if initCalled {
		t.Error("Init must never call bd init — it should fail cleanly instead")
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

// Proves: GetNextTask falls back to ready queue when no in-progress tasks.
func TestBD_GetNextTask_FallsBackToReady(t *testing.T) {
	b := setupBD(t, defaultMock())
	got, _ := b.GetNextTask()
	if got != "Fix the auth module" {
		t.Errorf("GetNextTask = %q, want %q", got, "Fix the auth module")
	}
}

// Proves: bd SkipTask sets status to open (not deferred).
func TestBD_SkipTask_SetsStatusOpen(t *testing.T) {
	var updateArgs []string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "update" && updateArgs == nil {
			updateArgs = args
			return "updated", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.SkipTask("abc123", "stuck_loop", ""); err != nil {
		t.Fatal(err)
	}
	if len(updateArgs) == 0 {
		t.Fatal("expected update to be called")
	}
	joined := strings.Join(updateArgs, " ")
	if !strings.Contains(joined, "--status=open") {
		t.Errorf("expected --status=open in update args, got: %v", updateArgs)
	}
	if strings.Contains(joined, "deferred") || strings.Contains(joined, "--defer") {
		t.Errorf("should not use deferred/defer, got: %v", updateArgs)
	}
}

// Proves: bd SkipTask adds a comment with the skip reason.
func TestBD_SkipTask_RecordsReasonAsComment(t *testing.T) {
	var commentArgs []string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 1 && args[0] == "comments" {
			commentArgs = args
			return "ok", nil
		}
		return "updated", nil
	}
	b := setupBD(t, runner)
	if err := b.SkipTask("abc123", "merge_failed", ""); err != nil {
		t.Fatal(err)
	}
	if len(commentArgs) == 0 {
		t.Fatal("expected bd comments add to be called")
	}
	joined := strings.Join(commentArgs, " ")
	if !strings.Contains(joined, "add") {
		t.Errorf("expected 'add' subcommand, got: %v", commentArgs)
	}
	if !strings.Contains(joined, "abc123") {
		t.Errorf("expected task ID in comment args, got: %v", commentArgs)
	}
	if !strings.Contains(joined, "merge_failed") {
		t.Errorf("expected reason in comment body, got: %v", commentArgs)
	}
}

// Proves: bd SkipTask records reason and detail as distinct pieces of the
// comment body rather than requiring the caller to pre-concatenate them.
func TestBD_SkipTask_RecordsDetailInComment(t *testing.T) {
	var commentArgs []string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 1 && args[0] == "comments" {
			commentArgs = args
			return "ok", nil
		}
		return "updated", nil
	}
	b := setupBD(t, runner)
	if err := b.SkipTask("abc123", SkipPushFailed, "ralph/some-branch"); err != nil {
		t.Fatal(err)
	}
	if len(commentArgs) == 0 {
		t.Fatal("expected bd comments add to be called")
	}
	joined := strings.Join(commentArgs, " ")
	if !strings.Contains(joined, string(SkipPushFailed)) {
		t.Errorf("expected reason category in comment body, got: %v", commentArgs)
	}
	if !strings.Contains(joined, "ralph/some-branch") {
		t.Errorf("expected detail in comment body, got: %v", commentArgs)
	}
}

// Proves: bd SkipTask persists the skip reason as bead metadata (skip_reason)
// via --set-metadata, not just as a human-readable comment.
func TestBD_SkipTask_SetsSkipReasonMetadata(t *testing.T) {
	var metadataArgs []string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--set-metadata") && strings.Contains(joined, "skip_reason=") {
			metadataArgs = args
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.SkipTask("abc123", SkipPushFailed, ""); err != nil {
		t.Fatal(err)
	}
	if metadataArgs == nil {
		t.Fatal("expected a --set-metadata skip_reason=... call")
	}
	joined := strings.Join(metadataArgs, " ")
	if !strings.Contains(joined, "skip_reason="+string(SkipPushFailed)) {
		t.Errorf("expected skip_reason=%s in metadata args, got: %v", SkipPushFailed, metadataArgs)
	}
	if !strings.Contains(joined, "abc123") {
		t.Errorf("expected task ID in metadata args, got: %v", metadataArgs)
	}
}

// Proves: bd SkipTask persists a non-empty detail as skip_detail metadata.
func TestBD_SkipTask_SetsSkipDetailMetadataWhenPresent(t *testing.T) {
	var metadataArgs []string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--set-metadata") && strings.Contains(joined, "skip_detail=") {
			metadataArgs = args
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.SkipTask("abc123", SkipPushFailed, "ralph/some-branch"); err != nil {
		t.Fatal(err)
	}
	if metadataArgs == nil {
		t.Fatal("expected a --set-metadata skip_detail=... call")
	}
	joined := strings.Join(metadataArgs, " ")
	if !strings.Contains(joined, "skip_detail=ralph/some-branch") {
		t.Errorf("expected skip_detail=ralph/some-branch in metadata args, got: %v", metadataArgs)
	}
}

// Proves: bd SkipTask does not write skip_detail metadata when detail is empty.
func TestBD_SkipTask_NoSkipDetailMetadataWhenDetailEmpty(t *testing.T) {
	var sawSkipDetail bool
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "skip_detail=") {
			sawSkipDetail = true
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.SkipTask("abc123", SkipPushFailed, ""); err != nil {
		t.Fatal(err)
	}
	if sawSkipDetail {
		t.Error("expected no skip_detail metadata call when detail is empty")
	}
}

// Proves: bd SkipTask reassigns the bead to config.TaskAssignee and adds the
// skipped label via --add-label so it leaves the loop's inbox with no separate
// filter needed. --label is not a valid bd update flag and must not be used.
func TestBD_SkipTask_ReassignsToTaskAssignee(t *testing.T) {
	var updateArgs []string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "update" && updateArgs == nil {
			updateArgs = args
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.SkipTask("abc123", "transport_error", ""); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(updateArgs, " ")
	if !strings.Contains(joined, "--status=open") {
		t.Errorf("SkipTask must set --status=open, got update args: %v", updateArgs)
	}
	if !strings.Contains(joined, "--assignee="+config.TaskAssignee) {
		t.Errorf("SkipTask must reassign to %s, got update args: %v", config.TaskAssignee, updateArgs)
	}
	if !strings.Contains(joined, "--add-label=skipped") {
		t.Errorf("SkipTask must use --add-label=skipped, got update args: %v", updateArgs)
	}
	if strings.Contains(joined, "--label=skipped") {
		t.Errorf("SkipTask must not use --label=skipped (invalid flag), got update args: %v", updateArgs)
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
	if err := b.SkipTask("", "reason", ""); err != nil {
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
	os.MkdirAll(filepath.Join(b.ProjectDir, ".beads"), 0755)
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
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "task-42" {
		t.Errorf("id = %q, want %q", info.ID, "task-42")
	}
	if info.Title != "Implement login" {
		t.Errorf("title = %q, want %q", info.Title, "Implement login")
	}
	if callCount != 1 {
		t.Errorf("expected 1 bd call (ready only), got %d", callCount)
	}
}

// Proves: GetNextTaskInfo prefers in-progress when priorities are equal.
func TestBD_GetNextTaskInfo_PrefersInProgressAtSamePriority(t *testing.T) {
	runner := mockBD(
		"3",
		map[string]string{"open": "1", "closed": "1", "in_progress": "1"},
		"[]",
		`[{"id":"new-1","title":"Start fresh","priority":2,"status":"open"},{"id":"wip-99","title":"Resume this","priority":2,"status":"in_progress"}]`,
	)
	b := setupBD(t, runner)
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "wip-99" || info.Title != "Resume this" {
		t.Errorf("expected wip-99/Resume this, got %s/%s", info.ID, info.Title)
	}
}

// Proves: a higher-priority task is selected over a lower-priority in-progress
// task from the same ready set.
func TestBD_GetNextTask_HigherPriorityReadyPreempts(t *testing.T) {
	runner := mockBD(
		"5",
		map[string]string{"open": "1", "closed": "2", "in_progress": "1"},
		"[]",
		`[{"id":"wip-1","title":"P3 feature","priority":3,"status":"in_progress"},{"id":"hot-1","title":"P0 critical bug","priority":0,"status":"open"}]`,
	)
	b := setupBD(t, runner)
	got, _ := b.GetNextTask()
	if got != "P0 critical bug" {
		t.Errorf("GetNextTask = %q, want %q", got, "P0 critical bug")
	}
}

// Proves: a higher-priority in-progress task is selected over a lower-priority
// open task from the same ready set.
func TestBD_GetNextTask_LowerPriorityReadyDoesNotPreempt(t *testing.T) {
	runner := mockBD(
		"5",
		map[string]string{"open": "1", "closed": "2", "in_progress": "1"},
		"[]",
		`[{"id":"wip-1","title":"P1 important","priority":1,"status":"in_progress"},{"id":"new-1","title":"P3 backlog","priority":3,"status":"open"}]`,
	)
	b := setupBD(t, runner)
	got, _ := b.GetNextTask()
	if got != "P1 important" {
		t.Errorf("GetNextTask = %q, want %q", got, "P1 important")
	}
}

// Proves: when a task has no explicit priority, default (2) is used
// for comparison.
func TestBD_GetNextTask_DefaultPriorityComparison(t *testing.T) {
	runner := mockBD(
		"5",
		map[string]string{"open": "1", "closed": "2", "in_progress": "1"},
		"[]",
		`[{"id":"wip-1","title":"No priority set","status":"in_progress"},{"id":"hot-1","title":"P0 urgent","priority":0}]`,
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

// Proves: defaultRunBD includes stderr in the error when a command fails.
func TestBD_DefaultRunBD_IncludesStderr(t *testing.T) {
	b := &BD{bdPath: "/bin/sh"}
	// Run a command that writes to stderr and exits non-zero.
	_, err := b.defaultRunBD(context.Background(), t.TempDir(), "-c", "echo 'bd error detail' >&2; exit 1")
	if err == nil {
		t.Fatal("expected error from failing command")
	}
	if !strings.Contains(err.Error(), "bd error detail") {
		t.Errorf("error should include stderr content, got: %v", err)
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

	// Write a config.toml so config is included.
	os.MkdirAll(filepath.Join(b.ProjectDir, ".ralph"), 0755)
	os.WriteFile(filepath.Join(b.ProjectDir, ".ralph", "config.toml"), []byte("max_iterations = 10\n"), 0644)

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
	// bd prime is intentionally NOT injected: its canned boilerplate carries a
	// SESSION-CLOSE push mandate and a "use bd remember" directive that
	// contradict ralph's own agent rules. The structured sections above supply
	// the live project state the agent actually needs.
	if strings.Contains(got, "bd prime output") {
		t.Error("bd prime output must not be injected into project context")
	}
	if strings.Contains(got, "bd workflow context") {
		t.Error("the bd workflow context section must not appear in project context")
	}
}

// Proves: ProjectContext gracefully handles missing config.toml.
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

// Proves: GetNextTaskInfo auto-prefixes titles with the detected component
// so the orchestrator always sees properly-prefixed task names.
func TestBD_GetNextTaskInfo_AutoPrefixesTitle(t *testing.T) {
	ready := `[{"id":"ralph-abc","title":"force-reset worktree after merge","priority":1}]`
	runner := mockBD("1", map[string]string{"open": "1"}, "[]", ready)
	b := setupBD(t, runner)

	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "ralph-abc" {
		t.Errorf("id = %q, want ralph-abc", info.ID)
	}
	if info.Title != "ralph loop: force-reset worktree after merge" {
		t.Errorf("title = %q, want %q", info.Title, "ralph loop: force-reset worktree after merge")
	}
}

// Proves: within the same priority, bugs are preferred over tasks, and tasks over enhancements.
func TestBD_GetNextTask_PrefersBugsOverTasksAtSamePriority(t *testing.T) {
	runner := mockBD(
		"3",
		map[string]string{"open": "3", "closed": "0", "in_progress": "0"},
		"[]",
		`[{"id":"enh-1","title":"Add dark mode","priority":2,"type":"feature"},{"id":"bug-1","title":"Fix crash on login","priority":2,"type":"bug"},{"id":"task-1","title":"Write docs","priority":2,"type":"task"}]`,
	)
	b := setupBD(t, runner)
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "bug-1" {
		t.Errorf("expected bug-1 (bug preferred over task/feature at same priority), got %q", info.ID)
	}
}

// Proves: tasks are preferred over features/enhancements at the same priority.
func TestBD_GetNextTask_PrefersTasksOverEnhancementsAtSamePriority(t *testing.T) {
	runner := mockBD(
		"2",
		map[string]string{"open": "2", "closed": "0", "in_progress": "0"},
		"[]",
		`[{"id":"enh-1","title":"Add dark mode","priority":2,"type":"feature"},{"id":"task-1","title":"Write docs","priority":2,"type":"task"}]`,
	)
	b := setupBD(t, runner)
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "task-1" {
		t.Errorf("expected task-1 (task preferred over feature at same priority), got %q", info.ID)
	}
}

// Proves: priority still trumps type — a higher-priority feature beats a lower-priority bug.
func TestBD_GetNextTask_PriorityTrumpsType(t *testing.T) {
	runner := mockBD(
		"2",
		map[string]string{"open": "2", "closed": "0", "in_progress": "0"},
		"[]",
		`[{"id":"bug-1","title":"Minor bug","priority":3,"type":"bug"},{"id":"feat-1","title":"Critical feature","priority":1,"type":"feature"}]`,
	)
	b := setupBD(t, runner)
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "feat-1" {
		t.Errorf("expected feat-1 (P1 feature beats P3 bug), got %q", info.ID)
	}
}

// Proves: GetNextTaskInfo does not double-prefix titles that already have a component prefix.
func TestBD_GetNextTaskInfo_NoDoublePrefixing(t *testing.T) {
	ready := `[{"id":"ralph-xyz","title":"ralph task: echo back beads","priority":2}]`
	runner := mockBD("1", map[string]string{"open": "1"}, "[]", ready)
	b := setupBD(t, runner)

	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "ralph task: echo back beads" {
		t.Errorf("title = %q, should be unchanged", info.Title)
	}
}

// Verifies that GetNextTaskInfo returns priority from the bd JSON response.
func TestBD_GetNextTaskInfo_ReturnsPriority(t *testing.T) {
	ready := `[{"id":"ralph-p1","title":"Urgent fix","priority":1}]`
	runner := mockBD("1", map[string]string{"open": "1"}, "[]", ready)
	b := setupBD(t, runner)

	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Priority == nil {
		t.Fatal("expected non-nil priority")
	}
	if *info.Priority != 1 {
		t.Errorf("priority = %d, want 1", *info.Priority)
	}
}

// Verifies that GetNextTaskInfo returns nil priority when the issue has no priority set.
func TestBD_GetNextTaskInfo_NilPriorityWhenUnset(t *testing.T) {
	ready := `[{"id":"ralph-np","title":"No priority task"}]`
	runner := mockBD("1", map[string]string{"open": "1"}, "[]", ready)
	b := setupBD(t, runner)

	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Priority != nil {
		t.Errorf("expected nil priority, got %d", *info.Priority)
	}
}

func TestBD_GetExternalRef(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "show" && args[2] == "--json" {
			return `[{"external_ref":"gh-42"}]`, nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	ref, err := b.GetExternalRef("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "gh-42" {
		t.Errorf("GetExternalRef = %q, want %q", ref, "gh-42")
	}
}

func TestBD_GetExternalRef_Empty(t *testing.T) {
	b := setupBD(t, defaultMock())
	ref, err := b.GetExternalRef("")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "" {
		t.Errorf("GetExternalRef with empty id = %q, want empty", ref)
	}
}

func TestBD_SetExternalRef(t *testing.T) {
	var capturedArgs []string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "update" {
			capturedArgs = args
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.SetExternalRef("abc123", "gh-42"); err != nil {
		t.Fatal(err)
	}
	if len(capturedArgs) == 0 {
		t.Fatal("expected update to be called")
	}
	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "--external-ref") {
		t.Errorf("expected --external-ref in args, got: %v", capturedArgs)
	}
	if !strings.Contains(joined, "gh-42") {
		t.Errorf("expected gh-42 in args, got: %v", capturedArgs)
	}
}

func TestBD_SetExternalRef_EmptyID(t *testing.T) {
	called := false
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		called = true
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.SetExternalRef("", "gh-42"); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("expected no bd calls with empty id")
	}
}

// SetMetadata calls bd update --set-metadata key=value for the given task.
func TestBD_SetMetadata(t *testing.T) {
	var capturedArgs []string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "update" {
			capturedArgs = args
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.SetMetadata("ralph-abc", "branch", "ralph/proj/ralph-abc-fix-bug"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "--set-metadata") {
		t.Errorf("expected --set-metadata in args, got: %v", capturedArgs)
	}
	if !strings.Contains(joined, "branch=ralph/proj/ralph-abc-fix-bug") {
		t.Errorf("expected branch=... in args, got: %v", capturedArgs)
	}
}

// SetMetadata is a no-op when id is empty.
func TestBD_SetMetadata_EmptyID(t *testing.T) {
	called := false
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		called = true
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.SetMetadata("", "branch", "val"); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("expected no bd calls with empty id")
	}
}

// GetMetadata parses the metadata map from bd show --json output.
func TestBD_GetMetadata(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "show" && args[2] == "--json" {
			return `[{"metadata":{"branch":"ralph/proj/ralph-abc-fix-bug"}}]`, nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	val, err := b.GetMetadata("ralph-abc", "branch")
	if err != nil {
		t.Fatal(err)
	}
	if val != "ralph/proj/ralph-abc-fix-bug" {
		t.Errorf("GetMetadata = %q, want %q", val, "ralph/proj/ralph-abc-fix-bug")
	}
}

// GetMetadata returns empty string when key is missing.
func TestBD_GetMetadata_MissingKey(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "show" && args[2] == "--json" {
			return `[{"metadata":{"other":"value"}}]`, nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	val, err := b.GetMetadata("ralph-abc", "branch")
	if err != nil {
		t.Fatal(err)
	}
	if val != "" {
		t.Errorf("GetMetadata missing key = %q, want empty", val)
	}
}

// GetMetadata returns empty string when metadata is null/absent.
func TestBD_GetMetadata_NoMetadata(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "show" && args[2] == "--json" {
			return `[{"id":"ralph-abc"}]`, nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	val, err := b.GetMetadata("ralph-abc", "branch")
	if err != nil {
		t.Fatal(err)
	}
	if val != "" {
		t.Errorf("GetMetadata no metadata = %q, want empty", val)
	}
}

// Proves: HasRemaining returns false when the loop inbox (bd ready --assignee=ralph-loop)
// returns no tasks — simulates all tasks having been reassigned away from ralph-loop.
func TestBD_HasRemaining_EmptyInbox(t *testing.T) {
	runner := mockBD(
		"1",
		map[string]string{"open": "1", "in_progress": "0", "closed": "0"},
		"[]",
		"[]", // bd ready returns nothing — tasks reassigned to ralph-task
	)
	b := setupBD(t, runner)

	has, err := b.HasRemaining()
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("HasRemaining should be false when loop inbox is empty")
	}
}

// HasOpenButAllSkipped returns false when no tasks exist at all.
func TestBD_HasOpenButAllSkipped_NoTasks(t *testing.T) {
	runner := mockBD(
		"0",
		map[string]string{"open": "0", "in_progress": "0", "closed": "0"},
		"[]",
		"[]",
	)
	b := setupBD(t, runner)

	got, err := HasOpenButAllSkipped(b)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("HasOpenButAllSkipped should return false when no tasks exist")
	}
}

// HasOpenButAllSkipped returns false when tasks exist and are selectable.
func TestBD_HasOpenButAllSkipped_TasksAvailable(t *testing.T) {
	runner := mockBD(
		"2",
		map[string]string{"open": "2", "in_progress": "0", "closed": "0"},
		"[]",
		`[{"id":"ralph-a","title":"Task A"},{"id":"ralph-b","title":"Task B"}]`,
	)
	b := setupBD(t, runner)

	got, err := HasOpenButAllSkipped(b)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("HasOpenButAllSkipped should return false when tasks are selectable")
	}
}

// Proves: in_progress task NOT in bd ready is not selected (AC #5, #6).
func TestBD_GetNextTask_InProgressNotInReadyIsSkipped(t *testing.T) {
	// bd list --status in_progress returns a task, but bd ready does NOT
	// include it (e.g. it has unsatisfied dependencies). getNextIssue must
	// not select it.
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "list":
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "in_progress") && strings.Contains(joined, "--json") {
				return `[{"id":"stuck-1","title":"Stuck task","priority":0,"status":"in_progress"}]`, nil
			}
			return "[]", nil
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[{"id":"ok-1","title":"Ready task","priority":2,"status":"open"}]`, nil
			}
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	got, _ := b.GetNextTask()
	if got == "Stuck task" {
		t.Error("getNextIssue selected in_progress task not in bd ready — should only use bd ready")
	}
	if got != "Ready task" {
		t.Errorf("GetNextTask = %q, want %q", got, "Ready task")
	}
}

// Proves: from the ready set, in_progress tasks are preferred over open tasks
// at the same priority.
func TestBD_GetNextTask_ReadyPrefersInProgress(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[
					{"id":"open-1","title":"Open task","priority":2,"status":"open"},
					{"id":"wip-1","title":"WIP task","priority":2,"status":"in_progress"}
				]`, nil
			}
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "wip-1" {
		t.Errorf("expected wip-1 (in_progress preferred), got %q", info.ID)
	}
}

// Proves: no bd list calls remain in getNextIssue — only bd ready is used.
func TestBD_GetNextTask_NoBDListCalls(t *testing.T) {
	var listCalled bool
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "list":
			listCalled = true
			return "[]", nil
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[{"id":"t-1","title":"A task"}]`, nil
			}
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.GetNextTask()
	if listCalled {
		t.Error("getNextIssue must not call bd list — only bd ready")
	}
}

// Proves: when resumeTaskID is set and the task is still in_progress and unblocked,
// it is returned directly without falling through to bd ready.
func TestBD_GetNextTaskInfo_ResumesLastTaskID(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "show":
			if len(args) >= 2 && args[1] == "ralph-resume" {
				return `[{"id":"ralph-resume","title":"Resumed task","priority":1,"status":"in_progress","type":"bug"}]`, nil
			}
		case "blocked":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[]`, nil
			}
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[{"id":"ralph-other","title":"Other task","priority":2,"status":"open"}]`, nil
			}
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.SetResumeTaskID("ralph-resume")
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "ralph-resume" {
		t.Errorf("expected resume task ralph-resume, got %q", info.ID)
	}
	if info.Title != "Resumed task" {
		t.Errorf("expected title %q, got %q", "Resumed task", info.Title)
	}
}

// Proves: when resumeTaskID points to a task whose dependency is no longer
// satisfied (a blocker reopened after the task was started), resumeTask does
// NOT resume it — it falls through to bd ready so the prerequisite is selected
// first. Guards against a stale current_task_id resuming a dependent task ahead
// of its prerequisite (e.g. tabi-huv8 resumed while tabi-l0cy was reopened).
// IsReady now uses bd blocked --json (status-agnostic) to check blocked state.
func TestBD_GetNextTaskInfo_BlockedResumeTaskFallsThrough(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "show":
			if len(args) >= 2 && args[1] == "ralph-huv8" {
				return `[{"id":"ralph-huv8","title":"Dependent task","priority":2,"status":"in_progress","type":"task"}]`, nil
			}
		case "blocked":
			if strings.Contains(strings.Join(args, " "), "--json") {
				// ralph-huv8 is in_progress but blocked — bd blocked lists it regardless of status.
				return `[{"id":"ralph-huv8","status":"in_progress"}]`, nil
			}
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[{"id":"ralph-l0cy","title":"Prerequisite task","priority":2,"status":"open"}]`, nil
			}
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.SetResumeTaskID("ralph-huv8")
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "ralph-l0cy" {
		t.Errorf("expected fall-through to ready prerequisite ralph-l0cy, got %q — a dependency-blocked resume target must not be resumed ahead of its prerequisite", info.ID)
	}
}

// Proves: when resumeTaskID points to a closed task, it falls through to
// bd ready for new work.
func TestBD_GetNextTaskInfo_ClosedResumeTaskFallsThrough(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "show":
			if len(args) >= 2 && args[1] == "ralph-done" {
				return `[{"id":"ralph-done","title":"Done task","priority":1,"status":"closed"}]`, nil
			}
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[{"id":"ralph-new","title":"New task","priority":2,"status":"open"}]`, nil
			}
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.SetResumeTaskID("ralph-done")
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "ralph-new" {
		t.Errorf("expected fallthrough to ralph-new, got %q", info.ID)
	}
}

// Proves: when resumeTaskID's bead is assigned to ralph-task (i.e. was skipped
// by reassignment), it is not resumed — falls through to bd ready.
func TestBD_GetNextTaskInfo_SkippedResumeTaskFallsThrough(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "show":
			// The skipped bead is now assigned to ralph-task, not ralph-loop.
			return `[{"id":"ralph-skip","title":"Skipped task","status":"open","assignee":"ralph-task"}]`, nil
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[{"id":"ralph-new","title":"New task","priority":2,"status":"open"}]`, nil
			}
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.SetResumeTaskID("ralph-skip")
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "ralph-new" {
		t.Errorf("expected fallthrough to ralph-new, got %q", info.ID)
	}
}

// ParseDependencyBlock extracts blocking task IDs from bd close errors.
func TestParseDependencyBlock_SingleBlocker(t *testing.T) {
	err := fmt.Errorf("exit status 1: cannot close ralph-qyoz: blocked by open issues [ralph-l2yh] (use --force to override)")
	blockers := ParseDependencyBlock(err)
	if len(blockers) != 1 || blockers[0] != "ralph-l2yh" {
		t.Errorf("expected [ralph-l2yh], got %v", blockers)
	}
}

// ParseDependencyBlock handles multiple blockers in a single error.
func TestParseDependencyBlock_MultipleBlockers(t *testing.T) {
	err := fmt.Errorf("cannot close ralph-abc: blocked by open issues [ralph-x1 ralph-x2] (use --force to override)")
	blockers := ParseDependencyBlock(err)
	if len(blockers) != 2 || blockers[0] != "ralph-x1" || blockers[1] != "ralph-x2" {
		t.Errorf("expected [ralph-x1 ralph-x2], got %v", blockers)
	}
}

// ParseDependencyBlock returns nil for non-dependency errors.
func TestParseDependencyBlock_NonDependencyError(t *testing.T) {
	err := fmt.Errorf("exit status 1: some other error")
	blockers := ParseDependencyBlock(err)
	if blockers != nil {
		t.Errorf("expected nil for non-dependency error, got %v", blockers)
	}
}

// ParseDependencyBlock returns nil for nil error.
func TestParseDependencyBlock_NilError(t *testing.T) {
	blockers := ParseDependencyBlock(nil)
	if blockers != nil {
		t.Errorf("expected nil for nil error, got %v", blockers)
	}
}

// Proves: a successful bd invocation writes one JSONL record to bd-calls.log
// with exitCode 0, killedByCtxCancel false, and the expected args.
func TestDefaultRunBD_LogsCleanInvocation(t *testing.T) {
	ralphDir := t.TempDir()
	b := &BD{bdPath: "/bin/sh", RalphDir: ralphDir}

	_, _ = b.defaultRunBD(context.Background(), t.TempDir(), "-c", "echo hello")

	data, err := os.ReadFile(filepath.Join(ralphDir, "bd-calls.log"))
	if err != nil {
		t.Fatalf("expected bd-calls.log to be created: %v", err)
	}
	line := bytes.TrimRight(data, "\n")
	var rec bdCallRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("expected valid JSON record: %v\nraw: %s", err, line)
	}
	if rec.ExitCode != 0 {
		t.Errorf("exitCode = %d, want 0", rec.ExitCode)
	}
	if rec.KilledByCtxCancel {
		t.Error("killedByCtxCancel should be false for clean invocation")
	}
	wantArgs := []string{"-c", "echo hello"}
	if len(rec.Args) != len(wantArgs) {
		t.Errorf("args = %v, want %v", rec.Args, wantArgs)
	} else {
		for i := range wantArgs {
			if rec.Args[i] != wantArgs[i] {
				t.Errorf("args[%d] = %q, want %q", i, rec.Args[i], wantArgs[i])
			}
		}
	}
}

// Proves: when the parent context is cancelled mid-command, the log record
// has killedByCtxCancel true and a non-empty ctxErr.
func TestDefaultRunBD_LogsContextCancellation(t *testing.T) {
	ralphDir := t.TempDir()
	b := &BD{bdPath: "/bin/sh", RalphDir: ralphDir}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _ = b.defaultRunBD(ctx, t.TempDir(), "-c", "sleep 10")

	data, err := os.ReadFile(filepath.Join(ralphDir, "bd-calls.log"))
	if err != nil {
		t.Fatalf("expected bd-calls.log to be created: %v", err)
	}
	line := bytes.TrimRight(data, "\n")
	var rec bdCallRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("expected valid JSON record: %v\nraw: %s", err, line)
	}
	if !rec.KilledByCtxCancel {
		t.Error("killedByCtxCancel should be true when context is cancelled")
	}
	if rec.CtxErr == "" {
		t.Error("ctxErr should be non-empty when context is cancelled")
	}
}

// Proves: a command exiting non-zero with stderr output produces a log record
// with exitCode != 0 and a populated stderrTail.
func TestDefaultRunBD_LogsNonZeroExit(t *testing.T) {
	ralphDir := t.TempDir()
	b := &BD{bdPath: "/bin/sh", RalphDir: ralphDir}

	_, _ = b.defaultRunBD(context.Background(), t.TempDir(), "-c", "echo 'bd error output' >&2; exit 2")

	data, err := os.ReadFile(filepath.Join(ralphDir, "bd-calls.log"))
	if err != nil {
		t.Fatalf("expected bd-calls.log to be created: %v", err)
	}
	line := bytes.TrimRight(data, "\n")
	var rec bdCallRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("expected valid JSON record: %v\nraw: %s", err, line)
	}
	if rec.ExitCode == 0 {
		t.Error("exitCode should be non-zero for failing command")
	}
	if rec.StderrTail == "" {
		t.Error("stderrTail should be populated for non-zero exit with stderr output")
	}
}

// Proves: when RalphDir is empty, no log file is created and the bd call's
// return value is unaffected.
func TestDefaultRunBD_NoLogPathSkipsWrite(t *testing.T) {
	b := &BD{
		bdPath:   "/bin/sh",
		RalphDir: "", // empty — logging disabled
	}
	workDir := t.TempDir()

	out, err := b.defaultRunBD(context.Background(), workDir, "-c", "echo hello")
	if err != nil {
		t.Fatalf("bd call should succeed with empty RalphDir, got: %v", err)
	}
	if out != "hello" {
		t.Errorf("output = %q, want %q", out, "hello")
	}

	// No log file should appear in the command's working dir (the only
	// temp dir we control) since RalphDir is empty and logging is skipped.
	if _, statErr := os.Stat(filepath.Join(workDir, "bd-calls.log")); !os.IsNotExist(statErr) {
		t.Error("bd-calls.log should not exist when RalphDir is empty")
	}
}

// Proves: ClaimTask explicitly sets status=in_progress and pins the bead to the
// loop's assignee (ralph-loop) — no reliance on --claim or BEADS_ACTOR.
func TestBD_ClaimTask_SetsStatusAndPinsAssignee(t *testing.T) {
	var capturedArgs []string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "update" {
			capturedArgs = args
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.ClaimTask("ralph-abc"); err != nil {
		t.Fatal(err)
	}
	if len(capturedArgs) == 0 {
		t.Fatal("expected update to be called")
	}
	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "--status=in_progress") {
		t.Errorf("expected --status=in_progress in update args, got: %v", capturedArgs)
	}
	if !strings.Contains(joined, "--assignee="+config.LoopAssignee) {
		t.Errorf("expected --assignee=%s in update args, got: %v", config.LoopAssignee, capturedArgs)
	}
	if strings.Contains(joined, "--claim") {
		t.Errorf("must not use --claim (implicit actor-based assignment), got: %v", capturedArgs)
	}
	if !strings.Contains(joined, "ralph-abc") {
		t.Errorf("expected task ID in update args, got: %v", capturedArgs)
	}
}

// Proves: ClaimTask is a no-op with empty id.
func TestBD_ClaimTask_EmptyID(t *testing.T) {
	called := false
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		called = true
		return "", nil
	}
	b := setupBD(t, runner)
	if err := b.ClaimTask(""); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("expected no bd calls with empty id")
	}
}

// Proves: after ClaimTask (status=in_progress), getNextIssue excludes the claimed
// task because bd ready does not surface in_progress issues. A second task becomes
// the selection target, proving no concurrent actor can pick up the claimed work.
func TestBD_ClaimTask_ClaimedTaskExcludedFromGetNextIssue(t *testing.T) {
	claimed := false
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "update":
			if strings.Contains(strings.Join(args, " "), "--status=in_progress") {
				claimed = true
				return "", nil
			}
			return "", nil
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				if claimed {
					// bd ready excludes in_progress tasks after claim
					return `[{"id":"other-task","title":"Other task","priority":2,"status":"open"}]`, nil
				}
				return `[{"id":"ralph-abc","title":"My task","priority":1,"status":"open"},{"id":"other-task","title":"Other task","priority":2,"status":"open"}]`, nil
			}
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)

	// Before claiming: ralph-abc is selected (higher priority)
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "ralph-abc" {
		t.Errorf("expected ralph-abc before claim, got %q", info.ID)
	}

	if err := b.ClaimTask("ralph-abc"); err != nil {
		t.Fatal(err)
	}

	// After claiming: bd ready excludes the in_progress task; other-task is selected
	info, err = b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID == "ralph-abc" {
		t.Error("claimed (in_progress) task must be excluded from getNextIssue — bd ready does not surface in_progress issues")
	}
	if info.ID != "other-task" {
		t.Errorf("expected other-task after claim, got %q", info.ID)
	}
}

// Proves: getNextIssue invokes bd ready with --exclude-type covering every
// container/meta type. This prevents the loop-spin failure mode where bd ready
// surfaces an epic (or other container) that the orchestrator selects, marks
// completed in-session, then re-selects on the next iteration because the
// container can't close while children are open — leading to the dedup +
// strand-protection spin that exhausts task selection attempts.
func TestBD_GetNextIssue_ExcludesContainerTypes(t *testing.T) {
	var readyArgs []string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		if args[0] == "ready" {
			readyArgs = append([]string{}, args...)
			return `[{"id":"t-1","title":"A task","type":"task"}]`, nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	if _, err := b.GetNextTaskInfo(); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(readyArgs, " ")
	for _, ty := range []string{"epic", "decision", "merge-request", "molecule", "convoy"} {
		if !strings.Contains(joined, ty) {
			t.Errorf("bd ready args missing exclude for type %q; got: %v", ty, readyArgs)
		}
	}
	if !strings.Contains(joined, "--exclude-type=") {
		t.Errorf("bd ready args missing --exclude-type flag; got: %v", readyArgs)
	}
}

// Proves: resumeTask rejects a resume target whose type is a container.
// Without this defense, a stale last_task_id pointing at an epic would bypass
// the --exclude-type filter applied to bd ready and reproduce the spin.
func TestBD_ResumeTask_RejectsContainerTypes(t *testing.T) {
	containerTypes := []string{"epic", "decision", "merge-request", "molecule", "convoy"}
	for _, ty := range containerTypes {
		t.Run(ty, func(t *testing.T) {
			runner := func(_ context.Context, dir string, args ...string) (string, error) {
				if len(args) == 0 {
					return "", errors.New("no args")
				}
				switch args[0] {
				case "show":
					if len(args) >= 2 && args[1] == "ralph-epic" {
						return fmt.Sprintf(`[{"id":"ralph-epic","title":"An epic","status":"open","type":%q}]`, ty), nil
					}
				case "ready":
					return `[{"id":"ralph-leaf","title":"Leaf task","type":"task","status":"open"}]`, nil
				}
				return "", nil
			}
			b := setupBD(t, runner)
			b.SetResumeTaskID("ralph-epic")
			info, err := b.GetNextTaskInfo()
			if err != nil {
				t.Fatal(err)
			}
			if info.ID == "ralph-epic" {
				t.Errorf("resumeTask returned container type %q — should have fallen through", ty)
			}
			if info.ID != "ralph-leaf" {
				t.Errorf("expected fallthrough to ralph-leaf, got %q", info.ID)
			}
		})
	}
}

// Proves: a ready P0 preempts a Ctrl+C'd P1 in-flight task on resume.
// AC: a strictly-higher-priority ready task is selected over the interrupted lower-priority task.
func TestBD_GetNextTaskInfo_P0ReadyPreemptsP1Resume(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "show":
			if len(args) >= 2 && args[1] == "ralph-wip" {
				return `[{"id":"ralph-wip","title":"WIP P1 task","priority":1,"status":"in_progress","type":"bug"}]`, nil
			}
		case "blocked":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[]`, nil
			}
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[{"id":"ralph-urgent","title":"Urgent P0 task","priority":0,"status":"open","type":"bug"}]`, nil
			}
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.SetResumeTaskID("ralph-wip")
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "ralph-urgent" {
		t.Errorf("expected P0 ready task ralph-urgent to preempt P1 resume, got %q", info.ID)
	}
}

// Proves: a P0 in-flight task is resumed even when another ready P0 exists (equal priority = no preempt).
// AC: equal-priority ready work does not displace the interrupted task.
func TestBD_GetNextTaskInfo_P0InFlightNotPreemptedByP0Ready(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "show":
			if len(args) >= 2 && args[1] == "ralph-wip" {
				return `[{"id":"ralph-wip","title":"WIP P0 task","priority":0,"status":"in_progress","type":"bug"}]`, nil
			}
		case "blocked":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[]`, nil
			}
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[{"id":"ralph-other","title":"Other P0 task","priority":0,"status":"open","type":"bug"}]`, nil
			}
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.SetResumeTaskID("ralph-wip")
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "ralph-wip" {
		t.Errorf("expected P0 resume task ralph-wip (equal priority = no preempt), got %q", info.ID)
	}
}

// Proves: when nothing is ready (bd ready returns empty), the interrupted task is resumed regardless.
// AC: no ready work -> in-flight task is resumed.
func TestBD_GetNextTaskInfo_NothingReadyResumesInFlight(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) == 0 {
			return "", errors.New("no args")
		}
		switch args[0] {
		case "show":
			if len(args) >= 2 && args[1] == "ralph-wip" {
				return `[{"id":"ralph-wip","title":"WIP task","priority":2,"status":"in_progress","type":"task"}]`, nil
			}
		case "blocked":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[]`, nil
			}
		case "ready":
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `[]`, nil
			}
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.SetResumeTaskID("ralph-wip")
	info, err := b.GetNextTaskInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "ralph-wip" {
		t.Errorf("expected resume of ralph-wip when nothing ready, got %q", info.ID)
	}
}

// Proves: a failed log write (e.g. read-only RalphDir) does not affect the
// bd call's return value or error.
func TestDefaultRunBD_LogWriteFailureIsBestEffort(t *testing.T) {
	ralphDir := t.TempDir()
	// Make the directory read-only so OpenFile fails.
	if err := os.Chmod(ralphDir, 0o555); err != nil {
		t.Skipf("cannot chmod temp dir: %v", err)
	}
	defer os.Chmod(ralphDir, 0o755) //nolint: errcheck

	b := &BD{bdPath: "/bin/sh", RalphDir: ralphDir}

	out, err := b.defaultRunBD(context.Background(), t.TempDir(), "-c", "echo hello")
	if err != nil {
		t.Errorf("bd call should succeed even when log write fails, got: %v", err)
	}
	if out != "hello" {
		t.Errorf("output = %q, want %q", out, "hello")
	}
}

// Proves: GetFullContext calls bd show --json (not text mode) and excludes labels
// from the agent's task context, preventing phase:* labels from misleading fresh agents.
func TestBD_GetFullContext_ExcludesLabels(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "show" && args[2] == "--json" {
			return `[{"title":"Fix the auth module","description":"Rewrite auth","acceptance_criteria":"Auth passes tests","labels":["observability","phase:implementing"]}]`, nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	ctx, err := b.GetFullContext("ralph-abc")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ctx, "observability") {
		t.Errorf("GetFullContext output must not contain labels, but found 'observability' in: %q", ctx)
	}
	if strings.Contains(ctx, "phase:implementing") {
		t.Errorf("GetFullContext output must not contain labels, but found 'phase:implementing' in: %q", ctx)
	}
}

// Proves: GetFullContext includes description and acceptance criteria in the agent context.
func TestBD_GetFullContext_IncludesDescriptionAndAC(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "show" && args[2] == "--json" {
			return `[{"title":"Fix the auth module","description":"Rewrite auth flow","acceptance_criteria":"Auth passes all tests"}]`, nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	ctx, err := b.GetFullContext("ralph-abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ctx, "Rewrite auth flow") {
		t.Errorf("GetFullContext output must contain description, got: %q", ctx)
	}
	if !strings.Contains(ctx, "Auth passes all tests") {
		t.Errorf("GetFullContext output must contain acceptance criteria, got: %q", ctx)
	}
}

// Proves: ensureDoltPort writes a port in [49152, 65535] when dolt.port is unset.
func TestBD_EnsureDoltPort_WritesPortWhenUnset(t *testing.T) {
	var setCalled bool
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "config" && args[1] == "get" && args[2] == "dolt.port" {
			return "dolt.port is not set in config.yaml", nil
		}
		if len(args) >= 3 && args[0] == "config" && args[1] == "set" && args[2] == "dolt.port" {
			setCalled = true
			port, _ := strconv.Atoi(args[3])
			if port < 49152 || port > 65535 {
				t.Errorf("port %d out of expected range [49152, 65535]", port)
			}
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.ensureDoltPort()
	if !setCalled {
		t.Error("expected bd config set dolt.port to be called when port is unset")
	}
}

// Proves: ensureDoltPort is a no-op when dolt.port is already set.
func TestBD_EnsureDoltPort_NoOpWhenAlreadySet(t *testing.T) {
	var setCalled bool
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "config" && args[1] == "get" && args[2] == "dolt.port" {
			return "50000", nil
		}
		if len(args) >= 3 && args[0] == "config" && args[1] == "set" && args[2] == "dolt.port" {
			setCalled = true
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.ensureDoltPort()
	if setCalled {
		t.Error("expected bd config set dolt.port NOT to be called when port is already set")
	}
}

// Proves: ensureDoltPort is deterministic — same ProjectDir always produces the same port.
func TestBD_EnsureDoltPort_Deterministic(t *testing.T) {
	var ports []string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "config" && args[1] == "get" && args[2] == "dolt.port" {
			return "not set in config.yaml", nil
		}
		if len(args) >= 4 && args[0] == "config" && args[1] == "set" && args[2] == "dolt.port" {
			ports = append(ports, args[3])
			return "", nil
		}
		return "", nil
	}
	b1 := &BD{ProjectDir: "/fixed/project/path", PromptsDir: t.TempDir(), RunBD: runner}
	b1.ensureDoltPort()
	b2 := &BD{ProjectDir: "/fixed/project/path", PromptsDir: t.TempDir(), RunBD: runner}
	b2.ensureDoltPort()
	if len(ports) != 2 {
		t.Fatalf("expected 2 port writes, got %d", len(ports))
	}
	if ports[0] != ports[1] {
		t.Errorf("ensureDoltPort is not deterministic: got %q then %q for same ProjectDir", ports[0], ports[1])
	}
}

// Proves: ensureTasksExport writes '../beads-tasks.jsonl' when export.path is unset.
func TestBD_EnsureTasksExport_WritesPathWhenUnset(t *testing.T) {
	var setCalled bool
	var setPath string
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "config" && args[1] == "get" && args[2] == "export.path" {
			return "export.path is not set in config.yaml", nil
		}
		if len(args) >= 4 && args[0] == "config" && args[1] == "set" && args[2] == "export.path" {
			setCalled = true
			setPath = args[3]
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.ensureTasksExport()
	if !setCalled {
		t.Error("expected bd config set export.path to be called when export.path is unset")
	}
	if setPath != "../beads-tasks.jsonl" {
		t.Errorf("export.path = %q, want %q", setPath, "../beads-tasks.jsonl")
	}
}

// Proves: ensureTasksExport is a no-op when export.path is already explicitly set.
func TestBD_EnsureTasksExport_NoOpWhenAlreadySet(t *testing.T) {
	var setCalled bool
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "config" && args[1] == "get" && args[2] == "export.path" {
			return "custom/path/issues.jsonl", nil
		}
		if len(args) >= 3 && args[0] == "config" && args[1] == "set" && args[2] == "export.path" {
			setCalled = true
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.ensureTasksExport()
	if setCalled {
		t.Error("expected bd config set export.path NOT to be called when export.path is already set")
	}
}

// Proves: ensureBackupGitPushDisabled sets backup.git-push=false when key is unset.
func TestBD_EnsureBackupGitPushDisabled_SetsWhenUnset(t *testing.T) {
	var setCalled bool
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "config" && args[1] == "get" && args[2] == "backup.git-push" {
			return "backup.git-push is not set in config.yaml", nil
		}
		if len(args) >= 4 && args[0] == "config" && args[1] == "set" && args[2] == "backup.git-push" && args[3] == "false" {
			setCalled = true
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.ensureBackupGitPushDisabled()
	if !setCalled {
		t.Error("expected bd config set backup.git-push false to be called when key is unset")
	}
}

// Proves: ensureBackupGitPushDisabled is a no-op when backup.git-push is already set.
func TestBD_EnsureBackupGitPushDisabled_NoOpWhenAlreadySet(t *testing.T) {
	var setCalled bool
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "config" && args[1] == "get" && args[2] == "backup.git-push" {
			return "false", nil
		}
		if len(args) >= 3 && args[0] == "config" && args[1] == "set" && args[2] == "backup.git-push" {
			setCalled = true
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.ensureBackupGitPushDisabled()
	if setCalled {
		t.Error("expected bd config set backup.git-push NOT to be called when key is already set")
	}
}

// Proves: ensureExportGitAddDisabled sets export.git-add=false when key is unset.
func TestBD_EnsureExportGitAddDisabled_SetsWhenUnset(t *testing.T) {
	var setCalled bool
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "config" && args[1] == "get" && args[2] == "export.git-add" {
			return "export.git-add is not set in config.yaml", nil
		}
		if len(args) >= 4 && args[0] == "config" && args[1] == "set" && args[2] == "export.git-add" && args[3] == "false" {
			setCalled = true
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.ensureExportGitAddDisabled()
	if !setCalled {
		t.Error("expected bd config set export.git-add false to be called when key is unset")
	}
}

// Proves: ensureExportGitAddDisabled sets export.git-add=false when current value is true.
func TestBD_EnsureExportGitAddDisabled_SetsWhenTrue(t *testing.T) {
	var setCalled bool
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "config" && args[1] == "get" && args[2] == "export.git-add" {
			return "true", nil
		}
		if len(args) >= 4 && args[0] == "config" && args[1] == "set" && args[2] == "export.git-add" && args[3] == "false" {
			setCalled = true
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.ensureExportGitAddDisabled()
	if !setCalled {
		t.Error("expected bd config set export.git-add false to be called when current value is true")
	}
}

// Proves: ensureExportGitAddDisabled is a no-op when export.git-add is already false.
func TestBD_EnsureExportGitAddDisabled_NoOpWhenFalse(t *testing.T) {
	var setCalled bool
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "config" && args[1] == "get" && args[2] == "export.git-add" {
			return "false", nil
		}
		if len(args) >= 3 && args[0] == "config" && args[1] == "set" && args[2] == "export.git-add" {
			setCalled = true
			return "", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	b.ensureExportGitAddDisabled()
	if setCalled {
		t.Error("expected bd config set export.git-add NOT to be called when already false")
	}
}

// Proves: Init calls ensureExportGitAddDisabled — a fake runner that tracks
// config get/set for export.git-add confirms the call is issued on Init.
func TestBD_Init_CallsEnsureExportGitAddDisabled(t *testing.T) {
	var exportGitAddSetCalled bool
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch args[0] {
		case "count":
			return "1", nil
		case "config":
			if len(args) >= 3 && args[1] == "get" && args[2] == "export.git-add" {
				return "export.git-add is not set in config.yaml", nil
			}
			if len(args) >= 4 && args[1] == "set" && args[2] == "export.git-add" && args[3] == "false" {
				exportGitAddSetCalled = true
				return "", nil
			}
			_ = joined
			return "not set in config.yaml", nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	os.MkdirAll(filepath.Join(b.ProjectDir, ".beads"), 0755)
	if err := b.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if !exportGitAddSetCalled {
		t.Error("Init must call ensureExportGitAddDisabled and issue config set export.git-add false when unset")
	}
}

// Proves: IsReady returns true when the id is not in bd blocked --json output.
func TestBD_IsReady_AllDepsClosed(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "blocked" && args[1] == "--json" {
			return `[{"id":"ralph-other","status":"open"}]`, nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	ready, err := b.IsReady("ralph-abc")
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Error("expected IsReady=true when id is not in bd blocked output")
	}
}

// Proves: IsReady returns false when the id appears in bd blocked --json output.
func TestBD_IsReady_OpenDep(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "blocked" && args[1] == "--json" {
			return `[{"id":"ralph-abc","status":"open"}]`, nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	ready, err := b.IsReady("ralph-abc")
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Error("expected IsReady=false when id appears in bd blocked output")
	}
}

// Proves: IsReady returns false for an in_progress id that appears in bd blocked --json.
// bd blocked is status-agnostic: in_progress tasks are included when blocked.
func TestBD_IsReady_InProgressDep(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "blocked" && args[1] == "--json" {
			return `[{"id":"ralph-abc","status":"in_progress"}]`, nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	ready, err := b.IsReady("ralph-abc")
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Error("expected IsReady=false when in_progress id appears in bd blocked output")
	}
}

// Proves: IsReady returns true when bd blocked --json returns an empty list.
func TestBD_IsReady_NoDeps(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "blocked" && args[1] == "--json" {
			return `[]`, nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	ready, err := b.IsReady("ralph-abc")
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Error("expected IsReady=true when bd blocked returns empty list")
	}
}

// Proves: GetFullContext includes open blocking dependencies (dependency_type=blocks)
// and excludes closed ones. Uses the bd 1.0.3 dependencies field, not blocked_by.
func TestBD_GetFullContext_OpenDepsIncludedClosedExcluded(t *testing.T) {
	runner := func(_ context.Context, dir string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "show" && args[2] == "--json" {
			return `[{"title":"Fix the auth module","description":"Rewrite auth","acceptance_criteria":"Auth passes","dependencies":[{"id":"ralph-dep1","title":"Dep open","status":"open","dependency_type":"blocks"},{"id":"ralph-dep2","title":"Dep closed","status":"closed","dependency_type":"blocks"},{"id":"ralph-dep3","title":"Dep depends-on","status":"open","dependency_type":"depends_on"}]}]`, nil
		}
		return "", nil
	}
	b := setupBD(t, runner)
	ctx, err := b.GetFullContext("ralph-abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ctx, "ralph-dep1") {
		t.Errorf("GetFullContext output must contain open dependency id, got: %q", ctx)
	}
	if !strings.Contains(ctx, "Dep open") {
		t.Errorf("GetFullContext output must contain open dependency title, got: %q", ctx)
	}
	if strings.Contains(ctx, "ralph-dep2") {
		t.Errorf("GetFullContext output must not contain closed dependency id, got: %q", ctx)
	}
	if strings.Contains(ctx, "Dep closed") {
		t.Errorf("GetFullContext output must not contain closed dependency title, got: %q", ctx)
	}
	if strings.Contains(ctx, "ralph-dep3") {
		t.Errorf("GetFullContext output must not contain depends_on dependency (not a blocker), got: %q", ctx)
	}
}

