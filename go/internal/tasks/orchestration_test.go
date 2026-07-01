package tasks

import (
	"testing"

	"github.com/brokenalarms/ralph/internal/config"
)

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
func (b *minimalBackend) SkipTask(string, SkipReason, string) error { return nil }
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
func (b *minimalBackend) ListAllInProgress() ([]TaskInfo, error)                { return nil, nil }
func (b *minimalBackend) IsReady(_ string) (bool, error)                        { return true, nil }
func (b *minimalBackend) Label() string                                          { return "beads" }
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
	_, _ = Next(b, "")

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

	_, _ = Next(b, "ralph-abc")

	if b.resumeIDSet != "ralph-abc" {
		t.Errorf("SetResumeTaskID = %q, want ralph-abc", b.resumeIDSet)
	}
}

// inProgressBackend stubs ListAllInProgress with a fixed set for orphan tests.
type inProgressBackend struct {
	minimalBackend
	inProgress []TaskInfo
}

func (b *inProgressBackend) ListAllInProgress() ([]TaskInfo, error) { return b.inProgress, nil }

// Proves: OrphanedLoopClaims returns only ralph-loop-assigned in_progress beads,
// excludes the resume target, and ignores beads assigned to any other actor.
func TestOrphanedLoopClaims_FiltersLoopOwnedExcludingResume(t *testing.T) {
	b := &inProgressBackend{inProgress: []TaskInfo{
		{ID: "loop-orphan", Assignee: config.LoopAssignee},
		{ID: "resume-target", Assignee: config.LoopAssignee},
		{ID: "task-owned", Assignee: config.TaskAssignee},
		{ID: "external", Assignee: "someone-else"},
		{ID: "", Assignee: config.LoopAssignee},
	}}

	got, err := OrphanedLoopClaims(b, "resume-target")
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0] != "loop-orphan" {
		t.Errorf("OrphanedLoopClaims = %v, want [loop-orphan] (loop-owned, not resume target, non-empty)", got)
	}
}

// Proves: with no resume target, every loop-owned in_progress bead is an orphan,
// while non-loop assignees are still excluded.
func TestOrphanedLoopClaims_NoResumeTarget(t *testing.T) {
	b := &inProgressBackend{inProgress: []TaskInfo{
		{ID: "a", Assignee: config.LoopAssignee},
		{ID: "b", Assignee: config.LoopAssignee},
		{ID: "c", Assignee: config.TaskAssignee},
	}}

	got, err := OrphanedLoopClaims(b, "")
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("OrphanedLoopClaims = %v, want [a b]", got)
	}
}

// reassignableBackend simulates a bead being reassigned: initially HasRemaining
// returns false (bead assigned to ralph-task), then true after reassignment back.
type reassignableBackend struct {
	minimalBackend
	remaining bool
}

func (b *reassignableBackend) HasRemaining() (bool, error) { return b.remaining, nil }

// Proves: reassigning a skipped bead back to ralph-loop makes Next/Poll select
// it again, because skip is pure assignee state — no separate filter exists.
func TestNext_ReassignedBeadIsSelectable(t *testing.T) {
	b := &reassignableBackend{
		minimalBackend: minimalBackend{nextInfo: TaskInfo{ID: "ralph-abc", Title: "Fix auth"}},
	}

	// Before reassignment: bead assigned to ralph-task, not in inbox.
	b.remaining = false
	has, err := Poll(b)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("Poll: expected false before reassignment to ralph-loop")
	}

	// After reassignment back to ralph-loop: bead appears in inbox.
	b.remaining = true
	has, err = Poll(b)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("Poll: expected true after reassignment back to ralph-loop")
	}

	// Next also picks it up — no skip filter to re-suppress it.
	info, err := Next(b, "")
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "ralph-abc" {
		t.Errorf("Next: expected ralph-abc after reassignment, got %q", info.ID)
	}
}
