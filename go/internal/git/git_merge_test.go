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
	"time"
)

// PostMergeUpdateMain updates local main to match origin/main after a merge,
// but does NOT touch the worktree.
func TestPostMergeUpdateMain_AdvancesLocalMain(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		BaseBranch: "main",
		State:      st,
		Logger:     &testLog{},
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

	// After cleanup the worktree moves to ralph/next (the task branch was deleted).
	if mgr.WorktreeBranch != "ralph/next" {
		t.Errorf("worktree branch should be ralph/next after task branch cleanup, got %q", mgr.WorktreeBranch)
	}
}

// PostMergeUpdateMain must not destroy uncommitted working-tree changes in the
// project dir when advancing local main to origin/main. Users editing files
// while ralph merges a PR must not lose their unsaved work.
func TestPostMergeUpdateMain_PreservesUncommittedWorkingTreeChanges(t *testing.T) {
	project, _ := initBareRepo(t)
	bare := filepath.Join(filepath.Dir(project), "bare.git")
	ralphDir := filepath.Join(project, ".ralph")

	// Commit a tracked file to main so we can modify it later.
	writeFile(t, project, "tracked.txt", "original\n")
	run(t, "git", "-C", project, "add", "tracked.txt")
	run(t, "git", "-C", project, "commit", "-m", "add tracked file")
	run(t, "git", "-C", project, "push", "origin", "main")

	mgr := &Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		BaseBranch: "main",
		State:      newMemState(),
		Logger:     &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// Simulate user editing a tracked file in the project dir without staging it.
	if err := os.WriteFile(filepath.Join(project, "tracked.txt"), []byte("user-modified\n"), 0o644); err != nil {
		t.Fatalf("write tracked.txt: %v", err)
	}

	// Push a new commit to origin/main from a separate clone (simulates PR merge).
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	writeFile(t, tmpClone, "merged-pr.txt", "merged content\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "squash-merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	mgr.PostMergeUpdateMain()

	// User's modification to tracked.txt must survive the main-update.
	got := strings.TrimSpace(gitOutput(project, "diff", "HEAD", "--", "tracked.txt"))
	if !strings.Contains(got, "user-modified") {
		t.Errorf("working tree change to tracked.txt was destroyed by PostMergeUpdateMain; diff:\n%s", got)
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
		BaseBranch: "main",
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

// PostMergeUpdateMain deletes the local task branch after rebasing onto main,
// so completed task branches don't accumulate in the local repo over time.
func TestPostMergeUpdateMain_DeletesLocalTaskBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	bare := filepath.Join(filepath.Dir(project), "bare.git")
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		BaseBranch: "main",
		State:      st,
		Logger:     &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("delete local branch", "ralph-4l32")
	taskBranch := mgr.WorktreeBranch

	// Commit on the task branch so it has a distinct local ref.
	writeFile(t, mgr.WorkDir, "task-work.txt", "work\n")
	run(t, "git", "-C", mgr.WorkDir, "add", "task-work.txt")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "task work")

	// Push a squash-merge commit to origin/main (simulating a merged PR).
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	writeFile(t, tmpClone, "merged-work.txt", "merged\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	mgr.PostMergeUpdateMain()

	// The task branch must no longer exist as a local ref.
	branches := gitOutput(project, "branch", "--list")
	if strings.Contains(branches, taskBranch) {
		t.Errorf("local task branch %q should have been deleted after merge, but still listed in: %s", taskBranch, branches)
	}
}

// PostMergeUpdateMain moves the worktree to ralph/next before deleting the task
// branch when the worktree is currently checked out on that branch, so git
// does not refuse the deletion.
func TestPostMergeUpdateMain_MovesToNextBranchWhenOnTaskBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	bare := filepath.Join(filepath.Dir(project), "bare.git")
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		BaseBranch: "main",
		State:      st,
		Logger:     &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("move to next", "ralph-4l32")
	taskBranch := mgr.WorktreeBranch

	// Commit on the task branch.
	writeFile(t, mgr.WorkDir, "task-work.txt", "work\n")
	run(t, "git", "-C", mgr.WorkDir, "add", "task-work.txt")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "task work")

	// Verify the worktree is on the task branch before PostMergeUpdateMain.
	checkedOutBefore := gitOutput(mgr.WorkDir, "symbolic-ref", "--short", "HEAD")
	if checkedOutBefore != taskBranch {
		t.Fatalf("expected worktree on %q, got %q", taskBranch, checkedOutBefore)
	}

	// Push a squash-merge commit to origin/main.
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	writeFile(t, tmpClone, "merged-work.txt", "merged\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	mgr.PostMergeUpdateMain()

	// Worktree must now be on ralph/next (not the deleted task branch).
	checkedOutAfter := gitOutput(mgr.WorkDir, "symbolic-ref", "--short", "HEAD")
	if checkedOutAfter != "ralph/next" {
		t.Errorf("worktree should be on ralph/next after branch cleanup, got %q", checkedOutAfter)
	}

	// WorktreeBranch field must reflect the new branch.
	if mgr.WorktreeBranch != "ralph/next" {
		t.Errorf("WorktreeBranch should be ralph/next, got %q", mgr.WorktreeBranch)
	}

	// Old task branch must be gone.
	branches := gitOutput(project, "branch", "--list")
	if strings.Contains(branches, taskBranch) {
		t.Errorf("old task branch %q should be deleted, still listed in: %s", taskBranch, branches)
	}

	// BranchRenamed must be false so the next task can rename ralph/next.
	if mgr.BranchRenamed {
		t.Error("BranchRenamed should be false after moving to ralph/next")
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

// PushAndCreatePR must pass the configured base branch to CreatePR,
// so PRs target the correct branch (e.g. develop) instead of the repo default (main).
func TestPushAndCreatePR_UsesBaseBranch(t *testing.T) {
	project, cleanup := initBareRepoWithBranch(t, "develop")
	defer cleanup()

	// WorkDir must differ from ProjectDir so Push doesn't bail early.
	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test/feature", wtDir)
	run(t, "git", "-C", wtDir, "commit", "--allow-empty", "-m", "feature commit")

	var capturedOpts CreatePROpts
	gh := &capturingGitHub{StubGitHub: StubGitHub{IsAvailable: true}}
	gh.createPR = func(opts CreatePROpts) (int, error) {
		capturedOpts = opts
		return 0, nil
	}

	log := &testLog{}
	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        wtDir,
		WorktreeBranch: "ralph/test/feature",
		BaseBranch:     "develop",
		Logger:         log,
		GitHub:         gh,
	}

	_, err := mgr.PushAndCreatePR(context.Background(), "", "test task", "")
	if err != nil {
		t.Fatalf("PushAndCreatePR failed: %v (log: %v)", err, log.messages)
	}

	if capturedOpts.Base != "develop" {
		t.Errorf("CreatePR should use base=develop, got base=%q", capturedOpts.Base)
	}
}

// PR created with --base-branch main targets main, not the repo default.
func TestPushAndCreatePR_BaseBranchMainTargetsMain(t *testing.T) {
	project, cleanup := initBareRepoWithBranch(t, "main")
	defer cleanup()

	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test/feature", wtDir)
	run(t, "git", "-C", wtDir, "commit", "--allow-empty", "-m", "feature commit")

	var capturedOpts CreatePROpts
	gh := &capturingGitHub{StubGitHub: StubGitHub{IsAvailable: true}}
	gh.createPR = func(opts CreatePROpts) (int, error) {
		capturedOpts = opts
		return 0, nil
	}

	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        wtDir,
		WorktreeBranch: "ralph/test/feature",
		BaseBranch:     "main",
		Logger:         &testLog{},
		GitHub:         gh,
	}

	_, err := mgr.PushAndCreatePR(context.Background(), "", "test task", "")
	if err != nil {
		t.Fatalf("PushAndCreatePR failed: %v", err)
	}

	if capturedOpts.Base != "main" {
		t.Errorf("CreatePR should use base=main, got base=%q", capturedOpts.Base)
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

	var capturedOpts CreatePROpts
	gh := &capturingGitHub{StubGitHub: StubGitHub{IsAvailable: true}}
	gh.createPR = func(opts CreatePROpts) (int, error) {
		capturedOpts = opts
		return 0, nil
	}

	log := &testLog{}
	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        wtDir,
		WorktreeBranch: "ralph/test/feature",
		BaseBranch:     "main",
		Logger:         log,
		GitHub:         gh,
	}

	_, err := mgr.PushAndCreatePR(context.Background(), "ralph-hm8", "fix: include bead ID in PR title", "")
	if err != nil {
		t.Fatalf("PushAndCreatePR failed: %v", err)
	}

	if !strings.Contains(capturedOpts.Title, "[ralph-hm8]") {
		t.Errorf("PR title should contain bead ID prefix [ralph-hm8], got: %q", capturedOpts.Title)
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

	var capturedOpts CreatePROpts
	gh := &capturingGitHub{StubGitHub: StubGitHub{IsAvailable: true}}
	gh.createPR = func(opts CreatePROpts) (int, error) {
		capturedOpts = opts
		return 0, nil
	}

	log := &testLog{}
	mgr := &Manager{
		ProjectDir:     project,
		WorkDir:        wtDir,
		WorktreeBranch: "ralph/test/feature",
		BaseBranch:     "main",
		Logger:         log,
		GitHub:         gh,
	}

	_, err := mgr.PushAndCreatePR(context.Background(), "", "add new feature", "")
	if err != nil {
		t.Fatalf("PushAndCreatePR failed: %v", err)
	}

	if strings.Contains(capturedOpts.Title, "[") {
		t.Errorf("PR title should not contain brackets when no bead ID, got: %q", capturedOpts.Title)
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
		StubGitHub: StubGitHub{IsAvailable: true, CreatedPR: 55},
		createPR: func(opts CreatePROpts) (int, error) {
			capturedOpts = opts
			return 55, nil
		},
	}

	dir := t.TempDir()
	mgr := &Manager{
		ProjectDir:     dir,
		BaseBranch: "main",
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
		StubGitHub: StubGitHub{IsAvailable: true, CreatedPR: 55},
		createPR: func(opts CreatePROpts) (int, error) {
			capturedOpts = opts
			return 55, nil
		},
	}

	dir := t.TempDir()
	mgr := &Manager{
		ProjectDir:     dir,
		BaseBranch: "main",
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
		BaseBranch: "main",
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
		BaseBranch: "main",
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

// AutoMergeCurrentBranch returns 0 and logs "No PR found" when no PR
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
		BaseBranch: "main",
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
	if !log.contains("No PR found") {
		t.Error("expected 'No PR found' log message")
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
		ProjectDir: project,
		RalphDir:   ralphDir,
		BaseBranch: "main",
		State:      st,
		Logger:     &testLog{},
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
		BaseBranch: "main",
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/test/01-merge-retry",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	mergeCalls := 0
	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      42,
		Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		MergeResults: []MergeResult{
			{Conflict: true, Message: "merge conflict"},
			{Merged: true},
		},
		OnMerge: func() { mergeCalls++ },
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
		BaseBranch: "main",
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/test/01-ci-retry",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      55,
		// First AwaitCI returns failure (triggers OnCIFailure). After CI fix,
		// subsequent checks return success so the merge retry proceeds.
		ChecksFunc: func(call int) []CICheckResult {
			if call == 1 {
				return []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}}
			}
			return []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}
		},
		MergeResults: []MergeResult{
			{Merged: true},
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
		OnCIFailure: func(ciErr *CIFailureError) CIFixResult {
			ciFixCalled = true
			return CIFixApplied
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
		BaseBranch: "main",
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		WorktreeBranch: "ralph/test/01-exhaust",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      99,
		Checks:      []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}},
		MergeResult: MergeResult{Blocked: true, Message: "CI failed"},
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
		OnCIFailure: func(ciErr *CIFailureError) CIFixResult {
			ciFixCalls++
			return CIFixApplied
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
		BaseBranch: "main",
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		WorktreeBranch: "ralph/test/01-unresolvable",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	mergeCalls := 0
	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      50,
		Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		MergeResults: []MergeResult{
			{Conflict: true, Message: "merge conflict"},
			{Conflict: true, Message: "merge conflict"},
			{Conflict: true, Message: "merge conflict"},
			{Conflict: true, Message: "merge conflict"},
		},
		OnMerge: func() { mergeCalls++ },
	}
	mgr.GitHub = gh

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	// Sequence for merge-base --is-ancestor:
	//   1. EnsureUpToDate: nil (up to date)
	//   2-3. Push (baseSHA="abc123"): nil, nil (no divergence, squash check)
	//   4. branchNeedsUpdate: nil (no update needed → proceed to executeMerge)
	//   5. ResolveConflict EnsureUpToDate: nil (up to date)
	//   6. ResolveConflict ancestry check: error (still diverged → UnresolvedConflictError)
	runner.OnSequence("merge-base --is-ancestor", []stubResponse{
		{"", nil},
		{"", nil},
		{"", nil},
		{"", nil},
		{"", nil},
		{"", fmt.Errorf("not ancestor")},
	})
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
		BaseBranch: "main",
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		WorktreeBranch: "ralph/test/01-subject",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      77,
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

// After a fix agent commits and force-pushes, MergeWithRetry must call
// AwaitCI to wait for fresh CI results on the new HEAD before retrying the
// merge. This prevents the retry from seeing the old (stale) failure status.
func TestMergeWithRetry_PushesFixAgentWorkBeforeRetry(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Manager{
		ProjectDir:     project,
		BaseBranch: "main",
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/test/01-ci-push-retry",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	// Track the order of operations to verify AwaitCI happens between
	// OnCIFailure and the merge retry.
	var events []string

	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      60,
		ChecksFunc: func(call int) []CICheckResult {
			if call == 1 {
				return []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}}
			}
			events = append(events, "await-ci")
			return []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}
		},
		OnMerge: func() {
			events = append(events, "merge-retry")
		},
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
		OnCIFailure: func(ciErr *CIFailureError) CIFixResult {
			ciFixCalled = true
			events = append(events, "fix-applied")
			return CIFixApplied
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

// When automatic rebase fails to resolve conflicts, MergeWithRetry calls
// the OnConflict callback to spawn a conflict resolution agent. If the
// agent resolves the conflict, the merge is retried and succeeds.
func TestMergeWithRetry_SpawnsConflictAgent(t *testing.T) {
	project, _ := initBareRepo(t)

	mgr := &Manager{
		ProjectDir:     project,
		BaseBranch: "main",
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		WorktreeBranch: "ralph/test/01-conflict-agent",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	mergeCalls := 0
	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      70,
		Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		MergeResults: []MergeResult{
			{Conflict: true, Message: "merge conflict"},
		},
		OnMerge: func() { mergeCalls++ },
	}
	mgr.GitHub = gh

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	// Sequence (no plain "rev-parse" stub → Push skips merge-base, baseSHA=""):
	//   1. Attempt 1 EnsureUpToDate: nil (up to date)
	//   2. Attempt 1 branchNeedsUpdate: nil (no update) → executeMerge → Conflict
	//   3. ResolveConflict EnsureUpToDate: nil
	//   4. ResolveConflict ancestry check: error → UnresolvedConflictError → OnConflict(true) → retry
	//   5. Attempt 2 EnsureUpToDate: nil
	//   6. Attempt 2 branchNeedsUpdate: nil → executeMerge → Merged
	runner.OnSequence("merge-base --is-ancestor", []stubResponse{
		{"", nil},
		{"", nil},
		{"", nil},
		{"", fmt.Errorf("not ancestor")},
		{"", nil},
		{"", nil},
	})
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
			if conflictErr.PRNumber != 70 {
				t.Errorf("expected PRNumber=70, got %d", conflictErr.PRNumber)
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
		BaseBranch: "main",
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		WorktreeBranch: "ralph/test/01-conflict-agent-fail",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	mergeCalls := 0
	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      71,
		Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
		MergeResults: []MergeResult{
			{Conflict: true, Message: "merge conflict"},
			{Conflict: true, Message: "merge conflict"},
		},
		OnMerge: func() { mergeCalls++ },
	}
	mgr.GitHub = gh

	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	// Sequence (no plain "rev-parse" stub → Push skips merge-base, baseSHA=""):
	//   1. Attempt 1 EnsureUpToDate: nil (up to date)
	//   2. Attempt 1 branchNeedsUpdate: nil (no update) → executeMerge → Conflict
	//   3. ResolveConflict EnsureUpToDate: nil
	//   4. ResolveConflict ancestry check: error → UnresolvedConflictError → OnConflict(false) → return
	runner.OnSequence("merge-base --is-ancestor", []stubResponse{
		{"", nil},
		{"", nil},
		{"", nil},
		{"", fmt.Errorf("not ancestor")},
	})
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
		BaseBranch: "main",
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
		BaseBranch: "main",
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
		BaseBranch: "main",
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
		BaseBranch: "main",
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
		BaseBranch: "main",
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
		BaseBranch: "main",
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
		BaseBranch: "main",
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

// AutoMergeCurrentBranch returns merged=true without pushing when the PR
// for the branch is already merged, so the caller can close the bead.
func TestAutoMergeCurrentBranch_ReturnsMergedForAlreadyMergedPR(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)

	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      0,       // no open PR
		PRNumber:    438,    // FindPR returns this
		PRState:     "MERGED", // GetPRState returns this
	}

	mgr := &Manager{
		ProjectDir:     "/project",
		WorkDir:        "/project/wt",
		WorktreeBranch: "ralph/test/01-merged",
		BaseBranch:     "main",
		Runner:         runner,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         &testLog{},
	}

	merged, err := mgr.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !merged {
		t.Fatal("expected merged=true for already-merged PR")
	}

	// Push should NOT have been called — no git push in the runner calls.
	if runner.CalledWith("push") {
		t.Error("should not push to an already-merged PR")
	}
	// Merge should NOT have been called.
	if gh.MergeCalls > 0 {
		t.Error("should not attempt merge on an already-merged PR")
	}
}

// AutoMergeCurrentBranch reopens a closed PR and proceeds to merge it,
// preventing the indefinite CI wait that occurs on closed PRs.
func TestAutoMergeCurrentBranch_ReopensClosedPR(t *testing.T) {
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

	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      0,       // no open PR initially
		PRNumber:    438,    // FindPR returns this
		PRState:     "CLOSED", // GetPRState returns this
		PRTitle:     "[ralph-rvta] Fix closed PR",
		Checks:      []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}},
	}

	mgr := &Manager{
		ProjectDir:     "/project",
		WorkDir:        "/project/wt",
		WorktreeBranch: "ralph/test/01-reopen",
		BaseBranch:     "main",
		Runner:         runner,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         &testLog{},
	}

	merged, err := mgr.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !merged {
		t.Fatal("expected merged=true after reopening closed PR")
	}
	if !gh.ReopenPRCalled {
		t.Error("expected ReopenPR to be called for the closed PR")
	}
	if gh.MergeCalls == 0 {
		t.Error("expected merge to be attempted after reopening")
	}
}

// CreatePR falls back to the REST API when both gh pr create and reopen fail.
// This covers the diverged-branch scenario: the old closed PR can't be reopened
// because the branch history diverged, so a fresh PR is created via the API.
func TestCreatePR_APIFallbackWhenReopenFails(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)

	gh := &StubGitHub{
		IsAvailable:          true,
		OpenPR:               0,
		CreatePRErr:          fmt.Errorf("a pull request already exists for branch"),
		PRNumber:             438,
		PRState:              "CLOSED",
		ReopenPRErr:          fmt.Errorf("Could not open the pull request"),
		CreatePRViaAPIResult: 500,
	}

	mgr := &Manager{
		ProjectDir:     "/project",
		WorkDir:        "/project/wt",
		WorktreeBranch: "ralph/test/diverged-branch",
		BaseBranch:     "main",
		Runner:         runner,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         &testLog{},
	}

	prNumber, err := mgr.CreatePR(context.Background(), "ralph-test", "Fix diverged", "body")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if prNumber != 500 {
		t.Errorf("expected new PR 500 from API fallback, got %d", prNumber)
	}
	if !gh.ReopenPRCalled {
		t.Error("expected ReopenPR to be called before API fallback")
	}
	if !gh.CreatePRViaAPICalled {
		t.Error("expected CreatePRViaAPI to be called as fallback")
	}
}

// CreatePR reopens a closed PR when gh pr create fails because a PR already
// exists for the branch, rather than returning an error.
func TestCreatePR_ReopensClosedPROnCreateFailure(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)

	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      0,                                                     // no open PR
		CreatePRErr: fmt.Errorf("a pull request already exists for branch"), // create fails
		PRNumber:    438,                                                  // FindPR returns this
		PRState:     "CLOSED",                                               // GetPRState returns this
	}

	mgr := &Manager{
		ProjectDir:     "/project",
		WorkDir:        "/project/wt",
		WorktreeBranch: "ralph/test/01-reopen-create",
		BaseBranch:     "main",
		Runner:         runner,
		GitHub:         gh,
		State:          newMemState(),
		Logger:         &testLog{},
	}

	prNumber, err := mgr.CreatePR(context.Background(), "ralph-test", "Fix bug", "body")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if prNumber != 438 {
		t.Errorf("expected reopened PR number 438, got %d", prNumber)
	}
	if !gh.ReopenPRCalled {
		t.Error("expected ReopenPR to be called")
	}
}

// When the fix agent makes no commits (CIFixNoCommits), MergeWithRetry retries
// with exponential backoff instead of immediately giving up. After MaxInfraRetries,
// it returns the CI error. This covers the case where CI fails due to infrastructure
// issues (billing, runner allocation) rather than actual test failures.
func TestMergeWithRetry_InfraFailureRetriesWithBackoff(t *testing.T) {
	project, _ := initBareRepo(t)

	mgr := &Manager{
		ProjectDir:     project,
		BaseBranch:     "main",
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		WorktreeBranch: "ralph/test/01-infra",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      77,
		Checks:      []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}},
		MergeResult: MergeResult{Blocked: true, Message: "CI failed"},
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

	noCommitCalls := 0
	var sleepDelays []time.Duration
	_, err := mgr.MergeWithRetry(context.Background(), MergeRetryOpts{
		OnCIFailure: func(ciErr *CIFailureError) CIFixResult {
			noCommitCalls++
			return CIFixNoCommits
		},
		SleepFunc: func(d time.Duration) {
			sleepDelays = append(sleepDelays, d)
		},
	})

	if err == nil {
		t.Fatal("expected error after exhausting infrastructure retries")
	}

	if noCommitCalls != MaxInfraRetries+1 {
		t.Errorf("expected %d CI fix calls (initial + %d retries), got %d", MaxInfraRetries+1, MaxInfraRetries, noCommitCalls)
	}

	if len(sleepDelays) != MaxInfraRetries {
		t.Fatalf("expected %d backoff sleeps, got %d", MaxInfraRetries, len(sleepDelays))
	}

	// Verify exponential backoff: 30s, 60s, 120s
	expectedDelays := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}
	for i, want := range expectedDelays {
		if sleepDelays[i] != want {
			t.Errorf("backoff[%d] = %s, want %s", i, sleepDelays[i], want)
		}
	}
}

// When the fix agent returns CIFixNoCommits but CI eventually passes on retry,
// the merge succeeds. Infrastructure issues are transient — backoff gives CI
// time to recover.
func TestMergeWithRetry_InfraFailureRecovery(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Manager{
		ProjectDir:     project,
		BaseBranch:     "main",
		WorkDir:        filepath.Join(t.TempDir(), "wt"),
		RalphDir:       ralphDir,
		WorktreeBranch: "ralph/test/01-infra-recover",
		State:          newMemState(),
		Logger:         &testLog{},
	}

	callCount := 0
	gh := &StubGitHub{
		IsAvailable: true,
		OpenPR:      88,
		ChecksFunc: func(call int) []CICheckResult {
			if call == 1 {
				return []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}}
			}
			return []CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}
		},
		MergeResults: []MergeResult{
			{Merged: true},
		},
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

	merged, err := mgr.MergeWithRetry(context.Background(), MergeRetryOpts{
		OnCIFailure: func(ciErr *CIFailureError) CIFixResult {
			callCount++
			return CIFixNoCommits
		},
		SleepFunc: func(d time.Duration) {},
	})

	if err != nil {
		t.Fatalf("expected merge to succeed after infra recovery, got: %v", err)
	}
	if !merged {
		t.Error("expected merge to succeed")
	}
	if callCount != 1 {
		t.Errorf("expected 1 infra retry before recovery, got %d", callCount)
	}
}

// When Admin=false (default), MergePR on StubGitHub returns Blocked when
// configured to do so — non-admin merges obey branch protection.
func TestMergeOpts_NonAdminReturnsBlocked(t *testing.T) {
	gh := &StubGitHub{
		IsAvailable: true,
		MergeResult: MergeResult{Blocked: true, Message: "branch protection rules"},
	}
	result := gh.MergePR(42, "https://github.com/test/repo.git", MergeOpts{DeleteBranch: true})
	if result.Merged {
		t.Error("expected non-admin merge to not succeed when branch protection blocks")
	}
	if !result.Blocked {
		t.Error("expected non-admin merge to return Blocked=true")
	}
	if gh.LastMergeOpts.Admin {
		t.Error("expected LastMergeOpts.Admin to be false for non-admin merge")
	}
}

// When Admin=true and REST succeeds (not Blocked), MergePR returns Merged
// without needing the admin fallback path.
func TestMergeOpts_AdminMergeReturnsMerged(t *testing.T) {
	gh := &StubGitHub{
		IsAvailable: true,
		MergeResult: MergeResult{Merged: true},
	}
	result := gh.MergePR(42, "https://github.com/test/repo.git", MergeOpts{DeleteBranch: true, Admin: true})
	if !result.Merged {
		t.Error("expected admin merge to succeed")
	}
	if result.Blocked {
		t.Error("expected admin merge to not return Blocked")
	}
	if !gh.LastMergeOpts.Admin {
		t.Error("expected LastMergeOpts.Admin to be true for admin merge")
	}
}

// When Admin=true and REST returns Blocked (405), MergePR falls back to
// gh pr merge --admin instead of returning Blocked to the caller.
// Admin fallback is only triggered by Blocked, never by other failures.
func TestMergeOpts_AdminFallback_OnlyWhenRESTReturnsBlocked(t *testing.T) {
	// Admin=true + REST returns Blocked → admin fallback succeeds.
	ghBlocked := &StubGitHub{
		IsAvailable: true,
		MergeResult: MergeResult{Blocked: true, Message: "branch protection rules"},
	}
	result := ghBlocked.MergePR(42, "https://github.com/test/repo.git", MergeOpts{Admin: true})
	if !result.Merged {
		t.Errorf("expected admin fallback to succeed when REST returns Blocked, got: %+v", result)
	}
	if result.Blocked {
		t.Error("expected admin fallback to clear Blocked=true")
	}

	// Admin=false + REST returns Blocked → stays Blocked (no fallback).
	ghNoAdmin := &StubGitHub{
		IsAvailable: true,
		MergeResult: MergeResult{Blocked: true, Message: "branch protection rules"},
	}
	resultNoAdmin := ghNoAdmin.MergePR(42, "https://github.com/test/repo.git", MergeOpts{Admin: false})
	if resultNoAdmin.Merged {
		t.Error("expected non-admin merge to stay Blocked when REST returns Blocked")
	}
	if !resultNoAdmin.Blocked {
		t.Error("expected Blocked=true when Admin=false and REST returns Blocked")
	}

	// Admin=true + AdminMergeResult configured → uses configured result.
	adminFail := MergeResult{Message: "admin merge failed: not permitted"}
	ghAdminFail := &StubGitHub{
		IsAvailable:      true,
		MergeResult:      MergeResult{Blocked: true},
		AdminMergeResult: &adminFail,
	}
	resultFail := ghAdminFail.MergePR(42, "https://github.com/test/repo.git", MergeOpts{Admin: true})
	if resultFail.Merged {
		t.Error("expected configured AdminMergeResult to be used, not default success")
	}
	if resultFail.Message != adminFail.Message {
		t.Errorf("expected AdminMergeResult message %q, got %q", adminFail.Message, resultFail.Message)
	}
}

// When CI fails due to infrastructure (zero job steps) and local tests passed,
// AutoMergeCurrentBranch sets Admin=true and retries the merge instead of
// returning ErrMergeBlockedByInfra. This allows ralph loop to bypass branch
// protection when CI is broken at the infrastructure level.
func TestAutoMergeCurrentBranch_InfraFailureWithLocalTestsUsesAdminMerge(t *testing.T) {
	project, _ := initBareRepo(t)
	stubCISleep(t)

	mgr := &Manager{
		ProjectDir:       project,
		BaseBranch:       "main",
		WorkDir:          filepath.Join(t.TempDir(), "wt"),
		WorktreeBranch:   "ralph/test/01-infra-admin",
		State:            newMemState(),
		Logger:           &testLog{},
		LocalTestsPassed: true,
	}

	gh := &StubGitHub{
		IsAvailable:  true,
		OpenPR:       55,
		Checks:       []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}},
		JobStepCount: 0, // zero steps = infrastructure failure
		MergeResult:  MergeResult{Merged: true},
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
		t.Fatalf("expected admin merge to succeed, got: %v", err)
	}
	if !merged {
		t.Error("expected merge to succeed via admin bypass")
	}
	if !gh.LastMergeOpts.Admin {
		t.Error("expected MergeOpts.Admin=true when bypassing branch protection after infra failure")
	}
}

// When ralph loop detects an infrastructure failure (zero job steps) and local
// tests passed, Admin=true is set. If the REST merge returns 405 (Blocked),
// the admin fallback kicks in and the merge succeeds.
func TestAutoMergeCurrentBranch_InfraBypassAdminFallbackOn405(t *testing.T) {
	project, _ := initBareRepo(t)
	stubCISleep(t)

	mgr := &Manager{
		ProjectDir:       project,
		BaseBranch:       "main",
		WorkDir:          filepath.Join(t.TempDir(), "wt"),
		WorktreeBranch:   "ralph/test/01-infra-admin-405",
		State:            newMemState(),
		Logger:           &testLog{},
		LocalTestsPassed: true,
	}

	gh := &StubGitHub{
		IsAvailable:  true,
		OpenPR:       55,
		Checks:       []CICheckResult{{Name: "ci", State: "FAILURE", Bucket: "fail"}},
		JobStepCount: 0, // zero steps = infrastructure failure
		MergeResult:  MergeResult{Blocked: true, Message: "branch protection"},
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
		t.Fatalf("expected admin fallback to succeed on 405, got: %v", err)
	}
	if !merged {
		t.Error("expected merge to succeed via admin fallback after infra failure + 405")
	}
	if !gh.LastMergeOpts.Admin {
		t.Error("expected MergeOpts.Admin=true for infra bypass path")
	}
}

// FlushUnpushedWork on the placeholder branch does not attempt push or PR
// creation — there is no task work to flush.
func TestFlushUnpushedWork_SkipsOnPlaceholderBranch(t *testing.T) {
	dir := t.TempDir()
	runner := newStubRunner()
	mgr := stubManager(dir, runner, nil)
	mgr.WorktreeBranch = WipBranchName()

	merged, err := mgr.FlushUnpushedWork(context.Background(), "ralph-test", "test task", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged {
		t.Error("FlushUnpushedWork on placeholder branch should return merged=false")
	}
	if runner.CalledWith("push") {
		t.Error("FlushUnpushedWork on placeholder branch must not attempt git push")
	}
}
