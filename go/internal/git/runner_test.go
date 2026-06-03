package git

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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

	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		nil,
		withRunner(r),
	)

	if !repo.refExists(repo.workDir, "refs/heads/main") {
		t.Error("refExists should return true when runner succeeds")
	}

	if !repo.remoteExists() {
		t.Error("remoteExists should return true when remote URL is non-empty")
	}
}

// detectDefaultBranch returns BaseBranch directly — no git calls, no fallback.
func TestManager_DetectDefaultBranch_ReturnsBaseBranch(t *testing.T) {
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: discardLog{}},
		nil,
	)
	branch := repo.detectDefaultBranch()
	if branch != "main" {
		t.Errorf("expected main, got %q", branch)
	}
}

// detectDefaultBranch with BaseBranch: "develop" returns "develop".
func TestManager_DetectDefaultBranch_ExplicitDevelop(t *testing.T) {
	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), BaseBranch: "develop", Logger: discardLog{}},
		nil,
	)
	branch := repo.detectDefaultBranch()
	if branch != "develop" {
		t.Errorf("expected develop, got %q", branch)
	}
}

// prTitle formats the PR title from taskID and taskDesc:
//   - prefixes with "[taskID] " when taskID is non-empty
//   - strips component prefixes like "ralph loop: " from taskDesc
//   - truncates to 70 chars (with "..." suffix) for long titles
//   - falls back to the worktree branch name when the result would be empty
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

worktree /home/user/project/.ralph/worktrees/ralph-20260101-01
HEAD def456
branch refs/heads/ralph/project/01-feature
`
	path := parseWorktreeForBranch(porcelain, "ralph/project/01-feature")
	if path != "/home/user/project/.ralph/worktrees/ralph-20260101-01" {
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

	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), Logger: discardLog{}},
		nil,
		withRunner(r),
	)
	out := repo.gitOutput(repo.workDir, "diff", "--stat")
	if out != "" {
		t.Errorf("expected empty on error, got %q", out)
	}
}

// Manager.gitCmdErr returns the runner's error, proving error propagation
// works through the injection layer.
func TestManager_GitCmdErr_PropagatesError(t *testing.T) {
	r := newStubRunner()
	r.On("push", "", fmt.Errorf("push rejected"))

	repo := newRepoForTest(
		Config{ProjectDir: t.TempDir(), Logger: discardLog{}},
		nil,
		withRunner(r),
	)
	err := repo.gitCmdErr(repo.workDir, "push", "-u", "origin", "feature")
	if err == nil || err.Error() != "push rejected" {
		t.Errorf("expected 'push rejected', got %v", err)
	}
}

// execRunner.Run includes git's stderr in the returned error on non-zero exit,
// so callers see the real "fatal: ..." message instead of bare "exit status N".
func TestExecRunner_Run_IncludesStderrInError(t *testing.T) {
	dir := t.TempDir()
	initCmd := exec.Command("git", "init", dir)
	if err := initCmd.Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	r := &execRunner{}
	_, err := r.Run(context.Background(), dir, "cat-file", "-t", "invalid-sha-that-does-not-exist-abc123")
	if err == nil {
		t.Fatal("expected error from cat-file on invalid sha, got nil")
	}
	if !strings.Contains(err.Error(), "fatal:") {
		t.Errorf("expected error to contain git stderr (fatal: ...), got: %v", err)
	}
}

