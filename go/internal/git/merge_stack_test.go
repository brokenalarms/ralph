package git

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCollectStackFromPRs_BottomUpOrder(t *testing.T) {
	allPRs := []PRInfo{
		{Number: 452, Head: "feature/a", Base: "main", State: "OPEN"},
		{Number: 459, Head: "feature/b", Base: "feature/a", State: "OPEN"},
		{Number: 460, Head: "feature/c", Base: "feature/b", State: "OPEN"},
	}

	result := collectStackFromPRs(allPRs, "460")

	if len(result.prs) != 3 {
		t.Fatalf("expected 3 PRs, got %d", len(result.prs))
	}
	if result.prs[0].number != 452 {
		t.Errorf("expected prs[0]=452 (bottom), got %d", result.prs[0].number)
	}
	if result.prs[1].number != 459 {
		t.Errorf("expected prs[1]=459, got %d", result.prs[1].number)
	}
	if result.prs[2].number != 460 {
		t.Errorf("expected prs[2]=460 (top), got %d", result.prs[2].number)
	}
	if result.baseBranch != "main" {
		t.Errorf("expected baseBranch=main, got %s", result.baseBranch)
	}
}

func TestCollectStackFromPRs_SkipsClosedPRs(t *testing.T) {
	allPRs := []PRInfo{
		{Number: 452, Head: "feature/a", Base: "main", State: "OPEN"},
		{Number: 459, Head: "feature/b", Base: "feature/a", State: "CLOSED"},
		{Number: 460, Head: "feature/c", Base: "feature/b", State: "OPEN"},
	}

	result := collectStackFromPRs(allPRs, "460")

	if len(result.prs) != 2 {
		t.Fatalf("expected 2 PRs (CLOSED skipped), got %d", len(result.prs))
	}
	if result.prs[0].number != 452 {
		t.Errorf("expected prs[0]=452, got %d", result.prs[0].number)
	}
	if result.prs[1].number != 460 {
		t.Errorf("expected prs[1]=460, got %d", result.prs[1].number)
	}
}

func TestCollectStackFromPRs_NonMainBaseBranch(t *testing.T) {
	allPRs := []PRInfo{
		{Number: 100, Head: "feature/x", Base: "develop", State: "OPEN"},
	}

	result := collectStackFromPRs(allPRs, "100")

	if result.baseBranch != "develop" {
		t.Errorf("expected baseBranch=develop, got %s", result.baseBranch)
	}
	if len(result.prs) != 1 || result.prs[0].number != 100 {
		t.Errorf("unexpected prs: %+v", result.prs)
	}
}

func TestCollectStackFromPRs_InvalidPRNumber(t *testing.T) {
	allPRs := []PRInfo{
		{Number: 1, Head: "feature/a", Base: "main", State: "OPEN"},
	}

	result := collectStackFromPRs(allPRs, "#321")
	if len(result.prs) != 0 {
		t.Errorf("expected empty stack for non-numeric input, got %d PRs", len(result.prs))
	}

	result = collectStackFromPRs(allPRs, "abc")
	if len(result.prs) != 0 {
		t.Errorf("expected empty stack for non-numeric input, got %d PRs", len(result.prs))
	}
}

// MergeStack returns error when no PRs found for the given number.
func TestMergeStack_NoPRsFound(t *testing.T) {
	dir := t.TempDir()
	initBareRepoIn(t, dir)
	gh := newStubGitHub(StubGitHubConfig{Available: true})
	repo := newRepoForTest(
		Config{ProjectDir: dir, BaseBranch: "main", Logger: discardLog{}},
		gh,
	)

	_, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "999"})
	if err == nil {
		t.Fatal("expected error when no PRs found")
	}
	if !strings.Contains(err.Error(), "no open PRs") {
		t.Errorf("expected 'no open PRs' error, got: %v", err)
	}
}

// MergeStack returns error when CI fails on a PR with real test failures (job steps > 0).
func TestMergeStack_CIFailureStops(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available:    true,
		PRs:          []StubPR{{Number: 1, Branch: "pr1", Base: "main", State: PRStateOpen}},
		Checks:       map[int][]CICheckResult{1: {{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
		JobStepCount: 3, // real test failures — not an infrastructure outage
	})
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
	)

	result, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "1"})
	if err == nil {
		t.Fatal("expected error when CI fails")
	}
	if !strings.Contains(err.Error(), "CI failed") {
		t.Errorf("expected 'CI failed' error, got: %v", err)
	}
	if result.MergedCount != 0 {
		t.Errorf("expected 0 merged, got %d", result.MergedCount)
	}
}

// MergeStack returns error when merge is blocked by branch protection.
func TestMergeStack_MergeBlockedStops(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:  1,
			Branch:  "pr1",
			Base:    "main",
			State:   PRStateOpen,
			Blocked: true, // world: branch protection requires review
		}},
		Checks: map[int][]CICheckResult{1:{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}},
	})
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
	)

	_, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "1"})
	if err == nil {
		t.Fatal("expected error when merge blocked")
	}
	if !strings.Contains(err.Error(), "blocked by branch protection") {
		t.Errorf("expected 'blocked' error, got: %v", err)
	}
}

// MergeStack returns error when merge has conflicts.
func TestMergeStack_MergeConflictStops(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:     1,
			Branch:     "pr1",
			Base:       "main",
			State:      PRStateOpen,
			Conflicted: true, // world: PR has merge conflicts with base
		}},
		Checks: map[int][]CICheckResult{1:{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}},
	})
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
	)

	_, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "1"})
	if err == nil {
		t.Fatal("expected error on merge conflict")
	}
	if !strings.Contains(err.Error(), "merge conflicts") {
		t.Errorf("expected 'merge conflicts' error, got: %v", err)
	}
}

// MergeStack succeeds for a single-PR stack with passing CI.
// Observable outcome: the PR ends up merged in the fake's world (State flips
// to Merged via the interface), and the returned result reflects the count.
// The previous test also asserted gh.MergeCalls == 1 — a stub call-history
// read, dropped as an anti-pattern. The PR-is-merged assertion is stronger:
// it checks the SUT actually caused the state change, not just that it
// invoked the method.
func TestMergeStack_SinglePRSuccess(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{Number: 42, Branch: "feature", Base: "main", State: PRStateOpen}},
		Checks: map[int][]CICheckResult{42:{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}},
	})
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
	)

	result, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "42"})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if result.MergedCount != 1 {
		t.Errorf("expected 1 merged, got %d", result.MergedCount)
	}
	if result.TotalPRs != 1 {
		t.Errorf("expected 1 total, got %d", result.TotalPRs)
	}
	// Verify the PR is actually merged in the world (observable via the interface).
	pr, _ := gh.GetPR(context.Background(), "owner/repo", 42)
	if pr == nil || pr.State != PRStateMerged {
		t.Errorf("expected PR 42 to be merged in the world, got state=%v", pr)
	}
}

// MergeStack proceeds through the merge when CI failed but no job steps ran —
// the GitHub Actions billing/runner outage left every required check in the
// "never executed" state. isInfrastructureFailure classifies this as infra,
// not test failure, so the bottom-up drain continues instead of aborting the
// stack.
func TestMergeStack_InfraFailureProceeds(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available:    true,
		PRs:          []StubPR{{Number: 1, Branch: "pr1", Base: "main", State: PRStateOpen}},
		Checks:       map[int][]CICheckResult{1: {{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
		JobStepCount: 0, // zero job steps = infrastructure outage, not real failure
	})
	runner := newStubRunner().On("remote get-url origin", "https://github.com/owner/repo.git", nil)
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
	)

	result, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "1"})
	if err != nil {
		t.Fatalf("expected success on infra failure, got: %v", err)
	}
	if result.MergedCount != 1 {
		t.Errorf("expected 1 merged, got %d", result.MergedCount)
	}
	pr, _ := gh.GetPR(context.Background(), "owner/repo", 1)
	if pr == nil || pr.State != PRStateMerged {
		t.Errorf("expected PR 1 merged despite infra CI failure, got state=%v", pr)
	}
}

// MergeStack with --no-ci-wait skips AwaitCI entirely and merges the stack
// without polling for CI. Used when the operator already knows CI is down
// and wants to drain a stack via branch-protection-allowed merges.
func TestMergeStack_SkipCIWaitMergesWithoutPolling(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs:       []StubPR{{Number: 1, Branch: "pr1", Base: "main", State: PRStateOpen}},
		// No Checks entry — AwaitCI would block waiting for results, but
		// SkipCIWait means it's never called.
	})
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
	)

	result, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "1", SkipCIWait: true})
	if err != nil {
		t.Fatalf("expected success with SkipCIWait, got: %v", err)
	}
	if result.MergedCount != 1 {
		t.Errorf("expected 1 merged, got %d", result.MergedCount)
	}
	pr, _ := gh.GetPR(context.Background(), "owner/repo", 1)
	if pr == nil || pr.State != PRStateMerged {
		t.Errorf("expected PR 1 merged when SkipCIWait, got state=%v", pr)
	}
}

// MergeStack drains a 7-PR stack when every PR's CI is in the infra-outage
// state (failed checks, zero job steps executed). Mirrors the tabi 2026-04-16
// scenario where a billing failure left PRs #670–#676 with red checks but no
// runs — every PR should still merge bottom-up.
func TestMergeStack_StackInfraFailureAllMerge(t *testing.T) {
	prs := make([]StubPR, 7)
	checks := make(map[int][]CICheckResult, 7)
	prevHead := "main"
	for i := 0; i < 7; i++ {
		num := 670 + i
		head := fmt.Sprintf("ralph/stack-%d", num)
		prs[i] = StubPR{Number: num, Branch: head, Base: prevHead, State: PRStateOpen}
		checks[num] = []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}}
		prevHead = head
	}
	gh := newStubGitHub(StubGitHubConfig{
		Available:    true,
		PRs:          prs,
		Checks:       checks,
		JobStepCount: 0, // every PR's CI never ran
	})
	runner := newStubRunner().On("remote get-url origin", "https://github.com/owner/repo.git", nil)
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
	)

	result, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "676"})
	if err != nil {
		t.Fatalf("expected stack drain on infra outage, got: %v", err)
	}
	if result.MergedCount != 7 || result.TotalPRs != 7 {
		t.Errorf("expected 7/7 merged, got %d/%d", result.MergedCount, result.TotalPRs)
	}
	for i := 0; i < 7; i++ {
		num := 670 + i
		pr, _ := gh.GetPR(context.Background(), "owner/repo", num)
		if pr == nil || pr.State != PRStateMerged {
			t.Errorf("expected PR #%d merged, got state=%v", num, pr)
		}
	}
}

// MergeStack on a 7-PR stack aborts on the first PR when CI failures are real
// (job steps executed, tests failed). Counterpart to the infra-outage test —
// confirms isInfrastructureFailure correctly distinguishes real failures.
func TestMergeStack_StackRealFailureStopsAtFirst(t *testing.T) {
	prs := make([]StubPR, 7)
	checks := make(map[int][]CICheckResult, 7)
	prevHead := "main"
	for i := 0; i < 7; i++ {
		num := 670 + i
		head := fmt.Sprintf("ralph/stack-%d", num)
		prs[i] = StubPR{Number: num, Branch: head, Base: prevHead, State: PRStateOpen}
		checks[num] = []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}}
		prevHead = head
	}
	gh := newStubGitHub(StubGitHubConfig{
		Available:    true,
		PRs:          prs,
		Checks:       checks,
		JobStepCount: 5, // tests actually ran and failed
	})
	runner := newStubRunner().On("remote get-url origin", "https://github.com/owner/repo.git", nil)
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
	)

	result, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "676"})
	if err == nil {
		t.Fatal("expected error on first PR's real CI failure")
	}
	if !strings.Contains(err.Error(), "CI failed on PR #670") {
		t.Errorf("expected error to name PR #670 (bottom of stack), got: %v", err)
	}
	if result.MergedCount != 0 {
		t.Errorf("expected 0 merged, got %d", result.MergedCount)
	}
}

// MergeStack rejects dirty working tree. Uses real git via execRunner so the
// dirty-tree detection (a real `git status` call) runs against the actual repo.
func TestMergeStack_DirtyTreeRejected(t *testing.T) {
	dir := t.TempDir()
	initBareRepoIn(t, dir)
	writeFile(t, dir, "dirty.txt", "uncommitted\n")
	run(t, "git", "-C", dir, "add", "dirty.txt")

	repo := newRepoForTest(
		Config{ProjectDir: dir, Logger: discardLog{}},
		newStubGitHub(StubGitHubConfig{Available: true}),
		withRunner(&execRunner{}),
	)

	_, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "1"})
	if err == nil {
		t.Fatal("expected error for dirty working tree")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("expected 'uncommitted changes' error, got: %v", err)
	}
}

// When --admin-merge-on-ci-infra-failure is set and isInfrastructureFailure returns true
// (zero job steps), runMergeStack must pass Admin: true to MergeStackPR so that
// branch protection is bypassed and the PR merges despite the infra outage.
func TestMergeStack_AdminMergeOnCIInfraFailureProceedsWithAdmin(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:  1,
			Branch:  "pr1",
			Base:    "main",
			State:   PRStateOpen,
			Blocked: true, // branch protection requires required checks to pass
		}},
		Checks:       map[int][]CICheckResult{1: {{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
		JobStepCount: 0, // zero job steps = infra outage, not real failure
	})
	runner := newStubRunner().On("remote get-url origin", "https://github.com/owner/repo.git", nil)
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
	)

	result, err := repo.MergeStack(context.Background(), MergeStackOpts{
		TopPR:                      "1",
		AdminMergeOnCIInfraFailure: true,
	})
	if err != nil {
		t.Fatalf("expected success with admin-merge-on-ci-infra-failure, got: %v", err)
	}
	if result.MergedCount != 1 {
		t.Errorf("expected 1 merged, got %d", result.MergedCount)
	}
	pr, _ := gh.GetPR(context.Background(), "owner/repo", 1)
	if pr == nil || pr.State != PRStateMerged {
		t.Errorf("expected PR 1 merged via admin override, got state=%v", pr)
	}
}

// When isInfrastructureFailure returns true but --admin-merge-on-ci-infra-failure is NOT
// set, runMergeStack proceeds without admin, hits branch protection, and surfaces
// a specific error naming the infra-only context to guide the operator.
func TestMergeStack_AdminMergeOnCIInfraFailureNotSetReturnsBlocked(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:  1,
			Branch:  "pr1",
			Base:    "main",
			State:   PRStateOpen,
			Blocked: true, // branch protection still blocks merge
		}},
		Checks:       map[int][]CICheckResult{1: {{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
		JobStepCount: 0, // zero job steps = infra outage
	})
	runner := newStubRunner().On("remote get-url origin", "https://github.com/owner/repo.git", nil)
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
	)

	_, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "1"}) // AdminMergeOnCIInfraFailure not set
	if err == nil {
		t.Fatal("expected error when branch protection blocks merge without admin flag")
	}
	if !strings.Contains(err.Error(), "blocked by branch protection despite infra-only CI failure") {
		t.Errorf("expected infra-specific blocked error, got: %v", err)
	}
}

// When --admin-on-infra-failure is set but CI failure has non-zero job steps
// (real test failures), the flag must have no effect: runMergeStack aborts
// with the existing 'CI failed' error and never attempts admin merge.
func TestMergeStack_AdminFlagNoEffectOnRealFailure(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:  1,
			Branch:  "pr1",
			Base:    "main",
			State:   PRStateOpen,
			Blocked: true,
		}},
		Checks:       map[int][]CICheckResult{1: {{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
		JobStepCount: 3, // non-zero: real test failure, not infra
	})
	runner := newStubRunner().On("remote get-url origin", "https://github.com/owner/repo.git", nil)
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
	)

	_, err := repo.MergeStack(context.Background(), MergeStackOpts{
		TopPR:                      "1",
		AdminMergeOnCIInfraFailure: true,
	})
	if err == nil {
		t.Fatal("expected CI failure error when job steps > 0")
	}
	if !strings.Contains(err.Error(), "CI failed on PR #1") {
		t.Errorf("expected 'CI failed' error, got: %v", err)
	}
	// Verify the PR was NOT merged — admin flag must have no effect on real failures.
	pr, _ := gh.GetPR(context.Background(), "owner/repo", 1)
	if pr != nil && pr.State == PRStateMerged {
		t.Errorf("PR must not be merged when CI failure is real (non-zero job steps)")
	}
}

// MergeStack retargets each intermediate PR's base to the default branch before
// merging the parent. This prevents GitHub from auto-closing dependent PRs when
// delete_branch_on_merge=false — by the time the parent branch is deleted, the
// dependent PR already points to main.
func TestMergeStack_RetargetsNextPRBaseBeforeMerge(t *testing.T) {
	// 3-PR stack: PR#1 (base=main) → PR#2 (base=pr1) → PR#3 (base=pr2)
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{
			{Number: 1, Branch: "pr1", Base: "main", State: PRStateOpen},
			{Number: 2, Branch: "pr2", Base: "pr1", State: PRStateOpen},
			{Number: 3, Branch: "pr3", Base: "pr2", State: PRStateOpen},
		},
		Checks: map[int][]CICheckResult{
			1: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
			2: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
			3: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		},
	})
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
	)

	result, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "3"})
	if err != nil {
		t.Fatalf("expected successful stack merge, got: %v", err)
	}
	if result.MergedCount != 3 {
		t.Errorf("expected 3 merged, got %d", result.MergedCount)
	}

	// After merge, PR#2 and PR#3 must have been retargeted to main before their
	// parent was merged — observable by checking the base in the stub's world.
	pr2, _ := gh.GetPR(context.Background(), "owner/repo", 2)
	if pr2 == nil || pr2.BaseRef != "main" {
		t.Errorf("expected PR#2 base to be 'main' (retargeted before PR#1 merged), got %q", func() string {
			if pr2 == nil {
				return "<nil>"
			}
			return pr2.BaseRef
		}())
	}
	pr3, _ := gh.GetPR(context.Background(), "owner/repo", 3)
	if pr3 == nil || pr3.BaseRef != "main" {
		t.Errorf("expected PR#3 base to be 'main' (retargeted before PR#2 merged), got %q", func() string {
			if pr3 == nil {
				return "<nil>"
			}
			return pr3.BaseRef
		}())
	}
}

// When runMergeStack retargets the next PR's base to main before merging the parent
// and the parent merge then fails, the next PR's base must be rolled back to the
// parent's head branch — otherwise the next PR's diff silently includes parent changes.
func TestMergeStack_RollsBackRetargetOnMergeFailure(t *testing.T) {
	// 2-PR stack: PR#1 (base=main, blocked) → PR#2 (base=pr1)
	// PR#2 is retargeted to main before PR#1 merge is attempted.
	// PR#1 merge fails (branch protection). PR#2 must be rolled back to "pr1".
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{
			{Number: 1, Branch: "pr1", Base: "main", State: PRStateOpen, Blocked: true},
			{Number: 2, Branch: "pr2", Base: "pr1", State: PRStateOpen},
		},
		Checks: map[int][]CICheckResult{
			1: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		},
	})
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
	)

	_, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "2"})
	if err == nil {
		t.Fatal("expected error when PR#1 merge is blocked")
	}

	// PR#2 base must be restored to "pr1" (PR#1's head branch, which still exists
	// since the merge failed), not left pointing to main.
	pr2, _ := gh.GetPR(context.Background(), "owner/repo", 2)
	got := "<nil>"
	if pr2 != nil {
		got = pr2.BaseRef
	}
	if got != "pr1" {
		t.Errorf("expected PR#2 base rolled back to 'pr1', got %q", got)
	}
}

// RebaseBranchOntoRemote must not run checkout, rebase, or reset commands with
// projectDir as workdir. All working-tree mutations must happen in a temp worktree.
func TestRebaseBranchOntoRemote_DoesNotMutateProjectDir(t *testing.T) {
	projectDir := t.TempDir()
	ralphDir := t.TempDir()
	runner := newStubRunner()

	repo := newRepoForTest(
		Config{ProjectDir: projectDir, RalphDir: ralphDir, Logger: discardLog{}},
		newStubGitHub(StubGitHubConfig{}),
		withRunner(runner),
	)

	_ = repo.RebaseBranchOntoRemote(context.Background(), "pr2", "main")

	mutating := map[string]bool{"checkout": true, "rebase": true, "reset": true, "pull": true, "restore": true}
	for _, call := range runner.Called() {
		if call.Dir != projectDir || len(call.Args) == 0 {
			continue
		}
		if mutating[call.Args[0]] {
			t.Errorf("projectDir used for working-tree-mutating command %q: args=%v", call.Args[0], call.Args)
		}
	}
}

// ResetBranchToRemote must use plumbing only (fetch + update-ref) — never
// checkout or reset --hard in projectDir.
func TestResetBranchToRemote_DoesNotMutateProjectDir(t *testing.T) {
	projectDir := t.TempDir()
	runner := newStubRunner()

	repo := newRepoForTest(
		Config{ProjectDir: projectDir, Logger: discardLog{}},
		newStubGitHub(StubGitHubConfig{}),
		withRunner(runner),
	)

	repo.ResetBranchToRemote(context.Background(), "main")

	for _, call := range runner.Called() {
		if call.Dir != projectDir || len(call.Args) == 0 {
			continue
		}
		if call.Args[0] == "checkout" || call.Args[0] == "reset" {
			t.Errorf("projectDir used for working-tree-mutating command %q: args=%v", call.Args[0], call.Args)
		}
	}
}

// MergeStack must not leave projectDir's working tree dirty. After a 2-PR stack
// merge where the second branch is rebased onto the updated default branch, the
// projectDir must remain clean (git status --porcelain returns no output).
func TestMergeStack_ProjectDirRemainsCleanAfterStackMerge(t *testing.T) {
	// Set up bare remote repo.
	remoteDir := t.TempDir()
	run(t, "git", "-C", remoteDir, "init", "--bare", "-b", "main")

	// Set up local project repo with the bare remote as origin.
	projectDir := t.TempDir()
	run(t, "git", "-C", projectDir, "init", "-b", "main")
	run(t, "git", "-C", projectDir, "config", "user.email", "test@test")
	run(t, "git", "-C", projectDir, "config", "user.name", "test")
	run(t, "git", "-C", projectDir, "remote", "add", "origin", remoteDir)

	// Initial commit on main, push to remote.
	writeFile(t, projectDir, "file.txt", "initial\n")
	run(t, "git", "-C", projectDir, "commit", "-m", "init")
	run(t, "git", "-C", projectDir, "push", "-u", "origin", "main")

	// Create pr1 branch with a file change, push.
	run(t, "git", "-C", projectDir, "checkout", "-b", "pr1")
	writeFile(t, projectDir, "pr1.txt", "pr1 change\n")
	run(t, "git", "-C", projectDir, "commit", "-m", "pr1 change")
	run(t, "git", "-C", projectDir, "push", "origin", "pr1")

	// Create pr2 branch from pr1 with another file change, push.
	run(t, "git", "-C", projectDir, "checkout", "-b", "pr2")
	writeFile(t, projectDir, "pr2.txt", "pr2 change\n")
	run(t, "git", "-C", projectDir, "commit", "-m", "pr2 change")
	run(t, "git", "-C", projectDir, "push", "origin", "pr2")

	// Add a commit to main (diverges from pr1/pr2 base, forces rebase in RebaseStack).
	run(t, "git", "-C", projectDir, "checkout", "main")
	writeFile(t, projectDir, "main_update.txt", "main update\n")
	run(t, "git", "-C", projectDir, "commit", "-m", "main update")
	run(t, "git", "-C", projectDir, "push", "origin", "main")

	// projectDir is now on main, clean.
	ralphDir := t.TempDir()

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{
			{Number: 1, Branch: "pr1", Base: "main", State: PRStateOpen},
			{Number: 2, Branch: "pr2", Base: "pr1", State: PRStateOpen},
		},
	})
	repo := newRepoForTest(
		Config{ProjectDir: projectDir, RalphDir: ralphDir, BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(&execRunner{}),
	)

	// SkipCIWait so the test doesn't need CI infrastructure.
	_, _ = repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "2", SkipCIWait: true})

	// Regardless of merge outcome, projectDir must have a clean working tree.
	out, _ := defaultRunner.Run(context.Background(), projectDir, "status", "--porcelain")
	if out != "" {
		t.Errorf("projectDir has uncommitted changes after MergeStack:\n%s", out)
	}
}

// MergeStack must refuse to proceed when the collected stack's base branch is
// neither cfg.BaseBranch nor an active stack parent — prevents merging into a
// stale/wrong branch.
func TestMergeStack_RejectsUnexpectedBase(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{
			{Number: 42, Branch: "feature", Base: "some-stale-origin-branch", State: PRStateOpen},
		},
	})
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
	)

	_, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "42"})
	if err == nil {
		t.Fatal("expected error when stack base is not cfg.BaseBranch or active stack parent")
	}
	if !strings.Contains(err.Error(), "base branch guard") {
		t.Errorf("expected 'base branch guard' in error, got: %v", err)
	}
}

// runMergeStack must not merge a PR when CI times out (AwaitCI returns CIPending).
// A pending-at-timeout result must return an error and leave the PR unmerged.
// This prevents unverified PRs from landing when the CI poll deadline expires.
func TestMergeStack_CIPendingTimeoutDoesNotMerge(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs:       []StubPR{{Number: 1, Branch: "pr1", Base: "main", State: PRStateOpen}},
		// Pending check — CI has not resolved; AwaitCI will return CIPending on timeout.
		Checks: map[int][]CICheckResult{1: {{Name: "ci", State: "PENDING", Bucket: "pending"}}},
	})
	repo := newRepoForTest(
		Config{
			ProjectDir:    t.TempDir(),
			BaseBranch:    "main",
			Logger:        discardLog{},
			CIPollTimeout: 1 * time.Millisecond, // expires immediately, forcing CIPending
		},
		gh,
	)

	result, err := repo.MergeStack(context.Background(), MergeStackOpts{TopPR: "1"})
	if err == nil {
		t.Fatal("expected error when CI times out (CIPending), got nil")
	}
	if result.MergedCount != 0 {
		t.Errorf("expected 0 merged on CI timeout, got %d", result.MergedCount)
	}
	pr, _ := gh.GetPR(context.Background(), "owner/repo", 1)
	if pr != nil && pr.State == PRStateMerged {
		t.Error("PR must not be merged when CI is pending at timeout")
	}
}

// runMergeStack must abort immediately when the context is canceled during CI polling.
// A pre-canceled context must cause AwaitCI to return CIPending with "interrupted",
// and runMergeStack must return an error without merging any PR.
func TestMergeStack_CancelContextDuringCIDoesNotMerge(t *testing.T) {
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs:       []StubPR{{Number: 1, Branch: "pr1", Base: "main", State: PRStateOpen}},
		// Pending check — CI not resolved; AwaitCI sees canceled context.
		Checks: map[int][]CICheckResult{1: {{Name: "ci", State: "PENDING", Bucket: "pending"}}},
	})
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		gh,
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so CI polling sees a done context immediately

	result, err := repo.MergeStack(ctx, MergeStackOpts{TopPR: "1"})
	if err == nil {
		t.Fatal("expected error when context is canceled during CI polling, got nil")
	}
	if result.MergedCount != 0 {
		t.Errorf("expected 0 merged on canceled context, got %d", result.MergedCount)
	}
	pr, _ := gh.GetPR(context.Background(), "owner/repo", 1)
	if pr != nil && pr.State == PRStateMerged {
		t.Error("PR must not be merged when context is canceled during CI wait")
	}
}

// initBareRepoIn initializes a git repo with one commit in the given directory.
func initBareRepoIn(t *testing.T, dir string) {
	t.Helper()
	run(t, "git", "-C", dir, "init", "-b", "main")
	run(t, "git", "-C", dir, "config", "user.email", "test@test")
	run(t, "git", "-C", dir, "config", "user.name", "test")
	writeFile(t, dir, "init.txt", "init\n")
	run(t, "git", "-C", dir, "add", "init.txt")
	run(t, "git", "-C", dir, "commit", "-m", "init")
}
