package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// PostMergeUpdateMain updates local main to match origin/main after a merge,
// but does NOT touch the worktree.
func TestPostMergeUpdateMain_AdvancesLocalMain(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		State:       st,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("completed task", "")

	// Simulate a commit landing on origin/main (as happens after merge)
	bare := filepath.Join(filepath.Dir(project), "bare.git")
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	writeFile(t, tmpClone, "merged-work.txt", "merged content\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	run(t, "git", "-C", project, "fetch", "origin", "main")
	localBefore := gitOutput(project, "rev-parse", "main")
	originMain := gitOutput(project, "rev-parse", "origin/main")
	if localBefore == originMain {
		t.Fatal("test setup: local main should not yet match origin/main")
	}

	mgr.PostMergeUpdateMain()

	localAfter := gitOutput(project, "rev-parse", "main")
	if localAfter != originMain {
		t.Errorf("local main should match origin/main: got %s, want %s", localAfter, originMain)
	}

	// Worktree branch should be unchanged
	if !strings.Contains(mgr.WorktreeBranch, "completed-task") {
		t.Errorf("worktree branch should still be the task branch, got %q", mgr.WorktreeBranch)
	}
}

// PostMergeUpdateMain must advance local main to match origin/main without
// leaving stale staged changes.
func TestPostMergeUpdateMain_AtomicResetNoStagedChanges(t *testing.T) {
	project, _ := initBareRepo(t)
	bare := filepath.Join(filepath.Dir(project), "bare.git")
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()
	log := &testLog{}

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				State:       st,
		Logger:      log,
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// Verify main is checked out in project dir (the typical worktree scenario)
	checkedOut := gitOutput(project, "symbolic-ref", "--short", "HEAD")
	if checkedOut != "main" {
		t.Fatalf("expected main checked out in project dir, got %q", checkedOut)
	}

	localMainBefore := gitOutput(project, "rev-parse", "main")

	// Simulate a commit landing on origin/main (as happens after squash-merge)
	// by pushing directly to the bare repo from a temp clone.
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	writeFile(t, tmpClone, "merged-work.txt", "merged content\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "squash-merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	// Fetch in project dir so origin/main advances
	run(t, "git", "-C", project, "fetch", "origin", "main")

	originMain := gitOutput(project, "rev-parse", "origin/main")
	if originMain == localMainBefore {
		t.Fatal("origin/main should have advanced")
	}

	// Call PostMergeUpdateMain (the method under test)
	mgr.PostMergeUpdateMain()

	// Local main should now match origin/main
	localMainAfter := gitOutput(project, "rev-parse", "main")
	if localMainAfter != originMain {
		t.Errorf("local main should match origin/main: got %s, want %s", localMainAfter, originMain)
	}

	// Main must still be checked out
	stillCheckedOut := gitOutput(project, "symbolic-ref", "--short", "HEAD")
	if stillCheckedOut != "main" {
		t.Errorf("main should still be checked out, got %q", stillCheckedOut)
	}

	// The index must have no staged changes — this is the critical assertion.
	// The old update-ref approach left the index stale, causing `git diff --cached`
	// to show staged reversions of the merged PR's files.
	diffIndex := strings.TrimSpace(gitOutput(project, "diff", "--cached", "--name-only"))
	if diffIndex != "" {
		t.Errorf("project dir should have no staged changes after postMergeUpdate, got:\n%s", diffIndex)
	}
}

// Proves the old two-step approach (update-ref + reset --hard HEAD) leaves
// stale staged changes, demonstrating why atomic reset is necessary.
func TestPostMergeUpdate_TwoStepLeavesStaleIndex(t *testing.T) {
	project, _ := initBareRepo(t)
	bare := filepath.Join(filepath.Dir(project), "bare.git")

	localMainBefore := gitOutput(project, "rev-parse", "main")

	// Push a new commit to origin/main via a temp clone.
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	writeFile(t, tmpClone, "merged-work.txt", "merged content\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "squash-merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	run(t, "git", "-C", project, "fetch", "origin", "main")

	originMain := gitOutput(project, "rev-parse", "origin/main")
	if originMain == localMainBefore {
		t.Fatal("origin/main should have advanced")
	}

	// Reproduce the old buggy two-step: update-ref advances the ref but
	// the index still reflects the old commit's tree.
	gitCmd(project, "update-ref", "refs/heads/main", originMain)
	// At this point git diff --cached will show staged reversions because
	// the index doesn't match the new HEAD.
	diffAfterUpdateRef := strings.TrimSpace(gitOutput(project, "diff", "--cached", "--name-only"))
	if diffAfterUpdateRef == "" {
		t.Skip("git version doesn't exhibit the stale-index behavior after update-ref")
	}

	// Confirm the stale index shows the merged file as a staged deletion
	if !strings.Contains(diffAfterUpdateRef, "merged-work.txt") {
		t.Errorf("expected stale index to show merged-work.txt as staged change, got:\n%s", diffAfterUpdateRef)
	}
}

// PostMergeUpdateMain logs "Updated local <branch> to latest origin" — not
// "Force-reset" or other force language — so normal operation logs are clear.
func TestPostMergeUpdateMain_LogSaysUpdatedLocalToLatest(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()
	log := &testLog{}

	mgr := &Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     log,
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// Push a commit to origin/main so PostMergeUpdateMain has work to do
	bare := filepath.Join(filepath.Dir(project), "bare.git")
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	writeFile(t, tmpClone, "new-file.txt", "content\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	mgr.PostMergeUpdateMain()

	if !log.contains("Updated local main to latest origin") {
		t.Errorf("expected log to contain 'Updated local main to latest origin', got: %v", log.messages)
	}
	for _, msg := range log.messages {
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "force") && !strings.Contains(lower, "enforce") {
			t.Errorf("log should not contain 'force' language in normal operation, got: %q", msg)
		}
	}
}

// NWOFromRemote must extract owner/repo from both SSH and HTTPS remote URLs
// so the GitHub API update-branch endpoint gets the correct repository path.
func TestNWOFromRemote_SSHAndHTTPS(t *testing.T) {
	tests := []struct {
		remote string
		want   string
	}{
		{"git@github.com:alice/my-repo.git", "alice/my-repo"},
		{"git@github.com:alice/my-repo", "alice/my-repo"},
		{"https://github.com/bob/other-repo.git", "bob/other-repo"},
		{"https://github.com/bob/other-repo", "bob/other-repo"},
		{"", ""},
	}
	for _, tt := range tests {
		got := NWOFromRemote(tt.remote)
		if got != tt.want {
			t.Errorf("NWOFromRemote(%q) = %q, want %q", tt.remote, got, tt.want)
		}
	}
}

// PushAndCreatePR must pass --base with the configured base branch to gh pr create,
// so PRs target the correct branch (e.g. develop) instead of the repo default (main).
func TestPushAndCreatePR_UsesBaseBranch(t *testing.T) {
	project, cleanup := initBareRepoWithBranch(t, "develop")
	defer cleanup()

	// Create a worktree on a feature branch (WorkDir must differ from ProjectDir).
	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test/feature", wtDir)
	run(t, "git", "-C", wtDir, "commit", "--allow-empty", "-m", "feature commit")

	// Create a fake gh script that records its arguments.
	binDir := t.TempDir()
	ghLog := filepath.Join(t.TempDir(), "gh-args.log")
	ghScript := filepath.Join(binDir, "gh")
	if err := os.WriteFile(ghScript, []byte(fmt.Sprintf(`#!/bin/sh
echo "$@" >> %s
# For pr list, output empty (no existing PR)
if [ "$2" = "list" ]; then
  echo ""
fi
`, ghLog)), 0755); err != nil {
		t.Fatalf("writing fake gh: %v", err)
	}
	// Prepend fake gh to PATH.
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+":"+origPath)

	log := &testLog{}
	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        wtDir,
		WorktreeBranch: "ralph/test/feature",
		BaseBranch:     "develop",
		Logger:         log,
	}

	_, err := mgr.PushAndCreatePR(context.Background(), "", "test task", "")
	if err != nil {
		t.Fatalf("PushAndCreatePR failed: %v (log: %v)", err, log.messages)
	}

	ghArgs, readErr := os.ReadFile(ghLog)
	if readErr != nil {
		t.Fatalf("reading gh log: %v", readErr)
	}

	lines := strings.Split(strings.TrimSpace(string(ghArgs)), "\n")
	// Find the pr create invocation.
	var createLine string
	for _, line := range lines {
		if strings.Contains(line, "pr create") {
			createLine = line
			break
		}
	}
	if createLine == "" {
		t.Fatal("expected gh pr create to be called, but it was not")
	}

	if !strings.Contains(createLine, "--base develop") {
		t.Errorf("gh pr create should include --base develop, got: %s", createLine)
	}
}

// PR titles must include the bead ID prefix so PRs are traceable back to
// their originating task.
func TestPushAndCreatePR_IncludesBeadIDInTitle(t *testing.T) {
	project, cleanup := initBareRepoWithBranch(t, "main")
	defer cleanup()

	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test/feature", wtDir)
	run(t, "git", "-C", wtDir, "commit", "--allow-empty", "-m", "feature commit")

	binDir := t.TempDir()
	ghLog := filepath.Join(t.TempDir(), "gh-args.log")
	ghScript := filepath.Join(binDir, "gh")
	if err := os.WriteFile(ghScript, []byte(fmt.Sprintf(`#!/bin/sh
echo "$@" >> %s
if [ "$2" = "list" ]; then
  echo ""
fi
`, ghLog)), 0755); err != nil {
		t.Fatalf("writing fake gh: %v", err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+":"+origPath)

	log := &testLog{}
	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        wtDir,
		WorktreeBranch: "ralph/test/feature",
		BaseBranch:     "main",
		Logger:         log,
	}

	_, err := mgr.PushAndCreatePR(context.Background(), "ralph-hm8", "fix: include bead ID in PR title", "")
	if err != nil {
		t.Fatalf("PushAndCreatePR failed: %v", err)
	}

	ghArgs, readErr := os.ReadFile(ghLog)
	if readErr != nil {
		t.Fatalf("reading gh log: %v", readErr)
	}

	lines := strings.Split(strings.TrimSpace(string(ghArgs)), "\n")
	var createLine string
	for _, line := range lines {
		if strings.Contains(line, "pr create") {
			createLine = line
			break
		}
	}
	if createLine == "" {
		t.Fatal("expected gh pr create to be called")
	}

	if !strings.Contains(createLine, "[ralph-hm8]") {
		t.Errorf("PR title should contain bead ID prefix [ralph-hm8], got: %s", createLine)
	}
}

// When no bead ID is provided, the PR title should be the task
// description without any bracketed prefix.
func TestPushAndCreatePR_NoBeadID(t *testing.T) {
	project, cleanup := initBareRepoWithBranch(t, "main")
	defer cleanup()

	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test/feature", wtDir)
	run(t, "git", "-C", wtDir, "commit", "--allow-empty", "-m", "feature commit")

	binDir := t.TempDir()
	ghLog := filepath.Join(t.TempDir(), "gh-args.log")
	ghScript := filepath.Join(binDir, "gh")
	if err := os.WriteFile(ghScript, []byte(fmt.Sprintf(`#!/bin/sh
echo "$@" >> %s
if [ "$2" = "list" ]; then
  echo ""
fi
`, ghLog)), 0755); err != nil {
		t.Fatalf("writing fake gh: %v", err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+":"+origPath)

	log := &testLog{}
	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        wtDir,
		WorktreeBranch: "ralph/test/feature",
		BaseBranch:     "main",
		Logger:         log,
	}

	_, err := mgr.PushAndCreatePR(context.Background(), "", "add new feature", "")
	if err != nil {
		t.Fatalf("PushAndCreatePR failed: %v", err)
	}

	ghArgs, readErr := os.ReadFile(ghLog)
	if readErr != nil {
		t.Fatalf("reading gh log: %v", readErr)
	}

	lines := strings.Split(strings.TrimSpace(string(ghArgs)), "\n")
	var createLine string
	for _, line := range lines {
		if strings.Contains(line, "pr create") {
			createLine = line
			break
		}
	}
	if createLine == "" {
		t.Fatal("expected gh pr create to be called")
	}

	if strings.Contains(createLine, "[") {
		t.Errorf("PR title should not contain brackets when no bead ID, got: %s", createLine)
	}
}

// PushAndCreatePR passes the body parameter through to CreatePR, so the
// PR description uses bead context instead of generic boilerplate.
func TestPushAndCreatePR_PassesBodyToCreatePR(t *testing.T) {
	r := newStubRunner()
	r.On("symbolic-ref refs/remotes/origin/HEAD", "refs/remotes/origin/main", nil)
	r.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	r.On("rev-list --count origin/main..HEAD", "3", nil)
	r.On("push", "", nil)
	r.On("fetch", "", nil)
	r.On("merge-base --is-ancestor", "", nil)

	var capturedOpts CreatePROpts
	gh := &capturingGitHub{
		StubGitHub: StubGitHub{IsAvailable: true, CreatedPR: "55"},
		createPR: func(opts CreatePROpts) error {
			capturedOpts = opts
			return nil
		},
	}

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

	body := "## Description\nFix auth middleware\n\n## Summary\nDone"
	_, err := mgr.PushAndCreatePR(context.Background(), "ralph-abc", "fix auth", body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedOpts.Body != body {
		t.Errorf("CreatePR body = %q, want %q", capturedOpts.Body, body)
	}
	if !strings.HasPrefix(capturedOpts.Title, "[ralph-abc]") {
		t.Errorf("title should start with [ralph-abc], got %q", capturedOpts.Title)
	}
}

// PushAndCreatePR uses the task description as body when no explicit body
// is provided, avoiding completely empty PR descriptions.
func TestPushAndCreatePR_FallsBackToTaskDescWhenNoBody(t *testing.T) {
	r := newStubRunner()
	r.On("symbolic-ref refs/remotes/origin/HEAD", "refs/remotes/origin/main", nil)
	r.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	r.On("rev-list --count origin/main..HEAD", "3", nil)
	r.On("push", "", nil)
	r.On("fetch", "", nil)
	r.On("merge-base --is-ancestor", "", nil)

	var capturedOpts CreatePROpts
	gh := &capturingGitHub{
		StubGitHub: StubGitHub{IsAvailable: true, CreatedPR: "55"},
		createPR: func(opts CreatePROpts) error {
			capturedOpts = opts
			return nil
		},
	}

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

	_, err := mgr.PushAndCreatePR(context.Background(), "ralph-abc", "fix auth middleware", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedOpts.Body != "fix auth middleware" {
		t.Errorf("CreatePR body should fall back to task desc, got %q", capturedOpts.Body)
	}
}

// AutoMergeCurrentBranch returns nil when no worktree branch is set,
// so --auto-merge is a safe no-op without worktree isolation.
func TestAutoMergeCurrentBranch_SkipsWhenNoWorktreeBranch(t *testing.T) {
	mgr := &Manager{
		WorkDir:    "/some/dir",
		ProjectDir: "/some/dir",
		Logger:     &testLog{},
	}
	merged, err := mgr.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if merged {
		t.Error("expected merged=false when no worktree branch")
	}
}

// AutoMergeCurrentBranch returns nil when WorkDir equals ProjectDir,
// avoiding merging from the project dir itself.
func TestAutoMergeCurrentBranch_SkipsWhenWorkDirIsProjectDir(t *testing.T) {
	mgr := &Manager{
		WorktreeBranch: "ralph/project/01-some-task",
		WorkDir:        "/some/dir",
		ProjectDir:     "/some/dir",
		Logger:         &testLog{},
	}
	merged, err := mgr.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if merged {
		t.Error("expected merged=false when WorkDir == ProjectDir")
	}
}

// AutoMergeCurrentBranch returns 0 and logs "No open PR found" when no PR
// exists for the branch, so an unpushed branch doesn't cause a failure.
func TestAutoMergeCurrentBranch_SkipsWhenNoPR(t *testing.T) {
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh CLI not available")
	}

	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("unpushed task", "")

	merged, err := mgr.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Errorf("expected nil error (skip), got %v", err)
	}
	if merged {
		t.Error("expected merged=false when no PR exists")
	}

	log := mgr.Logger.(*testLog)
	if !log.contains("No open PR found") {
		t.Error("expected 'No open PR found' log message")
	}
}

// mergeOpts always sets DeleteBranch=true since each task gets its own branch.
func TestMergeOpts_AlwaysDeletesBranch(t *testing.T) {
	mgr := &Manager{}
	opts := mgr.mergeOpts()
	if !opts.DeleteBranch {
		t.Fatal("mergeOpts should always set DeleteBranch")
	}
}

// ResolveConflict rebases onto the default branch and force-pushes, so
// the PR branch is updated and the merge can be retried.
func TestResolveConflict_RebasesAndForcePushes(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				State:       st,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("conflict task", "")

	// Create a commit in the worktree so we have something to push
	writeFile(t, mgr.WorkDir, "feature.txt", "feature content\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "add feature")

	// Push the branch so force-push has a remote tracking branch
	run(t, "git", "-C", mgr.WorkDir, "push", "-u", "origin", mgr.WorktreeBranch)

	// Add a commit to origin/main to make rebase non-trivial
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	bare := filepath.Join(filepath.Dir(project), "bare.git")
	run(t, "git", "clone", bare, tmpClone)
	run(t, "git", "-C", tmpClone, "config", "user.name", "test")
	run(t, "git", "-C", tmpClone, "config", "user.email", "test@test")
	writeFile(t, tmpClone, "other.txt", "other content\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "divergent commit")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	err := mgr.ResolveConflict(context.Background())
	if err != nil {
		t.Fatalf("ResolveConflict: %v", err)
	}

	// Verify we rebased onto the latest main (divergent commit should be ancestor)
	divergentRev := gitOutput(tmpClone, "rev-parse", "HEAD")
	if gitCmdErr(mgr.WorkDir, "merge-base", "--is-ancestor", divergentRev, "HEAD") != nil {
		t.Error("expected worktree HEAD to be based on the divergent commit after rebase")
	}
}

// MergeWithRetry recovers from a merge conflict by rebasing and force-pushing,
// then retrying the merge successfully.
func TestMergeWithRetry_RecoversFromConflict(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/test/01-merge-retry",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	mergeCalls := 0
	gh := &sequentialMergeGitHub{
		StubGitHub: StubGitHub{
			IsAvailable: true,
			OpenPR:      "42",
			Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		},
		mergeResults: []mergeResult{
			{output: "merge conflict", err: fmt.Errorf("merge conflict")},
			{output: "merged", err: nil},
		},
		onMerge: func() { mergeCalls++ },
	}
	mgr.GitHub = gh

	runner := newStubRunner()
	// Stub git operations that ResolveConflict + AutoMerge use.
	// merge-base --is-ancestor succeeds → EnsureUpToDate is a no-op,
	// ResolveConflict sees the branch as resolved and force-pushes.
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rebase", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("push --force-with-lease", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	mgr.Runner = runner

	merged, err := mgr.MergeWithRetry(context.Background(), MergeRetryOpts{})
	if err != nil {
		t.Fatalf("MergeWithRetry: %v", err)
	}
	if !merged {
		t.Error("expected merge to succeed after conflict resolution")
	}
	if mergeCalls < 2 {
		t.Errorf("expected at least 2 merge attempts, got %d", mergeCalls)
	}
}

// MergeWithRetry delegates CI failures to the OnCIFailure callback and
// retries the merge when the callback reports success.
func TestMergeWithRetry_DelegatesCIFailure(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/test/01-ci-retry",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	gh := &sequentialMergeGitHub{
		StubGitHub: StubGitHub{
			IsAvailable: true,
			OpenPR:      "55",
			Checks:      []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}},
		},
		// First AutoMerge returns CIFailureError before reaching MergePR.
		// After CI fix, the second AutoMerge reaches MergePR with CI passing.
		mergeResults: []mergeResult{
			{output: "merged", err: nil},
		},
	}
	mgr.GitHub = gh

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil) // already up to date
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	mgr.Runner = runner

	ciFixCalled := false
	merged, err := mgr.MergeWithRetry(context.Background(), MergeRetryOpts{
		OnCIFailure: func(ciErr *CIFailureError) bool {
			ciFixCalled = true
			return true
		},
	})
	if err != nil {
		t.Fatalf("MergeWithRetry: %v", err)
	}
	if !merged {
		t.Error("expected merge to succeed after CI fix")
	}
	if !ciFixCalled {
		t.Error("expected OnCIFailure callback to be called")
	}
}

// MergeWithRetry gives up after MaxMergeAttempts, preventing infinite loops.
func TestMergeWithRetry_ExhaustsRetries(t *testing.T) {
	project, _ := initBareRepo(t)

	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		WorktreeBranch: "ralph/test/01-exhaust",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      "99",
		Checks:      []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}},
		MergeErr:    fmt.Errorf("CI failed"),
	}
	mgr.GitHub = gh

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	mgr.Runner = runner

	ciFixCalls := 0
	_, err := mgr.MergeWithRetry(context.Background(), MergeRetryOpts{
		OnCIFailure: func(ciErr *CIFailureError) bool {
			ciFixCalls++
			return true
		},
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if ciFixCalls != MaxMergeAttempts {
		t.Errorf("expected %d CI fix attempts, got %d", MaxMergeAttempts, ciFixCalls)
	}
}

// MergeWithRetry stops immediately when ResolveConflict returns an
// UnresolvedConflictError instead of retrying force-pushes that won't help.
func TestMergeWithRetry_StopsOnUnresolvableConflict(t *testing.T) {
	project, _ := initBareRepo(t)

	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		WorktreeBranch: "ralph/test/01-unresolvable",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	mergeCalls := 0
	gh := &sequentialMergeGitHub{
		StubGitHub: StubGitHub{
			IsAvailable: true,
			OpenPR:      "50",
			Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		},
		mergeResults: []mergeResult{
			{output: "merge conflict", err: fmt.Errorf("merge conflict")},
			{output: "merge conflict", err: fmt.Errorf("merge conflict")},
			{output: "merge conflict", err: fmt.Errorf("merge conflict")},
			{output: "merge conflict", err: fmt.Errorf("merge conflict")},
		},
		onMerge: func() { mergeCalls++ },
	}
	mgr.GitHub = gh

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	// merge-base --is-ancestor returns error → still diverged after rebase
	runner.On("merge-base --is-ancestor", "", fmt.Errorf("not ancestor"))
	runner.On("rebase", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse", "abc123", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("log -1 --format=%s", "commit msg", nil)
	runner.On("reset --soft", "", nil)
	runner.On("commit", "", nil)
	runner.On("push", "", nil)
	mgr.Runner = runner

	_, err := mgr.MergeWithRetry(context.Background(), MergeRetryOpts{})
	if err == nil {
		t.Fatal("expected error for unresolvable conflict")
	}

	var unresolved *UnresolvedConflictError
	if !errors.As(err, &unresolved) {
		t.Fatalf("expected UnresolvedConflictError, got %T: %v", err, err)
	}

	// Only 1 merge attempt — should not retry after unresolvable conflict
	if mergeCalls != 1 {
		t.Errorf("expected exactly 1 merge attempt, got %d — retried pointlessly", mergeCalls)
	}
}

// AutoMergeCurrentBranch passes --subject with the PR title and number so the
// squash-merge commit message on main matches the PR title, not the single
// commit message GitHub would otherwise use.
func TestAutoMergeCurrentBranch_PassesPRTitleAsSubject(t *testing.T) {
	project, _ := initBareRepo(t)

	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		WorktreeBranch: "ralph/test/01-subject",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      "77",
		PRTitle:     "[ralph-31w] Fix squash-merge subject",
		Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
	}
	mgr.GitHub = gh

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("reset --hard", "", nil)
	mgr.Runner = runner

	merged, err := mgr.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("AutoMergeCurrentBranch: %v", err)
	}
	if !merged {
		t.Fatal("expected merge to succeed")
	}

	want := "[ralph-31w] Fix squash-merge subject (#77)"
	if gh.LastMergeOpts.Subject != want {
		t.Errorf("merge subject = %q, want %q", gh.LastMergeOpts.Subject, want)
	}
}

// sequentialMergeGitHub returns different merge results on successive calls,
// allowing tests to simulate conflict→success or CI-fail→success sequences.
type sequentialMergeGitHub struct {
	StubGitHub
	mergeResults []mergeResult
	mergeIdx     int
	checkCalls   int
	onMerge      func()
}

type mergeResult struct {
	output string
	err    error
}

func (s *sequentialMergeGitHub) MergePR(prNumber, repoURL string, opts MergeOpts) (string, error) {
	if s.onMerge != nil {
		s.onMerge()
	}
	if s.mergeIdx < len(s.mergeResults) {
		r := s.mergeResults[s.mergeIdx]
		s.mergeIdx++
		return r.output, r.err
	}
	return "merged", nil
}

func (s *sequentialMergeGitHub) ListChecks(prNumber, repoURL string) ([]CICheckResult, error) {
	s.checkCalls++
	// After the first check call, CI passes (simulating a fix was applied)
	if s.checkCalls > 1 && len(s.StubGitHub.Checks) > 0 && s.StubGitHub.Checks[0].State == "FAILURE" {
		return []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}, nil
	}
	return s.StubGitHub.Checks, s.StubGitHub.ChecksErr
}

// After a fix agent commits and force-pushes, MergeWithRetry must call
// AwaitCI to wait for fresh CI results on the new HEAD before retrying the
// merge. This prevents the retry from seeing the old (stale) failure status.
func TestMergeWithRetry_PushesFixAgentWorkBeforeRetry(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/test/01-ci-push-retry",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	// Track the order of operations to verify AwaitCI happens between
	// OnCIFailure and the merge retry.
	var events []string

	gh := &ciRetryGitHub{
		StubGitHub: StubGitHub{
			IsAvailable: true,
			OpenPR:      "60",
		},
		events: &events,
	}
	mgr.GitHub = gh

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	mgr.Runner = runner

	ciFixCalled := false
	merged, err := mgr.MergeWithRetry(context.Background(), MergeRetryOpts{
		OnCIFailure: func(ciErr *CIFailureError) bool {
			ciFixCalled = true
			events = append(events, "fix-applied")
			return true
		},
	})
	if err != nil {
		t.Fatalf("MergeWithRetry: %v", err)
	}
	if !merged {
		t.Error("expected merge to succeed after CI fix")
	}
	if !ciFixCalled {
		t.Error("expected OnCIFailure callback to be called")
	}

	// Verify AwaitCI was called between the fix and the merge retry.
	fixIdx, awaitIdx, mergeRetryIdx := -1, -1, -1
	for i, e := range events {
		switch e {
		case "fix-applied":
			fixIdx = i
		case "await-ci":
			if fixIdx >= 0 && awaitIdx < 0 {
				awaitIdx = i
			}
		case "merge-retry":
			if awaitIdx >= 0 && mergeRetryIdx < 0 {
				mergeRetryIdx = i
			}
		}
	}
	if fixIdx < 0 {
		t.Fatal("fix-applied event not recorded")
	}
	if awaitIdx < 0 {
		t.Fatal("AwaitCI not called after fix — new CI status would be stale")
	}
	if mergeRetryIdx < 0 {
		t.Fatal("merge retry not recorded after AwaitCI")
	}
	if !(fixIdx < awaitIdx && awaitIdx < mergeRetryIdx) {
		t.Errorf("wrong order: fix=%d, await=%d, merge=%d — expected fix < await < merge", fixIdx, awaitIdx, mergeRetryIdx)
	}
}

// ciRetryGitHub tracks the order of ListChecks and MergePR calls to verify
// that AwaitCI runs between the CI fix callback and the merge retry.
type ciRetryGitHub struct {
	StubGitHub
	events     *[]string
	checkCalls int
	mergeCalls int
}

func (c *ciRetryGitHub) ListChecks(prNumber, repoURL string) ([]CICheckResult, error) {
	c.checkCalls++
	// First call: CI fails (triggers OnCIFailure).
	// Subsequent calls: CI passes (after fix + force-push).
	if c.checkCalls == 1 {
		return []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}}, nil
	}
	*c.events = append(*c.events, "await-ci")
	return []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}, nil
}

func (c *ciRetryGitHub) MergePR(prNumber, repoURL string, opts MergeOpts) (string, error) {
	c.mergeCalls++
	*c.events = append(*c.events, "merge-retry")
	return "merged", nil
}

// When automatic rebase fails to resolve conflicts, MergeWithRetry calls
// the OnConflict callback to spawn a conflict resolution agent. If the
// agent resolves the conflict, the merge is retried and succeeds.
func TestMergeWithRetry_SpawnsConflictAgent(t *testing.T) {
	project, _ := initBareRepo(t)

	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		WorktreeBranch: "ralph/test/01-conflict-agent",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	mergeCalls := 0
	gh := &sequentialMergeGitHub{
		StubGitHub: StubGitHub{
			IsAvailable: true,
			OpenPR:      "70",
			Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		},
		mergeResults: []mergeResult{
			// First attempt: conflict
			{output: "merge conflict", err: fmt.Errorf("merge conflict")},
			// Second attempt (after agent fix): success
		},
		onMerge: func() { mergeCalls++ },
	}
	mgr.GitHub = gh

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	// merge-base --is-ancestor fails → unresolvable conflict
	runner.On("merge-base --is-ancestor", "", fmt.Errorf("not ancestor"))
	runner.On("rebase", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	mgr.Runner = runner

	conflictAgentCalled := false
	merged, err := mgr.MergeWithRetry(context.Background(), MergeRetryOpts{
		OnConflict: func(conflictErr *UnresolvedConflictError) bool {
			conflictAgentCalled = true
			if conflictErr.PRNumber != "70" {
				t.Errorf("expected PRNumber=70, got %s", conflictErr.PRNumber)
			}
			return true
		},
	})
	if err != nil {
		t.Fatalf("MergeWithRetry: %v", err)
	}
	if !merged {
		t.Error("expected merge to succeed after conflict agent resolved")
	}
	if !conflictAgentCalled {
		t.Error("expected OnConflict callback to be called")
	}
	if mergeCalls != 2 {
		t.Errorf("expected 2 merge attempts (conflict + retry), got %d", mergeCalls)
	}
}

// When the conflict resolution agent cannot resolve, MergeWithRetry returns
// UnresolvedConflictError without further retries.
func TestMergeWithRetry_SkipsAfterConflictAgentFails(t *testing.T) {
	project, _ := initBareRepo(t)

	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		WorktreeBranch: "ralph/test/01-conflict-agent-fail",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	mergeCalls := 0
	gh := &sequentialMergeGitHub{
		StubGitHub: StubGitHub{
			IsAvailable: true,
			OpenPR:      "71",
			Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		},
		mergeResults: []mergeResult{
			{output: "merge conflict", err: fmt.Errorf("merge conflict")},
			{output: "merge conflict", err: fmt.Errorf("merge conflict")},
		},
		onMerge: func() { mergeCalls++ },
	}
	mgr.GitHub = gh

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", fmt.Errorf("not ancestor"))
	runner.On("rebase", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	mgr.Runner = runner

	conflictAgentCalled := false
	_, err := mgr.MergeWithRetry(context.Background(), MergeRetryOpts{
		OnConflict: func(conflictErr *UnresolvedConflictError) bool {
			conflictAgentCalled = true
			return false // agent could not resolve
		},
	})

	if !conflictAgentCalled {
		t.Error("expected OnConflict callback to be called")
	}

	var unresolved *UnresolvedConflictError
	if !errors.As(err, &unresolved) {
		t.Fatalf("expected UnresolvedConflictError, got %T: %v", err, err)
	}

	// Only 1 merge attempt — callback returned false, no retry
	if mergeCalls != 1 {
		t.Errorf("expected 1 merge attempt, got %d", mergeCalls)
	}
}

// Push squashes multiple commits into one and force-pushes. Verifies the
// remote branch has exactly 1 commit after push, with identical tree content.
func TestPush_SquashesMultipleCommits(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test-squash", wtDir)

	// Create 3 commits on the worktree branch.
	for i := 0; i < 3; i++ {
		writeFile(t, wtDir, fmt.Sprintf("file%d.txt", i), fmt.Sprintf("content %d\n", i))
		run(t, "git", "-C", wtDir, "add", "-A")
		run(t, "git", "-C", wtDir, "commit", "-m", fmt.Sprintf("commit %d", i))
	}

	// Verify 3 commits ahead before push.
	countBefore := strings.TrimSpace(cmdOutput(t, "git", "-C", wtDir, "rev-list", "--count", "origin/main..HEAD"))
	if countBefore != "3" {
		t.Fatalf("expected 3 commits before push, got %s", countBefore)
	}

	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        wtDir,
		WorktreeBranch: "ralph/test-squash",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	if err := mgr.Push(context.Background()); err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	// Verify 1 commit on remote.
	countAfter := strings.TrimSpace(cmdOutput(t, "git", "-C", bare, "rev-list", "--count", "main..ralph/test-squash"))
	if countAfter != "1" {
		t.Errorf("expected 1 commit on remote after squash-push, got %s", countAfter)
	}

	// Verify all 3 files exist (tree content preserved).
	for i := 0; i < 3; i++ {
		file := fmt.Sprintf("file%d.txt", i)
		out := strings.TrimSpace(cmdOutput(t, "git", "-C", bare, "show", "ralph/test-squash:"+file))
		want := fmt.Sprintf("content %d", i)
		if out != want {
			t.Errorf("file%d.txt = %q, want %q", i, out, want)
		}
	}
}

// Push with a single commit is a no-op for squash — just pushes the commit.
func TestPush_SingleCommitNoOp(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test-single", wtDir)

	writeFile(t, wtDir, "only.txt", "content\n")
	run(t, "git", "-C", wtDir, "add", "-A")
	run(t, "git", "-C", wtDir, "commit", "-m", "single commit")

	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        wtDir,
		WorktreeBranch: "ralph/test-single",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	if err := mgr.Push(context.Background()); err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	count := strings.TrimSpace(cmdOutput(t, "git", "-C", bare, "rev-list", "--count", "main..ralph/test-single"))
	if count != "1" {
		t.Errorf("expected 1 commit on remote, got %s", count)
	}
}

// Push after fix agent: existing remote with 1 commit, add 2 more locally,
// push squashes back to 1.
func TestPush_AfterFixAgent(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test-fix", wtDir)

	// First commit + push (initial PR).
	writeFile(t, wtDir, "main.go", "package main\n")
	run(t, "git", "-C", wtDir, "add", "-A")
	run(t, "git", "-C", wtDir, "commit", "-m", "initial work")
	run(t, "git", "-C", wtDir, "push", "-u", "origin", "ralph/test-fix")

	// Fix agent adds 2 more commits locally.
	writeFile(t, wtDir, "fix1.go", "package fix1\n")
	run(t, "git", "-C", wtDir, "add", "-A")
	run(t, "git", "-C", wtDir, "commit", "-m", "fix attempt 1")
	writeFile(t, wtDir, "fix2.go", "package fix2\n")
	run(t, "git", "-C", wtDir, "add", "-A")
	run(t, "git", "-C", wtDir, "commit", "-m", "fix attempt 2")

	// 3 commits ahead of main locally.
	countLocal := strings.TrimSpace(cmdOutput(t, "git", "-C", wtDir, "rev-list", "--count", "origin/main..HEAD"))
	if countLocal != "3" {
		t.Fatalf("expected 3 local commits, got %s", countLocal)
	}

	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        wtDir,
		WorktreeBranch: "ralph/test-fix",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	if err := mgr.Push(context.Background()); err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	// Remote should have 1 commit with all 3 files.
	count := strings.TrimSpace(cmdOutput(t, "git", "-C", bare, "rev-list", "--count", "main..ralph/test-fix"))
	if count != "1" {
		t.Errorf("expected 1 commit on remote after fix-agent squash, got %s", count)
	}
	for _, file := range []string{"main.go", "fix1.go", "fix2.go"} {
		if cmdOutput(t, "git", "-C", bare, "show", "ralph/test-fix:"+file) == "" {
			t.Errorf("expected %s on remote", file)
		}
	}
}

// Push in a stacked branch: child branch (bar) stacks on parent (foo).
// Squash must only collapse bar's commits, not foo's. The result: foo has
// 1 commit ahead of main, bar has 1 commit ahead of foo, and GitHub sees
// a diff between them (PR creation would succeed).
func TestPush_StackedBranch_PreservesParent(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)

	// Create parent branch (foo) with 1 commit, push it.
	fooDir := filepath.Join(t.TempDir(), "foo-wt")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/foo", fooDir)
	writeFile(t, fooDir, "foo.go", "package foo\n")
	run(t, "git", "-C", fooDir, "add", "-A")
	run(t, "git", "-C", fooDir, "commit", "-m", "foo work")

	fooMgr := &Manager{
		ProjectDir:     project,
		WorkDir:        fooDir,
		WorktreeBranch: "ralph/foo",
		State:          newMemState(),
		Logger:         &testLog{},
	}
	if err := fooMgr.Push(context.Background()); err != nil {
		t.Fatalf("foo Push failed: %v", err)
	}

	// Create child branch (bar) from foo, add 2 commits.
	barDir := filepath.Join(t.TempDir(), "bar-wt")
	run(t, "git", "-C", project, "fetch", "origin", "ralph/foo")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/bar", barDir, "origin/ralph/foo")
	writeFile(t, barDir, "bar1.go", "package bar1\n")
	run(t, "git", "-C", barDir, "add", "-A")
	run(t, "git", "-C", barDir, "commit", "-m", "bar work 1")
	writeFile(t, barDir, "bar2.go", "package bar2\n")
	run(t, "git", "-C", barDir, "add", "-A")
	run(t, "git", "-C", barDir, "commit", "-m", "bar work 2")

	barMgr := &Manager{
		ProjectDir:     project,
		WorkDir:        barDir,
		WorktreeBranch: "ralph/bar",
		PrevBranch:     "ralph/foo",
		State:          newMemState(),
		Logger:         &testLog{},
	}
	if err := barMgr.Push(context.Background()); err != nil {
		t.Fatalf("bar Push failed: %v", err)
	}

	// foo should have exactly 1 commit ahead of main.
	fooCount := strings.TrimSpace(cmdOutput(t, "git", "-C", bare, "rev-list", "--count", "main..ralph/foo"))
	if fooCount != "1" {
		t.Errorf("foo: expected 1 commit ahead of main, got %s", fooCount)
	}

	// bar should have exactly 1 commit ahead of foo.
	barAheadOfFoo := strings.TrimSpace(cmdOutput(t, "git", "-C", bare, "rev-list", "--count", "ralph/foo..ralph/bar"))
	if barAheadOfFoo != "1" {
		t.Errorf("bar: expected 1 commit ahead of foo, got %s", barAheadOfFoo)
	}

	// bar should have exactly 2 commits ahead of main (foo's 1 + bar's 1).
	barAheadOfMain := strings.TrimSpace(cmdOutput(t, "git", "-C", bare, "rev-list", "--count", "main..ralph/bar"))
	if barAheadOfMain != "2" {
		t.Errorf("bar: expected 2 commits ahead of main, got %s", barAheadOfMain)
	}

	// bar's files should include both foo's and bar's work.
	for _, file := range []string{"foo.go", "bar1.go", "bar2.go"} {
		if cmdOutput(t, "git", "-C", bare, "show", "ralph/bar:"+file) == "" {
			t.Errorf("expected %s on remote bar branch", file)
		}
	}
}

// Push calls the PrePush callback before pushing, so a compile check
// failure aborts the push rather than sending broken code to the remote.
func TestPush_PrePushBlocksPush(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test-prepush", wtDir)

	writeFile(t, wtDir, "feature.go", "package main\n")
	run(t, "git", "-C", wtDir, "add", "-A")
	run(t, "git", "-C", wtDir, "commit", "-m", "add feature")

	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        wtDir,
		WorktreeBranch: "ralph/test-prepush",
		State:          newMemState(),
		Logger:         &testLog{},
		PrePush: func(ctx context.Context) error {
			return fmt.Errorf("compile check failed: missing interface method")
		},
	}

	err := mgr.Push(context.Background())
	if err == nil {
		t.Fatal("expected Push to fail when PrePush returns error")
	}
	if !strings.Contains(err.Error(), "pre-push check failed") {
		t.Errorf("expected pre-push check error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "missing interface method") {
		t.Errorf("expected original error in chain, got: %v", err)
	}

	// Verify nothing was pushed to remote.
	out := cmdOutput(t, "git", "-C", bare, "branch", "--list", "ralph/test-prepush")
	if strings.TrimSpace(out) != "" {
		t.Error("branch should not exist on remote after PrePush failure")
	}
}

// Push proceeds when PrePush succeeds.
func TestPush_PrePushPasses(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test-prepush-pass", wtDir)

	writeFile(t, wtDir, "feature.go", "package main\n")
	run(t, "git", "-C", wtDir, "add", "-A")
	run(t, "git", "-C", wtDir, "commit", "-m", "add feature")

	prePushCalled := false
	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        wtDir,
		WorktreeBranch: "ralph/test-prepush-pass",
		State:          newMemState(),
		Logger:         &testLog{},
		PrePush: func(ctx context.Context) error {
			prePushCalled = true
			return nil
		},
	}

	if err := mgr.Push(context.Background()); err != nil {
		t.Fatalf("Push should succeed when PrePush passes: %v", err)
	}
	if !prePushCalled {
		t.Error("PrePush callback was not invoked")
	}
}
