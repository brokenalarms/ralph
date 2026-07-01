package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PostMergeUpdateMain must not modify the ProjectDir checkout: no merge,
// update-ref, or other write operations on main. The worktree handles syncing
// via rebase; local main staying behind origin/main is intentional.
func TestPostMergeUpdateMain_DoesNotModifyProjectDir(t *testing.T) {
	project, _ := initBareRepo(t)
	bare := filepath.Join(filepath.Dir(project), "bare.git")
	ralphDir := filepath.Join(project, ".ralph")

	repo := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
	)
	if err := repo.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	repo.RenameBranchForTask("completed task", "")

	// Record local main SHA before PostMergeUpdateMain.
	localMainBefore := gitOutput(project, "rev-parse", "main")

	// Push a new commit to origin/main (simulates a merged PR).
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	writeFile(t, tmpClone, "merged-work.txt", "merged content\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	repo.PostMergeUpdateMain()

	// Local main must not have been advanced — ProjectDir is untouched.
	localMainAfter := gitOutput(project, "rev-parse", "main")
	if localMainAfter != localMainBefore {
		t.Errorf("PostMergeUpdateMain must not modify local main in projectDir: SHA changed from %s to %s", localMainBefore, localMainAfter)
	}

	// ProjectDir working tree must have no tracked file modifications.
	// (Untracked files like .ralph/ are ignored since they're expected.)
	status := strings.TrimSpace(gitOutput(project, "status", "--porcelain", "--untracked-files=no"))
	if status != "" {
		t.Errorf("ProjectDir has dirty tracked files after PostMergeUpdateMain:\n%s", status)
	}

	// Worktree moves to ralph/next after branch cleanup.
	if repo.worktreeBranch != "ralph/next" {
		t.Errorf("worktree branch should be ralph/next after task branch cleanup, got %q", repo.worktreeBranch)
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

	repo := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
	)
	if err := repo.SetupWorktree(context.Background()); err != nil {
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

	repo.PostMergeUpdateMain()

	// User's modification to tracked.txt must survive the main-update.
	got := strings.TrimSpace(gitOutput(project, "diff", "HEAD", "--", "tracked.txt"))
	if !strings.Contains(got, "user-modified") {
		t.Errorf("working tree change to tracked.txt was destroyed by PostMergeUpdateMain; diff:\n%s", got)
	}
}

// PostMergeUpdateMain must not log "force" language in normal operation —
// the rebase path is clean, and force-language would be alarming for users
// monitoring the stream log.
func TestPostMergeUpdateMain_RebasePathLogsCleanly(t *testing.T) {
	log := &testLog{}
	runner := newStubRunner()
	runner.On("fetch", "", nil)
	runner.On("rebase", "", nil)
	runner.On("checkout", "", nil)
	runner.On("clean", "", nil)
	runner.On("branch", "", nil)
	runner.On("rev-list", "0", nil)

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/worktree", BaseBranch: "main", Logger: log},
		nil,
		withRunner(runner),
		withWorktreeBranch("ralph/task-branch"),
	)

	repo.PostMergeUpdateMain()

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
	dir := t.TempDir()
	runner := newStubRunner()
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("fetch", "", nil)
	runner.On("rebase", "", nil)
	runner.On("checkout", "", nil)
	runner.On("clean", "", nil)
	runner.On("rev-parse --verify", "", fmt.Errorf("not found"))
	runner.On("merge-base --is-ancestor", "", fmt.Errorf("not ancestor"))
	runner.On("branch -D", "", nil)

	taskBranch := "ralph/ralph-4l32-delete-local-branch"
	repo := newRepoForTest(
		Config{ProjectDir: dir, WorkDir: filepath.Join(dir, "wt"), BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(runner),
		withWorktreeBranch(taskBranch),
		withBranchRenamed(true),
	)

	repo.PostMergeUpdateMain()

	if !runner.CalledWith("branch", "-D", taskBranch) {
		t.Errorf("expected branch -D %s to be called", taskBranch)
	}
}

// PostMergeUpdateMain moves the worktree to ralph/next before deleting the task
// branch when the worktree is currently checked out on that branch, so git
// does not refuse the deletion.
func TestPostMergeUpdateMain_MovesToNextBranchWhenOnTaskBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	bare := filepath.Join(filepath.Dir(project), "bare.git")
	ralphDir := filepath.Join(project, ".ralph")

	repo := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
	)
	if err := repo.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	repo.RenameBranchForTask("move to next", "ralph-4l32")
	taskBranch := repo.worktreeBranch

	// Commit on the task branch.
	writeFile(t, repo.workDir, "task-work.txt", "work\n")
	run(t, "git", "-C", repo.workDir, "add", "task-work.txt")
	run(t, "git", "-C", repo.workDir, "commit", "-m", "task work")

	// Verify the worktree is on the task branch before PostMergeUpdateMain.
	checkedOutBefore := gitOutput(repo.workDir, "symbolic-ref", "--short", "HEAD")
	if checkedOutBefore != taskBranch {
		t.Fatalf("expected worktree on %q, got %q", taskBranch, checkedOutBefore)
	}

	// Push a squash-merge commit to origin/main.
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	writeFile(t, tmpClone, "merged-work.txt", "merged\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	repo.PostMergeUpdateMain()

	// Worktree must now be on ralph/next (not the deleted task branch).
	checkedOutAfter := gitOutput(repo.workDir, "symbolic-ref", "--short", "HEAD")
	if checkedOutAfter != "ralph/next" {
		t.Errorf("worktree should be on ralph/next after branch cleanup, got %q", checkedOutAfter)
	}

	// WorktreeBranch field must reflect the new branch.
	if repo.worktreeBranch != "ralph/next" {
		t.Errorf("WorktreeBranch should be ralph/next, got %q", repo.worktreeBranch)
	}

	// Old task branch must be gone.
	branches := gitOutput(project, "branch", "--list")
	if strings.Contains(branches, taskBranch) {
		t.Errorf("old task branch %q should be deleted, still listed in: %s", taskBranch, branches)
	}

	// BranchRenamed must be false so the next task can rename ralph/next.
	if repo.branchRenamed {
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
// Observable via the base of the newly-created PR in the fake's world.
func TestPushAndCreatePR_UsesBaseBranch(t *testing.T) {
	project, cleanup := initBareRepoWithBranch(t, "develop")
	defer cleanup()

	// WorkDir must differ from ProjectDir so Push doesn't bail early.
	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test/feature", wtDir)
	run(t, "git", "-C", wtDir, "commit", "--allow-empty", "-m", "feature commit")

	gh := newStubGitHub(StubGitHubConfig{Available: true})

	repo := newRepoForTest(
		Config{ProjectDir: project, WorkDir: wtDir, BaseBranch: "develop", Logger: &testLog{}},
		gh,
		withRunner(&execRunner{}),
		withWorktreeBranch("ralph/test/feature"),
	)

	prNum, err := repo.PushAndCreatePR(context.Background(), "", "test task", "")
	if err != nil {
		t.Fatalf("PushAndCreatePR failed: %v", err)
	}
	if prNum == 0 {
		t.Fatal("expected non-zero PR number")
	}

	pr, _ := gh.GetPR(context.Background(), "", prNum)
	if pr == nil {
		t.Fatal("expected created PR to exist in world")
	}
	if pr.BaseRef != "develop" {
		t.Errorf("CreatePR should use base=develop, got base=%q", pr.BaseRef)
	}
}

// PR created with --base-branch main targets main, not the repo default.
func TestPushAndCreatePR_BaseBranchMainTargetsMain(t *testing.T) {
	project, cleanup := initBareRepoWithBranch(t, "main")
	defer cleanup()

	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test/feature", wtDir)
	run(t, "git", "-C", wtDir, "commit", "--allow-empty", "-m", "feature commit")

	gh := newStubGitHub(StubGitHubConfig{Available: true})

	repo := newRepoForTest(
		Config{ProjectDir: project, WorkDir: wtDir, BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(&execRunner{}),
		withWorktreeBranch("ralph/test/feature"),
	)

	prNum, err := repo.PushAndCreatePR(context.Background(), "", "test task", "")
	if err != nil {
		t.Fatalf("PushAndCreatePR failed: %v", err)
	}

	pr, _ := gh.GetPR(context.Background(), "", prNum)
	if pr == nil {
		t.Fatal("expected created PR to exist in world")
	}
	if pr.BaseRef != "main" {
		t.Errorf("CreatePR should use base=main, got base=%q", pr.BaseRef)
	}
}

// PR titles must include the bead ID prefix so PRs are traceable back to
// their originating task.
func TestPushAndCreatePR_IncludesBeadIDInTitle(t *testing.T) {
	runner := newStubRunner()
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("remote get-url origin", "https://github.com/owner/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("rev-parse", "abc123", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("log", "feature commit", nil)
	runner.On("push", "", nil)
	runner.On("rev-list --count", "1", nil)

	gh := newStubGitHub(StubGitHubConfig{Available: true})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/worktree", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/feature"),
	)

	_, err := repo.PushAndCreatePR(context.Background(), "ralph-hm8", "fix: include bead ID in PR title", "")
	if err != nil {
		t.Fatalf("PushAndCreatePR failed: %v", err)
	}

	_, title, _, _ := gh.FindPR(context.Background(), "ralph/test/feature", "")
	if !strings.Contains(title, "[ralph-hm8]") {
		t.Errorf("PR title should contain bead ID prefix [ralph-hm8], got: %q", title)
	}
}

// When no bead ID is provided, the PR title should be the task
// description without any bracketed prefix.
func TestPushAndCreatePR_NoBeadID(t *testing.T) {
	runner := newStubRunner()
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("remote get-url origin", "https://github.com/owner/repo.git", nil)
	runner.On("fetch", "", nil)
	runner.On("rev-parse", "abc123", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("log", "feature commit", nil)
	runner.On("push", "", nil)
	runner.On("rev-list --count", "1", nil)

	gh := newStubGitHub(StubGitHubConfig{Available: true})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/worktree", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/feature"),
	)

	_, err := repo.PushAndCreatePR(context.Background(), "", "add new feature", "")
	if err != nil {
		t.Fatalf("PushAndCreatePR failed: %v", err)
	}

	_, title, _, _ := gh.FindPR(context.Background(), "ralph/test/feature", "")
	if strings.Contains(title, "[") {
		t.Errorf("PR title should not contain brackets when no bead ID, got: %q", title)
	}
}

// AutoMergeCurrentBranch returns nil when no worktree branch is set,
// so --auto-merge is a safe no-op without worktree isolation.
func TestAutoMergeCurrentBranch_SkipsWhenNoWorktreeBranch(t *testing.T) {
	repo := newRepoForTest(
		Config{WorkDir: "/some/dir", ProjectDir: "/some/dir", BaseBranch: "main", Logger: &testLog{}},
		nil,
	)
	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if merged {
		t.Error("expected merged=false when no worktree branch")
	}
}

// AutoMergeCurrentBranch returns 0 and logs "No PR found" when no PR
// exists for the branch, so an unpushed branch doesn't cause a failure.
func TestAutoMergeCurrentBranch_SkipsWhenNoPR(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)

	gh := newStubGitHub(StubGitHubConfig{Available: true})
	log := &testLog{}

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: log},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/unpushed-task"),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Errorf("expected nil error (skip), got %v", err)
	}
	if merged {
		t.Error("expected merged=false when no PR exists")
	}

	if !log.contains("No PR found") {
		t.Error("expected 'No PR found' log message")
	}
}

// mergeOpts always sets DeleteBranch=true since each task gets its own branch.
func TestMergeOpts_AlwaysDeletesBranch(t *testing.T) {
	repo := newRepoForTest(Config{}, nil)
	opts := repo.mergeOpts()
	if !opts.DeleteBranch {
		t.Fatal("mergeOpts should always set DeleteBranch")
	}
}

// ResolveConflict rebases onto the default branch and force-pushes, so
// the PR branch is updated and the merge can be retried.
func TestResolveConflict_RebasesAndForcePushes(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	repo := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
	)
	if err := repo.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	repo.RenameBranchForTask("conflict task", "")

	// Create a commit in the worktree so we have something to push
	writeFile(t, repo.workDir, "feature.txt", "feature content\n")
	run(t, "git", "-C", repo.workDir, "commit", "-m", "add feature")

	// Push the branch so force-push has a remote tracking branch
	run(t, "git", "-C", repo.workDir, "push", "-u", "origin", repo.worktreeBranch)

	// Add a commit to origin/main to make rebase non-trivial
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	bare := filepath.Join(filepath.Dir(project), "bare.git")
	run(t, "git", "clone", bare, tmpClone)
	run(t, "git", "-C", tmpClone, "config", "user.name", "test")
	run(t, "git", "-C", tmpClone, "config", "user.email", "test@test")
	writeFile(t, tmpClone, "other.txt", "other content\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "divergent commit")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	err := repo.ResolveConflict(context.Background())
	if err != nil {
		t.Fatalf("ResolveConflict: %v", err)
	}

	// Verify we rebased onto the latest main (divergent commit should be ancestor)
	divergentRev := gitOutput(tmpClone, "rev-parse", "HEAD")
	if gitCmdErr(repo.workDir, "merge-base", "--is-ancestor", divergentRev, "HEAD") != nil {
		t.Error("expected worktree HEAD to be based on the divergent commit after rebase")
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

	repo := newRepoForTest(
		Config{ProjectDir: project, WorkDir: wtDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
		withWorktreeBranch("ralph/test-squash"),
	)

	if err := repo.Push(context.Background()); err != nil {
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
	dir := t.TempDir()
	runner := newStubRunner()
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("fetch", "", nil)
	runner.On("rev-parse origin/main", "abc123", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-parse HEAD", "def456", nil)
	runner.On("log -1 --format=%s", "single commit", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("push", "", nil)

	repo := newRepoForTest(
		Config{ProjectDir: dir, WorkDir: filepath.Join(dir, "wt"), BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(runner),
		withWorktreeBranch("ralph/test-single"),
	)

	if err := repo.Push(context.Background()); err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	if !runner.CalledWith("push") {
		t.Error("expected push to be called")
	}
	if runner.CalledWith("reset", "--soft") {
		t.Error("single commit should not trigger squash (reset --soft)")
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

	repo := newRepoForTest(
		Config{ProjectDir: project, WorkDir: wtDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
		withWorktreeBranch("ralph/test-fix"),
	)

	if err := repo.Push(context.Background()); err != nil {
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

	fooRepo := newRepoForTest(
		Config{ProjectDir: project, WorkDir: fooDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
		withWorktreeBranch("ralph/foo"),
	)
	if err := fooRepo.Push(context.Background()); err != nil {
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

	barRepo := newRepoForTest(
		Config{ProjectDir: project, WorkDir: barDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
		withWorktreeBranch("ralph/bar"),
		withPrevBranch("ralph/foo"),
	)
	if err := barRepo.Push(context.Background()); err != nil {
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

// Push calls verify.CompileCheck before pushing when compileCheckTimeout
// is set. A broken Go project triggers compile check failure which aborts
// the push rather than sending broken code to the remote.
func TestPush_CompileCheckBlocksPush(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test-prepush", wtDir)

	// Create a broken Go project so CompileCheck fails.
	writeFile(t, wtDir, "go.mod", "module broken\ngo 1.21\n")
	writeFile(t, wtDir, "main.go", "package main\nfunc main() { undefined() }\n")
	run(t, "git", "-C", wtDir, "add", "-A")
	run(t, "git", "-C", wtDir, "commit", "-m", "add broken code")

	repo := newRepoForTest(
		Config{
			ProjectDir:          project,
			WorkDir:             wtDir,
			BaseBranch:          "main",
			Logger:              &testLog{},
			CompileCheckTimeout: 30 * time.Second,
		},
		nil,
		withRunner(&execRunner{}),
		withWorktreeBranch("ralph/test-prepush"),
	)

	err := repo.Push(context.Background())
	if err == nil {
		t.Fatal("expected Push to fail when compile check fails")
	}
	if !strings.Contains(err.Error(), "pre-push compile check failed") {
		t.Errorf("expected compile check error, got: %v", err)
	}

	// Verify nothing was pushed to remote.
	out := cmdOutput(t, "git", "-C", bare, "branch", "--list", "ralph/test-prepush")
	if strings.TrimSpace(out) != "" {
		t.Error("branch should not exist on remote after compile check failure")
	}
}

// Push proceeds when compile check passes (no build system in worktree).
func TestPush_CompileCheckPasses(t *testing.T) {
	dir := t.TempDir()
	runner := newStubRunner()
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("fetch", "", nil)
	runner.On("rev-parse origin/main", "abc123", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-parse HEAD", "def456", nil)
	runner.On("log -1 --format=%s", "add feature", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("push", "", nil)

	repo := newRepoForTest(
		Config{
			ProjectDir: dir,
			WorkDir:    filepath.Join(dir, "wt"),
			BaseBranch: "main",
			Logger:     &testLog{},
		},
		nil,
		withRunner(runner),
		withWorktreeBranch("ralph/test-prepush-pass"),
	)

	if err := repo.Push(context.Background()); err != nil {
		t.Fatalf("Push should succeed: %v", err)
	}

	if !runner.CalledWith("push") {
		t.Error("expected push to be called")
	}
}

// AutoMergeCurrentBranch returns merged=true without pushing when the PR
// for the branch is already merged, so the caller can close the bead.
func TestAutoMergeCurrentBranch_ReturnsMergedForAlreadyMergedPR(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)

	// World has a merged PR for the branch. FindOpenPR returns 0 (state != open),
	// FindPR returns the PR so AutoMergeCurrentBranch follows the already-merged path.
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 438,
			Branch: "ralph/test/01-merged",
			State:  PRStateMerged,
		}},
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-merged"),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
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

	// PR must remain in Merged state (SUT did not attempt another merge).
	pr, _ := gh.GetPR(context.Background(), "", 438)
	if pr == nil || pr.State != PRStateMerged {
		t.Errorf("expected PR 438 to remain merged, got state=%v", pr)
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

	// World starts with a closed PR on the branch. The SUT should reopen it and
	// then successfully merge it. Observable: final state is Merged.
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 438,
			Branch: "ralph/test/01-reopen",
			Title:  "[ralph-rvta] Fix closed PR",
			State:  PRStateClosed,
		}},
		Checks: map[int][]CICheckResult{438: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}}},
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-reopen"),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !merged {
		t.Fatal("expected merged=true after reopening closed PR")
	}

	// After the SUT drives the pipeline, PR #438 must end up merged — proves
	// both reopen (closed → open) and subsequent merge (open → merged) happened.
	pr, _ := gh.GetPR(context.Background(), "", 438)
	if pr == nil || pr.State != PRStateMerged {
		t.Errorf("expected PR 438 to end up merged after reopen + auto-merge, got state=%v", pr)
	}
}

// CreatePR reopens a closed PR when gh pr create fails because a PR already
// exists for the branch, rather than returning an error.
func TestCreatePR_ReopensClosedPROnCreateFailure(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)

	// World: a closed PR #438 exists for the branch. `gh pr create` fails
	// with the "already exists" error, triggering the reopen path.
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 438,
			Branch: "ralph/test/01-reopen-create",
			State:  PRStateClosed,
		}},
		CreatePRErr: fmt.Errorf("a pull request already exists for branch"),
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-reopen-create"),
	)

	prNumber, err := repo.CreatePR(context.Background(), "ralph-test", "Fix bug", "body")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if prNumber != 438 {
		t.Errorf("expected reopened PR number 438, got %d", prNumber)
	}

	// Reopen observable via state: PR #438 must be Open after the SUT's call.
	pr, _ := gh.GetPR(context.Background(), "", 438)
	if pr == nil || pr.State != PRStateOpen {
		t.Errorf("expected PR 438 to be reopened (state=Open), got state=%v", pr)
	}
}

// CreatePR returns the merged PR number without error when the branch already has
// a merged PR — the push landed commits into the existing merged PR, so there is
// nothing left to do. No reopen or API-create fallback should be attempted.
func TestCreatePR_ReturnsMergedPRNumberOnAlreadyExists(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)

	// World: a merged PR #438 exists for the branch. `gh pr create` fails
	// with a 422 "already exists" error; code should detect the merged state
	// and return the merged PR number rather than attempting reopen/API create.
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number: 438,
			Branch: "ralph/already-merged-branch",
			State:  PRStateMerged,
		}},
		CreatePRErr: fmt.Errorf("A pull request already exists for brokenalarms:ralph/already-merged-branch"),
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/already-merged-branch"),
	)

	prNumber, err := repo.CreatePR(context.Background(), "ralph-test", "Fix merged", "body")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if prNumber != 438 {
		t.Errorf("expected merged PR number 438, got %d", prNumber)
	}

	// PR must remain Merged (no reopen attempted).
	pr, _ := gh.GetPR(context.Background(), "", 438)
	if pr == nil || pr.State != PRStateMerged {
		t.Errorf("expected PR 438 to remain merged, got state=%v", pr)
	}
}

// When branch protection persistently blocks the merge (405), AutoMergeCurrentBranch
// returns an error without merged=true. No admin bypass is ever used.
func TestAutoMergeCurrentBranch_BlockedMergeReturnsError(t *testing.T) {
	project := t.TempDir()
	stubCISleep(t)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:  55,
			Branch:  "ralph/test/01-blocked-no-bypass",
			State:   PRStateOpen,
			Blocked: true, // world: branch protection blocks merge
		}},
		Checks: map[int][]CICheckResult{55: {{Name: "ci", State: "SUCCESS", Bucket: "pass"}}},
	})

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

	repo := newRepoForTest(
		Config{
			ProjectDir: project,
			WorkDir:    filepath.Join(t.TempDir(), "wt"),
			BaseBranch: "main",
			Logger:     &testLog{},
		},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-blocked-no-bypass"),
	)

	merged, err := repo.AutoMergeCurrentBranch(context.Background())
	if merged {
		t.Error("expected merged=false when branch protection blocks merge")
	}
	if err == nil {
		t.Fatal("expected error when merge is blocked by branch protection")
	}

	// PR must remain Open (never merged).
	pr, _ := gh.GetPR(context.Background(), "", 55)
	if pr == nil || pr.State != PRStateOpen {
		t.Errorf("expected PR 55 to remain open after blocked merge, got state=%v", pr)
	}
}

// FlushUnpushedWork on the placeholder branch does not attempt push or PR
// creation — there is no task work to flush.
func TestFlushUnpushedWork_SkipsOnPlaceholderBranch(t *testing.T) {
	dir := t.TempDir()
	runner := newStubRunner()

	repo := newRepoForTest(
		Config{ProjectDir: dir, WorkDir: dir, BaseBranch: "main", Logger: discardLog{}},
		nil,
		withRunner(runner),
		withWorktreeBranch(WipBranchName()),
	)

	merged, err := repo.FlushUnpushedWork(context.Background(), "ralph-test", "test task", false)
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

// FlushUnpushedWork when origin/<branch> already has HEAD (0 commits ahead)
// returns (false, nil) without calling PushAndCreatePR — the previous task
// already shipped and there is nothing to flush.
func TestFlushUnpushedWork_SkipsWhenNoUnpushedCommits(t *testing.T) {
	dir := t.TempDir()
	runner := newStubRunner()
	// rev-parse --verify returns success → origin/branch exists
	runner.On("rev-parse", "", nil)
	// rev-list origin/branch..HEAD --count returns 0 → HEAD is not ahead
	runner.On("rev-list", "0", nil)

	repo := newRepoForTest(
		Config{ProjectDir: dir, WorkDir: dir, BaseBranch: "main", Logger: discardLog{}},
		nil,
		withRunner(runner),
		withWorktreeBranch("ralph/some-task"),
	)

	merged, err := repo.FlushUnpushedWork(context.Background(), "ralph-ne9f", "skip flush test", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged {
		t.Error("expected merged=false when no unpushed commits")
	}
	if runner.CalledWith("push") {
		t.Error("must not push when HEAD is not ahead of origin/branch")
	}
}

// FlushUnpushedWork when origin/<branch> doesn't exist but HEAD has no commits
// ahead of origin/main (e.g. after a squash-merge deleted the remote branch)
// returns (false, nil) without calling PushAndCreatePR.
func TestFlushUnpushedWork_SkipsWhenNoBranchAndNotAheadOfMain(t *testing.T) {
	dir := t.TempDir()
	runner := newStubRunner()
	// rev-parse --verify fails → origin/<branch> does not exist
	runner.On("rev-parse", "", fmt.Errorf("unknown ref"))
	// rev-list origin/main..HEAD --count returns "0" → HEAD is at origin/main
	runner.On("rev-list", "0", nil)

	repo := newRepoForTest(
		Config{ProjectDir: dir, WorkDir: dir, BaseBranch: "main", Logger: discardLog{}},
		nil,
		withRunner(runner),
		withWorktreeBranch("ralph/already-merged"),
	)

	merged, err := repo.FlushUnpushedWork(context.Background(), "ralph-k7hl", "already merged task", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged {
		t.Error("expected merged=false when branch has no commits ahead of main")
	}
	if runner.CalledWith("push") {
		t.Error("must not push when origin/<branch> absent and HEAD is at origin/main")
	}
}

// Ship must refuse to create a PR when the resolved base branch is neither
// cfg.BaseBranch nor the active stack parent — prevents orphaned PRs targeting
// a stale/wrong branch (the sharpe 2026-05-28 failure scenario).
// RemoteDefaultBranch reads the repo's default branch from origin/HEAD so
// ralph merge can populate cfg.BaseBranch without an explicit flag. Before this
// fix, handleMerge left BaseBranch empty and the base-branch guard refused every
// merge ("resolved base \"main\" is neither cfg.BaseBranch (\"\") ...").
func TestRemoteDefaultBranch_ReadsOriginHead(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	if got := RemoteDefaultBranch(project); got != "main" {
		t.Errorf("expected origin/HEAD default branch \"main\", got %q", got)
	}
}

func TestRemoteDefaultBranch_FallsBackToMainWhenUnset(t *testing.T) {
	dir := t.TempDir()
	run(t, "git", "init", "-b", "trunk", dir)
	// No origin remote configured, so refs/remotes/origin/HEAD is unset.
	if got := RemoteDefaultBranch(dir); got != "main" {
		t.Errorf("expected fallback \"main\" when origin/HEAD is unset, got %q", got)
	}
}

// With BaseBranch populated from RemoteDefaultBranch (the merge-command path),
// assertValidBase accepts the detected default and still rejects a mismatched
// stack base — the guard keeps its anti-stale value.
func TestAssertValidBase_MergeStyleDetectedDefault(t *testing.T) {
	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project", BaseBranch: "main", Logger: discardLog{}},
		nil,
	)
	if err := repo.assertValidBase("main"); err != nil {
		t.Errorf("expected nil when stack base matches detected default, got %v", err)
	}
	if err := repo.assertValidBase("develop"); err == nil {
		t.Error("expected guard error when stack base differs from detected default branch")
	}
}

func TestShip_RejectsUnexpectedBase(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("fetch", "", nil)
	runner.On("diff --quiet", "", nil)
	runner.On("diff --cached --quiet", "", nil)
	runner.On("rev-parse", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("rev-list --count", "1", nil)
	runner.On("push", "", nil)

	gh := newStubGitHub(StubGitHubConfig{Available: true})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: discardLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-unexpected-base"),
	)

	// Pass an explicit base that is neither "main" (cfg.BaseBranch) nor prevBranch.
	result, err := repo.Ship(context.Background(), ShipOpts{
		TaskID:     "ralph-z999",
		TaskTitle:  "test task",
		BaseBranch: "claude/some-stale-bot-branch",
		AutoMerge:  false,
	})
	if err == nil {
		t.Fatal("expected error when resolved base is not cfg.BaseBranch or active stack parent")
	}
	if result.PRNumber != 0 {
		t.Errorf("expected PRNumber=0 when base is rejected, got %d", result.PRNumber)
	}
	if !strings.Contains(err.Error(), "base branch guard") {
		t.Errorf("expected 'base branch guard' in error message, got: %v", err)
	}
	// The gh stub must not have received a CreatePR call — guard fires before API.
	// We confirm by checking that no PR was created in the fake world.
	branches, _ := gh.ListOpenPRBranches(context.Background(), "https://github.com/test/repo.git")
	if len(branches) != 0 {
		t.Errorf("expected no PR created, but found open PR branches: %v", branches)
	}
}

// executeMerge must return an error when the merged SHA is not an ancestor of
// origin/<BaseBranch> — catches the case where a merge landed on a dead/stale
// lineage rather than the real base branch (the sharpe 2026-05-28 failure).
func TestExecuteMerge_PostMergeAncestorCheckFails(t *testing.T) {
	runner := newStubRunner()
	runner.On("remote get-url origin", "https://github.com/test/repo.git", nil)
	runner.On("fetch", "", nil)
	// Post-merge ancestor check: always fails — merged SHA not on base branch.
	runner.On("merge-base --is-ancestor", "", errors.New("not ancestor of base"))

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{{
			Number:  42,
			Branch:  "ralph/test/01-orphan",
			HeadSHA: "deadbeef123",
			State:   PRStateOpen,
		}},
	})

	repo := newRepoForTest(
		Config{ProjectDir: "/project", WorkDir: "/project/wt", BaseBranch: "main", Logger: &testLog{}},
		gh,
		withRunner(runner),
		withWorktreeBranch("ralph/test/01-orphan"),
	)

	merged, err := repo.executeMerge(context.Background(), 42, "https://github.com/test/repo.git")
	if merged {
		t.Error("expected merged=false when post-merge ancestor check fails")
	}
	if err == nil {
		t.Fatal("expected error when merged SHA is not an ancestor of base branch")
	}
	if !strings.Contains(err.Error(), "NOT an ancestor") {
		t.Errorf("expected 'NOT an ancestor' in error, got: %v", err)
	}
}
