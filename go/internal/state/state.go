package state

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
)

// State represents the ralph state.json file. Fields use pointers for nullable
// JSON values (null vs zero distinction), matching the shell's state format.
type State struct {
	Iteration       int     `json:"iteration"`
	MaxIterations   *int    `json:"max_iterations"`
	RefactorEvery   *int    `json:"refactor_every"`
	Status          string  `json:"status"`
	StartedAt       *string `json:"started_at,omitempty"`
	LastTask        *string `json:"last_task,omitempty"`
	WorktreeDir     *string `json:"worktree_dir,omitempty"`
	WorktreeBranch  *string `json:"worktree_branch,omitempty"`
}

// Store manages reading and writing state.json, implementing the
// git.StateStore interface for compatibility with existing code.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore creates a Store for the given state.json path.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Path returns the state file path.
func (s *Store) Path() string {
	return s.path
}

// Init creates state.json with initial values if it doesn't exist.
// If it already exists, it's left untouched (resume case).
func (s *Store) Init(maxIterations, refactorEvery int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := os.Stat(s.path); err == nil {
		return nil
	}

	st := State{
		Iteration:     0,
		MaxIterations: &maxIterations,
		RefactorEvery: &refactorEvery,
		Status:        "initialized",
	}
	return s.writeLocked(st)
}

// Load reads the full state from disk.
func (s *Store) Load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.loadLocked()
}

// Save writes the full state to disk.
func (s *Store) Save(st State) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.writeLocked(st)
}

// Read implements git.StateStore. Returns the string value for a key.
func (s *Store) Read(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		return "", err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", err
	}

	v, ok := raw[key]
	if !ok {
		return "", nil
	}

	// Try string first, then number
	var str string
	if json.Unmarshal(v, &str) == nil {
		return str, nil
	}

	var num float64
	if json.Unmarshal(v, &num) == nil {
		if num == float64(int(num)) {
			return strconv.Itoa(int(num)), nil
		}
		return fmt.Sprintf("%g", num), nil
	}

	// null or other
	trimmed := string(v)
	if trimmed == "null" {
		return "", nil
	}
	return trimmed, nil
}

// Write implements git.StateStore. Sets a single key in state.json,
// auto-converting numeric strings to JSON numbers.
func (s *Store) Write(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// Convert numeric strings to JSON numbers (matches shell's jq tonumber)
	if n, err := strconv.Atoi(value); err == nil {
		raw[key], _ = json.Marshal(n)
	} else {
		raw[key], _ = json.Marshal(value)
	}

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, append(out, '\n'), 0o644)
}

// WriteConfig persists max_iterations and refactor_every to state.json.
// Called at the start of the execution phase so values are available for
// per-iteration re-read and mid-run editing.
func (s *Store) WriteConfig(maxIterations, refactorEvery int) error {
	if err := s.Write("max_iterations", strconv.Itoa(maxIterations)); err != nil {
		return err
	}
	return s.Write("refactor_every", strconv.Itoa(refactorEvery))
}

// ReadMaxIterations re-reads max_iterations from state.json. Returns the
// stored value or fallback if missing/invalid. Called each loop iteration
// so users can edit state.json mid-run.
func (s *Store) ReadMaxIterations(fallback int) int {
	v, err := s.Read("max_iterations")
	if err != nil || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// ReadRefactorEvery re-reads refactor_every from state.json. Returns the
// stored value or 0 if missing/invalid.
func (s *Store) ReadRefactorEvery() int {
	v, err := s.Read("refactor_every")
	if err != nil || v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

func (s *Store) loadLocked() (State, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, err
	}
	return st, nil
}

func (s *Store) writeLocked(st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, append(data, '\n'), 0o644)
}
