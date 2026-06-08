package tasks

import "testing"

// minimalBackend is an inline stub satisfying Backend for orchestration tests.
// Only SetResumeTaskID and GetNextTaskInfo need real implementations here;
// all other methods are no-ops.
type minimalBackend struct {
	resumeIDSet    string
	resumeIDCalled bool
	nextInfo       TaskInfo
}

func (b *minimalBackend) Init() error                               { return nil }
func (b *minimalBackend) HasRemaining() (bool, error)               { return false, nil }
func (b *minimalBackend) CountCompleted() (int, error)              { return 0, nil }
func (b *minimalBackend) CountRemaining() (int, error)              { return 0, nil }
func (b *minimalBackend) CountTotal() (int, error)                  { return 0, nil }
func (b *minimalBackend) GetNextTask() (string, error)              { return "", nil }
func (b *minimalBackend) GetNextTaskID() (string, error)            { return "", nil }
func (b *minimalBackend) GetNextTaskInfo() (TaskInfo, error)        { return b.nextInfo, nil }
func (b *minimalBackend) HasTasks() (bool, error)                   { return false, nil }
func (b *minimalBackend) CloseTask(string, string) error            { return nil }
func (b *minimalBackend) ClaimTask(string) error                    { return nil }
func (b *minimalBackend) SkipTask(string, string) error             { return nil }
func (b *minimalBackend) ReopenTask(string) error                   { return nil }
func (b *minimalBackend) SetState(_, _, _, _ string) error          { return nil }
func (b *minimalBackend) GetState(_, _ string) (string, error)      { return "", nil }
func (b *minimalBackend) ExecutionInstructions() (string, error)    { return "", nil }
func (b *minimalBackend) GetDescription(_ string) (string, error)   { return "", nil }
func (b *minimalBackend) GetAcceptance(_ string) (string, error)    { return "", nil }
func (b *minimalBackend) GetFullContext(_ string) (string, error)   { return "", nil }
func (b *minimalBackend) ProjectContext() (string, error)           { return "", nil }
func (b *minimalBackend) GetExternalRef(_ string) (string, error)   { return "", nil }
func (b *minimalBackend) SetExternalRef(_, _ string) error          { return nil }
func (b *minimalBackend) AppendNotes(_, _ string) error             { return nil }
func (b *minimalBackend) SetMetadata(_, _, _ string) error          { return nil }
func (b *minimalBackend) GetMetadata(_, _ string) (string, error)   { return "", nil }
func (b *minimalBackend) GetOpenDependents(_ string) ([]string, error)          { return nil, nil }
func (b *minimalBackend) ListInProgressByAssignee(_ string) ([]TaskInfo, error) { return nil, nil }
func (b *minimalBackend) IsReady(_ string) (bool, error)                        { return true, nil }
func (b *minimalBackend) Label() string                                          { return "beads" }
func (b *minimalBackend) SetSkippedIDs(_ []string)                  {}
func (b *minimalBackend) SetResumeTaskID(id string) {
	b.resumeIDCalled = true
	b.resumeIDSet = id
}

// Proves: Next always calls SetResumeTaskID, even when resumeID is empty.
// An empty call clears any previously-set resume target in the BD backend,
// preventing subsequent iterations from continuing to resume the same task.
func TestNext_AlwaysCallsSetResumeTaskID(t *testing.T) {
	b := &minimalBackend{nextInfo: TaskInfo{ID: "ralph-abc"}}

	// Simulate a prior call with a non-empty ID.
	b.SetResumeTaskID("ralph-abc")
	if b.resumeIDSet != "ralph-abc" {
		t.Fatalf("precondition: resumeIDSet = %q, want ralph-abc", b.resumeIDSet)
	}

	// Calling Next with an empty resumeID must clear the BD-internal resume state.
	b.resumeIDCalled = false
	_, _ = Next(b, "", nil)

	if !b.resumeIDCalled {
		t.Error("Next('', ...) must call SetResumeTaskID even with empty string to clear the BD resume target")
	}
	if b.resumeIDSet != "" {
		t.Errorf("SetResumeTaskID was called with %q, want empty string (clear)", b.resumeIDSet)
	}
}

// Proves: Next passes the resumeID to the backend when non-empty, so the
// first-iteration resume picks up where it left off.
func TestNext_SetsResumeIDWhenNonEmpty(t *testing.T) {
	b := &minimalBackend{nextInfo: TaskInfo{ID: "ralph-abc"}}

	_, _ = Next(b, "ralph-abc", nil)

	if b.resumeIDSet != "ralph-abc" {
		t.Errorf("SetResumeTaskID = %q, want ralph-abc", b.resumeIDSet)
	}
}
