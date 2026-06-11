package loop

import (
	"context"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
)

// newDiffTestLoop builds a Loop wired only with the git stub fetchVerifyDiff
// needs. fetchVerifyDiff touches l.git exclusively, so the other modules are
// left nil.
func newDiffTestLoop(gm git.Ops) *Loop {
	return New(Config{}, Modules{Git: gm, Logger: logging.New(nil)})
}

// When a PR exists, the PR diff is the preferred source regardless of the
// branch/iteration diffs.
func TestFetchVerifyDiff_PrefersPR(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		PRDiffForTaskResult: "pr-diff",
		DiffFromBaseResult:  "branch-diff",
		DiffFullResult:      "iteration-diff",
	})
	l := newDiffTestLoop(gm)

	diff, source := l.fetchVerifyDiff(context.Background(), "task-1", "headBefore", "")
	if diff != "pr-diff" || source != "PR" {
		t.Fatalf("expected (pr-diff, PR), got (%q, %q)", diff, source)
	}
}

// The false-skip regression: no PR exists yet, the work was committed in a
// prior iteration (so the iteration-local headBefore..HEAD diff is empty), but
// the branch is ahead of origin/<base>. fetchVerifyDiff must return the
// non-empty branch diff labeled "branch" — not the empty iteration diff —
// so the verifier sees the committed work instead of falsely skipping it.
func TestFetchVerifyDiff_NoPR_PriorIterationCommit_UsesBranchDiff(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		PRDiffForTaskResult: "",            // no PR
		DiffFromBaseResult:  "branch-diff", // commit ahead of origin/<base>
		DiffFullResult:      "",            // headBefore == HEAD: empty iteration diff
	})
	l := newDiffTestLoop(gm)

	diff, source := l.fetchVerifyDiff(context.Background(), "task-1", "same-sha", "")
	if diff != "branch-diff" || source != "branch" {
		t.Fatalf("expected (branch-diff, branch), got (%q, %q)", diff, source)
	}
}

// When no PR and no branch diff exist, fall back to the iteration-local diff.
func TestFetchVerifyDiff_FallsBackToIteration(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		PRDiffForTaskResult: "",
		DiffFromBaseResult:  "",
		DiffFullResult:      "iteration-diff",
	})
	l := newDiffTestLoop(gm)

	diff, source := l.fetchVerifyDiff(context.Background(), "task-1", "headBefore", "")
	if diff != "iteration-diff" || source != "iteration" {
		t.Fatalf("expected (iteration-diff, iteration), got (%q, %q)", diff, source)
	}
}

// No diffs available anywhere yields empty strings (verifier treats as no-op).
func TestFetchVerifyDiff_AllEmpty(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{})
	l := newDiffTestLoop(gm)

	diff, source := l.fetchVerifyDiff(context.Background(), "task-1", "headBefore", "")
	if diff != "" || source != "" {
		t.Fatalf("expected empty diff and source, got (%q, %q)", diff, source)
	}
}

// When signalTimeHead is set and the iteration diff is non-empty,
// fetchVerifyDiff prefers it over DiffFromBase — it cannot include unrelated
// commits that merged to origin/<base> mid-iteration.
func TestFetchVerifyDiff_SignalTimeHead_PrefersIterationDiffOverBase(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		PRDiffForTaskResult: "",
		DiffFromBaseResult:  "base-diff-with-unrelated",
		DiffFullResult:      "agent-work-diff",
	})
	l := newDiffTestLoop(gm)

	diff, source := l.fetchVerifyDiff(context.Background(), "task-1", "headBefore", "signal-sha")
	if diff != "agent-work-diff" || source != "branch" {
		t.Fatalf("expected (agent-work-diff, branch), got (%q, %q)", diff, source)
	}
}

// When signalTimeHead is set but the iteration diff is empty (prior-iteration
// commits only, headBefore==HEAD this iteration), fetchVerifyDiff falls through
// to DiffFromBase so the verifier still sees the prior-iteration work.
func TestFetchVerifyDiff_SignalTimeHead_EmptyIterationFallsThroughToBase(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		PRDiffForTaskResult: "",
		DiffFromBaseResult:  "prior-iteration-branch-diff",
		DiffFullResult:      "", // headBefore == HEAD: empty iteration diff
	})
	l := newDiffTestLoop(gm)

	diff, source := l.fetchVerifyDiff(context.Background(), "task-1", "same-sha", "signal-sha")
	if diff != "prior-iteration-branch-diff" || source != "branch" {
		t.Fatalf("expected (prior-iteration-branch-diff, branch), got (%q, %q)", diff, source)
	}
}
