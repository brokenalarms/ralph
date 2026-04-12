package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initBareRepoWithOrigin creates a bare repo, a clone (project dir), and sets
// up origin/HEAD — suitable for rebase tests that need a proper remote.
func initBareRepoWithOrigin(t *testing.T) (projectDir string, bareDir string) {
	t.Helper()
	tmp := t.TempDir()

	bare := filepath.Join(tmp, "bare.git")
	run(t, "git", "init", "--bare", "-b", "main", bare)

	project := filepath.Join(tmp, "project")
	run(t, "git", "clone", bare, project)
	run(t, "git", "-C", project, "config", "user.name", "test")
	run(t, "git", "-C", project, "config", "user.email", "test@test")
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "init")
	run(t, "git", "-C", project, "push", "-u", "origin", "main")
	run(t, "git", "-C", project, "remote", "set-head", "origin", "main")

	return project, bare
}

// setupRebaseMgr creates a Manager with a worktree ready for rebase testing.
// The worktree's origin points at the bare repo so fetch/push work correctly.
func setupRebaseMgr(t *testing.T, project, bare string) *Repo {
	t.Helper()
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := &Repo{
		projectDir:  project,
		baseBranch: "main",
		ralphDir:    ralphDir,
				state:       state,
		logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// Point worktree's origin at the bare repo
	run(t, "git", "-C", mgr.workDir, "remote", "set-url", "origin", bare)
	run(t, "git", "-C", mgr.workDir, "fetch", "origin")
	// Ensure git identity is configured (worktrees share config with parent,
	// but set explicitly so tests don't depend on global git config in CI)
	run(t, "git", "-C", mgr.workDir, "config", "user.name", "test")
	run(t, "git", "-C", mgr.workDir, "config", "user.email", "test@test")

	return mgr
}

// pushToOrigin pushes main from the project dir to origin.
func pushToOrigin(t *testing.T, projectDir string) {
	t.Helper()
	run(t, "git", "-C", projectDir, "push", "origin", "main", "-q")
}

// writeFile creates/overwrites a file and stages it.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	run(t, "git", "-C", dir, "add", name)
}


// Clean rebase succeeds when no squash merges have happened
func TestRebaseOntoDefaultBranch_CleanRebase(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// Add a commit on main
	writeFile(t, project, "mainfile.txt", "new file on main\n")
	run(t, "git", "-C", project, "commit", "-m", "add mainfile")
	pushToOrigin(t, project)

	// Add a commit in the worktree
	writeFile(t, mgr.workDir, "workfile.txt", "worktree file\n")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "add workfile")

	if err := mgr.RebaseOntoDefaultBranch(context.Background()); err != nil {
		t.Fatalf("RebaseOntoDefaultBranch failed: %v", err)
	}

	// Both files should be present after rebase
	if _, err := os.Stat(filepath.Join(mgr.workDir, "mainfile.txt")); err != nil {
		t.Error("mainfile.txt should exist after rebase")
	}
	if _, err := os.Stat(filepath.Join(mgr.workDir, "workfile.txt")); err != nil {
		t.Error("workfile.txt should exist after rebase")
	}
}

// Real conflicts return an error — caller decides what to do
// Rebase conflicts with local work cause the stack to diverge —
// returns nil (not an error), logs the divergence.
func TestRebaseOntoDefaultBranch_DivergesOnConflict(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	writeFile(t, mgr.workDir, "conflict.txt", "worktree version\n")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "worktree change")

	writeFile(t, project, "conflict.txt", "main version\n")
	run(t, "git", "-C", project, "commit", "-m", "main change")
	pushToOrigin(t, project)

	err := mgr.RebaseOntoDefaultBranch(context.Background())
	if err != nil {
		t.Fatalf("expected nil (stack diverges), got: %v", err)
	}
}

// Rebase is skipped when already up to date with origin
func TestRebaseOntoDefaultBranch_AlreadyUpToDate(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// No divergence — worktree is at same point as origin/main
	if err := mgr.RebaseOntoDefaultBranch(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	log := mgr.logger.(*testLog)
	if !log.contains("Already up to date") {
		t.Error("expected 'Already up to date' log message")
	}
}

// When HEAD is behind origin/main (e.g., after PostMergeReset followed by
// main advancing via another squash merge), rebase must fast-forward to
// include the new main commits rather than skipping with "already up to date".
func TestRebaseOntoDefaultBranch_FastForwardsWhenBehind(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// Simulate PostMergeReset: worktree is at origin/main.
	// Then main advances (another PR squash-merged on GitHub).
	writeFile(t, project, "newfeature.txt", "merged by someone else\n")
	run(t, "git", "-C", project, "commit", "-m", "squash: another PR")
	pushToOrigin(t, project)

	// Worktree HEAD is now behind origin/main
	if err := mgr.RebaseOntoDefaultBranch(context.Background()); err != nil {
		t.Fatalf("RebaseOntoDefaultBranch failed: %v", err)
	}

	// The new file from main should be present after rebase
	if _, err := os.Stat(filepath.Join(mgr.workDir, "newfeature.txt")); err != nil {
		t.Error("newfeature.txt should exist after rebasing onto advanced main")
	}

	log := mgr.logger.(*testLog)
	if log.contains("Already up to date") {
		t.Error("should NOT say 'Already up to date' when HEAD is behind origin/main")
	}
}

// When HEAD is ahead of origin/main (has local commits), rebase correctly
// identifies the branch as up-to-date since it already includes main.
func TestRebaseOntoDefaultBranch_SkipsWhenAhead(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// Add a local commit — HEAD is ahead of origin/main
	writeFile(t, mgr.workDir, "local.txt", "local work\n")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "local commit")

	if err := mgr.RebaseOntoDefaultBranch(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	log := mgr.logger.(*testLog)
	if !log.contains("Already up to date") {
		t.Error("expected 'Already up to date' when HEAD is ahead of origin/main")
	}
}


// Resuming a worktree fetches origin/main so subsequent rebase uses fresh refs,
// not stale local copies from the previous run.
func TestTryResumeWorktree_FetchesOriginOnResume(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// Record origin/main before any new commits
	oldRef := gitOutput(mgr.workDir, "rev-parse", "origin/main")

	// Push a new commit to origin (simulates main advancing while ralph was idle)
	writeFile(t, project, "newfile.txt", "pushed while idle\n")
	run(t, "git", "-C", project, "commit", "-m", "advance main")
	pushToOrigin(t, project)

	// Simulate resume: store worktree state, then call tryResumeWorktree
	_ = mgr.state.Write("worktree_dir", mgr.workDir)
	_ = mgr.state.Write("worktree_branch", mgr.worktreeBranch)

	if err := mgr.tryResumeWorktree(); err != nil {
		t.Fatalf("tryResumeWorktree: %v", err)
	}

	// After resume, origin/main should point at the new commit
	newRef := gitOutput(mgr.workDir, "rev-parse", "origin/main")
	if newRef == oldRef {
		t.Error("origin/main was not updated on resume — fetch did not run or failed silently")
	}
}


// TagTaskStart creates a git tag using the bd task ID when available
func TestTagTaskStart_WithTaskID(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Repo{
		projectDir:  project,
		baseBranch: "main",
		ralphDir:    ralphDir,
				state:       newMemState(),
		logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.TagTaskStart("ralph-abc")

	if !refExists(mgr.workDir, "task/ralph-abc/start") {
		t.Error("expected tag task/ralph-abc/start to exist")
	}
}

// TagTaskEnd creates an end tag at the current HEAD
func TestTagTaskEnd_WithTaskID(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Repo{
		projectDir:  project,
		baseBranch: "main",
		ralphDir:    ralphDir,
				state:       newMemState(),
		logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.TagTaskEnd("ralph-abc")

	if !refExists(mgr.workDir, "task/ralph-abc/end") {
		t.Error("expected tag task/ralph-abc/end to exist")
	}
}

// Tags fall back to the seq-slug from the branch name when no task ID is provided
func TestTagTaskStart_FallbackToSeqSlug(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Repo{
		projectDir:  project,
		baseBranch: "main",
		ralphDir:    ralphDir,
				state:       newMemState(),
		logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("Add user auth", "")
	mgr.TagTaskStart("")

	if !refExists(mgr.workDir, "task/add-user-auth/start") {
		t.Error("expected tag task/add-user-auth/start to exist")
	}
}

// Tags are no-ops when running without a worktree (WorkDir == ProjectDir)
func TestTagTaskStart_NoOpWithoutWorktree(t *testing.T) {
	mgr := &Repo{
		projectDir: "/some/dir",
		baseBranch: "main",
		workDir:    "/some/dir",
		logger:     &testLog{},
	}
	mgr.TagTaskStart("ralph-abc")
}

// Tags on the wip branch are skipped (no meaningful slug to extract)
func TestTagTaskStart_SkipsWipBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Repo{
		projectDir:  project,
		baseBranch: "main",
		ralphDir:    ralphDir,
				state:       newMemState(),
		logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// Branch is still ralph/project/wip — no task ID → no tag
	mgr.TagTaskStart("")

	tags := gitOutput(mgr.workDir, "tag", "-l", "task/*")
	if tags != "" {
		t.Errorf("expected no tags on /wip branch, got: %s", tags)
	}
}

// Start and end tags point at different commits when work happens between them
func TestTagStartEnd_DifferentCommits(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Repo{
		projectDir:  project,
		baseBranch: "main",
		ralphDir:    ralphDir,
				state:       newMemState(),
		logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.TagTaskStart("ralph-xyz")
	startRev := gitOutput(mgr.workDir, "rev-parse", "task/ralph-xyz/start")

	writeFile(t, mgr.workDir, "work.txt", "some work\n")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "do work")

	mgr.TagTaskEnd("ralph-xyz")
	endRev := gitOutput(mgr.workDir, "rev-parse", "task/ralph-xyz/end")

	if startRev == endRev {
		t.Error("start and end tags should point at different commits after work was done")
	}
}


// When PrevBranch is set but the branch no longer exists on the remote
// (e.g. merged and deleted), EnsureUpToDate silently falls back to the
// default branch instead of logging a warning — the missing branch is
// an expected condition, not an error.
func TestEnsureUpToDate_FallsBackSilentlyWhenPrevBranchMissing(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	mgr.prevBranch = "nonexistent-branch"

	log := mgr.logger.(*testLog)
	log.messages = nil

	err := mgr.EnsureUpToDate(context.Background())
	if err != nil {
		t.Fatalf("EnsureUpToDate should not error, got: %v", err)
	}

	if mgr.prevBranch != "" {
		t.Errorf("PrevBranch should be cleared after fallback, got %q", mgr.prevBranch)
	}

	for _, msg := range log.messages {
		if strings.Contains(strings.ToLower(msg), "fail") && strings.Contains(msg, "nonexistent") {
			t.Errorf("should not warn about missing PrevBranch, got: %q", msg)
		}
	}
}

func TestRebaseOntoDefaultBranch_CancelledContextReturnsContextError(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	writeFile(t, project, "mainfile.txt", "main content\n")
	run(t, "git", "-C", project, "commit", "-m", "advance main")
	pushToOrigin(t, project)

	writeFile(t, mgr.workDir, "workfile.txt", "worktree content\n")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "worktree commit")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := mgr.RebaseOntoDefaultBranch(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %T: %v", err, err)
	}

	var conflictErr *RebaseConflictError
	if errors.As(err, &conflictErr) {
		t.Error("cancelled context must not be misinterpreted as RebaseConflictError")
	}
}

