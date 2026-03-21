// Package tasks defines the task backend interface and provides
// implementations for different task tracking systems (checklist, bd).
package tasks

// Backend abstracts task tracking so ralph can drive iteration
// against different storage systems (plan.md checklists, beads/bd).
type Backend interface {
	// Init prepares the backend for use (e.g. health checks, file creation).
	Init() error

	// HasRemaining reports whether uncompleted tasks exist.
	HasRemaining() (bool, error)

	// CountCompleted returns the number of finished tasks.
	CountCompleted() (int, error)

	// CountRemaining returns the number of unfinished tasks.
	CountRemaining() (int, error)

	// CountTotal returns the total number of tasks.
	CountTotal() (int, error)

	// GetNextTask returns the description of the next task to work on.
	// Returns empty string when no tasks remain.
	GetNextTask() (string, error)

	// GetNextTaskID returns a backend-specific identifier for the next task.
	// Returns empty string for backends without IDs (e.g. checklist).
	GetNextTaskID() (string, error)

	// HasTasks reports whether any tasks exist at all.
	HasTasks() (bool, error)

	// NeedsPlanning reports whether the planning phase should run.
	NeedsPlanning() (bool, error)

	// PlanningSucceeded checks that planning produced valid tasks.
	PlanningSucceeded() (bool, error)

	// CloseTask marks a task as complete. The id parameter is backend-specific
	// (empty for checklist, a bd issue ID for the bd backend).
	CloseTask(id string, reason string) error

	// SkipTask marks a task as blocked/skipped. For bd, this closes with
	// "blocked: reason". For checklist, it replaces [ ] with [s] and appends
	// the reason.
	SkipTask(id string, reason string) error

	// ExecutionInstructions returns the prompt text for the execution phase.
	ExecutionInstructions() (string, error)

	// PlanningInstructions returns the prompt text for the planning phase.
	PlanningInstructions() string

	// Label returns a human-readable name for the backend ("checklist" or "beads").
	Label() string
}
