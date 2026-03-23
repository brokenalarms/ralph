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

	// GetNextTaskInfo returns both the ID and description of the next task
	// in a single backend query, avoiding the race condition where separate
	// GetNextTask/GetNextTaskID calls could return data from different tasks.
	GetNextTaskInfo() (id string, title string, err error)

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

	// ReopenTask sets an in-progress task back to open status so it
	// returns to the ready queue. Used when a higher-priority task
	// preempts the current in-progress task.
	ReopenTask(id string) error

	// ExecutionInstructions returns the prompt text for the execution phase.
	ExecutionInstructions() (string, error)

	// PlanningInstructions returns the prompt text for the planning phase.
	PlanningInstructions() string

	// SetState sets an operational state dimension on a task (e.g. phase=implementing).
	// Backends without state support (checklist) treat this as a no-op.
	SetState(id, dimension, value, reason string) error

	// GetState returns the current value of a state dimension on a task.
	// Returns empty string when unset or unsupported.
	GetState(id, dimension string) (string, error)

	// GetDescription returns the description/body of a task by ID.
	// Returns empty string for backends without descriptions.
	GetDescription(id string) (string, error)

	// GetFullContext returns the complete context of a task by ID,
	// including notes, labels, dependencies, and comments — everything
	// an agent needs to understand the task without running bd show.
	// Returns empty string for backends without rich context.
	GetFullContext(id string) (string, error)

	// ProjectContext returns pre-assembled context about the project's task
	// state for prompt injection. For bd, this includes open/closed beads,
	// project directory, config, and bd prime output. Checklist returns "".
	ProjectContext() (string, error)

	// Label returns a human-readable name for the backend ("checklist" or "beads").
	Label() string
}
