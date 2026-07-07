package git

import "testing"

// LeftoverRalphPRs filters to open, ralph-authored PRs and sorts them newest
// (highest PR number) first — the ordering the ralph-i003 startup prompt
// relies on to pick "the newest leftover branch" to offer as the stack head.
func TestLeftoverRalphPRs_FiltersAndSortsDescending(t *testing.T) {
	allPRs := []PRInfo{
		{Number: 1200, Head: "ralph/older-task", Base: "main", State: PRStateOpen},
		{Number: 1241, Head: "ralph/tabi-uael", Base: "main", State: PRStateOpen},
		{Number: 1150, Head: "ralph/merged-task", Base: "main", State: PRStateMerged},
		{Number: 1300, Head: "feature/unrelated", Base: "main", State: PRStateOpen},
		{Number: 1100, Head: "ralph/closed-task", Base: "main", State: PRStateClosed},
	}

	leftover := LeftoverRalphPRs(allPRs)

	if len(leftover) != 2 {
		t.Fatalf("expected 2 leftover ralph PRs, got %d: %v", len(leftover), leftover)
	}
	if leftover[0].Number != 1241 || leftover[0].Head != "ralph/tabi-uael" {
		t.Errorf("expected newest leftover PR #1241 first, got #%d %s", leftover[0].Number, leftover[0].Head)
	}
	if leftover[1].Number != 1200 || leftover[1].Head != "ralph/older-task" {
		t.Errorf("expected #1200 second, got #%d %s", leftover[1].Number, leftover[1].Head)
	}
}

// LeftoverRalphPRs returns nil (not a panic or a slice of zero-value junk)
// when no PRs are open ralph-authored branches — the "no prompt" case.
func TestLeftoverRalphPRs_EmptyWhenNoneMatch(t *testing.T) {
	allPRs := []PRInfo{
		{Number: 1, Head: "feature/x", Base: "main", State: PRStateOpen},
		{Number: 2, Head: "ralph/done", Base: "main", State: PRStateMerged},
	}

	leftover := LeftoverRalphPRs(allPRs)

	if len(leftover) != 0 {
		t.Errorf("expected no leftover PRs, got %v", leftover)
	}
}
