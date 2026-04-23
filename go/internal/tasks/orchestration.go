package tasks

// Availability reports whether tasks exist and are ready to work on.
type Availability struct {
	// HasRemaining is true when uncompleted tasks are available.
	HasRemaining bool
	// HasAny is true when at least one task exists (even if none remain).
	// Meaningful only when HasRemaining is false.
	HasAny bool
}

// CheckAvailability queries the backend for task availability in a single
// logical operation. When HasRemaining is false, HasAny distinguishes
// "all done" (HasAny=true) from "nothing ever created" (HasAny=false).
func CheckAvailability(b Backend) (Availability, error) {
	hasRemaining, err := b.HasRemaining()
	if err != nil {
		return Availability{}, err
	}
	if hasRemaining {
		return Availability{HasRemaining: true, HasAny: true}, nil
	}
	hasTasks, _ := b.HasTasks()
	return Availability{HasRemaining: false, HasAny: hasTasks}, nil
}

// Poll updates the backend's skip filter, then checks whether any tasks
// remain. Use during wait-mode polling so skipped IDs stay current.
func Poll(b Backend, skippedIDs []string) (bool, error) {
	b.SetSkippedIDs(skippedIDs)
	return b.HasRemaining()
}

// Next prepares the backend with resume and skip state, then returns the
// next task. Pass a non-empty resumeID only on the first iteration so a
// paused session picks up where it left off. Returns a zero TaskInfo when
// no task is available.
func Next(b Backend, resumeID string, skippedIDs []string) (TaskInfo, error) {
	b.SetSkippedIDs(skippedIDs)
	if resumeID != "" {
		b.SetResumeTaskID(resumeID)
	}
	return b.GetNextTaskInfo()
}

// HasOpenButAllSkipped reports whether open tasks exist but none are available
// after the skip filter is applied. When true, polling will never yield new
// work and the loop should exit rather than waiting forever.
func HasOpenButAllSkipped(b Backend) (bool, error) {
	count, err := b.CountRemaining()
	if err != nil {
		return false, err
	}
	if count == 0 {
		return false, nil
	}
	has, err := b.HasRemaining()
	if err != nil {
		return false, err
	}
	return !has, nil
}

// Progress returns the completed and total task counts from the backend.
func Progress(b Backend) (completed, total int) {
	c, _ := b.CountCompleted()
	t, _ := b.CountTotal()
	return c, t
}
