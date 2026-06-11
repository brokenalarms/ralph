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
	// excluding skipped or otherwise non-actionable items.
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

	// SkipTask reassigns the bead to config.TaskAssignee with status=open and
	// label=skipped, removing it from the loop's bd ready --assignee=ralph-loop
	// inbox. The reason is recorded as a bead comment. Un-skip by reassigning
	// back to ralph-loop via bd update <id> --assignee=ralph-loop.
	SkipTask(id string, reason string) error

	// SetResumeTaskID tells the backend to check this task first before
	// falling through to the ready queue. If the task is still
	// open/in_progress, it is returned by GetNextTaskInfo.
	SetResumeTaskID(id string)

	// ClaimTask sets a task to status=in_progress via the canonical beads
	// claim mechanism (bd update <id> --claim), removing it from bd ready
	// so no other actor can select it concurrently.
	ClaimTask(id string) error

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

	// GetExternalRef returns the external reference (e.g. "gh-123") for a task.
	GetExternalRef(id string) (string, error)

	// SetExternalRef sets the external reference on a task (e.g. "gh-123" for PR #123).
	SetExternalRef(id, ref string) error

	// AppendNotes appends a message to the task's notes field.
	AppendNotes(id, msg string) error

	// SetMetadata sets a custom metadata key-value pair on a task.
	SetMetadata(id, key, value string) error

	// GetMetadata returns the value of a custom metadata key on a task.
	GetMetadata(id, key string) (string, error)

	// GetOpenDependents returns the IDs of open issues that depend on the given task.
	// Returns nil when there are no open dependents or when the query fails.
	GetOpenDependents(id string) ([]string, error)

	// ListInProgressByAssignee returns TaskInfo for all in_progress tasks assigned
	// to the given assignee. Used to detect stuck-loop conditions where no tasks are
	// ready because they are all blocked by an in_progress task the loop owns.
	ListInProgressByAssignee(assignee string) ([]TaskInfo, error)

	// IsReady reports whether a task is ready to work on: its dependencies array
	// is empty OR every entry has status=closed. Returns false (no error) for any
	// non-closed dep. Use immediately before agent invocation to detect dep-graph
	// mutations that occurred between selection and start.
	IsReady(id string) (bool, error)

	// Label returns a human-readable name for the backend.
	Label() string
}
