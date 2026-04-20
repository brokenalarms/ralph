package git

import (
	"context"
	"errors"
	"fmt"
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
func setupRebaseMgr(t *testing.T, project, bare string) *repo {
	t.Helper()
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := newRepoForTest(Config{
		ProjectDir: project,
		BaseBranch: "main",
		RalphDir:   ralphDir,
		Logger:     &testLog{},
	}, nil, withRunner(&execRunner{}), withState(state))
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

// Genuine divergence (both sides modified the same line differently) causes
// EnsureUpToDate to return *LocalRebaseConflictError so callers know the
// branch could not be synced. The error is distinct from
// *UnresolvedConflictError (which carries PR-merge semantics).
func TestRebaseOntoDefaultBranch_DivergesOnConflict(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	writeFile(t, mgr.workDir, "conflict.txt", "worktree version\n")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "worktree change")

	writeFile(t, project, "conflict.txt", "main version\n")
	run(t, "git", "-C", project, "commit", "-m", "main change")
	pushToOrigin(t, project)

	err := mgr.RebaseOntoDefaultBranch(context.Background())
	if err == nil {
		t.Fatal("expected LocalRebaseConflictError, got nil")
	}
	var conflictErr *LocalRebaseConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected *LocalRebaseConflictError, got %T: %v", err, err)
	}
	if strings.Contains(err.Error(), "PR #") {
		t.Errorf("error message should not reference a PR number: %v", err)
	}
}

// A local commit that was squash-merged into origin/main (different SHA but
// same diff content) is detected and skipped by rebasecontinue — EnsureUpToDate
// returns nil without leaving phantom conflicts.
func TestEnsureUpToDate_SquashMergedCommit(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// Add a local commit in the worktree.
	writeFile(t, mgr.workDir, "feature.txt", "new feature\n")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "add feature")

	// Squash-merge the same change into main with a different commit message/SHA.
	writeFile(t, project, "feature.txt", "new feature\n")
	run(t, "git", "-C", project, "commit", "-m", "squash: add feature (#42)")
	pushToOrigin(t, project)

	err := mgr.EnsureUpToDate(context.Background())
	if err != nil {
		t.Fatalf("EnsureUpToDate should return nil for squash-merged commit, got: %v", err)
	}
}

// Genuine divergence: local commit and origin/main commit both modify the same
// line differently. EnsureUpToDate must return a *LocalRebaseConflictError
// carrying the branch and base names — callers on startup/branch-setup paths
// treat this as recoverable, while the merge-retry pipeline wraps it into
// *UnresolvedConflictError for PR semantics.
func TestEnsureUpToDate_GenuineConflictReturnsError(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// Create a shared file first so both sides can diverge from it.
	writeFile(t, project, "shared.txt", "original line\n")
	run(t, "git", "-C", project, "commit", "-m", "add shared")
	pushToOrigin(t, project)
	run(t, "git", "-C", mgr.workDir, "fetch", "origin")
	run(t, "git", "-C", mgr.workDir, "reset", "--hard", "origin/main")

	// Local branch modifies the file one way.
	writeFile(t, mgr.workDir, "shared.txt", "local change\n")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "local edit")

	// Main modifies the same file a different way.
	writeFile(t, project, "shared.txt", "main change\n")
	run(t, "git", "-C", project, "commit", "-m", "main edit")
	pushToOrigin(t, project)

	err := mgr.EnsureUpToDate(context.Background())
	if err == nil {
		t.Fatal("expected LocalRebaseConflictError, got nil")
	}
	var conflictErr *LocalRebaseConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected *LocalRebaseConflictError, got %T: %v", err, err)
	}
	if conflictErr.Base != "main" {
		t.Errorf("expected Base=main, got %q", conflictErr.Base)
	}
	if strings.Contains(err.Error(), "PR #") {
		t.Errorf("error message should not reference a PR number: %v", err)
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

	mgr := newRepoForTest(Config{
		ProjectDir: project,
		BaseBranch: "main",
		RalphDir:   ralphDir,
		Logger:     &testLog{},
	}, nil, withRunner(&execRunner{}))
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

	mgr := newRepoForTest(Config{
		ProjectDir: project,
		BaseBranch: "main",
		RalphDir:   ralphDir,
		Logger:     &testLog{},
	}, nil, withRunner(&execRunner{}))
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
	dir := t.TempDir()
	runner := newStubRunner()
	runner.On("branch -m", "", nil)
	runner.On("tag", "", nil)

	mgr := newRepoForTest(Config{
		ProjectDir: dir,
		WorkDir:    filepath.Join(dir, "wt"),
		BaseBranch: "main",
		Logger:     &testLog{},
	}, nil, withRunner(runner), withWorktreeBranch("ralph/next"))

	mgr.RenameBranchForTask("Add user auth", "")
	mgr.TagTaskStart("")

	if !runner.CalledWith("tag", "-f", "task/add-user-auth/start") {
		t.Error("expected tag task/add-user-auth/start to be created")
	}
}

// Tags are no-ops when running without a worktree (WorkDir == ProjectDir)
func TestTagTaskStart_NoOpWithoutWorktree(t *testing.T) {
	mgr := newRepoForTest(Config{
		ProjectDir: "/some/dir",
		BaseBranch: "main",
		WorkDir:    "/some/dir",
		Logger:     &testLog{},
	}, nil)
	mgr.TagTaskStart("ralph-abc")
}

// Tags on the wip branch are skipped (no meaningful slug to extract)
func TestTagTaskStart_SkipsWipBranch(t *testing.T) {
	dir := t.TempDir()
	runner := newStubRunner()

	mgr := newRepoForTest(Config{
		ProjectDir: dir,
		WorkDir:    filepath.Join(dir, "wt"),
		BaseBranch: "main",
		Logger:     &testLog{},
	}, nil, withRunner(runner), withWorktreeBranch(WipBranchName()))

	mgr.TagTaskStart("")

	if runner.CalledWith("tag") {
		t.Error("expected no tag command on wip branch")
	}
}

// Start and end tags point at different commits when work happens between them
func TestTagStartEnd_DifferentCommits(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := newRepoForTest(Config{
		ProjectDir: project,
		BaseBranch: "main",
		RalphDir:   ralphDir,
		Logger:     &testLog{},
	}, nil, withRunner(&execRunner{}))
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
	dir := t.TempDir()
	runner := newStubRunner()
	runner.On("fetch origin nonexistent-branch", "", fmt.Errorf("couldn't find remote ref"))
	runner.On("fetch origin main", "", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", nil)
	runner.On("merge-base --is-ancestor", "", nil)

	log := &testLog{}
	mgr := newRepoForTest(Config{
		ProjectDir: dir,
		WorkDir:    filepath.Join(dir, "wt"),
		BaseBranch: "main",
		Logger:     log,
	}, nil, withRunner(runner), withWorktreeBranch("ralph/some-task"), withPrevBranch("nonexistent-branch"))

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
	dir := t.TempDir()
	runner := newStubRunner()
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("fetch", "", fmt.Errorf("signal: killed"))

	mgr := newRepoForTest(Config{
		ProjectDir: dir,
		WorkDir:    filepath.Join(dir, "wt"),
		BaseBranch: "main",
		Logger:     &testLog{},
	}, nil, withRunner(runner), withWorktreeBranch("ralph/some-task"))

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

// validateStackParent clears prevBranch when the remote branch has been
// deleted (e.g. auto-deleted after its PR merged). This is the source bug
// for HTTP 422 base=invalid on a later CreatePR: setStackHead sets
// prevBranch at session start, then the parent PR merges during the
// iteration and its branch vanishes server-side. Without this guard, Push
// squashes against the stale local ref and CreatePR sends a base that
// GitHub no longer recognises.
func TestValidateStackParent_ClearsWhenRemoteBranchDeleted(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// Create parent branch on origin, one commit ahead of main, and fetch
	// it into the worktree so the local remote-tracking ref exists.
	run(t, "git", "-C", project, "checkout", "-b", "stack-parent")
	writeFile(t, project, "parent.txt", "parent work\n")
	run(t, "git", "-C", project, "commit", "-m", "parent")
	run(t, "git", "-C", project, "push", "origin", "stack-parent")
	run(t, "git", "-C", project, "checkout", "main")
	run(t, "git", "-C", mgr.workDir, "fetch", "origin", "stack-parent")

	mgr.SetPrevBranch("stack-parent")

	// Sanity: with the branch present on origin, validate does not clear.
	mgr.validateStackParent(context.Background())
	if mgr.prevBranch != "stack-parent" {
		t.Fatalf("prevBranch should remain while parent is live, got %q", mgr.prevBranch)
	}

	// Simulate the parent PR merging and its branch being auto-deleted:
	// delete the branch from the bare remote. The worktree's local
	// remote-tracking ref refs/remotes/origin/stack-parent is still
	// present — this is the stale-cache condition that produced the bug.
	run(t, "git", "-C", bare, "branch", "-D", "stack-parent")

	mgr.validateStackParent(context.Background())
	if mgr.prevBranch != "" {
		t.Errorf("prevBranch should be cleared after remote branch deletion, got %q", mgr.prevBranch)
	}
}

// validateStackParent clears prevBranch when the parent branch's tip is
// already on main (regular merge: the original commit is an ancestor of
// main). Stacking a new PR onto an already-landed parent would either be
// rejected by GitHub or produce a PR with no unique diff.
func TestValidateStackParent_ClearsWhenParentLandedOnMain(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// Create parent branch with a commit, push it, then fast-forward main
	// to that same commit so parent's tip becomes an ancestor of main.
	run(t, "git", "-C", project, "checkout", "-b", "stack-parent")
	writeFile(t, project, "parent.txt", "parent work\n")
	run(t, "git", "-C", project, "commit", "-m", "parent work")
	run(t, "git", "-C", project, "push", "origin", "stack-parent")

	run(t, "git", "-C", project, "checkout", "main")
	run(t, "git", "-C", project, "merge", "--ff-only", "stack-parent")
	run(t, "git", "-C", project, "push", "origin", "main")

	run(t, "git", "-C", mgr.workDir, "fetch", "origin")

	mgr.SetPrevBranch("stack-parent")
	mgr.validateStackParent(context.Background())
	if mgr.prevBranch != "" {
		t.Errorf("prevBranch should be cleared when parent has landed on main, got %q", mgr.prevBranch)
	}
}

// validateStackParent does NOT clear prevBranch when the parent is diverged
// from main but still has unmerged work. Pre-fix (ralph-op9h),
// BranchIsAheadOfMain rejected diverged branches, causing the stack parent
// to be wiped any time a pre-push rebase failed or another contributor
// landed a commit on main mid-iteration. The fix uses BranchIsAncestorOfMain
// (true only when work has actually landed via a regular merge) and lets
// the GitHub check distinguish squash-merge from rebase-failure divergence.
func TestValidateStackParent_PreservesDivergedParentWithUnmergedWork(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// Set up a diverged parent branch: branch has its own commit, main has
	// a different commit, and neither is an ancestor of the other.
	run(t, "git", "-C", project, "checkout", "-b", "stack-parent")
	writeFile(t, project, "parent.txt", "parent work\n")
	run(t, "git", "-C", project, "commit", "-m", "parent work")
	run(t, "git", "-C", project, "push", "origin", "stack-parent")

	run(t, "git", "-C", project, "checkout", "main")
	writeFile(t, project, "main.txt", "contributor commit\n")
	run(t, "git", "-C", project, "commit", "-m", "contributor commit on main")
	run(t, "git", "-C", project, "push", "origin", "main")

	run(t, "git", "-C", mgr.workDir, "fetch", "origin")

	mgr.SetPrevBranch("stack-parent")
	mgr.validateStackParent(context.Background())
	if mgr.prevBranch != "stack-parent" {
		t.Errorf("prevBranch should be preserved when parent is diverged but still has unmerged work, got %q", mgr.prevBranch)
	}
}

// validateStackParent is a no-op when prevBranch is empty — the common
// case at session start before any stack is established.
func TestValidateStackParent_EmptyPrevBranchNoOp(t *testing.T) {
	mgr := newRepoForTest(Config{ProjectDir: "/project", WorkDir: "/worktree", BaseBranch: "main", Logger: &testLog{}}, nil)

	mgr.SetPrevBranch("")
	mgr.validateStackParent(context.Background())
	if mgr.prevBranch != "" {
		t.Errorf("prevBranch must remain empty, got %q", mgr.prevBranch)
	}
}

func TestEnsureUpToDate_InvokesValidateStackParent(t *testing.T) {
	runner := newStubRunner()
	runner.On("fetch origin stack-parent", "", fmt.Errorf("couldn't find remote ref"))
	runner.On("fetch origin main", "", nil)
	runner.On("rev-parse --verify", "abc123", nil)
	runner.On("merge-base --is-ancestor", "", nil)

	mgr := newRepoForTest(Config{ProjectDir: "/project", WorkDir: "/worktree", BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner), withWorktreeBranch("ralph/feature"))
	mgr.SetPrevBranch("stack-parent")

	if err := mgr.EnsureUpToDate(context.Background()); err != nil {
		t.Fatalf("EnsureUpToDate should not error when parent vanished, got: %v", err)
	}
	if mgr.prevBranch != "" {
		t.Errorf("EnsureUpToDate must clear stale prevBranch, got %q", mgr.prevBranch)
	}
}
