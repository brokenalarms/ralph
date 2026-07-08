package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Verifies that Load can parse a state.json written by bash ralph,
// preserving all known fields and their types.
func TestLoad_BashCompatible(t *testing.T) {
	dir := t.TempDir()
	stateJSON := `{
  "iteration": 25,
  "status": "running",
  "started_at": "2026-03-20T03:09:21Z",
  "last_task": "Go: State management",
  "worktree_dir": "/tmp/worktrees/test-01",
  "worktree_branch": "ralph/test/01-state",
  "task_backend": "bd",
  "max_iterations": 50
}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(stateJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	st := NewStore(dir)
	s, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if s.Iteration != 25 {
		t.Errorf("Iteration = %d, want 25", s.Iteration)
	}
	if s.Status != "running" {
		t.Errorf("Status = %q, want %q", s.Status, "running")
	}
	if s.MaxIterations != 50 {
		t.Errorf("MaxIterations = %d, want 50", s.MaxIterations)
	}
	if s.TaskBackend != "bd" {
		t.Errorf("TaskBackend = %q, want %q", s.TaskBackend, "bd")
	}
	if s.WorktreeDir != "/tmp/worktrees/test-01" {
		t.Errorf("WorktreeDir = %q, want %q", s.WorktreeDir, "/tmp/worktrees/test-01")
	}
}

// Verifies that Load returns a zero state (not an error) when no
// state file exists — matches bash behavior on first run.
func TestLoad_MissingFile(t *testing.T) {
	st := NewStore(t.TempDir())
	s, err := st.Load()
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if s.Status != "" || s.Iteration != 0 {
		t.Errorf("Expected zero State, got status=%q iteration=%d", s.Status, s.Iteration)
	}
}

// Verifies that Save writes valid JSON via atomic rename and that the
// result can be re-loaded identically.
func TestSaveAndLoad_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	original := State{
		Iteration:         10,
		Status:            "planned",
		StartedAt:         "2026-03-20T00:00:00Z",
		LastTask:          "test task",
		TaskBackend:       "bd",
		MaxIterations:     20,
	}

	if err := st.Save(original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := st.Load()
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}

	if loaded.Iteration != original.Iteration ||
		loaded.Status != original.Status ||
		loaded.MaxIterations != original.MaxIterations ||
		loaded.LastTask != original.LastTask {
		t.Errorf("Roundtrip mismatch:\n  saved:  %+v\n  loaded: %+v", original, loaded)
	}
}

// Verifies that Save produces output with numbers as JSON numbers (not
// quoted strings), matching bash jq's `try tonumber` behavior.
func TestSave_NumericFieldsAreNumbers(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	if err := st.Save(State{Iteration: 5, MaxIterations: 20}); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(st.Path())
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	// "iteration" should be 5 not "5"
	if string(raw["iteration"]) != "5" {
		t.Errorf("iteration = %s, want raw 5 (number)", string(raw["iteration"]))
	}
	if string(raw["max_iterations"]) != "20" {
		t.Errorf("max_iterations = %s, want raw 20 (number)", string(raw["max_iterations"]))
	}
}

// Verifies Read/Write work like bash read_state/write_state: Write a value,
// then Read it back, with numeric values stored as JSON numbers.
func TestReadWrite_MatchesBashReadWriteState(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	if err := st.Save(State{}); err != nil {
		t.Fatal(err)
	}

	if err := st.Write("status", "running"); err != nil {
		t.Fatalf("Write status: %v", err)
	}
	if err := st.Write("iteration", "7"); err != nil {
		t.Fatalf("Write iteration: %v", err)
	}

	val, err := st.Read("status")
	if err != nil {
		t.Fatalf("Read status: %v", err)
	}
	if val != "running" {
		t.Errorf("Read status = %q, want %q", val, "running")
	}

	val, err = st.Read("iteration")
	if err != nil {
		t.Fatalf("Read iteration: %v", err)
	}
	if val != "7" {
		t.Errorf("Read iteration = %q, want %q", val, "7")
	}
}

// Verifies that unknown keys from the bash side are preserved through
// a load-modify-save cycle (forward compatibility).
func TestOverflow_PreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	stateJSON := `{
  "iteration": 1,
  "status": "running",
  "custom_flag": "hello",
  "custom_num": 42
}`
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(stateJSON), 0o644)

	st := NewStore(dir)
	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Modify a known field
	s.Status = "completed"
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}

	// Re-read raw JSON and check overflow keys survived
	data, _ := os.ReadFile(st.Path())
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)

	if string(raw["custom_flag"]) != `"hello"` {
		t.Errorf("custom_flag lost: %s", string(raw["custom_flag"]))
	}
	if string(raw["custom_num"]) != "42" {
		t.Errorf("custom_num lost: %s", string(raw["custom_num"]))
	}
	if string(raw["status"]) != `"completed"` {
		t.Errorf("status not updated: %s", string(raw["status"]))
	}
}

// Verifies that Write on an unknown key stores it in overflow and
// Read retrieves it — matching bash behavior for arbitrary keys.
func TestReadWrite_UnknownKey(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Save(State{})

	if err := st.Write("custom_thing", "abc"); err != nil {
		t.Fatal(err)
	}
	val, err := st.Read("custom_thing")
	if err != nil {
		t.Fatal(err)
	}
	if val != "abc" {
		t.Errorf("Read custom_thing = %q, want %q", val, "abc")
	}
}

// Verifies that Init creates a fresh state with "initialized" status when no
// state file exists, matching bash init_ralph_dir behavior.
func TestInit_CreatesFreshState(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	if err := st.Init(50); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(st.Path()); err != nil {
		t.Fatal("expected state file to exist after Init")
	}

	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxIterations != 50 {
		t.Errorf("MaxIterations = %d, want 50", s.MaxIterations)
	}
}

// Verifies that Init preserves existing state on resume — iteration and
// status are not overwritten.
func TestInit_PreservesStateOnResume(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	st.Save(State{Iteration: 3, Status: "running", MaxIterations: 50})

	if err := st.Init(50); err != nil {
		t.Fatal(err)
	}

	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Iteration != 3 {
		t.Errorf("Iteration = %d, want 3 (preserved)", s.Iteration)
	}
	if s.Status != "running" {
		t.Errorf("Status = %q, want %q (preserved)", s.Status, "running")
	}
	if s.MaxIterations != 50 {
		t.Errorf("MaxIterations = %d, want 50 (preserved)", s.MaxIterations)
	}
}

// Verifies that task_backend is persisted and read back correctly.
func TestTaskBackend_WrittenToState(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Save(State{})
	if err := st.Write("task_backend", "bd"); err != nil {
		t.Fatal(err)
	}
	val, _ := st.Read("task_backend")
	if val != "bd" {
		t.Errorf("task_backend = %q, want %q", val, "bd")
	}
}

// Verifies that Save uses atomic write — if we can read the file after
// Save, it contains valid JSON (no partial writes).
func TestSave_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	// Write initial state
	st.Save(State{Status: "initial"})

	// Overwrite — should be atomic
	st.Save(State{Status: "updated", Iteration: 99})

	data, err := os.ReadFile(st.Path())
	if err != nil {
		t.Fatal(err)
	}

	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("JSON invalid after atomic write: %v", err)
	}
	if s.Status != "updated" || s.Iteration != 99 {
		t.Errorf("State after atomic write: %+v", s)
	}
}

// Verifies that test result fields round-trip through state.json,
// proving the orchestrator can persist baseline and post-signal
// test results across restarts.
func TestState_TestResultFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	st.Write("last_test_result", "pass")
	st.Write("last_test_time", "2026-03-22T10:00:00Z")

	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.LastTestResult != "pass" {
		t.Errorf("LastTestResult = %q, want %q", s.LastTestResult, "pass")
	}
	if s.LastTestTime != "2026-03-22T10:00:00Z" {
		t.Errorf("LastTestTime = %q, want %q", s.LastTestTime, "2026-03-22T10:00:00Z")
	}
}

// Verifies that the last-green test tree cache (dir + tree hash) round-trips
// through state.json, so a new loop process can seed the verifier's
// in-memory cache from the prior session's last green run.
func TestState_GreenCacheFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	st.Write("last_green_dir", "/work/ralph-20260707-01")
	st.Write("last_green_tree", "abc123deadbeef")

	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.LastGreenDir != "/work/ralph-20260707-01" {
		t.Errorf("LastGreenDir = %q, want %q", s.LastGreenDir, "/work/ralph-20260707-01")
	}
	if s.LastGreenTree != "abc123deadbeef" {
		t.Errorf("LastGreenTree = %q, want %q", s.LastGreenTree, "abc123deadbeef")
	}
}

// Verifies that SaveCLIConfig/LoadCLIConfig round-trips a config map through
// state.json, enabling evolve restart to reconstruct args from semantic config.
func TestSaveCLIConfig_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Save(State{Iteration: 5, Status: "running"})

	cfg := map[string]string{
		"dir":        "/tmp/project",
		"max":        "20",
		"auto-merge": "true",
		"evolve":     "true",
	}

	if err := st.SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	loaded, err := st.LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig: %v", err)
	}

	for k, v := range cfg {
		if loaded[k] != v {
			t.Errorf("key %q = %q, want %q", k, loaded[k], v)
		}
	}

	// Verify other state fields are preserved.
	s, _ := st.Load()
	if s.Iteration != 5 {
		t.Errorf("Iteration = %d, want 5 (preserved)", s.Iteration)
	}
	if s.Status != "running" {
		t.Errorf("Status = %q, want running (preserved)", s.Status)
	}
}

// Verifies that LoadCLIConfig returns nil when no cli_config exists in state,
// so callers can fall back to raw args on first run.
func TestLoadCLIConfig_MissingReturnsNil(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Save(State{Status: "running"})

	cfg, err := st.LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil map, got %v", cfg)
	}
}

// Proves: ClearCLIConfig removes cli_config from state.json so stale flags
// don't persist across manual restarts. Other state fields must be preserved.
func TestClearCLIConfig_RemovesConfigPreservesState(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Save(State{Iteration: 3, Status: "running"})

	cfg := map[string]string{"evolve": "true", "max": "10"}
	if err := st.SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	// Verify cli_config exists before clearing.
	loaded, _ := st.LoadCLIConfig()
	if loaded == nil {
		t.Fatal("cli_config should exist before clear")
	}

	if err := st.ClearCLIConfig(); err != nil {
		t.Fatalf("ClearCLIConfig: %v", err)
	}

	// cli_config must be gone.
	loaded, err := st.LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig after clear: %v", err)
	}
	if loaded != nil {
		t.Errorf("expected nil after clear, got %v", loaded)
	}

	// Other state fields preserved.
	s, _ := st.Load()
	if s.Iteration != 3 {
		t.Errorf("Iteration = %d, want 3", s.Iteration)
	}
	if s.Status != "running" {
		t.Errorf("Status = %q, want running", s.Status)
	}
}

// Proves: ClearCLIConfig is safe to call when no cli_config exists.
func TestClearCLIConfig_NoopWhenMissing(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Save(State{Status: "running"})

	if err := st.ClearCLIConfig(); err != nil {
		t.Fatalf("ClearCLIConfig on empty state: %v", err)
	}
}

// Verifies that test result fields are preserved as known keys
// (not overflow) when serialized to JSON.
func TestState_TestResultFieldsInJSON(t *testing.T) {
	s := State{
		Status:         "running",
		LastTestResult: "fail",
		LastTestTime:   "2026-03-22T11:00:00Z",
		LastGreenDir:   "/work/ralph-20260707-01",
		LastGreenTree:  "abc123deadbeef",
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(data)
	for _, field := range []string{"last_test_result", "last_test_time", "last_green_dir", "last_green_tree"} {
		if !strings.Contains(raw, field) {
			t.Errorf("expected %q in JSON output: %s", field, raw)
		}
	}
	if strings.Contains(raw, "last_test_output") {
		t.Errorf("last_test_output should not appear in JSON output: %s", raw)
	}
}

// Proves: AddCompletedTask persists task entries — enabling ralph-task to verify
// tasks weren't falsely closed and setStackHead to find unmerged branches.
func TestCompletedTasks_AddAndRetrieve(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Init(5)

	if err := st.AddCompletedTask("ralph-abc", true); err != nil {
		t.Fatal(err)
	}
	if err := st.AddCompletedTask("ralph-def", false); err != nil {
		t.Fatal(err)
	}

	tasks, err := st.GetCompletedTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 completed tasks, got %d", len(tasks))
	}
	if tasks[0].ID != "ralph-abc" || !tasks[0].Merged {
		t.Errorf("first task = %+v, want {ralph-abc, true}", tasks[0])
	}
	if tasks[1].ID != "ralph-def" || tasks[1].Merged {
		t.Errorf("second task = %+v, want {ralph-def, false}", tasks[1])
	}
}

// Proves: ClearCompletedTasks removes all entries and omits the key from JSON.
func TestCompletedTasks_Clear(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Init(5)

	st.AddCompletedTask("ralph-abc", true)
	st.AddCompletedTask("ralph-def", false)

	if err := st.ClearCompletedTasks(); err != nil {
		t.Fatal(err)
	}

	tasks, err := st.GetCompletedTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected empty completed tasks, got %v", tasks)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if strings.Contains(string(data), "completed_tasks") {
		t.Errorf("expected completed_tasks omitted from JSON when empty, got: %s", data)
	}
}

// Proves: completed_tasks round-trips through JSON and persists across restarts.
func TestCompletedTasks_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	s := State{
		Iteration: 5,
		Status:    "running",
		CompletedTasks: []CompletedTaskEntry{
			{ID: "ralph-x", Merged: true},
			{ID: "ralph-y", Merged: false},
		},
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}

	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.CompletedTasks) != 2 {
		t.Fatalf("expected 2, got %d", len(loaded.CompletedTasks))
	}
	if loaded.CompletedTasks[0].ID != "ralph-x" || !loaded.CompletedTasks[0].Merged {
		t.Errorf("first entry unexpected: %+v", loaded.CompletedTasks[0])
	}
	if loaded.CompletedTasks[1].ID != "ralph-y" || loaded.CompletedTasks[1].Merged {
		t.Errorf("second entry unexpected: %+v", loaded.CompletedTasks[1])
	}
}

// Proves: AddPushedBranch records branches in push order (oldest first) and
// GetPushedBranches returns them in the same order.
func TestAddPushedBranch_RecordsInOrder(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Init(5)

	if err := st.AddPushedBranch("ralph/task-a"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddPushedBranch("ralph/task-b"); err != nil {
		t.Fatal(err)
	}

	branches, err := st.GetPushedBranches()
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 2 || branches[0] != "ralph/task-a" || branches[1] != "ralph/task-b" {
		t.Errorf("expected [ralph/task-a, ralph/task-b] oldest-first, got %v", branches)
	}
}

// Proves: AddPushedBranch is idempotent — pushing the same branch twice produces one entry.
func TestAddPushedBranch_NoDuplicates(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Init(5)

	st.AddPushedBranch("ralph/task-a")
	st.AddPushedBranch("ralph/task-a")

	branches, _ := st.GetPushedBranches()
	if len(branches) != 1 {
		t.Errorf("expected 1 entry (no duplicate), got %d: %v", len(branches), branches)
	}
}

// Proves: AddPushedBranch ignores empty branch names.
func TestAddPushedBranch_IgnoresEmpty(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Init(5)

	if err := st.AddPushedBranch(""); err != nil {
		t.Errorf("AddPushedBranch(\"\") returned error: %v", err)
	}
	branches, _ := st.GetPushedBranches()
	if len(branches) != 0 {
		t.Errorf("expected empty list, got %v", branches)
	}
}

// Proves: pushed_branches round-trips through JSON serialization.
func TestPushedBranches_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Init(5)

	st.AddPushedBranch("ralph/task-a")
	st.AddPushedBranch("ralph/task-b")

	s, _ := st.Load()
	data, _ := json.Marshal(s)
	if !strings.Contains(string(data), `"pushed_branches"`) {
		t.Errorf("expected pushed_branches in JSON, got %s", data)
	}

	var s2 State
	json.Unmarshal(data, &s2)
	if len(s2.PushedBranches) != 2 {
		t.Errorf("expected 2 pushed branches after round-trip, got %d", len(s2.PushedBranches))
	}
}

// Verifies CheckStop returns true when the stop file exists and removes it,
// proving the graceful shutdown signal is consumed exactly once.
func TestStore_CheckStop(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	if st.CheckStop() {
		t.Error("CheckStop should return false when stop file absent")
	}

	stopFile := filepath.Join(dir, "stop")
	if err := os.WriteFile(stopFile, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	if !st.CheckStop() {
		t.Error("CheckStop should return true after stop file created")
	}

	if _, err := os.Stat(stopFile); !os.IsNotExist(err) {
		t.Error("CheckStop should remove the stop file after returning true")
	}

	if st.CheckStop() {
		t.Error("CheckStop should return false after stop file has been removed")
	}
}

// Verifies WriteRunBranch writes the branch name to .run-branch and defaults
// to "ralph" when the branch string is empty, proving the pane-title updater
// integration is consistent.
func TestStore_WriteRunBranch(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	st.WriteRunBranch("ralph/project/01-fix-bug")
	data, err := os.ReadFile(filepath.Join(dir, ".run-branch"))
	if err != nil {
		t.Fatalf("expected .run-branch file: %v", err)
	}
	if string(data) != "ralph/project/01-fix-bug" {
		t.Errorf("got %q, want %q", string(data), "ralph/project/01-fix-bug")
	}

	st.WriteRunBranch("")
	data, _ = os.ReadFile(filepath.Join(dir, ".run-branch"))
	if string(data) != "ralph" {
		t.Errorf("empty branch should default to %q, got %q", "ralph", string(data))
	}
}

// Verifies UpdateStreamTask writes the formatted task string to .stream-task
// so the tmux pane title integration receives the correct content.
func TestStore_UpdateStreamTask(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	st.UpdateStreamTask("ralph-abc", "Add feature X", nil)
	data, err := os.ReadFile(filepath.Join(dir, ".stream-task"))
	if err != nil {
		t.Fatalf("expected .stream-task file: %v", err)
	}
	if string(data) != "ralph-abc: Add feature X" {
		t.Errorf("got %q, want %q", string(data), "ralph-abc: Add feature X")
	}

	st.UpdateStreamTask("", "Add feature Y", nil)
	data, _ = os.ReadFile(filepath.Join(dir, ".stream-task"))
	if string(data) != "Add feature Y" {
		t.Errorf("got %q, want %q", string(data), "Add feature Y")
	}

	p := 3
	st.UpdateStreamTask("ralph-xyz", "Some task", &p)
	data, _ = os.ReadFile(filepath.Join(dir, ".stream-task"))
	got := string(data)
	if !strings.Contains(got, "[P3]") {
		t.Errorf("stream task with priority should include [P3], got %q", got)
	}
	if !strings.Contains(got, "ralph-xyz") {
		t.Errorf("stream task should include task ID, got %q", got)
	}
}

// Verifies RecordCompletedTask appends labels to .completed-tasks, proving
// the plan pane can show cumulative session completions.
func TestStore_RecordCompletedTask(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	st.RecordCompletedTask("ralph-abc", "Add feature X")
	st.RecordCompletedTask("ralph-def", "Fix bug Y")

	data, err := os.ReadFile(filepath.Join(dir, ".completed-tasks"))
	if err != nil {
		t.Fatalf("expected .completed-tasks file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "ralph-abc" || lines[1] != "ralph-def" {
		t.Errorf("unexpected .completed-tasks content: %q", string(data))
	}
}

// Verifies ClearCompletedTasksFile removes .completed-tasks so the plan pane
// shows only the current session's completions after a fresh start.
func TestStore_ClearCompletedTasksFile(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	path := filepath.Join(dir, ".completed-tasks")
	if err := os.WriteFile(path, []byte("ralph-abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st.ClearCompletedTasksFile()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("ClearCompletedTasksFile should remove .completed-tasks")
	}
}

// Verifies TouchPlanFlash and TouchPlanRefresh create their respective signal
// files, proving the UI flash and refresh triggers reach the plan pane.
func TestStore_TouchPlanFiles(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	st.TouchPlanFlash()
	if _, err := os.Stat(filepath.Join(dir, ".plan-flash")); err != nil {
		t.Error("TouchPlanFlash should create .plan-flash")
	}

	st.TouchPlanRefresh()
	if _, err := os.Stat(filepath.Join(dir, ".plan-refresh")); err != nil {
		t.Error("TouchPlanRefresh should create .plan-refresh")
	}
}

// Proves: BeginIteration writes current_task_id (not last_task_id) to state.json,
// and both Read("current_task_id") and Read("last_task_id") return the same value
// for backwards compatibility.
func TestBeginIteration_WritesCurrentTaskID(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Init(5)

	st.BeginIteration("ralph-abc", "Fix bug", 1)

	// Canonical name must return the value.
	v, err := st.Read("current_task_id")
	if err != nil {
		t.Fatalf("Read current_task_id: %v", err)
	}
	if v != "ralph-abc" {
		t.Errorf("current_task_id = %q, want %q", v, "ralph-abc")
	}

	// Backwards-compat alias must also return the value.
	v2, err := st.Read("last_task_id")
	if err != nil {
		t.Fatalf("Read last_task_id: %v", err)
	}
	if v2 != "ralph-abc" {
		t.Errorf("last_task_id alias = %q, want %q", v2, "ralph-abc")
	}

	// JSON on disk must use current_task_id, not last_task_id.
	data, _ := os.ReadFile(st.Path())
	if !strings.Contains(string(data), `"current_task_id"`) {
		t.Errorf("expected current_task_id in JSON, got: %s", data)
	}
	if strings.Contains(string(data), `"last_task_id"`) {
		t.Errorf("last_task_id must not appear in written JSON: %s", data)
	}
}

// Proves: ClearCurrentTask clears current_task_id in state.json, signalling
// no task is actively in-flight after a terminal transition.
func TestClearCurrentTask_ClearsField(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Init(5)

	st.BeginIteration("ralph-abc", "Fix bug", 1)

	v, _ := st.Read("current_task_id")
	if v != "ralph-abc" {
		t.Fatalf("precondition: current_task_id should be ralph-abc, got %q", v)
	}

	if err := st.ClearCurrentTask(); err != nil {
		t.Fatalf("ClearCurrentTask: %v", err)
	}

	v, _ = st.Read("current_task_id")
	if v != "" {
		t.Errorf("current_task_id should be empty after clear, got %q", v)
	}

	// JSON on disk must not contain current_task_id once cleared.
	data, _ := os.ReadFile(st.Path())
	if strings.Contains(string(data), `"current_task_id"`) {
		t.Errorf("current_task_id should be absent from JSON after clear: %s", data)
	}
}

// Proves: Load reads last_task_id from old state.json files (written by bash ralph)
// into CurrentTaskID, providing backwards compatibility for one release.
func TestLoad_BackwardsCompatLastTaskID(t *testing.T) {
	dir := t.TempDir()
	stateJSON := `{
  "iteration": 5,
  "status": "running",
  "last_task_id": "ralph-xyz"
}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(stateJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	st := NewStore(dir)
	s, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if s.CurrentTaskID != "ralph-xyz" {
		t.Errorf("CurrentTaskID = %q, want ralph-xyz (read from last_task_id)", s.CurrentTaskID)
	}

	// Read via both aliases must return the same value.
	v, _ := st.Read("current_task_id")
	if v != "ralph-xyz" {
		t.Errorf("Read(current_task_id) = %q, want ralph-xyz", v)
	}
	v2, _ := st.Read("last_task_id")
	if v2 != "ralph-xyz" {
		t.Errorf("Read(last_task_id) = %q, want ralph-xyz", v2)
	}
}
