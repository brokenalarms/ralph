package tasks

import "testing"

// Proves the SkipReason typed enum exists with one stable string constant
// per skip category (ralph-6qg6 AC 1 & 2), so future consumers (bead
// metadata persistence, loop-control cascade logic) have a single typed
// source of truth instead of scattered string literals.
func TestSkipReason_ConstantsAreStableStrings(t *testing.T) {
	cases := []struct {
		reason SkipReason
		want   string
	}{
		{SkipCompaction, "compaction_detected"},
		{SkipIdleTimeout, "idle_timeout_max_failures"},
		{SkipFailedStart, "failed_start_limit_reached"},
		{SkipVerificationRejected, "verification_rejected"},
		{SkipCloseFailed, "close_failed"},
		{SkipDependencyBlocked, "dependency_blocked_by"},
		{SkipPushFailed, "push_failed"},
		{SkipPRCreationFailed, "pr_creation_failed"},
		{SkipTransportError, "transport_error"},
		{SkipAnalyzer, "analyzer"},
		{SkipAlreadyCompleted, "already_completed_this_session"},
		{SkipWouldStrandDependents, "would_strand_dependents"},
		{SkipStagnation, "stagnation"},
	}

	seen := make(map[SkipReason]bool, len(cases))
	for _, c := range cases {
		if string(c.reason) != c.want {
			t.Errorf("%v: got %q, want %q", c.reason, string(c.reason), c.want)
		}
		if seen[c.reason] {
			t.Errorf("duplicate SkipReason value %q", c.reason)
		}
		seen[c.reason] = true
	}
}
