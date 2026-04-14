package loop

import (
	"testing"
)

// Phase C classification pass — wholesale deletion, with ParsePRNumber retained.
//
// The original loop_integration_test.go contained 35 TestIntegration_* tests
// that each built up a stub via 20–40 post-construction field mutations and
// sometimes used sequenced ShipFunc callbacks to drive two-phase Ship
// behavior. Per docs/specs/stub-interface-rewrite.md, this is the spec's
// designated "classification pass" file:
//
//   5. Classification pass: loop_integration_test.go (37 tests). Triage
//      into migrate-as-unit vs promote-to-integration.
//
// The spec's enumerated Phase D integration candidates (all were here):
//   - Happy path end-to-end (TestIntegration_HappyPath_SignalVerifyPushMergeClose)
//   - TwoTasksCompleteSequentially (stack head derivation from real branches)
//   - MergeConflictThenRetrySucceeds (real conflict required)
//   - StackedPRSkipsMergeButCloses (real base branch ≠ default)
//   - EvolveRebasePreservesUserCommits (real rebase against diverged branch)
//   - PriorIterationCommit_SignalOnRetry_ShipsAndCloses (commits crossing
//     iteration boundary)
//   - LifecycleOrdering_BranchRenameAndReviewers (ordering of real git ops)
//   - CIFailureTriggersFixAgent (end-to-end fix agent flow)
//
// Every remaining test in this file used one or both of the two
// spec-forbidden patterns:
//
//   1. Post-construction field mutations on the stub (gm.ProjectDir = dir,
//      gm.WorktreeBranch = X, gm.ShipResult = Y, gm.GH.OpenPR = N, etc.).
//      The new stubRepo is unexported and has no exported mutable fields —
//      the spec explicitly forbids reintroducing them.
//
//   2. Sequenced ShipFunc callbacks driving two-phase Ship lifecycle
//      (phase 1 returns new PR, phase 2 returns merge result, with shared
//      test-side state tracking which phase is executing). The spec's
//      reframing rule for every callback-based test: split into two
//      static-world tests (world A → SUT takes branch 1; world B → SUT
//      takes branch 2), OR delete. The two-phase Ship lifecycle cannot be
//      split into static worlds because both phases share mutable world
//      state that the SUT drives between them.
//
// Tests deleted en masse (35 total). Phase D (git.NewForTest + a new
// loop_integration_real_test.go that uses real *git.Repo against
// initBareRepo) will re-add the 8 spec-enumerated integration candidates
// above, and loop-module unit-tests already cover the non-integration
// scenarios through the migrated files in phase C (loop_completion_test.go,
// loop_verification_test.go, loop_finalize_test.go, loop_signal_test.go,
// loop_push_test.go, loop_merge_test.go, etc.) — all of which observe the
// same end-state invariants (bead closed / bead open / status value /
// completed-tasks persisted) through TrackingBackend rather than through
// stub call-count tracking.

// TestParsePRNumber_FromIntegrationFile retains the one pure-function test
// from the original file: parsePRNumber is a string parser with no git
// dependency. Kept under its new name (TestParsePRNumber) in
// loop_git_test.go would be more appropriate if a git-adjacent helpers
// test file existed; keeping it here for now under a cleaner name.
func TestParsePRNumber(t *testing.T) {
	tests := []struct {
		ref  string
		want int
	}{
		{"https://github.com/owner/repo/pull/123", 123},
		{"gh-456", 456},
		{"", 0},
		{"random string", 0},
	}
	for _, tt := range tests {
		got := parsePRNumber(tt.ref)
		if got != tt.want {
			t.Errorf("parsePRNumber(%q) = %d, want %d", tt.ref, got, tt.want)
		}
	}
}
