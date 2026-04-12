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
	gh := &StubGitHub{IsAvailable: true, AllPRs: []PRInfo{}}
	mgr := &Repo{
		projectDir: dir,
		github:     gh,
		logger:     discardLog{},
	}

	_, err := mgr.MergeStack(context.Background(), MergeStackOpts{TopPR: "999"})
	if err == nil {
		t.Fatal("expected error when no PRs found")
	}
	if !strings.Contains(err.Error(), "no open PRs") {
		t.Errorf("expected 'no open PRs' error, got: %v", err)
	}
}

// MergeStack returns error when CI fails on a PR.
func TestMergeStack_CIFailureStops(t *testing.T) {
	gh := &StubGitHub{
		IsAvailable: true,
		AllPRs: []PRInfo{
			{Number: 1, Head: "pr1", Base: "main", State: "OPEN"},
		},
		Checks: []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}},
	}
	runner := newStubRunner()
	mgr := &Repo{
		projectDir: t.TempDir(),
		baseBranch: "main",
		github:     gh,
		runner:     runner,
		logger:     discardLog{},
	}

	result, err := mgr.MergeStack(context.Background(), MergeStackOpts{TopPR: "1"})
	if err == nil {
		t.Fatal("expected error when CI fails")
	}
	if !strings.Contains(err.Error(), "CI failed") {
		t.Errorf("expected 'CI failed' error, got: %v", err)
	}
	if result.MergedCount != 0 {
		t.Errorf("expected 0 merged, got %d", result.MergedCount)
	}
	if gh.MergeCalls > 0 {
		t.Errorf("MergePR should not be called when CI fails, got %d calls", gh.MergeCalls)
	}
}

// MergeStack returns error when merge is blocked by branch protection.
func TestMergeStack_MergeBlockedStops(t *testing.T) {
	gh := &StubGitHub{
		IsAvailable: true,
		AllPRs: []PRInfo{
			{Number: 1, Head: "pr1", Base: "main", State: "OPEN"},
		},
		Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		MergeResult: MergeResult{Blocked: true, Message: "requires review"},
	}
	runner := newStubRunner()
	mgr := &Repo{
		projectDir: t.TempDir(),
		baseBranch: "main",
		github:     gh,
		runner:     runner,
		logger:     discardLog{},
	}

	_, err := mgr.MergeStack(context.Background(), MergeStackOpts{TopPR: "1"})
	if err == nil {
		t.Fatal("expected error when merge blocked")
	}
	if !strings.Contains(err.Error(), "blocked by branch protection") {
		t.Errorf("expected 'blocked' error, got: %v", err)
	}
}

// MergeStack returns error when merge has conflicts.
func TestMergeStack_MergeConflictStops(t *testing.T) {
	gh := &StubGitHub{
		IsAvailable: true,
		AllPRs: []PRInfo{
			{Number: 1, Head: "pr1", Base: "main", State: "OPEN"},
		},
		Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		MergeResult: MergeResult{Conflict: true},
	}
	runner := newStubRunner()
	mgr := &Repo{
		projectDir: t.TempDir(),
		baseBranch: "main",
		github:     gh,
		runner:     runner,
		logger:     discardLog{},
	}

	_, err := mgr.MergeStack(context.Background(), MergeStackOpts{TopPR: "1"})
	if err == nil {
		t.Fatal("expected error on merge conflict")
	}
	if !strings.Contains(err.Error(), "merge conflicts") {
		t.Errorf("expected 'merge conflicts' error, got: %v", err)
	}
}

// MergeStack succeeds for a single-PR stack with passing CI.
func TestMergeStack_SinglePRSuccess(t *testing.T) {
	gh := &StubGitHub{
		IsAvailable: true,
		AllPRs: []PRInfo{
			{Number: 42, Head: "feature", Base: "main", State: "OPEN"},
		},
		Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		MergeResult: MergeResult{Merged: true},
	}
	runner := newStubRunner()
	mgr := &Repo{
		projectDir: t.TempDir(),
		baseBranch: "main",
		github:     gh,
		runner:     runner,
		logger:     discardLog{},
	}

	result, err := mgr.MergeStack(context.Background(), MergeStackOpts{TopPR: "42"})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if result.MergedCount != 1 {
		t.Errorf("expected 1 merged, got %d", result.MergedCount)
	}
	if result.TotalPRs != 1 {
		t.Errorf("expected 1 total, got %d", result.TotalPRs)
	}
	if gh.MergeCalls != 1 {
		t.Errorf("expected 1 MergePR call, got %d", gh.MergeCalls)
	}
}

// MergeStack rejects dirty working tree.
func TestMergeStack_DirtyTreeRejected(t *testing.T) {
	dir := t.TempDir()
	initBareRepoIn(t, dir)
	writeFile(t, dir, "dirty.txt", "uncommitted\n")
	run(t, "git", "-C", dir, "add", "dirty.txt")

	mgr := &Repo{
		projectDir: dir,
		github:     &StubGitHub{IsAvailable: true},
		logger:     discardLog{},
	}

	_, err := mgr.MergeStack(context.Background(), MergeStackOpts{TopPR: "1"})
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
