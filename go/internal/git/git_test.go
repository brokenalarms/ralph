package git

import (
	"context"
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

func (l *testLog) Log(_ string, format string, args ...any) {
	l.messages = append(l.messages, fmt.Sprintf(format, args...))
}
func (l *testLog) Warn(_ string, format string, args ...any) {
	l.messages = append(l.messages, "WARN: "+fmt.Sprintf(format, args...))
}
func (l *testLog) Error(_ string, format string, args ...any) {
	l.messages = append(l.messages, "ERROR: "+fmt.Sprintf(format, args...))
}

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
func initBareRepoWithBranch(t *testing.T, branch string) (string, func()) {
	t.Helper()
	tmp := t.TempDir()

	bare := filepath.Join(tmp, "bare.git")
	run(t, "git", "init", "--bare", "-b", branch, bare)

	project := filepath.Join(tmp, "project")
	run(t, "git", "clone", bare, project)
	run(t, "git", "-C", project, "config", "user.name", "test")
	run(t, "git", "-C", project, "config", "user.email", "test@test")
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "init")
	run(t, "git", "-C", project, "push", "-u", "origin", branch)
	run(t, "git", "-C", project, "remote", "set-head", "origin", branch)

	return project, func() {}
}

func initBareRepo(t *testing.T) (string, func()) {
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

// Slugs are limited to 4 words to keep branch names short
func TestSlugify_LimitsToFourWords(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"execution prompt reinforce boy scout rule applies", "execution-prompt-reinforce-boy"},
		{"fix a bug", "fix-a-bug"},
		{"one", "one"},
		{"limit branch name slug to three four words after bead ID", "limit-branch-name-slug"},
		{"hello world foo bar", "hello-world-foo-bar"},
		{"hello world foo bar baz", "hello-world-foo-bar"},
	}
	for _, tc := range cases {
		got := Slugify(tc.in)
		if got != tc.want {
			t.Errorf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}


// SetupWorktree creates a new worktree in .ralph/worktrees and records state
func TestSetupWorktree_CreatesWorktree(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	log := &testLog{}

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				State:       state,
		Logger:      log,
	}

	if err := mgr.SetupWorktree(context.Background()); err != nil {
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

// SetupWorktree in a non-git directory should return an error
func TestSetupWorktree_NonGitDirErrors(t *testing.T) {
	tmp := t.TempDir()
	mgr := &Manager{
		ProjectDir:  tmp,
		RalphDir:    filepath.Join(tmp, ".ralph"),
				Logger:      &testLog{},
	}

	err := mgr.SetupWorktree(context.Background())
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
				State:       state,
		Logger:      log,
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	firstWorkDir := mgr.WorkDir
	firstBranch := mgr.WorktreeBranch

	// Second run: resume
	mgr2 := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				Resume:      true,
		State:       state,
		Logger:      log,
	}
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      log,
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("old task name", "")
	oldBranch := mgr.WorktreeBranch

	log.messages = nil

	mgr2 := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				Resume:      true,
		State:       state,
		Logger:      log,
	}
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      log,
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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
				Resume:      true,
		State:       state,
		Logger:      log,
	}
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      log,
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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
				Resume:      true,
		State:       state,
		Logger:      log,
	}
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      log,
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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
				Resume:      true,
		State:       state,
		Logger:      log,
	}
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      log,
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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
				Resume:      true,
		State:       state,
		Logger:      log,
	}
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      log,
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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
				Resume:      true,
		State:       state,
		Logger:      log,
	}
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("resume SetupWorktree: %v", err)
	}

	if mgr2.WorktreeBranch != branchBefore {
		t.Errorf("branch = %q, want %q (valid branch should be preserved)", mgr2.WorktreeBranch, branchBefore)
	}
	if log.contains("Stale branch detected") {
		t.Error("should not detect stale branch for valid in-progress work")
	}
}


// RenameBranchForTask gives the temp branch a descriptive name for the current task
func TestRenameBranchForTask_RenamesBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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


// RotateBranch creates a fresh temp branch for the next iteration
func TestRotateBranch_CreatesNewTempBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	log := &testLog{}

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				State:       state,
		Logger:      log,
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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


// TaskSeq increments with each branch rename, producing sequential branch names
func TestTaskSeq_IncrementsAcrossRotations(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      log,
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	firstWorkDir := mgr.WorkDir

	os.RemoveAll(firstWorkDir)

	mgr2 := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				State:       newMemState(),
		Logger:      &testLog{},
	}
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	firstWorkDir := mgr.WorkDir

	mgr2 := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				State:       newMemState(),
		Logger:      &testLog{},
	}
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
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
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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
				Resume:      true,
		State:       state,
		Logger:      &testLog{},
	}
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
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

	preSetupMgr := &Manager{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, Logger: &testLog{}}
	preSetupMgr.EnsureGitignored(".ralph")
	run(t, "git", "-C", project, "push", "origin", "main", "-q")

	state := newMemState()
	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
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

	mgr := &Manager{ProjectDir: project, WorkDir: project, Logger: &testLog{}}
	mgr.EnsureGitignored(".ralph")

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

	dirtyMgr := &Manager{ProjectDir: project, WorkDir: project, Logger: &testLog{}}
	if !dirtyMgr.HasUncommittedChanges() {
		t.Error("should detect uncommitted changes")
	}
}

// RenameBranchForTask renames and RotateBranch rotates branches per task.
func TestRenameBranchForTask_AndRotateBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Manager{
		ProjectDir:  project,
		RalphDir:    ralphDir,
				State:       st,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	origBranch := mgr.WorktreeBranch
	mgr.RenameBranchForTask("First task", "")

	if mgr.WorktreeBranch == origBranch {
		t.Error("should rename the branch")
	}
	if !mgr.BranchRenamed {
		t.Error("BranchRenamed should be true after rename")
	}
	if mgr.TaskSeq != 1 {
		t.Errorf("TaskSeq should be 1, got %d", mgr.TaskSeq)
	}

	taskBranch := mgr.WorktreeBranch
	mgr.RotateBranch()

	if mgr.WorktreeBranch == taskBranch {
		t.Error("should rotate the branch")
	}
	if !strings.HasSuffix(mgr.WorktreeBranch, "/next") {
		t.Errorf("rotated branch %q should end with /next", mgr.WorktreeBranch)
	}
}


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

	mgr := &Manager{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, Logger: log}
	mgr.PruneOrphanedWorktrees()

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

	mgr := &Manager{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, Logger: log}
	mgr.PruneOrphanedWorktrees()

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

	mgr := &Manager{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, Logger: log}
	mgr.PruneOrphanedWorktrees()

	if _, err := os.Stat(filePath); err != nil {
		t.Errorf("regular file should not be removed: %v", err)
	}
}

