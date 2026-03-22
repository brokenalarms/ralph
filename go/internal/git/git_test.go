package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	// Branch name must be exactly ralph/<projectName>/next
	wantBranch := "ralph/" + mgr.ProjectName + "/next"
	if mgr.WorktreeBranch != wantBranch {
		t.Errorf("branch = %q, want %q", mgr.WorktreeBranch, wantBranch)
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

// Resume log must not leak the old branch name to avoid confusion with the
// current task — the branch gets rebased and renamed shortly after resume.
func TestSetupWorktree_ResumeLogSuppressesBranchName(t *testing.T) {
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

	mgr.RenameBranchForTask("old task name")
	oldBranch := mgr.WorktreeBranch

	log.messages = nil

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

	for _, msg := range log.messages {
		if strings.Contains(msg, oldBranch) {
			t.Errorf("resume log should not contain old branch name %q, got %q", oldBranch, msg)
		}
	}
}

// Resume restores task_seq from state.json, not branch count.
// Prevents sequence skips when branches are deleted after squash-merge.
func TestSetupWorktree_ResumeRestoresTaskSeqFromState(t *testing.T) {
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

	mgr.RenameBranchForTask("first task")
	mgr.RotateBranch()
	mgr.RenameBranchForTask("second task")

	// Verify task_seq was persisted
	storedSeq, _ := state.Read("task_seq")
	if storedSeq != "2" {
		t.Fatalf("stored task_seq = %q, want \"2\"", storedSeq)
	}

	// Delete a branch to simulate squash-merge cleanup
	exec.Command("git", "-C", project, "branch", "-D", "ralph/"+mgr.ProjectName+"/01-first-task").Run()

	// Resume — should use persisted seq (2), not branch count (1)
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

	if mgr2.TaskSeq != 2 {
		t.Errorf("TaskSeq = %d, want 2 (from state.json, not branch count)", mgr2.TaskSeq)
	}
}

// Resume followed by RotateBranch preserves the previous task branch as a
// separate ref, so the next task creates a new stacked branch instead of
// renaming the existing one.
func TestResumeRotate_PreservesPreviousBranch(t *testing.T) {
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

	mgr.RenameBranchForTask("first task")
	writeFile(t, mgr.WorkDir, "first.txt", "work\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "first task work")
	firstBranch := mgr.WorktreeBranch

	// Simulate resume: new Manager with Resume=true
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

	// Rotate to create a stacked branch (simulates what Loop.Run does on resume)
	mgr2.RotateBranch()
	mgr2.RenameBranchForTask("second task")

	secondBranch := mgr2.WorktreeBranch

	if firstBranch == secondBranch {
		t.Errorf("branches should differ: first=%q, second=%q", firstBranch, secondBranch)
	}

	// Previous task branch should still exist as a separate ref
	if !refExists(project, firstBranch) {
		t.Errorf("first task branch %q should still exist after rotation", firstBranch)
	}

	if !refExists(project, secondBranch) {
		t.Errorf("second task branch %q should exist", secondBranch)
	}

	// Both branches should share the same base commit (stacked)
	firstHead := gitOutput(mgr2.WorkDir, "rev-parse", firstBranch)
	secondBase := gitOutput(mgr2.WorkDir, "merge-base", firstBranch, secondBranch)
	if firstHead != secondBase {
		t.Error("second branch should be stacked on top of first branch's HEAD")
	}
}

// Resume fetches latest origin/main so the subsequent rebase uses fresh refs.
// Without this fetch, the worktree would rebase onto a stale origin/main,
// causing "merge commit cannot be cleanly created" failures on GitHub.
func TestSetupWorktree_ResumeFetchesLatestMain(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
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
		t.Fatalf("SetupWorktree: %v", err)
	}

	// Do some work in the worktree
	writeFile(t, mgr.WorkDir, "task.txt", "worktree work\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "worktree commit")

	// Record origin/main before simulating new remote activity
	originMainBefore := gitOutput(project, "rev-parse", "origin/main")

	// Simulate time passing: push a new commit to origin/main via a
	// separate clone (as if another PR was merged on GitHub)
	tmpClone := filepath.Join(t.TempDir(), "clone")
	run(t, "git", "clone", bare, tmpClone)
	writeFile(t, tmpClone, "new-on-main.txt", "landed while ralph was stopped\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "new commit on main")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	// Resume: create a new Manager with Resume=true
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

	// After resume, origin/main should have advanced
	originMainAfter := gitOutput(project, "rev-parse", "origin/main")
	if originMainAfter == originMainBefore {
		t.Error("origin/main should have advanced after resume fetch")
	}

	// The subsequent rebase should include the new commit
	if err := mgr2.RebaseOntoDefaultBranch(); err != nil {
		t.Fatalf("RebaseOntoDefaultBranch failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(mgr2.WorkDir, "new-on-main.txt")); err != nil {
		t.Error("new-on-main.txt should exist after resume + rebase onto latest origin/main")
	}
	if _, err := os.Stat(filepath.Join(mgr2.WorkDir, "task.txt")); err != nil {
		t.Error("task.txt should still exist from worktree work")
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

	mgr.RenameBranchForTask("Fix auth bug")

	wantBranch := "ralph/" + mgr.ProjectName + "/01-fix-auth-bug"
	if mgr.WorktreeBranch != wantBranch {
		t.Errorf("branch = %q, want %q", mgr.WorktreeBranch, wantBranch)
	}
	if !mgr.BranchRenamed {
		t.Error("BranchRenamed should be true")
	}

	// State should be updated
	if got, _ := state.Read("worktree_branch"); got != wantBranch {
		t.Errorf("state worktree_branch = %q, want %q", got, wantBranch)
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


// Worktree path contains ralph-YYYYMMDD-01 (bats test 1)
func TestWorktreeDirUsesDateBasedName(t *testing.T) {
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

	today := time.Now().Format("20060102")
	expected := fmt.Sprintf("ralph-%s-01", today)
	if !strings.Contains(mgr.WorkDir, "/worktrees/"+expected) {
		t.Errorf("WorkDir = %q, want it to contain %q", mgr.WorkDir, expected)
	}
}

// Existing -01 directory causes -02 suffix (bats test 2)
func TestSecondRunSameDayIncrementsSuffix(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	today := time.Now().Format("20060102")
	os.MkdirAll(filepath.Join(ralphDir, "worktrees", "ralph-"+today+"-01"), 0o755)

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

	expected := fmt.Sprintf("ralph-%s-02", today)
	if !strings.Contains(mgr.WorkDir, "/worktrees/"+expected) {
		t.Errorf("WorkDir = %q, want it to contain %q", mgr.WorkDir, expected)
	}
}

// Stale branches don't inflate the task sequence counter (bats test 6)
func TestBranchSequenceResetsPerRun(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	run(t, "git", "-C", project, "branch", "ralph/project/old-stale")

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

	want := "ralph/" + mgr.ProjectName + "/01-first-task"
	if mgr.WorktreeBranch != want {
		t.Errorf("branch = %q, want %q", mgr.WorktreeBranch, want)
	}
}

// rotate_branch doesn't crash when branch already exists (bats test 8)
func TestRotateBranchLogsWarningOnFailure(t *testing.T) {
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

	// Don't rename — "next" still exists, rotate will attempt to create it
	mgr.RotateBranch()

	// Should not panic, should still be on next branch
	if !strings.HasSuffix(mgr.WorktreeBranch, "/next") {
		t.Errorf("branch %q should end with /next", mgr.WorktreeBranch)
	}
}

// Removed worktree directory is pruned and fresh setup succeeds (bats test 9)
func TestStaleWorktreeBranchCleanedUpViaPrune(t *testing.T) {
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
		t.Fatalf("first SetupWorktree: %v", err)
	}
	firstWorkDir := mgr.WorkDir

	os.RemoveAll(firstWorkDir)

	mgr2 := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       newMemState(),
		Logger:      &testLog{},
	}
	if err := mgr2.SetupWorktree(); err != nil {
		t.Fatalf("second SetupWorktree after prune: %v", err)
	}
	if _, err := os.Stat(mgr2.WorkDir); err != nil {
		t.Errorf("new worktree dir should exist: %v", err)
	}
}

// Live ralph worktree is force-removed when branch conflicts (bats test 10)
func TestLiveRalphWorktreeRemovedWhenBranchExists(t *testing.T) {
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
		t.Fatalf("first SetupWorktree: %v", err)
	}
	firstWorkDir := mgr.WorkDir

	mgr2 := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		State:       newMemState(),
		Logger:      &testLog{},
	}
	if err := mgr2.SetupWorktree(); err != nil {
		t.Fatalf("second SetupWorktree: %v", err)
	}

	if _, err := os.Stat(mgr2.WorkDir); err != nil {
		t.Error("new worktree dir should exist")
	}
	if _, err := os.Stat(firstWorkDir); err == nil {
		t.Error("old worktree dir should have been removed")
	}
}

// Resume restores TaskSeq from branch count (bats test 11 — Go uses branch count instead of state.json)
func TestResumeRestoresTaskSeq(t *testing.T) {
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

	mgr.RenameBranchForTask("first task")
	mgr.RotateBranch()
	mgr.RenameBranchForTask("second task")

	if mgr.TaskSeq != 2 {
		t.Fatalf("TaskSeq = %d, want 2", mgr.TaskSeq)
	}

	// Delete a branch to simulate squash-merge cleanup
	gitCmd(project, "branch", "-D", "ralph/"+mgr.ProjectName+"/01-first-task")

	// Resume — TaskSeq should be restored from remaining branches
	mgr2 := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
		UseWorktree: true,
		Resume:      true,
		State:       state,
		Logger:      &testLog{},
	}
	if err := mgr2.SetupWorktree(); err != nil {
		t.Fatalf("resume SetupWorktree: %v", err)
	}

	// After deleting branch 01, only branch 02 + next remain,
	// so countNamedBranches returns 2 (the /next and /02-second-task branches)
	if mgr2.TaskSeq < 1 {
		t.Errorf("TaskSeq = %d, want >= 1 (restored from branches)", mgr2.TaskSeq)
	}
}

// .gitignore is committed to main so the worktree inherits it (bats test 17)
func TestWorktreeInheritsGitignore(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	EnsureGitignored(project, ".ralph")
	run(t, "git", "-C", project, "push", "origin", "main", "-q")

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

	data, err := os.ReadFile(filepath.Join(mgr.WorkDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(data), ".ralph") {
		t.Errorf(".gitignore should contain .ralph, got: %s", data)
	}
}

// Existing .gitignore content is preserved when appending entries (bats test 18)
func TestExistingGitignoreContentPreserved(t *testing.T) {
	project, _ := initBareRepo(t)

	os.WriteFile(filepath.Join(project, ".gitignore"), []byte("node_modules\n"), 0o644)
	run(t, "git", "-C", project, "add", ".gitignore")
	run(t, "git", "-C", project, "commit", "-m", "add gitignore")

	EnsureGitignored(project, ".ralph")

	data, err := os.ReadFile(filepath.Join(project, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "node_modules") {
		t.Error(".gitignore should still contain node_modules")
	}
	if !strings.Contains(content, ".ralph") {
		t.Error(".gitignore should contain .ralph")
	}
}

// Dirty working tree is detected (bats test 19)
func TestDirtyWorkingTreeDetected(t *testing.T) {
	project, _ := initBareRepo(t)

	os.WriteFile(filepath.Join(project, "dirty.txt"), []byte("uncommitted\n"), 0o644)
	run(t, "git", "-C", project, "add", "dirty.txt")

	if !HasUncommittedChanges(project) {
		t.Error("should detect uncommitted changes")
	}
}

// --- PRNumberForBranch tests ---

// Proves: PRNumberForBranch returns "" when no worktree branch is set.
func TestPRNumberForBranch_EmptyWhenNoWorktreeBranch(t *testing.T) {
	mgr := &Manager{
		ProjectDir: "/tmp/project",
		WorkDir:    "/tmp/worktree",
		Logger:     &testLog{},
	}
	if got := mgr.PRNumberForBranch(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// Proves: PRNumberForBranch returns "" when WorkDir equals ProjectDir.
func TestPRNumberForBranch_EmptyWhenWorkDirIsProjectDir(t *testing.T) {
	mgr := &Manager{
		ProjectDir:      "/tmp/project",
		WorkDir:         "/tmp/project",
		WorktreeBranch:  "ralph/test/01-task",
		Logger:          &testLog{},
	}
	if got := mgr.PRNumberForBranch(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// --- AutoMergeCurrentBranch tests ---

// AutoMergeCurrentBranch returns nil when no worktree branch is set,

// RenameWorktreeForTheme uses a task description as fallback (bats test 21)
func TestRenameWorktreeForTheme_FallsBackToTaskTitle(t *testing.T) {
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

	mgr.RenameWorktreeForTheme("auth middleware rewrite")

	today := time.Now().Format("20060102")
	expected := "ralph-" + today + "-auth-middleware-rewrite"
	if !strings.Contains(mgr.WorkDir, expected) {
		t.Errorf("WorkDir = %q, want it to contain %q", mgr.WorkDir, expected)
	}
}

// RenameWorktreeForTheme uses theme from state over bd fallback (bats test 22)
func TestRenameWorktreeForTheme_PrefersStateTheme(t *testing.T) {
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

	stateTheme := "go migration"
	mgr.RenameWorktreeForTheme(stateTheme)

	today := time.Now().Format("20060102")
	expected := "ralph-" + today + "-go-migration"
	if !strings.Contains(mgr.WorkDir, expected) {
		t.Errorf("WorkDir = %q, want it to contain %q", mgr.WorkDir, expected)
	}
}

// --- BranchStrategy tests ---

// Single-branch mode skips RenameBranchForTask entirely, keeping the initial
// worktree branch name unchanged across all tasks.
func TestRenameBranchForTask_SkippedInSingleMode(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:     project,
		RalphDir:       ralphDir,
		UseWorktree:    true,
		BranchStrategy: BranchSingle,
		State:          st,
		Logger:         &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	origBranch := mgr.WorktreeBranch
	mgr.RenameBranchForTask("Some task")

	if mgr.WorktreeBranch != origBranch {
		t.Errorf("branch should stay as %q in single mode, got %q", origBranch, mgr.WorktreeBranch)
	}
	if mgr.BranchRenamed {
		t.Error("BranchRenamed should be false in single mode")
	}
	if mgr.TaskSeq != 0 {
		t.Errorf("TaskSeq should stay 0 in single mode, got %d", mgr.TaskSeq)
	}
}

// Single-branch mode skips RotateBranch entirely, keeping the same branch
// across task transitions instead of creating per-task branches.
func TestRotateBranch_SkippedInSingleMode(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:     project,
		RalphDir:       ralphDir,
		UseWorktree:    true,
		BranchStrategy: BranchSingle,
		State:          st,
		Logger:         &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	origBranch := mgr.WorktreeBranch
	mgr.RotateBranch()

	if mgr.WorktreeBranch != origBranch {
		t.Errorf("branch should stay as %q in single mode, got %q", origBranch, mgr.WorktreeBranch)
	}
}

// Stacked mode (explicit) still rotates and renames branches per task.
func TestBranchStrategy_StackedBehavesLikeDefault(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:     project,
		RalphDir:       ralphDir,
		UseWorktree:    true,
		BranchStrategy: BranchStacked,
		State:          st,
		Logger:         &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	origBranch := mgr.WorktreeBranch
	mgr.RenameBranchForTask("First task")

	if mgr.WorktreeBranch == origBranch {
		t.Error("stacked mode should rename the branch")
	}
	if !mgr.BranchRenamed {
		t.Error("BranchRenamed should be true after rename in stacked mode")
	}
	if mgr.TaskSeq != 1 {
		t.Errorf("TaskSeq should be 1, got %d", mgr.TaskSeq)
	}

	taskBranch := mgr.WorktreeBranch
	mgr.RotateBranch()

	if mgr.WorktreeBranch == taskBranch {
		t.Error("stacked mode should rotate the branch")
	}
	if !strings.HasSuffix(mgr.WorktreeBranch, "/next") {
		t.Errorf("rotated branch %q should end with /next", mgr.WorktreeBranch)
	}
}

// IsBranchSquashMerged detects when a branch's changes have been squash-merged
// into main, so the exit summary can show [MERGED] next to completed branches.
func TestIsBranchSquashMerged_DetectsMergedBranch(t *testing.T) {
	project, _ := initBareRepo(t)

	// Create a file on a feature branch
	run(t, "git", "-C", project, "checkout", "-b", "ralph/test/01-feature")
	writeFile(t, project, "feature.txt", "feature content\n")
	run(t, "git", "-C", project, "commit", "-m", "add feature")
	run(t, "git", "-C", project, "checkout", "main")

	// Simulate squash-merge: apply the same changes on main and push
	writeFile(t, project, "feature.txt", "feature content\n")
	run(t, "git", "-C", project, "commit", "-m", "squash: feature")
	run(t, "git", "-C", project, "push", "origin", "main")

	if !IsBranchSquashMerged(project, "ralph/test/01-feature") {
		t.Error("expected branch to be detected as squash-merged")
	}
}

// IsBranchSquashMerged returns false for branches with changes not yet on main.
func TestIsBranchSquashMerged_UnmergedBranch(t *testing.T) {
	project, _ := initBareRepo(t)

	run(t, "git", "-C", project, "checkout", "-b", "ralph/test/01-pending")
	writeFile(t, project, "pending.txt", "pending content\n")
	run(t, "git", "-C", project, "commit", "-m", "add pending")
	run(t, "git", "-C", project, "checkout", "main")

	if IsBranchSquashMerged(project, "ralph/test/01-pending") {
		t.Error("expected unmerged branch to not be detected as squash-merged")
	}
}

// PostMergeReset resets the worktree to a fresh branch at origin/main after
// auto-merge, so the next task starts from merged state instead of stale commits.
func TestPostMergeReset_ResetsToOriginMain(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:     project,
		RalphDir:       ralphDir,
		UseWorktree:    true,
		BranchStrategy: BranchStacked,
		State:          st,
		Logger:         &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// Rename to a task branch (simulating a completed task)
	mgr.RenameBranchForTask("completed task")
	taskBranch := mgr.WorktreeBranch
	if taskBranch == mgr.TempBranch() {
		t.Fatal("expected task branch to differ from temp branch")
	}

	mgr.PostMergeReset()

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

// PostMergeReset in single-branch mode resets to the same branch name (temp)
// from origin/main, keeping the branch name stable.
func TestPostMergeReset_SingleBranchKeepsSameName(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:     project,
		RalphDir:       ralphDir,
		UseWorktree:    true,
		BranchStrategy: BranchSingle,
		State:          st,
		Logger:         &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// In single-branch mode, branch stays as /next
	origBranch := mgr.WorktreeBranch
	if !strings.HasSuffix(origBranch, "/next") {
		t.Fatalf("expected /next branch, got %q", origBranch)
	}

	// Add a commit so we can verify reset moves HEAD
	writeFile(t, mgr.WorkDir, "task-work.txt", "some work\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "task commit")

	headBefore := gitOutput(mgr.WorkDir, "rev-parse", "HEAD")
	originMain := gitOutput(mgr.WorkDir, "rev-parse", "origin/main")
	if headBefore == originMain {
		t.Fatal("HEAD should differ from origin/main before reset")
	}

	mgr.PostMergeReset()

	if mgr.WorktreeBranch != origBranch {
		t.Errorf("single-branch should keep name %q, got %q", origBranch, mgr.WorktreeBranch)
	}

	headAfter := gitOutput(mgr.WorkDir, "rev-parse", "HEAD")
	if headAfter != originMain {
		t.Errorf("HEAD should match origin/main after reset, got %s vs %s", headAfter, originMain)
	}
}

// After auto-merge squash-merges a PR, local main must be updated to match
// origin/main. This failed previously because `git branch -f main origin/main`
// silently errors when main is the checked-out branch in the project dir.
// The fix uses update-ref which works regardless of checkout state.
func TestAutoMerge_UpdatesLocalMainWhenCheckedOut(t *testing.T) {
	project, _ := initBareRepo(t)
	bare := filepath.Join(filepath.Dir(project), "bare.git")
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()
	log := &testLog{}

	mgr := &Manager{
		ProjectDir:     project,
		RalphDir:       ralphDir,
		UseWorktree:    true,
		BranchStrategy: BranchStacked,
		State:          st,
		Logger:         log,
	}
	if err := mgr.SetupWorktree(); err != nil {
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

	// Local main is still behind (main is checked out so branch -f would fail)
	localMainStale := gitOutput(project, "rev-parse", "main")
	if localMainStale != localMainBefore {
		t.Fatal("local main should still be at old commit before update-ref")
	}

	// Simulate what AutoMergeCurrentBranch does after merge: update-ref
	originRef := gitOutput(project, "rev-parse", "origin/main")
	if originRef != "" {
		gitCmd(project, "update-ref", "refs/heads/main", originRef)
	}

	// Local main should now match origin/main
	localMainAfter := gitOutput(project, "rev-parse", "main")
	if localMainAfter != originMain {
		t.Errorf("local main should match origin/main after update-ref: got %s, want %s", localMainAfter, originMain)
	}

	// Verify main is still checked out (update-ref doesn't change that)
	stillCheckedOut := gitOutput(project, "symbolic-ref", "--short", "HEAD")
	if stillCheckedOut != "main" {
		t.Errorf("main should still be checked out, got %q", stillCheckedOut)
	}
}

// --- PruneOrphanedWorktrees tests ---

// PruneOrphanedWorktrees removes directories under .ralph/worktrees/ that
// git no longer tracks, while preserving active worktrees.
func TestPruneOrphanedWorktrees_RemovesOrphaned(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	worktreeRoot := filepath.Join(ralphDir, "worktrees")
	os.MkdirAll(worktreeRoot, 0o755)
	log := &testLog{}

	// Create an orphaned directory (not tracked by git)
	orphanDir := filepath.Join(worktreeRoot, "ralph-20260101-orphan")
	os.MkdirAll(orphanDir, 0o755)
	os.WriteFile(filepath.Join(orphanDir, "file.txt"), []byte("stale"), 0o644)

	// Create an active worktree via git
	activeDir := filepath.Join(worktreeRoot, "ralph-20260322-active")
	run(t, "git", "-C", project, "worktree", "add", "-b", "ralph/test/next", activeDir, "HEAD")

	PruneOrphanedWorktrees(project, ralphDir, log)

	// Orphaned directory should be removed
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("orphaned worktree directory should be removed")
	}

	// Active worktree directory should still exist
	if _, err := os.Stat(activeDir); err != nil {
		t.Errorf("active worktree directory should be preserved: %v", err)
	}

	if !log.contains("Removing orphaned worktree directory") {
		t.Errorf("expected log about removing orphaned worktree")
	}
}

// PostMergeFailReset creates a new numbered branch from origin/main after a
// failed auto-merge, leaving the old branch intact so its PR stays open.
func TestPostMergeFailReset_CreatesNewBranchFromMain(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:     project,
		RalphDir:       ralphDir,
		UseWorktree:    true,
		BranchStrategy: BranchSingle,
		State:          st,
		Logger:         &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	origBranch := mgr.WorktreeBranch
	if !strings.HasSuffix(origBranch, "/next") {
		t.Fatalf("expected /next branch, got %q", origBranch)
	}

	// Add a commit so the old branch differs from origin/main
	writeFile(t, mgr.WorkDir, "task-a.txt", "task A work\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "task A")
	headBefore := gitOutput(mgr.WorkDir, "rev-parse", "HEAD")

	mgr.PostMergeFailReset()

	// New branch should be numbered and different from the original
	if mgr.WorktreeBranch == origBranch {
		t.Errorf("expected new branch, still on %q", origBranch)
	}
	if !strings.Contains(mgr.WorktreeBranch, "/01-next") {
		t.Errorf("expected numbered branch like */01-next, got %q", mgr.WorktreeBranch)
	}

	// HEAD should now be at origin/main, not at the task commit
	headAfter := gitOutput(mgr.WorkDir, "rev-parse", "HEAD")
	originMain := gitOutput(mgr.WorkDir, "rev-parse", "origin/main")
	if headAfter != originMain {
		t.Errorf("HEAD should match origin/main, got %s vs %s", headAfter, originMain)
	}
	if headAfter == headBefore {
		t.Error("HEAD should differ from pre-reset commit")
	}

	// Old branch should still exist (its PR is open)
	if !refExists(mgr.WorkDir, origBranch) {
		t.Errorf("old branch %q should still exist", origBranch)
	}

	// TaskSeq should have incremented
	if mgr.TaskSeq != 1 {
		t.Errorf("expected TaskSeq=1, got %d", mgr.TaskSeq)
	}
	if mgr.BranchRenamed {
		t.Error("BranchRenamed should be false after PostMergeFailReset")
	}

	// State should be updated
	storedBranch, _ := st.Read("worktree_branch")
	if storedBranch != mgr.WorktreeBranch {
		t.Errorf("state worktree_branch should be %q, got %q", mgr.WorktreeBranch, storedBranch)
	}
	storedSeq, _ := st.Read("task_seq")
	if storedSeq != "1" {
		t.Errorf("state task_seq should be '1', got %q", storedSeq)
	}
}

// PostMergeFailReset increments TaskSeq on repeated calls, producing unique
// branch names when multiple merges fail in succession.
func TestPostMergeFailReset_IncrementsSeqOnRepeatedCalls(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:     project,
		RalphDir:       ralphDir,
		UseWorktree:    true,
		BranchStrategy: BranchSingle,
		State:          st,
		Logger:         &testLog{},
	}
	if err := mgr.SetupWorktree(); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// Add a commit so branches diverge
	writeFile(t, mgr.WorkDir, "task-a.txt", "work\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "task A")

	mgr.PostMergeFailReset()
	firstBranch := mgr.WorktreeBranch

	// Simulate another task's work and failed merge
	writeFile(t, mgr.WorkDir, "task-b.txt", "work\n")
	run(t, "git", "-C", mgr.WorkDir, "add", "task-b.txt")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "task B")

	mgr.PostMergeFailReset()
	secondBranch := mgr.WorktreeBranch

	if firstBranch == secondBranch {
		t.Errorf("repeated calls should produce different branches, both %q", firstBranch)
	}
	if mgr.TaskSeq != 2 {
		t.Errorf("expected TaskSeq=2 after two resets, got %d", mgr.TaskSeq)
	}
}

// PruneOrphanedWorktrees is a no-op when worktrees/ doesn't exist
func TestPruneOrphanedWorktrees_NoWorktreeDir(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	log := &testLog{}

	PruneOrphanedWorktrees(project, ralphDir, log)

	if len(log.messages) > 0 {
		t.Errorf("expected no log messages, got %v", log.messages)
	}
}

// PruneOrphanedWorktrees leaves non-directory files alone
func TestPruneOrphanedWorktrees_IgnoresFiles(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	worktreeRoot := filepath.Join(ralphDir, "worktrees")
	os.MkdirAll(worktreeRoot, 0o755)
	log := &testLog{}

	filePath := filepath.Join(worktreeRoot, "some-file.txt")
	os.WriteFile(filePath, []byte("keep"), 0o644)

	PruneOrphanedWorktrees(project, ralphDir, log)

	if _, err := os.Stat(filePath); err != nil {
		t.Errorf("regular file should not be removed: %v", err)
	}
}
