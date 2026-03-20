package state

import (
	"encoding/json"
	"os"
	"path/filepath"
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
  "max_iterations": 50,
  "iterations_since_refactor": 3,
  "refactor_every": 0
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
	if s.RefactorEvery != 0 {
		t.Errorf("RefactorEvery = %d, want 0", s.RefactorEvery)
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
		Iteration:               10,
		Status:                  "planned",
		StartedAt:               "2026-03-20T00:00:00Z",
		LastTask:                "test task",
		TaskBackend:             "checklist",
		MaxIterations:           20,
		IterationsSinceRefactor: 2,
		RefactorEvery:           5,
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
		loaded.LastTask != original.LastTask ||
		loaded.RefactorEvery != original.RefactorEvery {
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

// Verifies Get/Set work like bash read_state/write_state: Set a value,
// then Get it back, with numeric values stored as JSON numbers.
func TestGetSet_MatchesBashReadWriteState(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	// Initialize with empty state
	if err := st.Save(State{}); err != nil {
		t.Fatal(err)
	}

	if err := st.Set("status", "running"); err != nil {
		t.Fatalf("Set status: %v", err)
	}
	if err := st.Set("iteration", "7"); err != nil {
		t.Fatalf("Set iteration: %v", err)
	}

	val, err := st.Get("status")
	if err != nil {
		t.Fatalf("Get status: %v", err)
	}
	if val != "running" {
		t.Errorf("Get status = %q, want %q", val, "running")
	}

	val, err = st.Get("iteration")
	if err != nil {
		t.Fatalf("Get iteration: %v", err)
	}
	if val != "7" {
		t.Errorf("Get iteration = %q, want %q", val, "7")
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

// Verifies that Set on an unknown key stores it in overflow and
// Get retrieves it — matching bash behavior for arbitrary keys.
func TestGetSet_UnknownKey(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.Save(State{})

	if err := st.Set("custom_thing", "abc"); err != nil {
		t.Fatal(err)
	}
	val, err := st.Get("custom_thing")
	if err != nil {
		t.Fatal(err)
	}
	if val != "abc" {
		t.Errorf("Get custom_thing = %q, want %q", val, "abc")
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
