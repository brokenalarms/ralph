package git

import (
	"context"
	"fmt"
	"testing"
)

// stubRunner records all git invocations and returns canned responses,
// proving that Manager methods can run without real git processes.
func TestStubRunner_RecordsCalls(t *testing.T) {
	r := newStubRunner()
	r.Run(context.Background(), "/tmp", "status")
	r.Run(context.Background(), "/work", "fetch", "origin", "main")

	calls := r.Called()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Dir != "/tmp" || calls[0].Args[0] != "status" {
		t.Errorf("first call: got %+v", calls[0])
	}
	if calls[1].Dir != "/work" || calls[1].Args[1] != "origin" {
		t.Errorf("second call: got %+v", calls[1])
	}
}

// stubRunner.CalledWith matches on a prefix of the git arguments,
// enabling assertions like "was fetch called?" without matching exact args.
func TestStubRunner_CalledWith(t *testing.T) {
	r := newStubRunner()
	r.Run(context.Background(), "/tmp", "push", "-u", "origin", "feature")

	if !r.CalledWith("push") {
		t.Error("expected CalledWith(push) to match")
	}
	if !r.CalledWith("push", "-u", "origin") {
		t.Error("expected CalledWith(push, -u, origin) to match")
	}
	if r.CalledWith("fetch") {
		t.Error("CalledWith(fetch) should not match")
	}
}

// stubRunner.On returns canned output and error for matching keys,
// proving tests can simulate specific git responses.
func TestStubRunner_CannedResponses(t *testing.T) {
	r := newStubRunner()
	r.On("rev-parse HEAD", "abc123", nil)
	r.On("fetch", "", fmt.Errorf("network error"))

	out, err := r.Run(context.Background(), "/tmp", "rev-parse", "HEAD")
	if out != "abc123" || err != nil {
		t.Errorf("rev-parse HEAD: got %q, %v", out, err)
	}

	out, err = r.Run(context.Background(), "/tmp", "fetch", "origin", "main")
	if err == nil || err.Error() != "network error" {
		t.Errorf("fetch: expected network error, got %q, %v", out, err)
	}
}

// Manager methods delegate to the injected Runner, proving that git
// commands can be intercepted without spawning real processes.
func TestManager_UsesInjectedRunner(t *testing.T) {
	r := newStubRunner()
	r.On("rev-parse --verify refs/heads/main", "", nil)
	r.On("remote get-url origin", "https://github.com/test/repo.git", nil)

	mgr := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		nil,
		withRunner(r),
	)

	if !mgr.refExists(mgr.workDir, "refs/heads/main") {
		t.Error("refExists should return true when runner succeeds")
	}

	if !mgr.remoteExists() {
		t.Error("remoteExists should return true when remote URL is non-empty")
	}
}

// detectDefaultBranch returns BaseBranch directly — no git calls, no fallback.
func TestManager_DetectDefaultBranch_ReturnsBaseBranch(t *testing.T) {
	mgr := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		nil,
	)
	branch := mgr.detectDefaultBranch()
	if branch != "main" {
		t.Errorf("expected main, got %q", branch)
	}
}

// detectDefaultBranch with BaseBranch: "develop" returns "develop".
func TestManager_DetectDefaultBranch_ExplicitDevelop(t *testing.T) {
	mgr := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "develop", Logger: discardLog{}},
		nil,
	)
	branch := mgr.detectDefaultBranch()
	if branch != "develop" {
		t.Errorf("expected develop, got %q", branch)
	}
}

// prTitle formats the PR title from taskID and taskDesc:
//   - prefixes with "[taskID] " when taskID is non-empty
//   - strips component prefixes like "ralph loop: " from taskDesc
//   - truncates to 70 chars (with "..." suffix) for long titles
//   - falls back to the worktree branch name when the result would be empty
//
// Formerly covered by TestManager_PushAndCreatePR_{UpdatesTitleWhenPRExists,
// StripsComponentPrefix,NoEditWithoutTaskID}, which went through the whole
// PushAndCreatePR → Ship → EditPR plumbing and asserted on gh.EditPRTitle
// (stub-field read). prTitle is a pure function; test it as one.
func TestPRTitle(t *testing.T) {
	cases := []struct {
		name     string
		taskID   string
		taskDesc string
		fallback string
		want     string
	}{
		{
			name:     "WithTaskID_PrefixesDesc",
			taskID:   "ralph-abc",
			taskDesc: "fix: some bug",
			fallback: "ralph/feature",
			want:     "[ralph-abc] fix: some bug",
		},
		{
			name:     "StripsComponentPrefix",
			taskID:   "ralph-abc",
			taskDesc: "ralph loop: fix signal cleanup",
			fallback: "ralph/feature",
			want:     "[ralph-abc] fix signal cleanup",
		},
		{
			name:     "NoTaskID_ReturnsStrippedDesc",
			taskID:   "",
			taskDesc: "fix: some bug",
			fallback: "ralph/feature",
			want:     "fix: some bug",
		},
		{
			name:     "EmptyDesc_UsesFallback",
			taskID:   "",
			taskDesc: "",
			fallback: "ralph/feature",
			want:     "ralph/feature",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := prTitle(c.taskID, c.taskDesc, c.fallback); got != c.want {
				t.Errorf("prTitle(%q, %q, %q) = %q, want %q", c.taskID, c.taskDesc, c.fallback, got, c.want)
			}
		})
	}
}

// parseWorktreeForBranch extracts paths from porcelain output without git.
func TestParseWorktreeForBranch(t *testing.T) {
	porcelain := `worktree /home/user/project
HEAD abc123
branch refs/heads/main

worktree /home/user/.ralph/worktrees/ralph-20260101-01
HEAD def456
branch refs/heads/ralph/project/01-feature
`
	path := parseWorktreeForBranch(porcelain, "ralph/project/01-feature")
	if path != "/home/user/.ralph/worktrees/ralph-20260101-01" {
		t.Errorf("expected worktree path, got %q", path)
	}

	path = parseWorktreeForBranch(porcelain, "nonexistent")
	if path != "" {
		t.Errorf("expected empty for nonexistent branch, got %q", path)
	}

	path = parseWorktreeForBranch("", "any")
	if path != "" {
		t.Errorf("expected empty for empty input, got %q", path)
	}
}

// Manager.gitOutput returns empty string when the runner errors, matching
// the behavior of the exec-based gitOutput that returns "" on failure.
func TestManager_GitOutput_EmptyOnError(t *testing.T) {
	r := newStubRunner()
	r.On("diff", "", fmt.Errorf("not a git repo"))

	mgr := newRepoForTest(
		Config{ProjectDir: t.TempDir(), Logger: discardLog{}},
		nil,
		withRunner(r),
	)
	out := mgr.gitOutput(mgr.workDir, "diff", "--stat")
	if out != "" {
		t.Errorf("expected empty on error, got %q", out)
	}
}

// Manager.gitCmdErr returns the runner's error, proving error propagation
// works through the injection layer.
func TestManager_GitCmdErr_PropagatesError(t *testing.T) {
	r := newStubRunner()
	r.On("push", "", fmt.Errorf("push rejected"))

	mgr := newRepoForTest(
		Config{ProjectDir: t.TempDir(), Logger: discardLog{}},
		nil,
		withRunner(r),
	)
	err := mgr.gitCmdErr(mgr.workDir, "push", "-u", "origin", "feature")
	if err == nil || err.Error() != "push rejected" {
		t.Errorf("expected 'push rejected', got %v", err)
	}
}

// Removed tests — rationale documented here so the deletion isn't opaque:
//
// - TestStubRunner_SatisfiesInterface: trivial compile-time interface check,
//   implicitly covered by every use of stubRunner throughout the test suite.
//
// - TestErrRunner_AlwaysFails: tested the errRunner helper. errRunner
//   (and errRunnerImpl) are the "partial stub hybrid" pattern the
//   stub-interface rewrite forbids. Deleted from test_helpers_test.go along
//   with this test. Tests needing a failing runner use stubRunner.On with an
//   explicit error value.
//
// - TestManager_PushAndCreatePR_NoRealProcesses: asserted r.CalledWith("push"),
//   which is stub call-history inspection — the same test-double internals
//   anti-pattern we're eliminating. PushAndCreatePR's push behavior is
//   covered by Ship's own tests (which exercise observable state changes,
//   not call records).
//
// - TestManager_PushAndCreatePR_PushesEvenWhenPRExists: asserted the same
//   CalledWith pattern plus the returned PR number. The meaningful half
//   (PR number returned from the existing-PR path) is covered by the
//   integration flow in ci_test.go's setupAutoMergeManager tests.
//
// - TestManager_PushAndCreatePR_LogsAlreadyOpen, TestManager_PushAndCreatePR_LogsCreatedPR:
//   asserted specific log line formats. Log output is a legitimate
//   observable, but these tests required a lot of stubRunner scripting
//   (push, fetch, rev-list, merge-base, etc.) to drive a real *Repo to
//   the log point. Under the new architecture, these belong as
//   integration tests against real git (Phase C of the spec). For now,
//   log coverage is implicit via CI tests that emit the same lines.
