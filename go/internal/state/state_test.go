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
// proving the orchestrator can persist pre-iteration and post-signal
// test results across restarts.
func TestState_TestResultFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	st.Write("last_test_result", "pass")
	st.Write("last_test_output", "all 42 tests passed")
	st.Write("last_test_time", "2026-03-22T10:00:00Z")

	s, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.LastTestResult != "pass" {
		t.Errorf("LastTestResult = %q, want %q", s.LastTestResult, "pass")
	}
	if s.LastTestOutput != "all 42 tests passed" {
		t.Errorf("LastTestOutput = %q, want %q", s.LastTestOutput, "all 42 tests passed")
	}
	if s.LastTestTime != "2026-03-22T10:00:00Z" {
		t.Errorf("LastTestTime = %q, want %q", s.LastTestTime, "2026-03-22T10:00:00Z")
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

// Verifies that test result fields are preserved as known keys
// (not overflow) when serialized to JSON.
func TestState_TestResultFieldsInJSON(t *testing.T) {
	s := State{
		Status:         "running",
		LastTestResult: "fail",
		LastTestOutput: "FAIL: TestFoo",
		LastTestTime:   "2026-03-22T11:00:00Z",
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(data)
	for _, field := range []string{"last_test_result", "last_test_output", "last_test_time"} {
		if !strings.Contains(raw, field) {
			t.Errorf("expected %q in JSON output: %s", field, raw)
		}
	}
}

// Proves: AddSkippedTask persists IDs to state.json and deduplicates.
func TestSkippedTasks_AddAndDeduplicate(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Init(5)

	if err := st.AddSkippedTask("ralph-abc"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSkippedTask("ralph-def"); err != nil {
		t.Fatal(err)
	}
	// Duplicate should be ignored.
	if err := st.AddSkippedTask("ralph-abc"); err != nil {
		t.Fatal(err)
	}

	skipped, err := st.GetSkippedTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 2 {
		t.Fatalf("expected 2 skipped tasks, got %d: %v", len(skipped), skipped)
	}
	if skipped[0] != "ralph-abc" || skipped[1] != "ralph-def" {
		t.Errorf("unexpected order: %v", skipped)
	}
}

// Proves: ClearSkippedTasks removes all entries from state.json.
func TestSkippedTasks_Clear(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Init(5)

	st.AddSkippedTask("ralph-abc")
	st.AddSkippedTask("ralph-def")

	if err := st.ClearSkippedTasks(); err != nil {
		t.Fatal(err)
	}

	skipped, err := st.GetSkippedTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Errorf("expected empty skip list, got %v", skipped)
	}

	// Verify omitempty: skipped_tasks should not appear in JSON when empty.
	data, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if strings.Contains(string(data), "skipped_tasks") {
		t.Errorf("expected skipped_tasks omitted from JSON when empty, got: %s", data)
	}
}

// Proves: skipped_tasks round-trips through JSON serialization.
func TestSkippedTasks_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	s := State{
		Iteration:    5,
		Status:       "running",
		SkippedTasks: []string{"ralph-x", "ralph-y"},
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}

	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.SkippedTasks) != 2 || loaded.SkippedTasks[0] != "ralph-x" {
		t.Errorf("expected [ralph-x ralph-y], got %v", loaded.SkippedTasks)
	}
}

// Proves: AddCompletedTask persists task IDs — enabling ralph-task to verify
// tasks weren't falsely closed.
func TestCompletedTasks_AddAndRetrieve(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Init(5)

	if err := st.AddCompletedTask("ralph-abc"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddCompletedTask("ralph-def"); err != nil {
		t.Fatal(err)
	}

	tasks, err := st.GetCompletedTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 completed tasks, got %d", len(tasks))
	}
	if tasks[0] != "ralph-abc" {
		t.Errorf("first task = %q, want ralph-abc", tasks[0])
	}
	if tasks[1] != "ralph-def" {
		t.Errorf("second task = %q, want ralph-def", tasks[1])
	}
}

// Proves: ClearCompletedTasks removes all entries and omits the key from JSON.
func TestCompletedTasks_Clear(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Init(5)

	st.AddCompletedTask("ralph-abc")
	st.AddCompletedTask("ralph-def")

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
		Iteration:      5,
		Status:         "running",
		CompletedTasks: []string{"ralph-x", "ralph-y"},
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
	if loaded.CompletedTasks[0] != "ralph-x" || loaded.CompletedTasks[1] != "ralph-y" {
		t.Errorf("unexpected: %+v", loaded.CompletedTasks)
	}
}

// Proves: old-format completed_tasks (array of objects) is migrated to ID-only
// strings on load, so existing state.json files don't break after the upgrade.
func TestCompletedTasks_MigrateOldFormat(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	oldJSON := `{
  "iteration": 3,
  "status": "running",
  "completed_tasks": [
    {"id": "ralph-abc", "title": "fix widget", "pr_number": "42", "close_reason": "Fixed in PR #42"},
    {"id": "ralph-def", "title": "add tests", "pr_number": "43", "close_reason": "Fixed in PR #43"}
  ]
}`
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(oldJSON), 0o644)

	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.CompletedTasks) != 2 {
		t.Fatalf("expected 2 migrated tasks, got %d", len(loaded.CompletedTasks))
	}
	if loaded.CompletedTasks[0] != "ralph-abc" {
		t.Errorf("first migrated task = %q, want ralph-abc", loaded.CompletedTasks[0])
	}
	if loaded.CompletedTasks[1] != "ralph-def" {
		t.Errorf("second migrated task = %q, want ralph-def", loaded.CompletedTasks[1])
	}
}
