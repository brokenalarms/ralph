package git

import (
	"context"
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
	gh := NewStubGitHubCfg(StubGitHubConfig{Available: true})
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

// MergeStack returns error when CI fails on a PR.
func TestMergeStack_CIFailureStops(t *testing.T) {
	gh := NewStubGitHubCfg(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{Number: 1, Branch: "pr1", Base: "main", State: PRStateOpen}},
		Checks: map[int][]CICheckResult{1:{{Name: "ci", State: "FAILURE", Bucket: "fail"}}},
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
	gh := NewStubGitHubCfg(StubGitHubConfig{
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
	gh := NewStubGitHubCfg(StubGitHubConfig{
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
	gh := NewStubGitHubCfg(StubGitHubConfig{
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

// MergeStack rejects dirty working tree. Uses real git via execRunner so the
// dirty-tree detection (a real `git status` call) runs against the actual repo.
func TestMergeStack_DirtyTreeRejected(t *testing.T) {
	dir := t.TempDir()
	initBareRepoIn(t, dir)
	writeFile(t, dir, "dirty.txt", "uncommitted\n")
	run(t, "git", "-C", dir, "add", "dirty.txt")

	repo := newRepoForTest(
		Config{ProjectDir: dir, Logger: discardLog{}},
		NewStubGitHubCfg(StubGitHubConfig{Available: true}),
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
