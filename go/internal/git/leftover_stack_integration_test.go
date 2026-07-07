package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// leftoverStackScenario sets up a real bare origin + project where a
// "leftover" branch (simulating an open PR left behind by a prior ralph
// loop run) has diverged from main: main advanced with a commit the
// leftover branch lacks, and the leftover branch has a commit main lacks.
// This is the exact ralph-i003 incident topology — PR #1241's branch sat
// open while a later run's main moved past it.
func leftoverStackScenario(t *testing.T) (mgr *repo, leftoverBranch string) {
	t.Helper()
	project, bare := initBareRepoWithOrigin(t)
	leftoverBranch = "ralph/tabi-uael"

	run(t, "git", "-C", project, "checkout", "-b", leftoverBranch)
	writeFile(t, project, "leftover.txt", "leftover work\n")
	run(t, "git", "-C", project, "commit", "-m", "leftover: add leftover.txt")
	run(t, "git", "-C", project, "push", "-u", "origin", leftoverBranch)
	run(t, "git", "-C", project, "checkout", "main")

	writeFile(t, project, "main-advance.txt", "main moved on\n")
	run(t, "git", "-C", project, "commit", "-m", "main: advance past leftover branch")
	pushToOrigin(t, project)

	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs: []StubPR{
			{Number: 1241, Branch: leftoverBranch},
		},
	})
	mgr = newRepoForTest(Config{
		ProjectDir: project,
		BaseBranch: "main",
		RalphDir:   filepath.Join(project, ".ralph"),
		Logger:     &testLog{},
	}, gh, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	run(t, "git", "-C", mgr.workDir, "remote", "set-url", "origin", bare)
	run(t, "git", "-C", mgr.workDir, "fetch", "origin")
	run(t, "git", "-C", mgr.workDir, "config", "user.name", "test")
	run(t, "git", "-C", mgr.workDir, "config", "user.email", "test@test")

	return mgr, leftoverBranch
}

// Regression test for ralph-i003: choosing "y" at the leftover-PR startup
// prompt (mirrored here by mgr.SetAdoptedStackBranch, exactly as
// cmd/ralph's checkLeftoverRalphPRs calls it) must stack the first task's
// branch on top of the leftover branch, even though it diverged from main —
// the whole reason the prompt exists is that BranchIsAheadOfMain alone
// would otherwise reject it and silently build past the open PR.
func TestSyncWorktreeBase_AdoptedLeftoverBranch_FirstTaskBranchesOnLeftover(t *testing.T) {
	mgr, leftoverBranch := leftoverStackScenario(t)

	mgr.SetAdoptedStackBranch(leftoverBranch)

	if err := mgr.SyncWorktreeBase(context.Background(), []string{leftoverBranch}); err != nil {
		t.Fatalf("SyncWorktreeBase: %v", err)
	}
	if mgr.GetPrevBranch() != leftoverBranch {
		t.Fatalf("expected stack head %s, got %q", leftoverBranch, mgr.GetPrevBranch())
	}

	branch, err := mgr.BranchForTask(context.Background(), "ralph-xyz", "First task after adopt", BranchTaskMeta{})
	if err != nil {
		t.Fatalf("BranchForTask: %v", err)
	}
	if branch == leftoverBranch {
		t.Fatalf("expected a new task branch, not the leftover branch itself")
	}
	if _, statErr := os.Stat(filepath.Join(mgr.workDir, "leftover.txt")); statErr != nil {
		t.Errorf("expected leftover.txt (only on %s) in the first task's branch — it was not based on the adopted leftover branch: %v", leftoverBranch, statErr)
	}
}

// Companion to the adoption test: choosing "n" (no SetAdoptedStackBranch
// call) with the same diverged-leftover topology proceeds from main exactly
// as today — the first task's branch must NOT carry the leftover branch's
// content.
func TestSyncWorktreeBase_NoAdoption_FirstTaskBranchesFromMain(t *testing.T) {
	mgr, leftoverBranch := leftoverStackScenario(t)

	if err := mgr.SyncWorktreeBase(context.Background(), []string{leftoverBranch}); err != nil {
		t.Fatalf("SyncWorktreeBase: %v", err)
	}
	if mgr.GetPrevBranch() != "" {
		t.Fatalf("expected no stack head without adoption, got %q", mgr.GetPrevBranch())
	}

	if _, err := mgr.BranchForTask(context.Background(), "ralph-xyz", "First task from main", BranchTaskMeta{}); err != nil {
		t.Fatalf("BranchForTask: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(mgr.workDir, "leftover.txt")); statErr == nil {
		t.Errorf("leftover.txt should not be present — first task must branch from main, not the un-adopted leftover branch")
	}
	if _, statErr := os.Stat(filepath.Join(mgr.workDir, "main-advance.txt")); statErr != nil {
		t.Errorf("expected main-advance.txt (from main) to be present: %v", statErr)
	}
}
