package tasks

import "github.com/brokenalarms/ralph/internal/config"

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

// Poll checks whether any tasks remain in the loop's inbox.
// Use during wait-mode polling.
func Poll(b Backend) (bool, error) {
	return b.HasRemaining()
}

// Next prepares the backend with the resume target, then returns the next
// task. Pass a non-empty resumeID only on the first iteration so a paused
// session picks up where it left off. An empty resumeID explicitly clears
// any previously-set resume target so subsequent iterations start fresh
// from the priority queue. Returns a zero TaskInfo when no task is available.
func Next(b Backend, resumeID string) (TaskInfo, error) {
	b.SetResumeTaskID(resumeID)
	return b.GetNextTaskInfo()
}

// HasOpenButAllSkipped reports whether tasks exist in the ralph-loop inbox
// (CountRemaining > 0) but none are immediately selectable (HasRemaining = false).
// With the assignee-based skip model, skipped tasks are reassigned to
// ralph-task and leave the loop's inbox entirely, so this function returns
// false when all tasks are skipped (inbox is empty and CountRemaining is 0).
// It returns true only when in_progress tasks exist but bd ready returns nothing,
// which is the stuck-in-progress detection path.
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

// StuckState describes the stall condition where in_progress tasks block all
// remaining open work.
type StuckState struct {
	// LoopOwnedStuckTaskIDs are in_progress task IDs assigned to the loop itself.
	// These are orphaned claims the loop can safely recover by reopening.
	LoopOwnedStuckTaskIDs []string
	// ExternalStuckTaskIDs are in_progress task IDs assigned to a non-loop actor.
	// These require human intervention to resolve.
	ExternalStuckTaskIDs []string
	// BlockedTaskIDs are the IDs of open tasks that cannot proceed until stuck tasks close.
	BlockedTaskIDs []string
}

// DetectBlockedByInProgress checks whether the empty ready queue is caused by
// in_progress tasks that have open dependents. Scans all in_progress tasks
// (any assignee) and separates them into loop-owned and externally-owned.
// Returns a non-nil StuckState when a stall is detected, nil when no stall exists.
func DetectBlockedByInProgress(b Backend) (*StuckState, error) {
	all, err := b.ListAllInProgress()
	if err != nil || len(all) == 0 {
		return nil, err
	}
	var loopStuck, externalStuck, blockedIDs []string
	for _, task := range all {
		deps, depErr := b.GetOpenDependents(task.ID)
		if depErr != nil {
			continue
		}
		if len(deps) > 0 {
			if task.Assignee == config.LoopAssignee {
				loopStuck = append(loopStuck, task.ID)
			} else {
				externalStuck = append(externalStuck, task.ID)
			}
			blockedIDs = append(blockedIDs, deps...)
		}
	}
	if len(loopStuck) == 0 && len(externalStuck) == 0 {
		return nil, nil
	}
	return &StuckState{
		LoopOwnedStuckTaskIDs: loopStuck,
		ExternalStuckTaskIDs:  externalStuck,
		BlockedTaskIDs:        blockedIDs,
	}, nil
}
