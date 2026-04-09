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

	"github.com/brokenalarms/ralph/internal/logging"
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
	messages   []string
	lineBuffer string
}

func (l *testLog) format(o logging.Opts, format string, args ...any) string {
	msg := fmt.Sprintf(format, args...)
	if o.Link != nil {
		msg += " " + o.Link.URL + " " + o.Link.Text
	}
	if o.Branch != "" {
		msg += " " + o.Branch
	}
	switch o.Level {
	case logging.Warn:
		return "WARN: " + msg
	case logging.Error:
		return "ERROR: " + msg
	default:
		return msg
	}
}

func (l *testLog) Emit(o logging.Opts, format string, args ...any) {
	l.messages = append(l.messages, l.format(o, format, args...))
}

// EmitInPlace starts an in-place line by recording the formatted first segment.
func (l *testLog) EmitInPlace(o logging.Opts, format string, args ...any) {
	l.lineBuffer = l.format(o, format, args...)
}

// EmitAppend appends raw text to the current in-place line buffer.
func (l *testLog) EmitAppend(format string, args ...any) {
	l.lineBuffer += fmt.Sprintf(format, args...)
}

// EmitFinalInPlace commits the accumulated line buffer to messages.
func (l *testLog) EmitFinalInPlace() {
	if l.lineBuffer != "" {
		l.messages = append(l.messages, l.lineBuffer)
		l.lineBuffer = ""
	}
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

func cmdOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v failed: %v", name, args, err)
	}
	return string(out)
}


// BranchIsAncestorOfMain returns true when a branch's work has landed on main.
func TestBranchIsAncestorOfMain(t *testing.T) {
	project, _ := initBareRepo(t)
	mgr := &Repo{
		ProjectDir: project,
		BaseBranch: "main",
		WorkDir:    project,
		Logger:     &testLog{},
	}

	// Create a feature branch with work ahead of main.
	run(t, "git", "-C", project, "checkout", "-b", "feature-ahead")
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "feature work")
	run(t, "git", "-C", project, "push", "-u", "origin", "feature-ahead")
	run(t, "git", "-C", project, "checkout", "main")

	if mgr.BranchIsAncestorOfMain("feature-ahead") {
		t.Error("feature-ahead has unmerged work, should not be ancestor of main")
	}

	// Simulate merge: fast-forward main to include the feature work.
	run(t, "git", "-C", project, "merge", "feature-ahead")
	run(t, "git", "-C", project, "push", "origin", "main")

	if !mgr.BranchIsAncestorOfMain("feature-ahead") {
		t.Error("feature-ahead was merged, should be ancestor of main")
	}

	if mgr.BranchIsAncestorOfMain("no-such-branch") {
		t.Error("non-existent branch should not be ancestor of main")
	}
}

// BranchIsAheadOfMain returns true only when the branch is cleanly ahead
// (main is an ancestor). Diverged and landed branches return false.
func TestBranchIsAheadOfMain(t *testing.T) {
	project, _ := initBareRepo(t)
	mgr := &Repo{
		ProjectDir: project,
		BaseBranch: "main",
		WorkDir:    project,
		Logger:     &testLog{},
	}

	// Branch with work ahead of main — should return true.
	run(t, "git", "-C", project, "checkout", "-b", "ahead-branch")
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "ahead work")
	run(t, "git", "-C", project, "push", "-u", "origin", "ahead-branch")
	run(t, "git", "-C", project, "checkout", "main")

	if !mgr.BranchIsAheadOfMain("ahead-branch") {
		t.Error("ahead-branch is cleanly ahead of main, should return true")
	}

	// Merge the branch, then advance main past it — branch is now behind.
	run(t, "git", "-C", project, "merge", "ahead-branch")
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "main moves ahead")
	run(t, "git", "-C", project, "push", "origin", "main")

	if mgr.BranchIsAheadOfMain("ahead-branch") {
		t.Error("ahead-branch is behind main (landed), should return false")
	}

	// Diverged branch: branch and main both have commits the other doesn't.
	run(t, "git", "-C", project, "checkout", "-b", "diverged-branch")
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "diverged work")
	run(t, "git", "-C", project, "push", "-u", "origin", "diverged-branch")
	run(t, "git", "-C", project, "checkout", "main")
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "main diverges")
	run(t, "git", "-C", project, "push", "origin", "main")

	if mgr.BranchIsAheadOfMain("diverged-branch") {
		t.Error("diverged-branch has diverged from main, should return false")
	}

	if mgr.BranchIsAheadOfMain("no-such-branch") {
		t.Error("non-existent branch should return false")
	}
}

// BranchName returns a canonical branch name from beadID and slug
func TestBranchName(t *testing.T) {
	// With bead ID: ralph/<beadID>-<slug>
	got := BranchName("ralph-abc1", "fix-auth-bug")
	want := "ralph/ralph-abc1-fix-auth-bug"
	if got != want {
		t.Errorf("BranchName with beadID = %q, want %q", got, want)
	}

	// Without bead ID: ralph/<slug>
	got = BranchName("", "fix-auth-bug")
	want = "ralph/fix-auth-bug"
	if got != want {
		t.Errorf("BranchName without beadID = %q, want %q", got, want)
	}
}

func TestWipBranchName(t *testing.T) {
	got := WipBranchName()
	want := "ralph/next"
	if got != want {
		t.Errorf("WipBranchName = %q, want %q", got, want)
	}
}

func TestNormalizeBranch_StripsDuplicatePrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ralph/ralph/ralph-abc-slug", "ralph/ralph-abc-slug"},
		{"ralph/ralph-abc-slug", "ralph/ralph-abc-slug"},
		{"ralph-abc-slug", "ralph/ralph-abc-slug"},
		{"wip", "ralph/wip"},
	}
	for _, tc := range cases {
		got := normalizeBranch(tc.in)
		if got != tc.want {
			t.Errorf("normalizeBranch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// BranchListPattern returns the glob for listing all ralph branches
func TestBranchListPattern(t *testing.T) {
	got := BranchListPattern()
	want := "ralph/*"
	if got != want {
		t.Errorf("BranchListPattern = %q, want %q", got, want)
	}
}

// Changing BranchName format is a one-line change (branchPrefix constant)
func TestBranchName_UsesSharedPrefix(t *testing.T) {
	bn := BranchName("id", "slug")
	wip := WipBranchName()
	pattern := BranchListPattern()

	// All three share the same prefix
	prefix := "ralph/"
	if !strings.HasPrefix(bn, prefix) {
		t.Errorf("BranchName %q missing prefix %q", bn, prefix)
	}
	if !strings.HasPrefix(wip, prefix) {
		t.Errorf("WipBranchName %q missing prefix %q", wip, prefix)
	}
	if !strings.HasPrefix(pattern, prefix) {
		t.Errorf("BranchListPattern %q missing prefix %q", pattern, prefix)
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

	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
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

	// Branch name must be exactly ralph/next (placeholder between tasks)
	wantBranch := "ralph/next"
	if mgr.WorktreeBranch != wantBranch {
		t.Errorf("branch = %q, want %q", mgr.WorktreeBranch, wantBranch)
	}
}

// SetupWorktree in a non-git directory should return an error
func TestSetupWorktree_NonGitDirErrors(t *testing.T) {
	tmp := t.TempDir()
	mgr := &Repo{
		ProjectDir:  tmp,
		BaseBranch: "main",
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
	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
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
	mgr2 := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
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

	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
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

	mgr2 := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
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


// RenameBranchForTask does not set PrevBranch — that's controlled by
// setStackHead in the loop via SetPrevBranch. Rename only changes the
// branch name.
func TestRenameBranchForTask_DoesNotSetPrevBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	log := &testLog{}

	mgr := &Repo{
		ProjectDir: project,
		BaseBranch: "main",
		RalphDir:   ralphDir,
		State:      state,
		Logger:     log,
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("first task", "ralph-aaa")
	mgr.PrepareForNextTask("ralph-bbb")
	mgr.RenameBranchForTask("second task", "ralph-bbb")

	if mgr.PrevBranch != "" {
		t.Errorf("PrevBranch = %q, want empty (set by setStackHead, not rename)", mgr.PrevBranch)
	}
}

// PrepareForNextTask creates a fresh wip branch so the next task doesn't
// reuse the previous task's branch name.
func TestPrepareForNextTask_CreatesFreshBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := &Repo{
		ProjectDir: project,
		BaseBranch: "main",
		RalphDir:   ralphDir,
		State:      state,
		Logger:     &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("first task", "ralph-aaa")
	firstBranch := mgr.WorktreeBranch
	if firstBranch == "ralph/next" {
		t.Fatal("branch should have been renamed from ralph/next")
	}

	mgr.PrepareForNextTask("ralph-bbb")

	if mgr.WorktreeBranch != "ralph/next" {
		t.Errorf("after PrepareForNextTask, branch = %q, want ralph/next", mgr.WorktreeBranch)
	}
	if mgr.BranchRenamed {
		t.Error("BranchRenamed should be false after PrepareForNextTask")
	}

	mgr.RenameBranchForTask("second task", "ralph-bbb")
	if mgr.WorktreeBranch == firstBranch {
		t.Errorf("second task reused first task's branch %q", firstBranch)
	}
	if mgr.WorktreeBranch == "ralph/next" {
		t.Error("second task should have been renamed from ralph/next")
	}
}

// PrepareForNextTask discards uncommitted changes so dirty files from a
// previous task don't carry over into the next task's branch.
func TestPrepareForNextTask_DiscardsUncommittedChanges(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Repo{
		ProjectDir: project,
		BaseBranch: "main",
		RalphDir:   ralphDir,
		State:      newMemState(),
		Logger:     &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("first task", "ralph-aaa")

	// Simulate dirty state: tracked modification and untracked file.
	trackedFile := filepath.Join(mgr.WorkDir, "tracked.txt")
	if err := os.WriteFile(trackedFile, []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", mgr.WorkDir, "add", "tracked.txt")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "add tracked")

	// Modify the tracked file without committing.
	if err := os.WriteFile(trackedFile, []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create an untracked file.
	untrackedFile := filepath.Join(mgr.WorkDir, "untracked.txt")
	if err := os.WriteFile(untrackedFile, []byte("untracked"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr.PrepareForNextTask("ralph-bbb")

	// Tracked file must be restored to its committed content.
	got, err := os.ReadFile(trackedFile)
	if err != nil {
		t.Fatalf("reading tracked file: %v", err)
	}
	if string(got) != "initial" {
		t.Errorf("tracked file content = %q, want %q", string(got), "initial")
	}

	// Untracked file must be removed.
	if _, err := os.Stat(untrackedFile); !os.IsNotExist(err) {
		t.Error("untracked file should have been removed by PrepareForNextTask")
	}
}

// PrepareForNextTask deletes the old task branch after moving to the
// placeholder, so completed task branches don't accumulate locally.
func TestPrepareForNextTask_DeletesOldBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := &Repo{
		ProjectDir: project,
		BaseBranch: "main",
		RalphDir:   ralphDir,
		State:      newMemState(),
		Logger:     &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("task to clean up", "ralph-del1")
	taskBranch := mgr.WorktreeBranch

	// Commit so the task branch has history and can be deleted.
	writeFile(t, mgr.WorkDir, "work.txt", "done\n")
	run(t, "git", "-C", mgr.WorkDir, "add", "work.txt")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "task work")

	mgr.PrepareForNextTask("ralph-next1")

	// Worktree must be on the placeholder branch.
	if mgr.WorktreeBranch != WipBranchName() {
		t.Errorf("WorktreeBranch = %q, want %q", mgr.WorktreeBranch, WipBranchName())
	}

	// Old task branch must no longer exist.
	branches := gitOutput(project, "branch", "--list")
	if strings.Contains(branches, taskBranch) {
		t.Errorf("old task branch %q should be deleted after PrepareForNextTask, still in: %s", taskBranch, branches)
	}
}

// PrepareForNextTask discards uncommitted changes when switching to a different
// task (last_task_id differs from nextTaskID), so stale files don't leak across tasks.
func TestPrepareForNextTask_CleansOnTaskSwitch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	_ = state.Write("last_task_id", "ralph-aaa")

	mgr := &Repo{
		ProjectDir: project,
		BaseBranch: "main",
		RalphDir:   ralphDir,
		State:      state,
		Logger:     &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("first task", "ralph-aaa")

	trackedFile := filepath.Join(mgr.WorkDir, "tracked.txt")
	if err := os.WriteFile(trackedFile, []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", mgr.WorkDir, "add", "tracked.txt")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "add tracked")
	if err := os.WriteFile(trackedFile, []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	untrackedFile := filepath.Join(mgr.WorkDir, "untracked.txt")
	if err := os.WriteFile(untrackedFile, []byte("untracked"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr.PrepareForNextTask("ralph-bbb")

	got, err := os.ReadFile(trackedFile)
	if err != nil {
		t.Fatalf("reading tracked file: %v", err)
	}
	if string(got) != "initial" {
		t.Errorf("tracked file content = %q, want %q after task switch", string(got), "initial")
	}
	if _, err := os.Stat(untrackedFile); !os.IsNotExist(err) {
		t.Error("untracked file should have been removed when switching tasks")
	}
}

// PrepareForNextTask preserves uncommitted changes when resuming the same task
// (last_task_id matches nextTaskID), so in-progress work survives crash/restart.
func TestPrepareForNextTask_PreservesChangesWhenResumingSameTask(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	_ = state.Write("last_task_id", "ralph-aaa")

	mgr := &Repo{
		ProjectDir: project,
		BaseBranch: "main",
		RalphDir:   ralphDir,
		State:      state,
		Logger:     &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("first task", "ralph-aaa")

	trackedFile := filepath.Join(mgr.WorkDir, "tracked.txt")
	if err := os.WriteFile(trackedFile, []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", mgr.WorkDir, "add", "tracked.txt")
	run(t, "git", "-C", mgr.WorkDir, "commit", "-m", "add tracked")
	if err := os.WriteFile(trackedFile, []byte("in-progress"), 0o644); err != nil {
		t.Fatal(err)
	}
	untrackedFile := filepath.Join(mgr.WorkDir, "wip.txt")
	if err := os.WriteFile(untrackedFile, []byte("work in progress"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr.PrepareForNextTask("ralph-aaa")

	got, err := os.ReadFile(trackedFile)
	if err != nil {
		t.Fatalf("reading tracked file: %v", err)
	}
	if string(got) != "in-progress" {
		t.Errorf("tracked file content = %q, want %q when resuming same task", string(got), "in-progress")
	}
	if _, err := os.Stat(untrackedFile); os.IsNotExist(err) {
		t.Error("untracked file should be preserved when resuming same task")
	}
}

// SetPrevBranch explicitly sets PrevBranch and persists to state.
func TestSetPrevBranch(t *testing.T) {
	state := newMemState()
	mgr := &Repo{State: state, Logger: &testLog{}}
	mgr.SetPrevBranch("ralph/ralph-abc-task")
	if mgr.PrevBranch != "ralph/ralph-abc-task" {
		t.Errorf("PrevBranch = %q, want ralph/ralph-abc-task", mgr.PrevBranch)
	}
	v, _ := state.Read("prev_branch")
	if v != "ralph/ralph-abc-task" {
		t.Errorf("state prev_branch = %q, want ralph/ralph-abc-task", v)
	}
}

// Resume detects a squash-merged branch and silently resets the worktree to
// origin/main instead of leaving stale commits that will conflict on rebase.
// Resume with a valid branch preserves it and continues from there.
func TestSetupWorktree_ResumeKeepsValidBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	log := &testLog{}

	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
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
	mgr2 := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
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

	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
		RalphDir:    ralphDir,
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	if err := mgr.RenameBranchForTask("Fix auth bug", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantBranch := "ralph/fix-auth-bug"
	if mgr.WorktreeBranch != wantBranch {
		t.Errorf("branch = %q, want %q", mgr.WorktreeBranch, wantBranch)
	}
	if !mgr.BranchRenamed {
		t.Error("BranchRenamed should be true")
	}

	if got, _ := state.Read("worktree_branch"); got != wantBranch {
		t.Errorf("state worktree_branch = %q, want %q", got, wantBranch)
	}
}

// RenameBranchForTask includes the bead ID in the branch name for traceability
func TestRenameBranchForTask_IncludesTaskID(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
		RalphDir:    ralphDir,
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	if err := mgr.RenameBranchForTask("Fix auth bug", "ralph-abc1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantBranch := "ralph/ralph-abc1-fix-auth-bug"
	if mgr.WorktreeBranch != wantBranch {
		t.Errorf("branch = %q, want %q", mgr.WorktreeBranch, wantBranch)
	}
}

// RenameBranchForTask only renames once per iteration (idempotent guard)
func TestRenameBranchForTask_OnlyRenamesOnce(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
		RalphDir:    ralphDir,
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	if err := mgr.RenameBranchForTask("First task", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	firstBranch := mgr.WorktreeBranch

	if err := mgr.RenameBranchForTask("Second task", ""); err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	if mgr.WorktreeBranch != firstBranch {
		t.Errorf("branch changed on second call: %q → %q", firstBranch, mgr.WorktreeBranch)
	}
}

// RenameBranchForTask is a no-op when running without a worktree
func TestRenameBranchForTask_NoOpWithoutWorktree(t *testing.T) {
	mgr := &Repo{
		ProjectDir: "/some/dir",
		BaseBranch: "main",
		WorkDir:    "/some/dir",
		Logger:     &testLog{},
	}
	if err := mgr.RenameBranchForTask("anything", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mgr.BranchRenamed {
		t.Error("should not rename when WorkDir == ProjectDir")
	}
}


// Worktree path contains ralph-YYYYMMDD-01 (bats test 1)
func TestWorktreeDirUsesDateBasedName(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
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

	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
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

// Stale branches don't affect new branch naming (bats test 6)
func TestBranchNamingIgnoresStale(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	run(t, "git", "-C", project, "branch", "ralph/project/old-stale")

	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
		RalphDir:    ralphDir,
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("First task", "")

	want := "ralph/first-task"
	if mgr.WorktreeBranch != want {
		t.Errorf("branch = %q, want %q", mgr.WorktreeBranch, want)
	}
}


// Removed worktree directory is pruned and fresh setup succeeds (bats test 9)
func TestStaleWorktreeBranchCleanedUpViaPrune(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
		RalphDir:    ralphDir,
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	firstWorkDir := mgr.WorkDir

	os.RemoveAll(firstWorkDir)

	mgr2 := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
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

	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
		RalphDir:    ralphDir,
				State:       state,
		Logger:      &testLog{},
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	firstWorkDir := mgr.WorkDir

	mgr2 := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
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

// .gitignore is committed to main so the worktree inherits it (bats test 17)
func TestWorktreeInheritsGitignore(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	preSetupMgr := &Repo{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}
	preSetupMgr.EnsureGitignored(".ralph")
	run(t, "git", "-C", project, "push", "origin", "main", "-q")

	state := newMemState()
	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
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

	mgr := &Repo{ProjectDir: project, WorkDir: project, Logger: &testLog{}, BaseBranch: "main"}
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

	dirtyMgr := &Repo{ProjectDir: project, WorkDir: project, Logger: &testLog{}, BaseBranch: "main"}
	if !dirtyMgr.HasUncommittedChanges() {
		t.Error("should detect uncommitted changes")
	}
}

// RenameBranchForTask renames the branch and sets BranchRenamed.
func TestRenameBranchForTask_RenamesAndSetsFlag(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := &Repo{
		ProjectDir:  project,
		BaseBranch: "main",
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

	mgr := &Repo{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, Logger: log, BaseBranch: "main"}
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

	mgr := &Repo{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, Logger: log, BaseBranch: "main"}
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

	mgr := &Repo{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, Logger: log, BaseBranch: "main"}
	mgr.PruneOrphanedWorktrees()

	if _, err := os.Stat(filePath); err != nil {
		t.Errorf("regular file should not be removed: %v", err)
	}
}

// RepoRoot called on the project root returns the project dir itself.
func TestRepoRoot_ReturnsProjectRoot(t *testing.T) {
	project, _ := initBareRepo(t)
	want, _ := filepath.EvalSymlinks(project)

	got, err := RepoRoot(project)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("RepoRoot = %q, want %q", got, want)
	}
}

// RepoRoot called on a subdirectory resolves to the repo root, not the subdir.
func TestRepoRoot_SubdirResolvesToRoot(t *testing.T) {
	project, _ := initBareRepo(t)
	want, _ := filepath.EvalSymlinks(project)
	subdir := filepath.Join(project, "nested", "deep")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := RepoRoot(subdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("RepoRoot = %q, want %q", got, want)
	}
}

// RepoRoot returns an error when called outside any git repository.
func TestRepoRoot_NonGitDirErrors(t *testing.T) {
	tmp := t.TempDir()

	_, err := RepoRoot(tmp)
	if err == nil {
		t.Fatal("expected error for non-git dir, got nil")
	}
}

