package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// memState is an in-memory StateStore for tests.
type memState struct {
	data map[string]string
}

func newMemState() *memState { return &memState{data: make(map[string]string)} }

func (s *memState) Read(key string) (string, error) {
	v, ok := s.data[key]
	if !ok {
		return "", nil
	}
	return v, nil
}

func (s *memState) Write(key, value string) error {
	s.data[key] = value
	return nil
}

// testLog captures log output for assertion.
type testLog struct {
	messages []string
}

func (l *testLog) Log(format string, args ...any)   { l.messages = append(l.messages, fmt.Sprintf(format, args...)) }
func (l *testLog) Warn(format string, args ...any)  { l.messages = append(l.messages, "WARN: "+fmt.Sprintf(format, args...)) }
func (l *testLog) Error(format string, args ...any) { l.messages = append(l.messages, "ERROR: "+fmt.Sprintf(format, args...)) }

func (l *testLog) contains(substr string) bool {
	for _, m := range l.messages {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

// initBareRepo creates a bare repo with one commit, plus a clone to act as
// the "project dir". Returns (projectDir, cleanup).
func initBareRepo(t *testing.T) (string, func()) {
	t.Helper()
	tmp := t.TempDir()

	bare := filepath.Join(tmp, "bare.git")
	run(t, "git", "init", "--bare", bare)

	project := filepath.Join(tmp, "project")
	run(t, "git", "clone", bare, project)
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "init")
	run(t, "git", "-C", project, "push", "-u", "origin", "main")
	// Set origin/HEAD so detectDefaultBranch works
	run(t, "git", "-C", project, "remote", "set-head", "origin", "main")

	return project, func() {}
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}

// --- Slugify tests ---

// Slugify converts free-form text to a safe branch/path slug
func TestSlugify_BasicConversion(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Hello World", "hello-world"},
		{"  Fix bug #42  ", "fix-bug-42"},
		{"UPPER CASE", "upper-case"},
		{"special!@#chars", "special-chars"},
		{"already-good", "already-good"},
		{"", ""},
		{strings.Repeat("a", 60), strings.Repeat("a", 50)},
	}
	for _, tc := range cases {
		got := Slugify(tc.in)
		if got != tc.want {
			t.Errorf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Truncation at 50 chars should not leave a trailing dash
func TestSlugify_TruncationTrimsTrailingDash(t *testing.T) {
	// 49 a's + space + more text → slug is "aaa...a-more-text", truncated at 50
	input := strings.Repeat("a", 49) + " bbb"
	got := Slugify(input)
	if len(got) > 50 {
		t.Errorf("slug too long: %d chars", len(got))
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("slug ends with dash: %q", got)
	}
}

// --- SetupWorktree tests ---

// SetupWorktree creates a new worktree in .ralph/worktrees and records state
func TestSetupWorktree_CreatesWorktree(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	log := &testLog{}

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       state,
		Logger:      log,
	}

	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree failed: %v", err)
	}

	// Worktree directory must exist
	if _, err := os.Stat(mgr.WorkDir); err != nil {
		t.Fatalf("WorkDir does not exist: %v", err)
	}

	// State must be recorded
	if got, _ := state.Read("worktree_dir"); got != mgr.WorkDir {
		t.Errorf("state worktree_dir = %q, want %q", got, mgr.WorkDir)
	}
	if got, _ := state.Read("worktree_branch"); got != mgr.WorktreeBranch {
		t.Errorf("state worktree_branch = %q, want %q", got, mgr.WorktreeBranch)
	}

	// Branch name follows temp_branch convention
	if !strings.HasSuffix(mgr.WorktreeBranch, "/next") {
		t.Errorf("branch %q should end with /next", mgr.WorktreeBranch)
	}
}

// --no-worktree mode should skip worktree creation entirely
func TestSetupWorktree_NoWorktreeMode(t *testing.T) {
	project, _ := initBareRepo(t)
	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    filepath.Join(project, ".ralph"),
		UseWorktree: false,
		Logger:      &testLog{},
	}

	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mgr.WorkDir != project {
		t.Errorf("WorkDir = %q, want project dir %q", mgr.WorkDir, project)
	}
}

// SetupWorktree in a non-git directory should return an error
func TestSetupWorktree_NonGitDirErrors(t *testing.T) {
	tmp := t.TempDir()
	mgr := &Manager{
		ProjectDir:  tmp,
		RalphDir:    filepath.Join(tmp, ".ralph"),
		UseWorktree: true,
		Logger:      &testLog{},
	}

	err := mgr.SetupWorktree()
	if err == nil {
		t.Fatal("expected error for non-git dir")
	}
	if !strings.Contains(err.Error(), "not a git repo") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Resume mode reuses a previously created worktree from state
func TestSetupWorktree_Resume(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	log := &testLog{}

	// First run: create worktree
	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       state,
		Logger:      log,
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	firstWorkDir := mgr.WorkDir
	firstBranch := mgr.WorktreeBranch

	// Second run: resume
	mgr2 := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		Resume:      true,
		State:       state,
		Logger:      log,
	}
	if err := mgr2.SetupWorktree(); err != nil {
		t.Fatalf("resume SetupWorktree: %v", err)
	}

	if mgr2.WorkDir != firstWorkDir {
		t.Errorf("resumed WorkDir = %q, want %q", mgr2.WorkDir, firstWorkDir)
	}
	if mgr2.WorktreeBranch != firstBranch {
		t.Errorf("resumed branch = %q, want %q", mgr2.WorktreeBranch, firstBranch)
	}
}

// --- RenameBranchForTask tests ---

// RenameBranchForTask gives the temp branch a descriptive name for the current task
func TestRenameBranchForTask_RenamesBranch(t *testing.T) {
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

	mgr.RenameBranchForTask("Add user authentication")

	wantPrefix := "ralph/" + mgr.ProjectName + "/01-add-user-authentication"
	if mgr.WorktreeBranch != wantPrefix {
		t.Errorf("branch = %q, want %q", mgr.WorktreeBranch, wantPrefix)
	}
	if !mgr.BranchRenamed {
		t.Error("BranchRenamed should be true")
	}

	// State should be updated
	if got, _ := state.Read("worktree_branch"); got != wantPrefix {
		t.Errorf("state worktree_branch = %q, want %q", got, wantPrefix)
	}
}

// RenameBranchForTask only renames once per iteration (idempotent guard)
func TestRenameBranchForTask_OnlyRenamesOnce(t *testing.T) {
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

	mgr.RenameBranchForTask("First task")
	firstBranch := mgr.WorktreeBranch

	mgr.RenameBranchForTask("Second task")
	if mgr.WorktreeBranch != firstBranch {
		t.Errorf("branch changed on second call: %q → %q", firstBranch, mgr.WorktreeBranch)
	}
}

// RenameBranchForTask is a no-op when running without a worktree
func TestRenameBranchForTask_NoOpWithoutWorktree(t *testing.T) {
	mgr := &Manager{
		ProjectDir: "/some/dir",
		WorkDir:    "/some/dir",
		Logger:     &testLog{},
	}
	mgr.RenameBranchForTask("anything")
	if mgr.BranchRenamed {
		t.Error("should not rename when WorkDir == ProjectDir")
	}
}

// --- RotateBranch tests ---

// RotateBranch creates a fresh temp branch for the next iteration
func TestRotateBranch_CreatesNewTempBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	log := &testLog{}

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       state,
		Logger:      log,
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// Rename to a task branch first
	mgr.RenameBranchForTask("Some task")
	taskBranch := mgr.WorktreeBranch

	// Now rotate
	mgr.RotateBranch()

	if mgr.WorktreeBranch == taskBranch {
		t.Error("branch should have changed after rotate")
	}
	if !strings.HasSuffix(mgr.WorktreeBranch, "/next") {
		t.Errorf("rotated branch %q should end with /next", mgr.WorktreeBranch)
	}
	if mgr.BranchRenamed {
		t.Error("BranchRenamed should be reset to false after rotation")
	}
}

// --- RenameWorktreeForTheme tests ---

// RenameWorktreeForTheme gives the worktree directory a thematic name
func TestRenameWorktreeForTheme_RenamesDirectory(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	log := &testLog{}

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       state,
		Logger:      log,
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	origDir := mgr.WorkDir

	mgr.RenameWorktreeForTheme("Auth Refactor")

	if mgr.WorkDir == origDir {
		t.Error("WorkDir should have changed after theme rename")
	}
	if !strings.Contains(mgr.WorkDir, "auth-refactor") {
		t.Errorf("WorkDir %q should contain 'auth-refactor'", mgr.WorkDir)
	}
	// Old dir should no longer exist, new dir should
	if _, err := os.Stat(origDir); err == nil {
		t.Error("old worktree dir should not exist after move")
	}
	if _, err := os.Stat(mgr.WorkDir); err != nil {
		t.Errorf("new worktree dir should exist: %v", err)
	}
	// State updated
	if got, _ := state.Read("worktree_dir"); got != mgr.WorkDir {
		t.Errorf("state worktree_dir = %q, want %q", got, mgr.WorkDir)
	}
}

// RenameWorktreeForTheme is a no-op when there's no worktree
func TestRenameWorktreeForTheme_NoOpWithoutWorktree(t *testing.T) {
	mgr := &Manager{
		ProjectDir: "/some/dir",
		WorkDir:    "/some/dir",
		Logger:     &testLog{},
	}
	mgr.RenameWorktreeForTheme("Something")
	// No panic, no change
	if mgr.WorkDir != "/some/dir" {
		t.Error("should not change WorkDir when equal to ProjectDir")
	}
}

// RenameWorktreeForTheme avoids collision by appending a sequence number
func TestRenameWorktreeForTheme_CollisionAvoidance(t *testing.T) {
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

	// Pre-create the target directory to force collision
	slug := Slugify("My Theme")
	collisionDir := filepath.Join(ralphDir, "worktrees", "ralph-"+mgr.WorkDir[len(mgr.WorkDir)-11:len(mgr.WorkDir)-3]+"-"+slug)
	// Actually, let's construct it properly
	today := strings.Split(filepath.Base(mgr.WorkDir), "-")[1] // extract date
	collisionDir = filepath.Join(ralphDir, "worktrees", "ralph-"+today+"-"+slug)
	os.MkdirAll(collisionDir, 0o755)

	mgr.RenameWorktreeForTheme("My Theme")

	// Should have appended -2
	if !strings.HasSuffix(mgr.WorkDir, slug+"-2") {
		t.Errorf("expected collision suffix, got %q", mgr.WorkDir)
	}
}

// --- TaskSeq increment tests ---

// TaskSeq increments with each branch rename, producing sequential branch names
func TestTaskSeq_IncrementsAcrossRotations(t *testing.T) {
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

	// First task
	mgr.RenameBranchForTask("First")
	if mgr.TaskSeq != 1 {
		t.Errorf("TaskSeq = %d, want 1", mgr.TaskSeq)
	}

	// Rotate and rename again
	mgr.RotateBranch()
	mgr.RenameBranchForTask("Second")
	if mgr.TaskSeq != 2 {
		t.Errorf("TaskSeq = %d, want 2", mgr.TaskSeq)
	}
	if !strings.Contains(mgr.WorktreeBranch, "/02-second") {
		t.Errorf("branch %q should contain /02-second", mgr.WorktreeBranch)
	}
}

// --- RebaseOntoDefaultBranch tests ---

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
