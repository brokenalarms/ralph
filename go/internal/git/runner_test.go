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

	mgr := stubManager(t.TempDir(), r, nil)

	if !mgr.refExists(mgr.WorkDir, "refs/heads/main") {
		t.Error("refExists should return true when runner succeeds")
	}

	if !mgr.remoteExists() {
		t.Error("remoteExists should return true when remote URL is non-empty")
	}

	calls := r.Called()
	if len(calls) < 2 {
		t.Fatalf("expected at least 2 runner calls, got %d", len(calls))
	}
}

// Manager.detectDefaultBranch uses the Runner for the symbolic-ref lookup,
// proving it works without shelling out to git.
func TestManager_DetectDefaultBranch_UsesRunner(t *testing.T) {
	r := newStubRunner()
	r.On("symbolic-ref refs/remotes/origin/HEAD", "refs/remotes/origin/develop", nil)

	mgr := stubManager(t.TempDir(), r, nil)
	branch := mgr.detectDefaultBranch()
	if branch != "develop" {
		t.Errorf("expected develop, got %q", branch)
	}
}

// Manager.detectDefaultBranch falls back to "develop" when the runner
// returns empty output (no remote HEAD configured).
func TestManager_DetectDefaultBranch_Fallback(t *testing.T) {
	r := newStubRunner()
	mgr := stubManager(t.TempDir(), r, nil)
	branch := mgr.detectDefaultBranch()
	if branch != "develop" {
		t.Errorf("expected develop fallback, got %q", branch)
	}
}

// Manager.detectDefaultBranch returns BaseBranch when set, without
// calling the runner at all.
func TestManager_DetectDefaultBranch_Override(t *testing.T) {
	r := newStubRunner()
	mgr := stubManager(t.TempDir(), r, nil)
	mgr.BaseBranch = "main"
	branch := mgr.detectDefaultBranch()
	if branch != "main" {
		t.Errorf("expected main, got %q", branch)
	}
	if r.CalledWith("symbolic-ref") {
		t.Error("should not call symbolic-ref when BaseBranch is set")
	}
}

// stubRunner satisfies the Runner interface, proving test doubles can
// replace all git command execution without implementing exec.Command.
func TestStubRunner_SatisfiesInterface(t *testing.T) {
	var _ Runner = &stubRunner{}
	var _ Runner = newStubRunner()
}


// Manager with stubRunner: PushAndCreatePR uses the runner for git
// commands and the GitHub stub for PR operations, proving the full
// push/PR flow can be tested without real processes.
func TestManager_PushAndCreatePR_NoRealProcesses(t *testing.T) {
	r := newStubRunner()
	r.On("symbolic-ref refs/remotes/origin/HEAD", "refs/remotes/origin/main", nil)
	r.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	r.On("rev-list --count origin/main..HEAD", "3", nil)
	r.On("push", "", nil)
	r.On("fetch", "", nil)
	r.On("merge-base --is-ancestor", "", nil)

	gh := &stubGitHub{available: true, openPR: ""}

	dir := t.TempDir()
	mgr := &Manager{
		ProjectDir:     dir,
		WorkDir:        dir + "/worktree",
		WorktreeBranch: "ralph/test/01-feature",
		Runner:         r,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         discardLog{},
	}

	_, err := mgr.PushAndCreatePR(context.Background(), "test-123", "test feature", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !r.CalledWith("push") {
		t.Error("expected push to be called when no PR exists")
	}
}

// PushAndCreatePR skips push when the agent already pushed and a PR exists.
func TestManager_PushAndCreatePR_SkipsPushWhenPRExists(t *testing.T) {
	r := newStubRunner()
	r.On("symbolic-ref refs/remotes/origin/HEAD", "refs/remotes/origin/main", nil)
	r.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	r.On("rev-list --count origin/main..HEAD", "3", nil)
	r.On("fetch", "", nil)
	r.On("merge-base --is-ancestor", "", nil)

	gh := &stubGitHub{available: true, openPR: "42"}

	dir := t.TempDir()
	mgr := &Manager{
		ProjectDir:     dir,
		WorkDir:        dir + "/worktree",
		WorktreeBranch: "ralph/test/01-feature",
		Runner:         r,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         discardLog{},
	}

	_, err := mgr.PushAndCreatePR(context.Background(), "test-123", "test feature", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if r.CalledWith("push") {
		t.Error("push should be skipped when a PR already exists")
	}
}

// PushAndCreatePR updates the PR title with the bead ID when a PR already
// exists — the agent creates PRs without the bead prefix.
func TestManager_PushAndCreatePR_UpdatesTitleWhenPRExists(t *testing.T) {
	r := newStubRunner()
	r.On("symbolic-ref refs/remotes/origin/HEAD", "refs/remotes/origin/main", nil)
	r.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	r.On("rev-list --count origin/main..HEAD", "3", nil)
	r.On("fetch", "", nil)
	r.On("merge-base --is-ancestor", "", nil)

	gh := &stubGitHub{available: true, openPR: "42"}

	dir := t.TempDir()
	mgr := &Manager{
		ProjectDir:     dir,
		WorkDir:        dir + "/worktree",
		WorktreeBranch: "ralph/test/01-feature",
		Runner:         r,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         discardLog{},
	}

	_, err := mgr.PushAndCreatePR(context.Background(), "ralph-abc", "fix: some bug", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "[ralph-abc] fix: some bug"
	if gh.editPRTitle != want {
		t.Errorf("EditPR title = %q, want %q", gh.editPRTitle, want)
	}
}

// PushAndCreatePR does not call EditPR when no bead ID is available.
func TestManager_PushAndCreatePR_NoEditWithoutTaskID(t *testing.T) {
	r := newStubRunner()
	r.On("symbolic-ref refs/remotes/origin/HEAD", "refs/remotes/origin/main", nil)
	r.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	r.On("rev-list --count origin/main..HEAD", "3", nil)
	r.On("fetch", "", nil)
	r.On("merge-base --is-ancestor", "", nil)

	gh := &stubGitHub{available: true, openPR: "42"}

	dir := t.TempDir()
	mgr := &Manager{
		ProjectDir:     dir,
		WorkDir:        dir + "/worktree",
		WorktreeBranch: "ralph/test/01-feature",
		Runner:         r,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         discardLog{},
	}

	_, err := mgr.PushAndCreatePR(context.Background(), "", "fix: some bug", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gh.editPRTitle != "" {
		t.Errorf("EditPR should not be called without taskID, but got title %q", gh.editPRTitle)
	}
}

// PushAndCreatePR logs "already open" when a PR exists before push.
func TestManager_PushAndCreatePR_LogsAlreadyOpen(t *testing.T) {
	r := newStubRunner()
	r.On("symbolic-ref refs/remotes/origin/HEAD", "refs/remotes/origin/main", nil)
	r.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	r.On("rev-list --count origin/main..HEAD", "3", nil)
	r.On("fetch", "", nil)
	r.On("merge-base --is-ancestor", "", nil)

	gh := &stubGitHub{available: true, openPR: "42"}

	dir := t.TempDir()
	log := &testLog{}
	mgr := &Manager{
		ProjectDir:     dir,
		WorkDir:        dir + "/worktree",
		WorktreeBranch: "ralph/test/01-feature",
		Runner:         r,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         log,
	}

	_, err := mgr.PushAndCreatePR(context.Background(), "ralph-abc", "fix: some bug", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !log.contains("PR #42 already open") {
		t.Errorf("expected 'already open' log, got: %v", log.messages)
	}
	if log.contains("Created PR") {
		t.Error("should not log 'Created PR' when PR already exists")
	}
}

// PushAndCreatePR logs "Created PR #N" when a new PR is created.
func TestManager_PushAndCreatePR_LogsCreatedPR(t *testing.T) {
	r := newStubRunner()
	r.On("symbolic-ref refs/remotes/origin/HEAD", "refs/remotes/origin/main", nil)
	r.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	r.On("rev-list --count origin/main..HEAD", "3", nil)
	r.On("push", "", nil)
	r.On("fetch", "", nil)
	r.On("merge-base --is-ancestor", "", nil)

	gh := &stubGitHub{available: true, openPR: "", createdPR: "99"}

	dir := t.TempDir()
	log := &testLog{}
	mgr := &Manager{
		ProjectDir:     dir,
		WorkDir:        dir + "/worktree",
		WorktreeBranch: "ralph/test/01-feature",
		Runner:         r,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         log,
	}

	_, err := mgr.PushAndCreatePR(context.Background(), "ralph-abc", "fix: some bug", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !log.contains("Created PR #99") {
		t.Errorf("expected 'Created PR #99' log, got: %v", log.messages)
	}
	if log.contains("already open") {
		t.Error("should not log 'already open' when PR was just created")
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

// errRunner produces a Runner where every call fails, proving tests can
// simulate git failures without real processes.
func TestErrRunner_AlwaysFails(t *testing.T) {
	r := errRunner("git crashed")
	_, err := r.Run(context.Background(), "/tmp", "status")
	if err == nil || err.Error() != "git crashed" {
		t.Errorf("expected 'git crashed' error, got %v", err)
	}
}

// Manager.gitOutput returns empty string when the runner errors, matching
// the behavior of the exec-based gitOutput that returns "" on failure.
func TestManager_GitOutput_EmptyOnError(t *testing.T) {
	r := newStubRunner()
	r.On("diff", "", fmt.Errorf("not a git repo"))

	mgr := stubManager(t.TempDir(), r, nil)
	out := mgr.gitOutput(mgr.WorkDir, "diff", "--stat")
	if out != "" {
		t.Errorf("expected empty on error, got %q", out)
	}
}

// Manager.gitCmdErr returns the runner's error, proving error propagation
// works through the injection layer.
func TestManager_GitCmdErr_PropagatesError(t *testing.T) {
	r := newStubRunner()
	r.On("push", "", fmt.Errorf("push rejected"))

	mgr := stubManager(t.TempDir(), r, nil)
	err := mgr.gitCmdErr(mgr.WorkDir, "push", "-u", "origin", "feature")
	if err == nil || err.Error() != "push rejected" {
		t.Errorf("expected 'push rejected', got %v", err)
	}
}
