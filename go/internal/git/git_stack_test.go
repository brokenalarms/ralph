package git

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

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
