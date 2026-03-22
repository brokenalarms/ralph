package git

import (
	"errors"
	"os"
	"os/exec"
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
	run(t, "git", "init", "--bare", bare)

	project := filepath.Join(tmp, "project")
	run(t, "git", "clone", bare, project)
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "init")
	run(t, "git", "-C", project, "push", "-u", "origin", "main")
	run(t, "git", "-C", project, "remote", "set-head", "origin", "main")

	return project, bare
}

// setupRebaseMgr creates a Manager with a worktree ready for rebase testing.
// The worktree's origin points at the bare repo so fetch/push work correctly.
func setupRebaseMgr(t *testing.T, project, bare string) *Manager {
	t.Helper()
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// Point worktree's origin at the bare repo
	run(t, "git", "-C", mgr.WorkDir, "remote", "set-url", "origin", bare)
	run(t, "git", "-C", mgr.WorkDir, "fetch", "origin")

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

// --- RebaseOntoDefaultBranch tests ---

// Clean rebase succeeds when no squash merges have happened
func TestRebaseOntoDefaultBranch_CleanRebase(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// Add a commit on main
	writeFile(t, project, "mainfile.txt", "new file on main\n")
	run(t, "git", "-C", project, "commit", "-m", "add mainfile")
	pushToOrigin(t, project)

	// Add a commit in the worktree
	writeFile(t, mgr.WorkDir, "workfile.txt", "worktree file\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "add workfile")

	if err := mgr.RebaseOntoDefaultBranch(); err != nil {
		t.Fatalf("RebaseOntoDefaultBranch failed: %v", err)
	}

	// Both files should be present after rebase
	if _, err := os.Stat(filepath.Join(mgr.WorkDir, "mainfile.txt")); err != nil {
		t.Error("mainfile.txt should exist after rebase")
	}
	if _, err := os.Stat(filepath.Join(mgr.WorkDir, "workfile.txt")); err != nil {
		t.Error("workfile.txt should exist after rebase")
	}
}

// Squash-merged branches are detected and skipped during rebase
func TestRebaseOntoDefaultBranch_SkipsSquashMergedBranches(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)

	// Create a shared file on main before branching
	writeFile(t, project, "shared.txt", "original\n")
	run(t, "git", "-C", project, "commit", "-m", "add shared")
	pushToOrigin(t, project)

	mgr := setupRebaseMgr(t, project, bare)

	// Branch 01: modify shared.txt in multiple commits (creates intermediate
	// history that will conflict with a squash on rebase)
	mgr.RenameBranchForTask("first task")
	writeFile(t, mgr.WorkDir, "shared.txt", "step one\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "first task step one")
	writeFile(t, mgr.WorkDir, "shared.txt", "final\n")
	writeFile(t, mgr.WorkDir, "first.txt", "first\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "first task final")

	// Branch 02: new file
	mgr.RotateBranch()
	mgr.RenameBranchForTask("second task")
	writeFile(t, mgr.WorkDir, "second.txt", "second\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "second task")

	// Simulate squash-merge of branch 01 into main on origin
	writeFile(t, project, "shared.txt", "final\n")
	writeFile(t, project, "first.txt", "first\n")
	run(t, "git", "-C", project, "commit", "-m", "squash: first task")
	pushToOrigin(t, project)

	if err := mgr.RebaseOntoDefaultBranch(); err != nil {
		t.Fatalf("RebaseOntoDefaultBranch failed: %v", err)
	}

	log := mgr.Logger.(*testLog)
	if !log.contains("squash-merged") {
		t.Error("expected log message about squash-merged branch")
	}
	if _, err := os.Stat(filepath.Join(mgr.WorkDir, "second.txt")); err != nil {
		t.Error("second.txt should exist after rebase past squash-merged branch")
	}
}

// Squash-merge detection works even when main has additional unrelated commits
func TestRebaseOntoDefaultBranch_DetectsSquashMergeWithExtraMainCommits(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)

	writeFile(t, project, "shared.txt", "original\n")
	run(t, "git", "-C", project, "commit", "-m", "add shared")
	pushToOrigin(t, project)

	mgr := setupRebaseMgr(t, project, bare)

	// Branch 01
	mgr.RenameBranchForTask("first task")
	writeFile(t, mgr.WorkDir, "shared.txt", "step one\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "first task step one")
	writeFile(t, mgr.WorkDir, "shared.txt", "final\n")
	writeFile(t, mgr.WorkDir, "first.txt", "first\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "first task final")

	// Branch 02
	mgr.RotateBranch()
	mgr.RenameBranchForTask("second task")
	writeFile(t, mgr.WorkDir, "second.txt", "second\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "second task")

	// Squash-merge branch 01
	writeFile(t, project, "shared.txt", "final\n")
	writeFile(t, project, "first.txt", "first\n")
	run(t, "git", "-C", project, "commit", "-m", "squash: first task")

	// Another unrelated PR on main
	writeFile(t, project, "other.txt", "other pr work\n")
	run(t, "git", "-C", project, "commit", "-m", "other: unrelated PR")
	pushToOrigin(t, project)

	if err := mgr.RebaseOntoDefaultBranch(); err != nil {
		t.Fatalf("RebaseOntoDefaultBranch failed: %v", err)
	}

	log := mgr.Logger.(*testLog)
	if !log.contains("squash-merged") {
		t.Error("expected log message about squash-merged branch")
	}
	if _, err := os.Stat(filepath.Join(mgr.WorkDir, "second.txt")); err != nil {
		t.Error("second.txt should exist")
	}
	if _, err := os.Stat(filepath.Join(mgr.WorkDir, "other.txt")); err != nil {
		t.Error("other.txt should exist after rebase (from main)")
	}
}

// Stacked branches modifying the same file are handled correctly when
// the base branch is squash-merged with conflict resolution
func TestRebaseOntoDefaultBranch_StackedBranchesSameFile(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)

	// Initial file
	writeFile(t, project, "tests.txt", "test_alpha() { run alpha; }\ntest_beta() { run beta; }\ntest_gamma() { run gamma; }\n")
	run(t, "git", "-C", project, "commit", "-m", "initial tests")
	pushToOrigin(t, project)

	mgr := setupRebaseMgr(t, project, bare)

	// Branch 03: add tests in multiple commits
	mgr.RenameBranchForTask("add more tests")
	writeFile(t, mgr.WorkDir, "tests.txt", "test_alpha() { run alpha; }\ntest_beta() { run beta; }\ntest_gamma() { run gamma; }\n\n// new tests\ntest_delta() {\n  setup();\n  run delta;\n}\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "add delta test")

	writeFile(t, mgr.WorkDir, "tests.txt", "test_alpha() { run alpha; }\ntest_beta() { run beta; }\ntest_gamma() { run gamma; }\n\n// new tests\ntest_delta() {\n  setup();\n  run delta;\n}\ntest_epsilon() {\n  setup();\n  run epsilon;\n}\n\n// layout-dependent tests\ntest_overlay() {\n  const el = makeElement(\"DIV\", { top: 10 });\n  assert.ok(el.style.top === \"10px\");\n}\ntest_clipping() {\n  const el = makeElement(\"DIV\", { overflow: \"hidden\" });\n  assert.ok(!isVisible(el));\n}\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "add epsilon, overlay, clipping tests")

	// Branch 04: move layout tests to separate file
	mgr.RotateBranch()
	mgr.RenameBranchForTask("move layout tests")
	writeFile(t, mgr.WorkDir, "tests.txt", "test_alpha() { run alpha; }\ntest_beta() { run beta; }\ntest_gamma() { run gamma; }\n\n// new tests\ntest_delta() {\n  setup();\n  run delta;\n}\ntest_epsilon() {\n  setup();\n  run epsilon;\n}\n")
	writeFile(t, mgr.WorkDir, "layout_tests.txt", "// layout-dependent tests (moved from tests.txt)\ntest_overlay() {\n  const el = makeElement(\"DIV\", { top: 10 });\n  assert.ok(el.style.top === \"10px\");\n}\ntest_clipping() {\n  const el = makeElement(\"DIV\", { overflow: \"hidden\" });\n  assert.ok(!isVisible(el));\n}\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "move layout tests to separate file")

	// Another PR lands on main touching same file
	writeFile(t, project, "tests.txt", "test_alpha() { run alpha; }\ntest_beta() { run beta; }\ntest_gamma() { run gamma; }\n// added by another PR\ntest_zeta() { run zeta; }\n")
	run(t, "git", "-C", project, "commit", "-m", "other PR: add zeta test")

	// Squash-merge of branch 03 into main (with conflict resolution)
	writeFile(t, project, "tests.txt", "test_alpha() { run alpha; }\ntest_beta() { run beta; }\ntest_gamma() { run gamma; }\n// added by another PR\ntest_zeta() { run zeta; }\n\n// new tests\ntest_delta() {\n  setup();\n  run delta;\n}\ntest_epsilon() {\n  setup();\n  run epsilon;\n}\n\n// layout-dependent tests\ntest_overlay() {\n  const el = makeElement(\"DIV\", { top: 10 });\n  assert.ok(el.style.top === \"10px\");\n}\ntest_clipping() {\n  const el = makeElement(\"DIV\", { overflow: \"hidden\" });\n  assert.ok(!isVisible(el));\n}\n")
	run(t, "git", "-C", project, "commit", "-m", "squash: add more tests")
	pushToOrigin(t, project)

	if err := mgr.RebaseOntoDefaultBranch(); err != nil {
		t.Fatalf("RebaseOntoDefaultBranch failed: %v", err)
	}

	log := mgr.Logger.(*testLog)
	if !log.contains("squash-merged") {
		t.Error("expected log message about squash-merged branch")
	}

	// Branch 04's layout_tests.txt should be present
	if _, err := os.Stat(filepath.Join(mgr.WorkDir, "layout_tests.txt")); err != nil {
		t.Error("layout_tests.txt should exist after rebase")
	}

	// tests.txt should NOT contain layout tests (branch 04 moved them out)
	testsContent, err := os.ReadFile(filepath.Join(mgr.WorkDir, "tests.txt"))
	if err != nil {
		t.Fatalf("read tests.txt: %v", err)
	}
	if strings.Contains(string(testsContent), "test_overlay") {
		t.Error("tests.txt should not contain test_overlay (branch 04 moved it)")
	}
	if !strings.Contains(string(testsContent), "test_alpha") {
		t.Error("tests.txt should still contain test_alpha")
	}
}

// Real conflicts (not squash-merge related) halt rebase with an error
func TestRebaseOntoDefaultBranch_HaltsOnRealConflicts(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// Conflicting changes to same file
	writeFile(t, mgr.WorkDir, "conflict.txt", "worktree version\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "worktree change")

	writeFile(t, project, "conflict.txt", "main version\n")
	run(t, "git", "-C", project, "commit", "-m", "main change")
	pushToOrigin(t, project)

	err := mgr.RebaseOntoDefaultBranch()
	if err == nil {
		t.Fatal("expected error for real conflicts")
	}
	if !strings.Contains(err.Error(), "real conflicts") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Rebase is skipped when already up to date with origin
func TestRebaseOntoDefaultBranch_AlreadyUpToDate(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// No divergence — worktree is at same point as origin/main
	if err := mgr.RebaseOntoDefaultBranch(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	log := mgr.Logger.(*testLog)
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
	if err := mgr.RebaseOntoDefaultBranch(); err != nil {
		t.Fatalf("RebaseOntoDefaultBranch failed: %v", err)
	}

	// The new file from main should be present after rebase
	if _, err := os.Stat(filepath.Join(mgr.WorkDir, "newfeature.txt")); err != nil {
		t.Error("newfeature.txt should exist after rebasing onto advanced main")
	}

	log := mgr.Logger.(*testLog)
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
	writeFile(t, mgr.WorkDir, "local.txt", "local work\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "local commit")

	if err := mgr.RebaseOntoDefaultBranch(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	log := mgr.Logger.(*testLog)
	if !log.contains("Already up to date") {
		t.Error("expected 'Already up to date' when HEAD is ahead of origin/main")
	}
}

// --- tryResumeWorktree fetch tests ---

// Resuming a worktree fetches origin/main so subsequent rebase uses fresh refs,
// not stale local copies from the previous run.
func TestTryResumeWorktree_FetchesOriginOnResume(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	// Record origin/main before any new commits
	oldRef := gitOutput(mgr.WorkDir, "rev-parse", "origin/main")

	// Push a new commit to origin (simulates main advancing while ralph was idle)
	writeFile(t, project, "newfile.txt", "pushed while idle\n")
	run(t, "git", "-C", project, "commit", "-m", "advance main")
	pushToOrigin(t, project)

	// Simulate resume: store worktree state, then call tryResumeWorktree
	_ = mgr.State.Write("worktree_dir", mgr.WorkDir)
	_ = mgr.State.Write("worktree_branch", mgr.WorktreeBranch)

	if err := mgr.tryResumeWorktree(); err != nil {
		t.Fatalf("tryResumeWorktree: %v", err)
	}

	// After resume, origin/main should point at the new commit
	newRef := gitOutput(mgr.WorkDir, "rev-parse", "origin/main")
	if newRef == oldRef {
		t.Error("origin/main was not updated on resume — fetch did not run or failed silently")
	}
}

// --- TagTaskStart / TagTaskEnd tests ---

// TagTaskStart creates a git tag using the bd task ID when available
func TestTagTaskStart_WithTaskID(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       newMemState(),
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.TagTaskStart("ralph-abc")

	if !refExists(mgr.WorkDir, "task/ralph-abc/start") {
		t.Error("expected tag task/ralph-abc/start to exist")
	}
}

// TagTaskEnd creates an end tag at the current HEAD
func TestTagTaskEnd_WithTaskID(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       newMemState(),
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.TagTaskEnd("ralph-abc")

	if !refExists(mgr.WorkDir, "task/ralph-abc/end") {
		t.Error("expected tag task/ralph-abc/end to exist")
	}
}

// Tags fall back to the seq-slug from the branch name when no task ID is provided
func TestTagTaskStart_FallbackToSeqSlug(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       newMemState(),
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("Add user auth")
	mgr.TagTaskStart("")

	if !refExists(mgr.WorkDir, "task/01-add-user-auth/start") {
		t.Error("expected tag task/01-add-user-auth/start to exist")
	}
}

// Tags are no-ops when running without a worktree (WorkDir == ProjectDir)
func TestTagTaskStart_NoOpWithoutWorktree(t *testing.T) {
	mgr := &Manager{
		ProjectDir: "/some/dir",
		WorkDir:    "/some/dir",
		Logger:     &testLog{},
	}
	mgr.TagTaskStart("ralph-abc")
}

// Tags on the temp /next branch are skipped (no meaningful slug to extract)
func TestTagTaskStart_SkipsNextBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       newMemState(),
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// Branch is still ralph/project/next — no task ID → no tag
	mgr.TagTaskStart("")

	tags := gitOutput(mgr.WorkDir, "tag", "-l", "task/*")
	if tags != "" {
		t.Errorf("expected no tags on /next branch, got: %s", tags)
	}
}

// Start and end tags point at different commits when work happens between them
func TestTagStartEnd_DifferentCommits(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       newMemState(),
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.TagTaskStart("ralph-xyz")
	startRev := gitOutput(mgr.WorkDir, "rev-parse", "task/ralph-xyz/start")

	writeFile(t, mgr.WorkDir, "work.txt", "some work\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "do work")

	mgr.TagTaskEnd("ralph-xyz")
	endRev := gitOutput(mgr.WorkDir, "rev-parse", "task/ralph-xyz/end")

	if startRev == endRev {
		t.Error("start and end tags should point at different commits after work was done")
	}
}

// --- RecreateFromMain tests ---

// RecreateFromMain removes the old worktree and creates a fresh one from main,
// recovering from squash-merge rebase conflicts where completed work is already on main.
func TestRecreateFromMain_CreatesCleanWorktree(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)

	writeFile(t, project, "base.txt", "base content\n")
	run(t, "git", "-C", project, "commit", "-m", "add base file")
	pushToOrigin(t, project)

	mgr := setupRebaseMgr(t, project, bare)

	// Add some work in the worktree
	mgr.RenameBranchForTask("first task")
	writeFile(t, mgr.WorkDir, "task.txt", "task work\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "task work")

	oldBranch := mgr.WorktreeBranch

	// Simulate squash-merge by adding the same content to main
	writeFile(t, project, "task.txt", "task work\n")
	run(t, "git", "-C", project, "commit", "-m", "squash: first task")
	pushToOrigin(t, project)

	if err := mgr.RecreateFromMain(); err != nil {
		t.Fatalf("RecreateFromMain failed: %v", err)
	}

	// Worktree should exist
	if _, err := os.Stat(mgr.WorkDir); err != nil {
		t.Fatalf("worktree should exist after recreation: %v", err)
	}

	// Branch should be reset to /next
	if !strings.HasSuffix(mgr.WorktreeBranch, "/next") {
		t.Errorf("branch %q should end with /next after recreation", mgr.WorktreeBranch)
	}

	// Old task branches should be deleted
	branches := ListProjectBranches(project, mgr.ProjectName)
	for _, b := range branches {
		if b == oldBranch {
			t.Errorf("old branch %q should have been deleted", oldBranch)
		}
	}

	// New worktree should contain base.txt and task.txt from main
	if _, err := os.Stat(filepath.Join(mgr.WorkDir, "base.txt")); err != nil {
		t.Error("base.txt should exist in fresh worktree (from main)")
	}
	if _, err := os.Stat(filepath.Join(mgr.WorkDir, "task.txt")); err != nil {
		t.Error("task.txt should exist in fresh worktree (squash-merged to main)")
	}

	// TaskSeq and BranchRenamed should be reset
	if mgr.TaskSeq != 0 {
		t.Errorf("TaskSeq should be 0 after recreation, got %d", mgr.TaskSeq)
	}
	if mgr.BranchRenamed {
		t.Error("BranchRenamed should be false after recreation")
	}
}

// RecreateFromMain errors when no worktree is active (WorkDir == ProjectDir)
func TestRecreateFromMain_ErrorsWithoutWorktree(t *testing.T) {
	mgr := &Manager{
		ProjectDir: "/some/dir",
		WorkDir:    "/some/dir",
		Logger:     &testLog{},
	}
	err := mgr.RecreateFromMain()
	if err == nil {
		t.Fatal("expected error when no worktree is active")
	}
}

// RecreateFromMain succeeds even when an external worktree (e.g. a Claude
// sub-agent in .claude/worktrees/) has one of ralph's branches checked out.
// Without the fix, `git branch -D` would fail because the branch is in use.
func TestRecreateFromMain_PrunesExternalWorktreeHoldingBranch(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)

	writeFile(t, project, "base.txt", "base\n")
	run(t, "git", "-C", project, "commit", "-m", "base")
	pushToOrigin(t, project)

	mgr := setupRebaseMgr(t, project, bare)
	mgr.RenameBranchForTask("some task")

	writeFile(t, mgr.WorkDir, "work.txt", "work\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "work")

	taskBranch := mgr.WorktreeBranch

	// Rotate ralph's worktree to /next so the task branch is free for the
	// external worktree to check out (mirrors real scenario: ralph continues
	// on /next while a Claude sub-agent holds the old task branch).
	mgr.TaskSeq = 1
	mgr.RotateBranch()

	// Simulate an external worktree (like a Claude sub-agent) checking out
	// the old task branch, making `git branch -D` fail.
	externalWt := filepath.Join(t.TempDir(), "agent-worktree")
	run(t, "git", "-C", project, "worktree", "add", externalWt, taskBranch)

	// Squash-merge the work onto main so RecreateFromMain is valid
	writeFile(t, project, "work.txt", "work\n")
	run(t, "git", "-C", project, "commit", "-m", "squash: some task")
	pushToOrigin(t, project)

	if err := mgr.RecreateFromMain(); err != nil {
		t.Fatalf("RecreateFromMain should handle external worktrees, got: %v", err)
	}

	// The external worktree directory should have been removed
	if _, err := os.Stat(externalWt); !os.IsNotExist(err) {
		t.Error("external worktree directory should have been removed")
	}

	// The old task branch should be gone
	for _, b := range ListProjectBranches(project, mgr.ProjectName) {
		if b == taskBranch {
			t.Errorf("branch %q should have been deleted", taskBranch)
		}
	}
}

// RebaseOntoDefaultBranch returns a RebaseConflictError for real conflicts,
// allowing callers to distinguish conflict types for recovery.
func TestRebaseOntoDefaultBranch_ReturnsTypedError(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	mgr := setupRebaseMgr(t, project, bare)

	writeFile(t, mgr.WorkDir, "conflict.txt", "worktree version\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "worktree change")

	writeFile(t, project, "conflict.txt", "main version\n")
	run(t, "git", "-C", project, "commit", "-m", "main change")
	pushToOrigin(t, project)

	err := mgr.RebaseOntoDefaultBranch()
	if err == nil {
		t.Fatal("expected error for real conflicts")
	}

	var conflictErr *RebaseConflictError
	if !errors.As(err, &conflictErr) {
		t.Errorf("expected RebaseConflictError, got %T: %v", err, err)
	}
}

// --- AutoMergeCurrentBranch tests ---

// AutoMergeCurrentBranch returns nil when no worktree branch is set,
// so --auto-merge is a safe no-op without worktree isolation.
func TestAutoMergeCurrentBranch_SkipsWhenNoWorktreeBranch(t *testing.T) {
	mgr := &Manager{
		WorkDir:    "/some/dir",
		ProjectDir: "/some/dir",
		Logger:     &testLog{},
	}
	merged, err := mgr.AutoMergeCurrentBranch()
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
	merged, err := mgr.AutoMergeCurrentBranch()
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
		UseWorktree: true,
		State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("unpushed task")

	merged, err := mgr.AutoMergeCurrentBranch()
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

// Single-branch mode omits --delete-branch from gh pr merge so the remote
// branch survives for the next task's push.
func TestGhMergeArgs_SingleBranchOmitsDeleteBranch(t *testing.T) {
	mgr := &Manager{BranchStrategy: BranchSingle}
	args := mgr.ghMergeArgs("42", "https://github.com/org/repo")
	for _, a := range args {
		if a == "--delete-branch" {
			t.Fatal("single-branch mode must not include --delete-branch")
		}
	}
}

// Stacked mode includes --delete-branch since each task gets its own branch.
func TestGhMergeArgs_StackedIncludesDeleteBranch(t *testing.T) {
	mgr := &Manager{BranchStrategy: BranchStacked}
	args := mgr.ghMergeArgs("42", "https://github.com/org/repo")
	found := false
	for _, a := range args {
		if a == "--delete-branch" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("stacked mode should include --delete-branch")
	}
}
