package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RebaseBranchOntoRemote must rebase the single branch onto the advanced
// base and force-push the result to origin, then remove the temp worktree
// and temp branch it created — proving the single-branch (force-with-lease,
// no --update-refs) path of the shared rebaseInTempWorktree helper works.
func TestRebaseBranchOntoRemote_RebasesAndForcePushesSingleBranch(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	repo := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
	)

	// Branch off main, then advance main so the branch needs rebasing.
	run(t, "git", "-C", project, "checkout", "-b", "feature-x")
	writeFile(t, project, "feature.txt", "feature work\n")
	run(t, "git", "-C", project, "commit", "-m", "feature commit")
	run(t, "git", "-C", project, "push", "-u", "origin", "feature-x")
	run(t, "git", "-C", project, "checkout", "main")

	writeFile(t, project, "mainfile.txt", "advance main\n")
	run(t, "git", "-C", project, "commit", "-m", "advance main")
	run(t, "git", "-C", project, "push", "origin", "main")

	if err := repo.RebaseBranchOntoRemote(context.Background(), "feature-x", "main"); err != nil {
		t.Fatalf("RebaseBranchOntoRemote failed: %v", err)
	}

	// origin/feature-x must now contain both mainfile.txt (rebased in) and
	// feature.txt (the branch's own commit).
	run(t, "git", "-C", project, "fetch", "origin", "feature-x")
	mainOnFeature := gitOutput(project, "show", "origin/feature-x:mainfile.txt")
	if !strings.Contains(mainOnFeature, "advance main") {
		t.Errorf("origin/feature-x missing rebased-in mainfile.txt content, got: %q", mainOnFeature)
	}
	featureFile := gitOutput(project, "show", "origin/feature-x:feature.txt")
	if !strings.Contains(featureFile, "feature work") {
		t.Errorf("origin/feature-x missing its own feature.txt content, got: %q", featureFile)
	}

	// Temp worktree and temp branch must be cleaned up.
	wtDir := filepath.Join(ralphDir, "worktrees", "rebase-feature-x")
	if _, err := os.Stat(wtDir); err == nil {
		t.Errorf("temp worktree %s should have been removed after RebaseBranchOntoRemote", wtDir)
	}
	if repo.refExists(project, "refs/heads/ralph-rebase/feature-x") {
		t.Error("temp branch ralph-rebase/feature-x should have been deleted after RebaseBranchOntoRemote")
	}

	bareRefs := cmdOutput(t, "git", "-C", bare, "for-each-ref", "--format=%(refname)")
	if strings.Contains(bareRefs, "ralph-rebase/feature-x") {
		t.Error("temp branch ralph-rebase/feature-x should not have been pushed to origin")
	}
}

// RebaseBranchOntoRemote must remove the temp worktree and temp branch when
// the rebase hits an unresolvable conflict — the single-branch path is not
// resumable, so leaving the worktree behind would strand it and diverge from
// the pre-refactor behavior that always cleaned up on conflict.
func TestRebaseBranchOntoRemote_ConflictCleansUpWorktree(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	repo := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
	)

	// Seed a shared file on main so both sides edit the same line.
	writeFile(t, project, "shared.txt", "base line\n")
	run(t, "git", "-C", project, "commit", "-m", "add shared")
	run(t, "git", "-C", project, "push", "origin", "main")

	// Branch off main and change the shared line.
	run(t, "git", "-C", project, "checkout", "-b", "feature-x")
	writeFile(t, project, "shared.txt", "feature line\n")
	run(t, "git", "-C", project, "commit", "-m", "feature edit")
	run(t, "git", "-C", project, "push", "-u", "origin", "feature-x")
	run(t, "git", "-C", project, "checkout", "main")

	// Advance main with an incompatible edit to the same line so the rebase
	// conflicts and cannot be auto-resolved.
	writeFile(t, project, "shared.txt", "main line\n")
	run(t, "git", "-C", project, "commit", "-m", "main edit")
	run(t, "git", "-C", project, "push", "origin", "main")

	if err := repo.RebaseBranchOntoRemote(context.Background(), "feature-x", "main"); err == nil {
		t.Fatal("RebaseBranchOntoRemote should have returned an error on unresolvable conflict")
	}

	// The temp worktree and temp branch must be cleaned up even on failure.
	wtDir := filepath.Join(ralphDir, "worktrees", "rebase-feature-x")
	if _, err := os.Stat(wtDir); err == nil {
		t.Errorf("temp worktree %s should have been removed after a conflicting RebaseBranchOntoRemote", wtDir)
	}
	if repo.refExists(project, "refs/heads/ralph-rebase/feature-x") {
		t.Error("temp branch ralph-rebase/feature-x should have been deleted after a conflicting RebaseBranchOntoRemote")
	}
}

// RebaseStack must rebase every branch in the stack with --update-refs onto
// the advanced base, force-push all of them to origin, and clean up the temp
// worktree and temp branch — proving the multi-branch (--update-refs, plain
// --force) path of the shared rebaseInTempWorktree helper works, and that
// stack ordering (bottom is still an ancestor of top) is preserved.
func TestRebaseStack_RebasesAndForcePushesAllBranches(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	repo := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
	)

	// Build a two-PR stack: stack-bottom off main, stack-top off stack-bottom.
	run(t, "git", "-C", project, "checkout", "-b", "stack-bottom")
	writeFile(t, project, "bottom.txt", "bottom work\n")
	run(t, "git", "-C", project, "commit", "-m", "bottom commit")
	run(t, "git", "-C", project, "push", "-u", "origin", "stack-bottom")

	run(t, "git", "-C", project, "checkout", "-b", "stack-top")
	writeFile(t, project, "top.txt", "top work\n")
	run(t, "git", "-C", project, "commit", "-m", "top commit")
	run(t, "git", "-C", project, "push", "-u", "origin", "stack-top")

	run(t, "git", "-C", project, "checkout", "main")
	writeFile(t, project, "mainfile.txt", "advance main\n")
	run(t, "git", "-C", project, "commit", "-m", "advance main")
	run(t, "git", "-C", project, "push", "origin", "main")

	err := repo.RebaseStack(context.Background(), RebaseStackOpts{
		TopBranch:   "stack-top",
		BaseBranch:  "main",
		TopPR:       1,
		AllBranches: []string{"stack-bottom", "stack-top"},
	})
	if err != nil {
		t.Fatalf("RebaseStack failed: %v", err)
	}

	run(t, "git", "-C", project, "fetch", "origin", "stack-bottom")
	run(t, "git", "-C", project, "fetch", "origin", "stack-top")

	for _, branch := range []string{"stack-bottom", "stack-top"} {
		content := gitOutput(project, "show", "origin/"+branch+":mainfile.txt")
		if !strings.Contains(content, "advance main") {
			t.Errorf("origin/%s missing rebased-in mainfile.txt content, got: %q", branch, content)
		}
	}
	topFile := gitOutput(project, "show", "origin/stack-top:top.txt")
	if !strings.Contains(topFile, "top work") {
		t.Errorf("origin/stack-top missing its own top.txt content, got: %q", topFile)
	}

	// Stack ordering preserved: stack-bottom must still be an ancestor of stack-top.
	if !repo.isAncestor(project, "origin/stack-bottom", "origin/stack-top") {
		t.Error("origin/stack-bottom should remain an ancestor of origin/stack-top after RebaseStack")
	}

	// Temp worktree and temp branch must be cleaned up.
	wtDir := filepath.Join(ralphDir, "worktrees", "merge-stack-top")
	if _, err := os.Stat(wtDir); err == nil {
		t.Errorf("temp worktree %s should have been removed after RebaseStack", wtDir)
	}
	if repo.refExists(project, "refs/heads/ralph-merge/stack-top") {
		t.Error("temp branch ralph-merge/stack-top should have been deleted after RebaseStack")
	}

	bareRefs := cmdOutput(t, "git", "-C", bare, "for-each-ref", "--format=%(refname)")
	if strings.Contains(bareRefs, "ralph-merge/stack-top") {
		t.Error("temp branch ralph-merge/stack-top should not have been pushed to origin")
	}
}

// ResetBranchToRemote must fast-forward the checked-out branch via
// merge --ff-only (not update-ref) so the working tree stays clean after a
// PR merge. Advancing the ref without updating the index corrupts visible
// git state and causes phantom staged deletions.
func TestResetBranchToRemote_CheckedOutBranch_FastForwards(t *testing.T) {
	project, _ := initBareRepo(t)
	bare := filepath.Join(filepath.Dir(project), "bare.git")

	ralphDir := filepath.Join(project, ".ralph")
	repo := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
	)

	// Confirm projectDir is checked out on main.
	head := gitOutput(project, "symbolic-ref", "--short", "HEAD")
	if head != "main" {
		t.Fatalf("expected HEAD=main, got %q", head)
	}

	// Push a new commit to origin/main from a separate clone (simulates merged PR).
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	run(t, "git", "-C", tmpClone, "config", "user.name", "test")
	run(t, "git", "-C", tmpClone, "config", "user.email", "test@test")
	writeFile(t, tmpClone, "merged-file.txt", "merged content\n")
	run(t, "git", "-C", tmpClone, "add", "merged-file.txt")
	run(t, "git", "-C", tmpClone, "commit", "-m", "merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	// Record local main SHA before the call.
	localMainBefore := gitOutput(project, "rev-parse", "main")
	originMainAfter := gitOutput(project, "ls-remote", "origin", "refs/heads/main")
	originSHA := strings.Fields(originMainAfter)[0]
	if localMainBefore == originSHA {
		t.Fatal("test setup: local main already equals origin/main; the simulated merge had no effect")
	}

	repo.ResetBranchToRemote(context.Background(), "main")

	// AC1: git status --porcelain must be clean — no phantom staged changes.
	status := strings.TrimSpace(gitOutput(project, "status", "--porcelain", "--untracked-files=no"))
	if status != "" {
		t.Errorf("git status is dirty after ResetBranchToRemote with main checked out:\n%s", status)
	}

	// AC2: local main equals origin/main and the merged file is present.
	localMainAfter := gitOutput(project, "rev-parse", "main")
	if localMainAfter == localMainBefore {
		t.Error("local main was not fast-forwarded to origin/main")
	}
	if localMainAfter != originSHA {
		t.Errorf("local main SHA=%s, want origin/main SHA=%s", localMainAfter, originSHA)
	}
	merged := gitOutput(project, "show", "HEAD:merged-file.txt")
	if !strings.Contains(merged, "merged content") {
		t.Errorf("merged-file.txt not in working tree after fast-forward, got: %q", merged)
	}
}

// ResetBranchToRemote must use update-ref (not merge --ff-only) when the
// target branch is not checked out, leaving HEAD on the current branch.
func TestResetBranchToRemote_NotCheckedOut_UsesUpdateRef(t *testing.T) {
	project, _ := initBareRepo(t)
	bare := filepath.Join(filepath.Dir(project), "bare.git")
	ralphDir := filepath.Join(project, ".ralph")

	// Checkout a different branch so main is NOT the current branch.
	run(t, "git", "-C", project, "checkout", "-b", "ralph/some-task")
	head := gitOutput(project, "symbolic-ref", "--short", "HEAD")
	if head != "ralph/some-task" {
		t.Fatalf("expected HEAD=ralph/some-task, got %q", head)
	}

	repo := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}},
		nil,
		withRunner(&execRunner{}),
	)

	// Push a new commit to origin/main from a separate clone.
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	run(t, "git", "-C", tmpClone, "config", "user.name", "test")
	run(t, "git", "-C", tmpClone, "config", "user.email", "test@test")
	writeFile(t, tmpClone, "other-merged.txt", "other content\n")
	run(t, "git", "-C", tmpClone, "add", "other-merged.txt")
	run(t, "git", "-C", tmpClone, "commit", "-m", "another merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	localMainBefore := gitOutput(project, "rev-parse", "main")
	originOut := gitOutput(project, "ls-remote", "origin", "refs/heads/main")
	originSHA := strings.Fields(originOut)[0]

	repo.ResetBranchToRemote(context.Background(), "main")

	// AC3: HEAD stays on the other branch.
	headAfter := gitOutput(project, "symbolic-ref", "--short", "HEAD")
	if headAfter != "ralph/some-task" {
		t.Errorf("HEAD should remain on ralph/some-task, got %q", headAfter)
	}

	// AC3: local main ref has been advanced via update-ref.
	localMainAfter := gitOutput(project, "rev-parse", "main")
	if localMainAfter == localMainBefore {
		t.Error("local main was not advanced when not checked out")
	}
	if localMainAfter != originSHA {
		t.Errorf("local main SHA=%s, want origin/main SHA=%s", localMainAfter, originSHA)
	}
}

// ResetBranchToRemote must log a warning and leave projectDir untouched when
// origin/<branch> is not a fast-forward of the checked-out branch (diverged).
func TestResetBranchToRemote_CheckedOut_NonFastForward_LeavesUntouched(t *testing.T) {
	project, _ := initBareRepo(t)
	bare := filepath.Join(filepath.Dir(project), "bare.git")

	log := &testLog{}
	ralphDir := filepath.Join(project, ".ralph")
	repo := newRepoForTest(
		Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log},
		nil,
		withRunner(&execRunner{}),
	)

	// Add a local commit on main to make the branch diverge from what we'll
	// push to origin. This creates a genuine divergence: local has commit A,
	// origin will have commit B — neither is a fast-forward of the other.
	writeFile(t, project, "local-only.txt", "local divergence\n")
	run(t, "git", "-C", project, "add", "local-only.txt")
	run(t, "git", "-C", project, "commit", "-m", "local diverging commit")

	localMainBefore := gitOutput(project, "rev-parse", "main")

	// Push a different commit to origin from a separate clone so origin/main
	// diverges from local main.
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	run(t, "git", "-C", tmpClone, "config", "user.name", "test")
	run(t, "git", "-C", tmpClone, "config", "user.email", "test@test")
	writeFile(t, tmpClone, "remote-only.txt", "remote divergence\n")
	run(t, "git", "-C", tmpClone, "add", "remote-only.txt")
	run(t, "git", "-C", tmpClone, "commit", "-m", "remote diverging commit")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	repo.ResetBranchToRemote(context.Background(), "main")

	// AC4: projectDir is left untouched — local SHA unchanged.
	localMainAfter := gitOutput(project, "rev-parse", "main")
	if localMainAfter != localMainBefore {
		t.Errorf("projectDir was modified despite non-fast-forward: SHA changed from %s to %s", localMainBefore, localMainAfter)
	}

	// AC4: a warning was logged.
	if !log.contains("WARN") {
		t.Error("expected a warning to be logged when ff-only fails, but none found in log messages")
	}
}
