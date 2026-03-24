package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// PostMergeReset resets the worktree to a fresh branch at origin/main after
// auto-merge, so the next task starts from merged state instead of stale commits.
func TestPostMergeReset_ResetsToOriginMain(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:     project,
		RalphDir:       ralphDir,
		State:  st,
		Logger: &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// Rename to a task branch (simulating a completed task)
	mgr.RenameBranchForTask("completed task", "")
	taskBranch := mgr.WorktreeBranch
	if taskBranch == mgr.TempBranch() {
		t.Fatal("expected task branch to differ from temp branch")
	}

	if err := mgr.PostMergeReset(); err != nil {
		t.Fatalf("PostMergeReset: %v", err)
	}

	if mgr.WorktreeBranch != mgr.TempBranch() {
		t.Errorf("expected branch %q after reset, got %q", mgr.TempBranch(), mgr.WorktreeBranch)
	}
	if mgr.BranchRenamed {
		t.Error("BranchRenamed should be false after PostMergeReset")
	}

	// Old task branch should be deleted
	if refExists(mgr.WorkDir, taskBranch) {
		t.Errorf("old task branch %q should have been deleted", taskBranch)
	}
}

// Force-reset must clean both dirty tracked files and untracked files left
// by the previous task, so the next task starts with a pristine worktree
// matching origin/main exactly.
func TestPostMergeReset_CleansUntrackedAndDirtyFiles(t *testing.T) {
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

	mgr.RenameBranchForTask("dirty task", "")

	// Create an untracked file (simulating build artifacts or generated files)
	untrackedPath := filepath.Join(mgr.WorkDir, "leftover-artifact.txt")
	if err := os.WriteFile(untrackedPath, []byte("stale artifact\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Modify a tracked file without committing (dirty working tree)
	trackedPath := filepath.Join(mgr.WorkDir, "dirty-edit.txt")
	writeFile(t, mgr.WorkDir, "dirty-edit.txt", "original\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "add tracked file")
	if err := os.WriteFile(trackedPath, []byte("modified\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := mgr.PostMergeReset(); err != nil {
		t.Fatalf("PostMergeReset: %v", err)
	}

	if _, err := os.Stat(untrackedPath); !os.IsNotExist(err) {
		t.Error("untracked file should have been removed by force-reset")
	}

	// Tracked file from the task branch should no longer exist (it wasn't on origin/main)
	if _, err := os.Stat(trackedPath); !os.IsNotExist(err) {
		t.Error("tracked file from task branch should not exist after reset to origin/main")
	}

	headAfter := gitOutput(mgr.WorkDir, "rev-parse", "HEAD")
	originMain := gitOutput(mgr.WorkDir, "rev-parse", "origin/main")
	if headAfter != originMain {
		t.Errorf("HEAD should match origin/main, got %s vs %s", headAfter, originMain)
	}
}

// After auto-merge squash-merges a PR, postMergeUpdate must advance local
// main to match origin/main without leaving stale staged changes. The old
// two-step approach (update-ref + reset --hard HEAD) left the index pointing
// at the old tree between steps, staging reversions of merged PR work. The
// fix uses a single atomic `git reset --hard origin/main`.
func TestPostMergeUpdate_AtomicResetNoStagedChanges(t *testing.T) {
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

	// Call postMergeUpdate (the method under test)
	merged, err := mgr.postMergeUpdate("42")
	if err != nil {
		t.Fatalf("postMergeUpdate failed: %v", err)
	}
	if !merged {
		t.Fatal("postMergeUpdate should return true")
	}

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

// nwoFromRemote must extract owner/repo from both SSH and HTTPS remote URLs
// so the GitHub API update-branch endpoint gets the correct repository path.
func TestNwoFromRemote_SSHAndHTTPS(t *testing.T) {
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
		got := nwoFromRemote(tt.remote)
		if got != tt.want {
			t.Errorf("nwoFromRemote(%q) = %q, want %q", tt.remote, got, tt.want)
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

	err := mgr.PushAndCreatePR(context.Background(), "", "test task")
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

	err := mgr.PushAndCreatePR(context.Background(), "ralph-hm8", "fix: include bead ID in PR title")
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

	err := mgr.PushAndCreatePR(context.Background(), "", "add new feature")
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
		stubGitHub: stubGitHub{
			available: true,
			openPR:    "42",
			checks:    []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		},
		mergeResults: []mergeResult{
			{output: "merge conflict", err: fmt.Errorf("merge conflict")},
			{output: "merged", err: nil},
		},
		onMerge: func() { mergeCalls++ },
	}
	mgr.GitHub = gh

	runner := newStubRunner()
	// Stub git operations that ResolveConflict + AutoMerge use
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("merge-base --is-ancestor", "", fmt.Errorf("not ancestor"))
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
		stubGitHub: stubGitHub{
			available: true,
			openPR:    "55",
			checks:    []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}},
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

	gh := &stubGitHub{
		available: true,
		openPR:    "99",
		checks:    []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}},
		mergeErr:  fmt.Errorf("CI failed"),
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

	gh := &stubGitHub{
		available: true,
		openPR:    "77",
		prTitle:   "[ralph-31w] Fix squash-merge subject",
		checks:    []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
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
	if gh.mergeOpts.Subject != want {
		t.Errorf("merge subject = %q, want %q", gh.mergeOpts.Subject, want)
	}
}

// sequentialMergeGitHub returns different merge results on successive calls,
// allowing tests to simulate conflict→success or CI-fail→success sequences.
type sequentialMergeGitHub struct {
	stubGitHub
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
	if s.checkCalls > 1 && len(s.stubGitHub.checks) > 0 && s.stubGitHub.checks[0].State == "FAILURE" {
		return []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}, nil
	}
	return s.stubGitHub.checks, s.stubGitHub.checksErr
}
