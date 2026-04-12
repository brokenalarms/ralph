package git

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// fileStateStore implements stateStore by reading/writing a JSON file.
// This is the production implementation constructed by newRepo from
// Config.RalphDir. The file format matches internal/state.Store so the
// orchestrator and git module share the same state file.
type fileStateStore struct {
	path string
}

func newFileStateStore(ralphDir string) *fileStateStore {
	return &fileStateStore{path: filepath.Join(ralphDir, "state.json")}
}

func (s *fileStateStore) Read(key string) (string, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return "", err
	}
	v, ok := m[key]
	if !ok {
		return "", nil
	}
	switch tv := v.(type) {
	case string:
		return tv, nil
	case float64:
		if tv == float64(int(tv)) {
			return fmt.Sprintf("%d", int(tv)), nil
		}
		return fmt.Sprintf("%g", tv), nil
	case bool:
		if tv {
			return "true", nil
		}
		return "false", nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

func (s *fileStateStore) Write(key, value string) error {
	data, err := os.ReadFile(s.path)
	var m map[string]any
	switch {
	case os.IsNotExist(err):
		m = make(map[string]any)
	case err != nil:
		return err
	case len(data) > 0:
		if err := json.Unmarshal(data, &m); err != nil {
			return err
		}
		if m == nil {
			m = make(map[string]any)
		}
	default:
		m = make(map[string]any)
	}
	m[key] = value

	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*.json.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), s.path)
}
