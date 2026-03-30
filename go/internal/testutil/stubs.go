package testutil

import (
	"sync"

	"github.com/brokenalarms/ralph/internal/tasks"
)

// StubBackend implements tasks.Backend for testing without shelling out to
// bd or reading plan files. Configure fields to control task state.
type StubBackend struct {
	Remaining    int
	Completed    int
	Total        int
	NextTask     string
	NextID       string
	NextPriority *int
	BackendLabel string
	Description  string
	Acceptance   string
	FullContext   string
	SkippedTask  string
	SkipReason   string
}

func (s *StubBackend) Init() error                    { return nil }
func (s *StubBackend) HasRemaining() (bool, error)    { return s.Remaining > 0, nil }
func (s *StubBackend) CountCompleted() (int, error)   { return s.Completed, nil }
func (s *StubBackend) CountRemaining() (int, error)   { return s.Remaining, nil }
func (s *StubBackend) CountTotal() (int, error)       { return s.Total, nil }
func (s *StubBackend) GetNextTask() (string, error)   { return s.NextTask, nil }
func (s *StubBackend) GetNextTaskID() (string, error) { return s.NextID, nil }
func (s *StubBackend) GetNextTaskInfo() (tasks.TaskInfo, error) {
	return tasks.TaskInfo{ID: s.NextID, Title: s.NextTask, Priority: s.NextPriority}, nil
}
func (s *StubBackend) HasTasks() (bool, error)        { return s.Total > 0, nil }
func (s *StubBackend) CloseTask(string, string) error { return nil }
func (s *StubBackend) SkipTask(id, reason string) error {
	s.SkippedTask = id
	s.SkipReason = reason
	return nil
}
func (s *StubBackend) SetSkippedIDs(_ []string)                   {}
func (s *StubBackend) ReopenTask(string) error                    { return nil }
func (s *StubBackend) SetState(_, _, _, _ string) error           { return nil }
func (s *StubBackend) GetState(_, _ string) (string, error)       { return "", nil }
func (s *StubBackend) ExecutionInstructions() (string, error)     { return "", nil }
func (s *StubBackend) GetDescription(_ string) (string, error)    { return s.Description, nil }
func (s *StubBackend) GetAcceptance(_ string) (string, error)     { return s.Acceptance, nil }
func (s *StubBackend) GetFullContext(_ string) (string, error)    { return s.FullContext, nil }
func (s *StubBackend) ProjectContext() (string, error)            { return "", nil }
func (s *StubBackend) GetExternalRef(_ string) (string, error)    { return "", nil }
func (s *StubBackend) SetExternalRef(_, _ string) error           { return nil }
func (s *StubBackend) AppendNotes(_, _ string) error              { return nil }
func (s *StubBackend) SetMetadata(_, _, _ string) error           { return nil }
func (s *StubBackend) GetMetadata(_, _ string) (string, error)    { return "", nil }
func (s *StubBackend) Label() string {
	if s.BackendLabel != "" {
		return s.BackendLabel
	}
	return "beads"
}

// MutableBackend extends StubBackend with mutex-protected reads for
// simulating task transitions mid-run. All shared fields live on the
// embedded StubBackend; only Metadata and ExternalRefs are additions.
type MutableBackend struct {
	StubBackend
	mu           sync.Mutex
	Metadata     map[string]map[string]string
	ExternalRefs map[string]string
}

func (m *MutableBackend) HasRemaining() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Remaining > 0, nil
}
func (m *MutableBackend) CountCompleted() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Completed, nil
}
func (m *MutableBackend) CountRemaining() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Remaining, nil
}
func (m *MutableBackend) CountTotal() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Total, nil
}
func (m *MutableBackend) GetNextTask() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.NextTask, nil
}
func (m *MutableBackend) GetNextTaskID() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.NextID, nil
}
func (m *MutableBackend) GetNextTaskInfo() (tasks.TaskInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return tasks.TaskInfo{ID: m.NextID, Title: m.NextTask, Priority: m.NextPriority}, nil
}
func (m *MutableBackend) HasTasks() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Total > 0, nil
}
func (m *MutableBackend) GetDescription(_ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Description, nil
}
func (m *MutableBackend) GetExternalRef(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ExternalRefs != nil {
		return m.ExternalRefs[id], nil
	}
	return "", nil
}
func (m *MutableBackend) GetMetadata(id, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Metadata != nil {
		if keys, ok := m.Metadata[id]; ok {
			return keys[key], nil
		}
	}
	return "", nil
}

// Lock exposes the mutex for tests that need to modify fields atomically.
func (m *MutableBackend) Lock()   { m.mu.Lock() }
func (m *MutableBackend) Unlock() { m.mu.Unlock() }

// TrackingBackend extends MutableBackend to record CloseTask and SkipTask calls.
type TrackingBackend struct {
	MutableBackend
	ClosedIDs    []string
	CloseReasons []string
	CloseMu      sync.Mutex
	SkippedIDs   []string
	SkipReasons  []string
	SkipMu       sync.Mutex
}

func (t *TrackingBackend) CloseTask(id string, reason string) error {
	t.CloseMu.Lock()
	t.ClosedIDs = append(t.ClosedIDs, id)
	t.CloseReasons = append(t.CloseReasons, reason)
	t.CloseMu.Unlock()
	return nil
}

func (t *TrackingBackend) SkipTask(id string, reason string) error {
	t.SkipMu.Lock()
	t.SkippedIDs = append(t.SkippedIDs, id)
	t.SkipReasons = append(t.SkipReasons, reason)
	t.SkipMu.Unlock()
	return nil
}
