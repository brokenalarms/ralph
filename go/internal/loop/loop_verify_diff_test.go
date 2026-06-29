package loop

import (
	"context"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
)

// newDiffTestLoop builds a Loop wired only with the git stub fetchVerifyDiff
// needs. With no task backend, the external-ref PR-recovery path is inert, so
// these loops exercise the local-diff sources exclusively.
func newDiffTestLoop(gm git.Ops) *Loop {
	return New(Config{}, Modules{Git: gm, Logger: logging.New(nil)})
}

// newDiffTestLoopWithRef builds a Loop whose task backend returns the given
// external-ref for any task, so the resume-recovery PR path can be exercised.
func newDiffTestLoopWithRef(gm git.Ops, taskID, ref string) *Loop {
	backend := newMetadataBackend()
	if ref != "" {
		_ = backend.SetExternalRef(taskID, ref)
	}
	return New(Config{}, Modules{Git: gm, Logger: logging.New(nil), TaskBackend: backend})
}

// The local branch diff is the primary, authoritative source: even when a PR
// exists (external-ref set, PR diff available), the branch diff wins because
// the loop just produced these commits locally. The PR is never consulted while
// the local branch is ahead of the base.
func TestFetchVerifyDiff_PrefersBranchOverPR(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		DiffFromBaseResult: "branch-diff",
		PRDiffForRefResult: "pr-diff",
		DiffFullResult:     "iteration-diff",
	})
	l := newDiffTestLoopWithRef(gm, "task-1", "https://github.com/o/r/pull/715")

	diff, source := l.fetchVerifyDiff(context.Background(), "task-1", "headBefore", "")
	if diff != "branch-diff" || source != "branch" {
		t.Fatalf("expected (branch-diff, branch), got (%q, %q)", diff, source)
	}
}

// Resume recovery: the local branch has no commits ahead of the base (e.g. a
// freshly recreated worktree), but a prior run pushed a PR. fetchVerifyDiff
// resolves that PR from the exact external-ref — never a search — and returns
// its diff labeled "PR".
func TestFetchVerifyDiff_ResumeRecovery_UsesExternalRefPR(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		DiffFromBaseResult: "", // local branch not ahead of base
		PRDiffForRefResult: "pr-diff",
		DiffFullResult:     "",
	})
	l := newDiffTestLoopWithRef(gm, "task-1", "https://github.com/o/r/pull/715")

	diff, source := l.fetchVerifyDiff(context.Background(), "task-1", "headBefore", "")
	if diff != "pr-diff" || source != "PR" {
		t.Fatalf("expected (pr-diff, PR), got (%q, %q)", diff, source)
	}
}

// Without an external-ref there is no PR to recover, so an empty local branch
// diff must NOT silently pull some other PR's diff — it falls through to the
// iteration diff (or empty). Guards against reintroducing a search-based lookup.
func TestFetchVerifyDiff_NoExternalRef_DoesNotConsultPR(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		DiffFromBaseResult: "",
		PRDiffForRefResult: "pr-diff", // present, but must be unreachable without a ref
		DiffFullResult:     "iteration-diff",
	})
	l := newDiffTestLoop(gm) // no task backend → no external-ref

	diff, source := l.fetchVerifyDiff(context.Background(), "task-1", "headBefore", "")
	if diff != "iteration-diff" || source != "iteration" {
		t.Fatalf("expected (iteration-diff, iteration), got (%q, %q)", diff, source)
	}
}

// The false-skip regression: no PR exists yet, the work was committed in a
// prior iteration (so the iteration-local headBefore..HEAD diff is empty), but
// the branch is ahead of origin/<base>. fetchVerifyDiff must return the
// non-empty branch diff labeled "branch" — not the empty iteration diff —
// so the verifier sees the committed work instead of falsely skipping it.
func TestFetchVerifyDiff_NoPR_PriorIterationCommit_UsesBranchDiff(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		DiffFromBaseResult: "branch-diff", // commit ahead of origin/<base>
		DiffFullResult:     "",            // headBefore == HEAD: empty iteration diff
	})
	l := newDiffTestLoop(gm)

	diff, source := l.fetchVerifyDiff(context.Background(), "task-1", "same-sha", "")
	if diff != "branch-diff" || source != "branch" {
		t.Fatalf("expected (branch-diff, branch), got (%q, %q)", diff, source)
	}
}

// When no branch diff and no PR exist, fall back to the iteration-local diff.
func TestFetchVerifyDiff_FallsBackToIteration(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		DiffFromBaseResult: "",
		DiffFullResult:     "iteration-diff",
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

// When both DiffFromBase and DiffFull are non-empty, fetchVerifyDiff prefers
// DiffFromBase (three-dot, branch-complete) over the iteration-local DiffFull.
// DiffFromBase is safe against mid-iteration merges because it uses the
// merge-base, and it covers all iterations' commits.
func TestFetchVerifyDiff_PrefersBranchDiffOverIteration(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		DiffFromBaseResult: "full-branch-diff",
		DiffFullResult:     "partial-iteration-diff",
	})
	l := newDiffTestLoop(gm)

	diff, source := l.fetchVerifyDiff(context.Background(), "task-1", "headBefore", "signal-sha")
	if diff != "full-branch-diff" || source != "branch" {
		t.Fatalf("expected (full-branch-diff, branch), got (%q, %q)", diff, source)
	}
}

// Multi-iteration regression: when the final iteration commits a partial slice
// (non-empty headBefore..HEAD), the verifier must still receive the complete
// branch diff (origin/<base>...HEAD), not the iteration-local slice. Reproduces
// the tabi/tabi-36jq case where test files committed in prior iterations were
// absent from the diff fed to the verifier.
func TestFetchVerifyDiff_MultiIterationFinalCommit_PrefersBranchDiff(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		DiffFromBaseResult: "full-branch-diff",   // all iterations' work
		DiffFullResult:     "partial-slice-diff", // final iteration only
	})
	l := newDiffTestLoop(gm)

	diff, source := l.fetchVerifyDiff(context.Background(), "task-1", "headBefore", "signal-sha")
	if diff != "full-branch-diff" || source != "branch" {
		t.Fatalf("expected (full-branch-diff, branch), got (%q, %q)", diff, source)
	}
}

// When the iteration diff is empty (prior-iteration commits only,
// headBefore==HEAD this iteration), fetchVerifyDiff still surfaces the
// prior-iteration work via DiffFromBase.
func TestFetchVerifyDiff_EmptyIterationFallsThroughToBase(t *testing.T) {
	gm := git.NewStub(git.StubRepoConfig{
		DiffFromBaseResult: "prior-iteration-branch-diff",
		DiffFullResult:     "", // headBefore == HEAD: empty iteration diff
	})
	l := newDiffTestLoop(gm)

	diff, source := l.fetchVerifyDiff(context.Background(), "task-1", "same-sha", "signal-sha")
	if diff != "prior-iteration-branch-diff" || source != "branch" {
		t.Fatalf("expected (prior-iteration-branch-diff, branch), got (%q, %q)", diff, source)
	}
}
