package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// CompletedTaskEntry records a task that completed (or was verified) during a
// loop run. Merged is true when the PR was successfully merged; false when the
// work is verified but the PR is still open.
type CompletedTaskEntry struct {
	ID     string `json:"id"`
	Merged bool   `json:"merged"`
}

// State represents the ralph loop state persisted in .ralph/state.json.
// Fields use interface{} values for numeric/string flexibility — the bash
// implementation stores numbers as JSON numbers and strings as JSON strings,
// and we must preserve that behavior for compatibility.
type State struct {
	Iteration              int    `json:"iteration"`
	Status                 string `json:"status"`
	StartedAt              string `json:"started_at,omitempty"`
	LastTask               string `json:"last_task,omitempty"`
	WorktreeDir            string `json:"worktree_dir,omitempty"`
	WorktreeBranch         string `json:"worktree_branch,omitempty"`
	TaskBackend            string `json:"task_backend,omitempty"`
	MaxIterations      int `json:"max_iterations"`
	LastTestResult string `json:"last_test_result,omitempty"`
	LastTestTime   string `json:"last_test_time,omitempty"`
	CompletedTasks []CompletedTaskEntry `json:"completed_tasks,omitempty"`
	SkippedTasks   []string             `json:"skipped_tasks,omitempty"`

	// Overflow captures unknown keys so round-tripping preserves them.
	Overflow map[string]json.RawMessage `json:"-"`
}

// MarshalJSON produces JSON that merges known fields with overflow keys,
// preserving any fields the bash side added that we don't model.
func (s State) MarshalJSON() ([]byte, error) {
	// Marshal known fields via an alias to avoid recursion.
	type Alias State
	known, err := json.Marshal(Alias(s))
	if err != nil {
		return nil, err
	}

	if len(s.Overflow) == 0 {
		return known, nil
	}

	// Merge: start with overflow, then overlay known fields (known wins).
	merged := make(map[string]json.RawMessage, len(s.Overflow)+10)
	for k, v := range s.Overflow {
		merged[k] = v
	}

	var knownMap map[string]json.RawMessage
	if err := json.Unmarshal(known, &knownMap); err != nil {
		return nil, err
	}
	for k, v := range knownMap {
		merged[k] = v
	}

	return json.Marshal(merged)
}

// UnmarshalJSON parses known fields and stores the rest in Overflow.
func (s *State) UnmarshalJSON(data []byte) error {
	// Collect all keys first so we can handle migration before typed unmarshal.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// Migrate old-format completed_tasks (string array) to the new
	// [{id, merged}] object format before the typed unmarshal.
	if ct, ok := raw["completed_tasks"]; ok {
		if migrated := migrateCompletedTasks(ct); migrated != nil {
			rewritten, _ := json.Marshal(migrated)
			raw["completed_tasks"] = json.RawMessage(rewritten)
			data, _ = json.Marshal(raw)
		}
	}

	type Alias State
	var alias Alias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}

	knownKeys := map[string]bool{
		"iteration": true, "status": true, "started_at": true,
		"last_task": true, "worktree_dir": true, "worktree_branch": true,
		"task_backend": true, "max_iterations": true,
		"last_test_result": true, "last_test_output": true, "last_test_time": true,
		"completed_tasks": true,
		"skipped_tasks":   true,
	}

	alias.Overflow = nil
	for k, v := range raw {
		if !knownKeys[k] {
			if alias.Overflow == nil {
				alias.Overflow = make(map[string]json.RawMessage)
			}
			alias.Overflow[k] = v
		}
	}

	*s = State(alias)
	return nil
}

// Store manages reading and writing state.json in a ralph directory.
type Store struct {
	path string // full path to state.json
}

// NewStore creates a Store for the given .ralph directory.
func NewStore(ralphDir string) *Store {
	return &Store{path: filepath.Join(ralphDir, "state.json")}
}

// Path returns the full path to the state file.
func (st *Store) Path() string {
	return st.path
}

// Load reads and parses the state file. Returns a zero State if the
// file does not exist.
func (st *Store) Load() (State, error) {
	data, err := os.ReadFile(st.path)
	if os.IsNotExist(err) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("reading state: %w", err)
	}

	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("parsing state: %w", err)
	}
	return s, nil
}

// Save writes state to disk using atomic temp-file + rename.
func (st *Store) Save(s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(st.path)
	tmp, err := os.CreateTemp(dir, ".state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing temp file: %w", err)
	}

	if err := os.Rename(tmpName, st.path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}

// Read reads a single key from the state, matching bash read_state() behavior.
// Returns the value as a string, or "" if the key doesn't exist.
func (st *Store) Read(key string) (string, error) {
	s, err := st.Load()
	if err != nil {
		return "", err
	}
	return getField(s, key), nil
}

// Write writes a single key to the state, matching bash write_state() behavior.
// Numeric strings are stored as JSON numbers for compatibility.
func (st *Store) Write(key, value string) error {
	s, err := st.Load()
	if err != nil {
		return err
	}
	setField(&s, key, value)
	return st.Save(s)
}

// BeginIteration records that a new iteration is starting for the given task.
func (st *Store) BeginIteration(taskID, taskTitle string, iteration int) {
	s, _ := st.Load()
	s.Iteration = iteration
	s.Status = "running"
	s.LastTask = taskTitle
	setField(&s, "last_task_id", taskID)
	st.Save(s)
}

// Init initializes state with config values. Creates the file if missing.
func (st *Store) Init(maxIterations int) error {
	s, _ := st.Load()
	if s.MaxIterations == 0 {
		s.MaxIterations = maxIterations
	}
	return st.Save(s)
}

// WriteConfig writes max_iterations to state.
func (st *Store) WriteConfig(maxIterations int) {
	st.Write("max_iterations", strconv.Itoa(maxIterations))
}

// SaveCLIConfig writes CLI config key-value pairs into state.json under a
// "cli_config" key. This allows evolve restart to reconstruct args from the
// semantic config rather than replaying raw CLI args.
func (st *Store) SaveCLIConfig(cfg map[string]string) error {
	s, err := st.Load()
	if err != nil {
		return err
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if s.Overflow == nil {
		s.Overflow = make(map[string]json.RawMessage)
	}
	s.Overflow["cli_config"] = json.RawMessage(data)
	return st.Save(s)
}

// ClearCLIConfig removes cli_config from state.json so stale flags don't
// persist across manual restarts.
func (st *Store) ClearCLIConfig() error {
	s, err := st.Load()
	if err != nil {
		return err
	}
	delete(s.Overflow, "cli_config")
	return st.Save(s)
}

// LoadCLIConfig reads the "cli_config" map from state.json. Returns nil map
// if not present.
func (st *Store) LoadCLIConfig() (map[string]string, error) {
	s, err := st.Load()
	if err != nil {
		return nil, err
	}
	if s.Overflow == nil {
		return nil, nil
	}
	raw, ok := s.Overflow["cli_config"]
	if !ok {
		return nil, nil
	}
	var cfg map[string]string
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing cli_config: %w", err)
	}
	return cfg, nil
}

// ReadMaxIterations returns max_iterations from state, falling back to the given default.
func (st *Store) ReadMaxIterations(defaultVal int) int {
	v, _ := st.Read("max_iterations")
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return defaultVal
}


// AddCompletedTask appends a task entry to the completed list.
// merged is true when the PR was successfully merged; false when the work is
// verified but the PR is still open (merge pending).
func (st *Store) AddCompletedTask(id string, merged bool) error {
	s, err := st.Load()
	if err != nil {
		return err
	}
	s.CompletedTasks = append(s.CompletedTasks, CompletedTaskEntry{ID: id, Merged: merged})
	return st.Save(s)
}

// GetCompletedTasks returns all completed task entries.
func (st *Store) GetCompletedTasks() ([]CompletedTaskEntry, error) {
	s, err := st.Load()
	if err != nil {
		return nil, err
	}
	return s.CompletedTasks, nil
}

// ClearCompletedTasks removes all entries from the completed tasks list.
func (st *Store) ClearCompletedTasks() error {
	s, err := st.Load()
	if err != nil {
		return err
	}
	s.CompletedTasks = nil
	return st.Save(s)
}

// AddSkippedTask appends a task ID to the skipped list if not already present.
func (st *Store) AddSkippedTask(id string) error {
	s, err := st.Load()
	if err != nil {
		return err
	}
	for _, existing := range s.SkippedTasks {
		if existing == id {
			return nil
		}
	}
	s.SkippedTasks = append(s.SkippedTasks, id)
	return st.Save(s)
}

// GetSkippedTasks returns all skipped task IDs.
func (st *Store) GetSkippedTasks() ([]string, error) {
	s, err := st.Load()
	if err != nil {
		return nil, err
	}
	return s.SkippedTasks, nil
}

// getField extracts a named field from State as a string.
func getField(s State, key string) string {
	switch key {
	case "iteration":
		return strconv.Itoa(s.Iteration)
	case "status":
		return s.Status
	case "started_at":
		return s.StartedAt
	case "last_task":
		return s.LastTask
	case "worktree_dir":
		return s.WorktreeDir
	case "worktree_branch":
		return s.WorktreeBranch
	case "task_backend":
		return s.TaskBackend
	case "max_iterations":
		return strconv.Itoa(s.MaxIterations)
	case "last_test_result":
		return s.LastTestResult
	case "last_test_time":
		return s.LastTestTime
	default:
		if s.Overflow != nil {
			if raw, ok := s.Overflow[key]; ok {
				var v interface{}
				if json.Unmarshal(raw, &v) == nil {
					return fmt.Sprintf("%v", v)
				}
			}
		}
		return ""
	}
}

// setField updates a named field on State. Numeric strings become ints
// to match bash jq behavior: .$key = ($v | try tonumber catch $v).
func setField(s *State, key, value string) {
	switch key {
	case "iteration":
		s.Iteration, _ = strconv.Atoi(value)
	case "status":
		s.Status = value
	case "started_at":
		s.StartedAt = value
	case "last_task":
		s.LastTask = value
	case "worktree_dir":
		s.WorktreeDir = value
	case "worktree_branch":
		s.WorktreeBranch = value
	case "task_backend":
		s.TaskBackend = value
	case "max_iterations":
		s.MaxIterations, _ = strconv.Atoi(value)
	case "last_test_result":
		s.LastTestResult = value
	case "last_test_time":
		s.LastTestTime = value
	default:
		// Unknown key — store in overflow, converting numeric strings to numbers.
		if s.Overflow == nil {
			s.Overflow = make(map[string]json.RawMessage)
		}
		if n, err := strconv.Atoi(value); err == nil {
			s.Overflow[key] = json.RawMessage(strconv.Itoa(n))
		} else {
			data, _ := json.Marshal(value)
			s.Overflow[key] = json.RawMessage(data)
		}
	}
}

// migrateCompletedTasks converts old-format completed_tasks to the new
// [{id, merged}] object format. Returns nil if already in the new format.
//
// Handled formats:
//   - New: [{id, merged}] — no migration needed
//   - Old: ["id1", "id2"] — string array, treated as merged:true
func migrateCompletedTasks(raw json.RawMessage) []CompletedTaskEntry {
	// New format: array of {id, merged} objects — strings can't unmarshal as
	// objects so this only succeeds for the new format. No migration needed.
	var entries []CompletedTaskEntry
	if json.Unmarshal(raw, &entries) == nil {
		return nil
	}
	// Old format: string array — migrate to [{id, merged:true}].
	var strs []string
	if json.Unmarshal(raw, &strs) != nil {
		return nil
	}
	result := make([]CompletedTaskEntry, 0, len(strs))
	for _, id := range strs {
		if id != "" {
			result = append(result, CompletedTaskEntry{ID: id, Merged: true})
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
