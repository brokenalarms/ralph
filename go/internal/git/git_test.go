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
	run(t, "git", "init", "--bare", "-b", "main", bare)

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

	mgr.RenameBranchForTask("old task name", "")
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

	mgr.RenameBranchForTask("first task", "")
	mgr.RotateBranch()
	mgr.RenameBranchForTask("second task", "")

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

	mgr.RenameBranchForTask("first task", "")
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
	mgr2.RenameBranchForTask("second task", "")

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

// Resume detects a squash-merged branch and silently resets the worktree to
// origin/main instead of leaving stale commits that will conflict on rebase.
func TestSetupWorktree_ResumeResetsSquashMergedBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	log := &testLog{}

	// Create worktree with a task branch and a commit
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
	mgr.RenameBranchForTask("feature work", "")
	writeFile(t, mgr.WorkDir, "feature.txt", "feature content\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "add feature")
	oldBranch := mgr.WorktreeBranch

	// Simulate squash-merge: apply the same changes on main and push
	run(t, "git", "-C", project, "checkout", "main")
	writeFile(t, project, "feature.txt", "feature content\n")
	run(t, "git", "-C", project, "commit", "-m", "squash: feature work")
	run(t, "git", "-C", project, "push", "origin", "main")
	run(t, "git", "-C", project, "checkout", "-")

	// Resume — should detect squash-merged branch and reset to main
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

	tempBranch := mgr2.TempBranch()
	if mgr2.WorktreeBranch != tempBranch {
		t.Errorf("branch = %q, want temp branch %q after squash-merge reset", mgr2.WorktreeBranch, tempBranch)
	}
	if refExists(mgr2.WorkDir, oldBranch) {
		t.Errorf("old branch %q should be deleted after squash-merge reset", oldBranch)
	}
	if !log.contains("Stale branch detected") {
		t.Error("expected log message about stale branch detection")
	}
}

// Resume with a deleted branch ref (e.g. killed between merge and reset)
// silently recreates from main instead of failing.
func TestSetupWorktree_ResumeResetsDeletedBranch(t *testing.T) {
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
	mgr.RenameBranchForTask("doomed task", "")

	// Force the worktree onto a different branch so the old one can be deleted
	run(t, "git", "-C", mgr.WorkDir, "checkout", "-B", "ralph/"+mgr.ProjectName+"/next", "HEAD")
	run(t, "git", "-C", mgr.WorkDir, "branch", "-D", mgr.WorktreeBranch)

	// But state still references the deleted branch
	state.Write("worktree_branch", mgr.WorktreeBranch)

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

	tempBranch := mgr2.TempBranch()
	if mgr2.WorktreeBranch != tempBranch {
		t.Errorf("branch = %q, want temp branch %q after deleted-branch reset", mgr2.WorktreeBranch, tempBranch)
	}
	if !log.contains("Stale branch detected") {
		t.Error("expected log message about stale branch detection")
	}
}

// Resume with a valid, non-squash-merged branch should NOT reset — the
// branch has work that still needs to be rebased normally.
func TestSetupWorktree_ResumeKeepsValidBranch(t *testing.T) {
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
	mgr.RenameBranchForTask("in progress work", "")
	writeFile(t, mgr.WorkDir, "wip.txt", "work in progress\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "wip commit")
	branchBefore := mgr.WorktreeBranch

	// Resume without squash-merging — branch should be preserved
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

	if mgr2.WorktreeBranch != branchBefore {
		t.Errorf("branch = %q, want %q (valid branch should be preserved)", mgr2.WorktreeBranch, branchBefore)
	}
	if log.contains("Stale branch detected") {
		t.Error("should not detect stale branch for valid in-progress work")
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

	mgr.RenameBranchForTask("Fix auth bug", "")

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

// RenameBranchForTask includes the bead ID in the branch name for traceability
func TestRenameBranchForTask_IncludesTaskID(t *testing.T) {
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

	mgr.RenameBranchForTask("Fix auth bug", "ralph-abc1")

	wantBranch := "ralph/" + mgr.ProjectName + "/01-ralph-abc1-fix-auth-bug"
	if mgr.WorktreeBranch != wantBranch {
		t.Errorf("branch = %q, want %q", mgr.WorktreeBranch, wantBranch)
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

	mgr.RenameBranchForTask("First task", "")
	firstBranch := mgr.WorktreeBranch

	mgr.RenameBranchForTask("Second task", "")
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
	mgr.RenameBranchForTask("anything", "")
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
	mgr.RenameBranchForTask("Some task", "")
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
	mgr.RenameBranchForTask("First", "")
	if mgr.TaskSeq != 1 {
		t.Errorf("TaskSeq = %d, want 1", mgr.TaskSeq)
	}

	// Rotate and rename again
	mgr.RotateBranch()
	mgr.RenameBranchForTask("Second", "")
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

	mgr.RenameBranchForTask("First task", "")

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

	mgr.RenameBranchForTask("first task", "")
	mgr.RotateBranch()
	mgr.RenameBranchForTask("second task", "")

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

// --- AutoMergeCurrentBranch tests ---

// AutoMergeCurrentBranch returns nil when no worktree branch is set,

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
	mgr.RenameBranchForTask("Some task", "")

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
	mgr.RenameBranchForTask("First task", "")

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

	if !IsBranchSquashMerged(project, "ralph/test/01-feature", "") {
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

	if IsBranchSquashMerged(project, "ralph/test/01-pending", "") {
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
	mgr.RenameBranchForTask("completed task", "")
	taskBranch := mgr.WorktreeBranch
	if taskBranch == mgr.TempBranch() {
		t.Fatal("expected task branch to differ from temp branch")
	}

	if err := mgr.PostMergeReset(); err != nil {
		t.Fatalf("PostMergeReset: %v", err)
	}

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

	if err := mgr.PostMergeReset(); err != nil {
		t.Fatalf("PostMergeReset: %v", err)
	}

	if mgr.WorktreeBranch != origBranch {
		t.Errorf("single-branch should keep name %q, got %q", origBranch, mgr.WorktreeBranch)
	}

	headAfter := gitOutput(mgr.WorkDir, "rev-parse", "HEAD")
	if headAfter != originMain {
		t.Errorf("HEAD should match origin/main after reset, got %s vs %s", headAfter, originMain)
	}
}

// Force-reset must clean both dirty tracked files and untracked files left
// by the previous task, so the next task starts with a pristine worktree
// matching origin/main exactly.
func TestPostMergeReset_CleansUntrackedAndDirtyFiles(t *testing.T) {
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

	mgr.RenameBranchForTask("dirty task", "")

	// Create an untracked file (simulating build artifacts or generated files)
	untrackedPath := filepath.Join(mgr.WorkDir, "leftover-artifact.txt")
	if err := os.WriteFile(untrackedPath, []byte("stale artifact\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Modify a tracked file without committing (dirty working tree)
	trackedPath := filepath.Join(mgr.WorkDir, "dirty-edit.txt")
	writeFile(t, mgr.WorkDir, "dirty-edit.txt", "original\n")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "add tracked file")
	if err := os.WriteFile(trackedPath, []byte("modified\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := mgr.PostMergeReset(); err != nil {
		t.Fatalf("PostMergeReset: %v", err)
	}

	if _, err := os.Stat(untrackedPath); !os.IsNotExist(err) {
		t.Error("untracked file should have been removed by force-reset")
	}

	// Tracked file from the task branch should no longer exist (it wasn't on origin/main)
	if _, err := os.Stat(trackedPath); !os.IsNotExist(err) {
		t.Error("tracked file from task branch should not exist after reset to origin/main")
	}

	headAfter := gitOutput(mgr.WorkDir, "rev-parse", "HEAD")
	originMain := gitOutput(mgr.WorkDir, "rev-parse", "origin/main")
	if headAfter != originMain {
		t.Errorf("HEAD should match origin/main, got %s vs %s", headAfter, originMain)
	}
}

// After auto-merge squash-merges a PR, postMergeUpdate must advance local
// main to match origin/main without leaving stale staged changes. The old
// two-step approach (update-ref + reset --hard HEAD) left the index pointing
// at the old tree between steps, staging reversions of merged PR work. The
// fix uses a single atomic `git reset --hard origin/main`.
func TestPostMergeUpdate_AtomicResetNoStagedChanges(t *testing.T) {
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

	// Call postMergeUpdate (the method under test)
	merged, err := mgr.postMergeUpdate("42")
	if err != nil {
		t.Fatalf("postMergeUpdate failed: %v", err)
	}
	if !merged {
		t.Fatal("postMergeUpdate should return true")
	}

	// Local main should now match origin/main
	localMainAfter := gitOutput(project, "rev-parse", "main")
	if localMainAfter != originMain {
		t.Errorf("local main should match origin/main: got %s, want %s", localMainAfter, originMain)
	}

	// Main must still be checked out
	stillCheckedOut := gitOutput(project, "symbolic-ref", "--short", "HEAD")
	if stillCheckedOut != "main" {
		t.Errorf("main should still be checked out, got %q", stillCheckedOut)
	}

	// The index must have no staged changes — this is the critical assertion.
	// The old update-ref approach left the index stale, causing `git diff --cached`
	// to show staged reversions of the merged PR's files.
	diffIndex := strings.TrimSpace(gitOutput(project, "diff", "--cached", "--name-only"))
	if diffIndex != "" {
		t.Errorf("project dir should have no staged changes after postMergeUpdate, got:\n%s", diffIndex)
	}
}

// Proves the old two-step approach (update-ref + reset --hard HEAD) leaves
// stale staged changes, demonstrating why atomic reset is necessary.
func TestPostMergeUpdate_TwoStepLeavesStaleIndex(t *testing.T) {
	project, _ := initBareRepo(t)
	bare := filepath.Join(filepath.Dir(project), "bare.git")

	localMainBefore := gitOutput(project, "rev-parse", "main")

	// Push a new commit to origin/main via a temp clone.
	tmpClone := filepath.Join(t.TempDir(), "tmp-clone")
	run(t, "git", "clone", bare, tmpClone)
	writeFile(t, tmpClone, "merged-work.txt", "merged content\n")
	run(t, "git", "-C", tmpClone, "commit", "-m", "squash-merged PR")
	run(t, "git", "-C", tmpClone, "push", "origin", "main")

	run(t, "git", "-C", project, "fetch", "origin", "main")

	originMain := gitOutput(project, "rev-parse", "origin/main")
	if originMain == localMainBefore {
		t.Fatal("origin/main should have advanced")
	}

	// Reproduce the old buggy two-step: update-ref advances the ref but
	// the index still reflects the old commit's tree.
	gitCmd(project, "update-ref", "refs/heads/main", originMain)
	// At this point git diff --cached will show staged reversions because
	// the index doesn't match the new HEAD.
	diffAfterUpdateRef := strings.TrimSpace(gitOutput(project, "diff", "--cached", "--name-only"))
	if diffAfterUpdateRef == "" {
		t.Skip("git version doesn't exhibit the stale-index behavior after update-ref")
	}

	// Confirm the stale index shows the merged file as a staged deletion
	if !strings.Contains(diffAfterUpdateRef, "merged-work.txt") {
		t.Errorf("expected stale index to show merged-work.txt as staged change, got:\n%s", diffAfterUpdateRef)
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
