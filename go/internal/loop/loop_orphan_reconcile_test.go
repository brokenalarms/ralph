package loop

import (
	"testing"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/testutil"
)

// orphanReconcileBackend records ReopenTask calls and serves a fixed
// in_progress set so reconcileOrphanedClaims can be tested in isolation.
type orphanReconcileBackend struct {
	testutil.StubBackend
	inProgress []tasks.TaskInfo
	reopened   []string
}

func (b *orphanReconcileBackend) ListAllInProgress() ([]tasks.TaskInfo, error) {
	return b.inProgress, nil
}

func (b *orphanReconcileBackend) ReopenTask(id string) error {
	b.reopened = append(b.reopened, id)
	return nil
}

// Proves: at startup the loop reopens every loop-owned in_progress bead that is
// NOT the resume target, and leaves the resume target and non-loop beads alone.
// This is the recovery path for claims stranded by a prior crashed/killed/
// abandoned session — without it, an orphan with no open dependents is invisible
// to `bd ready` and never recovered.
func TestReconcileOrphanedClaims_ReopensOrphansExceptResumeTarget(t *testing.T) {
	_, st := setupTestDir(t)
	if err := st.Write("current_task_id", "resume-target"); err != nil {
		t.Fatal(err)
	}

	backend := &orphanReconcileBackend{inProgress: []tasks.TaskInfo{
		{ID: "orphan-1", Assignee: config.LoopAssignee},
		{ID: "resume-target", Assignee: config.LoopAssignee},
		{ID: "task-owned", Assignee: config.TaskAssignee},
		{ID: "orphan-2", Assignee: config.LoopAssignee},
	}}

	l := &Loop{
		state:       st,
		taskBackend: backend,
		logger:      logging.New(nil),
	}

	l.reconcileOrphanedClaims()

	if len(backend.reopened) != 2 {
		t.Fatalf("reopened = %v, want exactly [orphan-1 orphan-2]", backend.reopened)
	}
	for _, id := range backend.reopened {
		if id == "resume-target" {
			t.Error("resume target must NOT be reopened — it is a genuine mid-task resume")
		}
		if id == "task-owned" {
			t.Error("ralph-task-owned bead must NOT be reopened — it left the loop's inbox")
		}
	}
}

// Proves: with no resume target set, all loop-owned in_progress beads are
// reopened (none is the active task on a fresh start).
func TestReconcileOrphanedClaims_NoResumeTargetReopensAll(t *testing.T) {
	_, st := setupTestDir(t)

	backend := &orphanReconcileBackend{inProgress: []tasks.TaskInfo{
		{ID: "a", Assignee: config.LoopAssignee},
		{ID: "b", Assignee: config.LoopAssignee},
	}}

	l := &Loop{
		state:       st,
		taskBackend: backend,
		logger:      logging.New(nil),
	}

	l.reconcileOrphanedClaims()

	if len(backend.reopened) != 2 {
		t.Fatalf("reopened = %v, want [a b]", backend.reopened)
	}
}
