package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Verifies that Init creates state.json with the provided max_iterations
// and refactor_every values, matching the shell's initial state structure.
func TestInitCreatesStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(path)

	if err := s.Init(50, 0); err != nil {
		t.Fatalf("Init: %v", err)
	}

	st, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if st.Iteration != 0 {
		t.Errorf("Iteration = %d, want 0", st.Iteration)
	}
	if st.MaxIterations == nil || *st.MaxIterations != 50 {
		t.Errorf("MaxIterations = %v, want 50", st.MaxIterations)
	}
	if st.RefactorEvery == nil || *st.RefactorEvery != 0 {
		t.Errorf("RefactorEvery = %v, want 0", st.RefactorEvery)
	}
	if st.Status != "initialized" {
		t.Errorf("Status = %q, want initialized", st.Status)
	}
}

// Verifies that Init is a no-op when state.json already exists (resume case).
func TestInitSkipsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(path)

	s.Init(50, 0)
	// Modify a value
	s.Write("status", "running")

	// Second Init should not reset the file
	s.Init(99, 10)

	st, _ := s.Load()
	if st.Status != "running" {
		t.Error("Init overwrote existing state.json")
	}
}

// Verifies that WriteConfig persists max_iterations and refactor_every,
// and the per-iteration readers return the updated values.
func TestWriteConfigAndReread(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(path)
	s.Init(50, 0)

	if err := s.WriteConfig(30, 5); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	if got := s.ReadMaxIterations(50); got != 30 {
		t.Errorf("ReadMaxIterations = %d, want 30", got)
	}
	if got := s.ReadRefactorEvery(); got != 5 {
		t.Errorf("ReadRefactorEvery = %d, want 5", got)
	}
}

// Verifies that users can edit state.json mid-run and the readers pick up
// the new values — the core dynamic reconfiguration feature.
func TestMidRunEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(path)
	s.Init(50, 0)
	s.WriteConfig(50, 0)

	// Simulate user editing state.json to change max_iterations
	s.Write("max_iterations", "25")

	if got := s.ReadMaxIterations(50); got != 25 {
		t.Errorf("after mid-run edit, ReadMaxIterations = %d, want 25", got)
	}
}

// Verifies that ReadMaxIterations returns the fallback when the key is
// missing or the file doesn't exist.
func TestReadMaxIterationsFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(path)

	// File doesn't exist
	if got := s.ReadMaxIterations(50); got != 50 {
		t.Errorf("missing file: ReadMaxIterations = %d, want 50", got)
	}
}

// Verifies that Write auto-converts numeric strings to JSON numbers,
// matching the shell's jq tonumber behavior.
func TestWriteNumericConversion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(path)
	s.Init(50, 0)

	s.Write("iteration", "5")

	data, _ := os.ReadFile(path)
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)

	// Should be stored as JSON number 5, not string "5"
	if string(raw["iteration"]) != "5" {
		t.Errorf("iteration stored as %s, want JSON number 5", string(raw["iteration"]))
	}
}

// Verifies that Read/Write implement the git.StateStore interface contract:
// string values round-trip correctly.
func TestReadWriteStringValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(path)
	s.Init(50, 0)

	s.Write("worktree_dir", "/tmp/ralph-worktree")
	got, err := s.Read("worktree_dir")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "/tmp/ralph-worktree" {
		t.Errorf("Read(worktree_dir) = %q, want /tmp/ralph-worktree", got)
	}
}

// Verifies that Read returns empty string for null JSON values,
// matching the shell's jq `// empty` behavior.
func TestReadNullValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(path)
	s.Init(50, 0)

	got, err := s.Read("started_at")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "" {
		t.Errorf("Read(started_at) = %q, want empty for null", got)
	}
}
