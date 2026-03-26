// Package tasks defines the task backend interface and the bd implementation.
package tasks

// TaskInfo holds the result of a GetNextTaskInfo call.
type TaskInfo struct {
	ID       string
	Title    string
	Priority *int
}

// Backend abstracts task tracking so ralph can drive iteration.
type Backend interface {
	// Init prepares the backend for use (e.g. health checks).
	Init() error

	// HasRemaining reports whether uncompleted tasks exist.
	HasRemaining() (bool, error)

	// CountCompleted returns the number of finished tasks.
	CountCompleted() (int, error)

	// CountRemaining returns the number of unfinished tasks.
	CountRemaining() (int, error)

	// CountTotal returns the number of actionable tasks (remaining + completed),
	// excluding deferred or otherwise non-actionable items.
	CountTotal() (int, error)

	// GetNextTask returns the description of the next task to work on.
	// Returns empty string when no tasks remain.
	GetNextTask() (string, error)

	// GetNextTaskID returns the identifier for the next task.
	// Returns empty string when no tasks remain.
	GetNextTaskID() (string, error)

	// GetNextTaskInfo returns the ID, description, and priority of the next task
	// in a single backend query, avoiding the race condition where separate
	// GetNextTask/GetNextTaskID calls could return data from different tasks.
	GetNextTaskInfo() (TaskInfo, error)

	// HasTasks reports whether any tasks exist at all.
	HasTasks() (bool, error)

	// CloseTask marks a task as complete.
	CloseTask(id string, reason string) error

	// SkipTask marks a task as blocked/skipped with a reason.
	SkipTask(id string, reason string) error

	// ReopenTask sets an in-progress task back to open status so it
	// returns to the ready queue.
	ReopenTask(id string) error

	// ExecutionInstructions returns the prompt text for the execution phase.
	ExecutionInstructions() (string, error)

	// SetState sets an operational state dimension on a task (e.g. phase=implementing).
	SetState(id, dimension, value, reason string) error

	// GetState returns the current value of a state dimension on a task.
	GetState(id, dimension string) (string, error)

	// GetDescription returns the description/body of a task by ID.
	GetDescription(id string) (string, error)

	// GetAcceptance returns the acceptance criteria for a task by ID.
	GetAcceptance(id string) (string, error)

	// GetFullContext returns the complete human-readable task context
	// (title, description, notes, labels, dependencies, comments).
	GetFullContext(id string) (string, error)

	// ProjectContext returns pre-assembled context about the project's task
	// state for prompt injection (open/closed beads, config, bd prime output).
	ProjectContext() (string, error)

	// Label returns a human-readable name for the backend.
	Label() string
}
