package tasks

// SkipReason categorizes why the loop skipped a task, so downstream
// consumers (bead metadata, loop-control cascade/stagnation logic) can
// branch on the reason category rather than parsing free-text strings.
type SkipReason string

const (
	SkipCompaction            SkipReason = "compaction_detected"
	SkipIdleTimeout           SkipReason = "idle_timeout_max_failures"
	SkipFailedStart           SkipReason = "failed_start_limit_reached"
	SkipVerificationRejected  SkipReason = "verification_rejected"
	SkipCloseFailed           SkipReason = "close_failed"
	SkipDependencyBlocked     SkipReason = "dependency_blocked_by"
	SkipPushFailed            SkipReason = "push_failed"
	SkipPRCreationFailed      SkipReason = "pr_creation_failed"
	SkipMergeFailed           SkipReason = "merge_failed"
	SkipTransportError        SkipReason = "transport_error"
	SkipAnalyzer              SkipReason = "analyzer"
	SkipAlreadyCompleted      SkipReason = "already_completed_this_session"
	SkipWouldStrandDependents SkipReason = "would_strand_dependents"
	SkipStagnation            SkipReason = "stagnation"
)
