package git

import (
	"context"
	"fmt"
	"strings"
	"testing"
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
	pr, _ := gh.GetPR("owner/repo", 42)
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
	pr, _ := gh.GetPR("owner/repo", 1)
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
	pr, _ := gh.GetPR("owner/repo", 1)
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
		pr, _ := gh.GetPR("owner/repo", num)
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
	pr, _ := gh.GetPR("owner/repo", 1)
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
	pr, _ := gh.GetPR("owner/repo", 1)
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
	pr2, _ := gh.GetPR("owner/repo", 2)
	if pr2 == nil || pr2.BaseRef != "main" {
		t.Errorf("expected PR#2 base to be 'main' (retargeted before PR#1 merged), got %q", func() string {
			if pr2 == nil {
				return "<nil>"
			}
			return pr2.BaseRef
		}())
	}
	pr3, _ := gh.GetPR("owner/repo", 3)
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
	pr2, _ := gh.GetPR("owner/repo", 2)
	got := "<nil>"
	if pr2 != nil {
		got = pr2.BaseRef
	}
	if got != "pr1" {
		t.Errorf("expected PR#2 base rolled back to 'pr1', got %q", got)
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
