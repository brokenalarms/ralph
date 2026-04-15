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

// testLog captures log output for assertion. Handles AppendStart/Continue/End
// to accumulate in-place lines into a single messages entry.
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
	if o.Append {
		l.lineBuffer += l.format(o, format, args...)
	} else {
		if l.lineBuffer != "" {
			l.messages = append(l.messages, l.lineBuffer)
			l.lineBuffer = ""
		}
		msg := l.format(o, format, args...)
		if msg != "\n" && msg != "" {
			l.messages = append(l.messages, msg)
		}
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
	mgr := newRepoForTest(Config{ProjectDir: project, WorkDir: project, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}))

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
	mgr := newRepoForTest(Config{ProjectDir: project, WorkDir: project, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}))

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

// BranchHasUnmergedWork returns true when the branch has commits not on main,
// including diverged branches where main also has commits not on the branch.
func TestBranchHasUnmergedWork(t *testing.T) {
	project, _ := initBareRepo(t)
	mgr := newRepoForTest(Config{ProjectDir: project, WorkDir: project, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}))

	// Branch cleanly ahead of main — has unmerged work.
	run(t, "git", "-C", project, "checkout", "-b", "ahead-branch")
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "ahead work")
	run(t, "git", "-C", project, "push", "-u", "origin", "ahead-branch")
	run(t, "git", "-C", project, "checkout", "main")

	if !mgr.BranchHasUnmergedWork("ahead-branch") {
		t.Error("ahead-branch is cleanly ahead of main, should return true")
	}

	// Merge ahead-branch, advance main — ahead-branch is now behind, no unmerged work.
	run(t, "git", "-C", project, "merge", "ahead-branch")
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "main moves ahead")
	run(t, "git", "-C", project, "push", "origin", "main")

	if mgr.BranchHasUnmergedWork("ahead-branch") {
		t.Error("ahead-branch is landed (behind main), should return false")
	}

	// Diverged branch: branch and main both have commits the other doesn't.
	// BranchIsAheadOfMain returns false for this case, but BranchHasUnmergedWork must return true.
	run(t, "git", "-C", project, "checkout", "-b", "diverged-branch")
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "diverged work")
	run(t, "git", "-C", project, "push", "-u", "origin", "diverged-branch")
	run(t, "git", "-C", project, "checkout", "main")
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "main diverges")
	run(t, "git", "-C", project, "push", "origin", "main")

	if mgr.BranchIsAheadOfMain("diverged-branch") {
		t.Error("precondition: diverged-branch should NOT be ahead of main (BranchIsAheadOfMain must return false)")
	}
	if !mgr.BranchHasUnmergedWork("diverged-branch") {
		t.Error("diverged-branch has commits ahead of main, BranchHasUnmergedWork must return true")
	}

	if mgr.BranchHasUnmergedWork("no-such-branch") {
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

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log}, nil, withRunner(&execRunner{}), withState(state))

	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree failed: %v", err)
	}

	// Worktree directory must exist
	if _, err := os.Stat(mgr.workDir); err != nil {
		t.Fatalf("WorkDir does not exist: %v", err)
	}

	// State must be recorded
	if got, _ := state.Read("worktree_dir"); got != mgr.workDir {
		t.Errorf("state worktree_dir = %q, want %q", got, mgr.workDir)
	}
	if got, _ := state.Read("worktree_branch"); got != mgr.worktreeBranch {
		t.Errorf("state worktree_branch = %q, want %q", got, mgr.worktreeBranch)
	}

	// Branch name must be exactly ralph/next (placeholder between tasks)
	wantBranch := "ralph/next"
	if mgr.worktreeBranch != wantBranch {
		t.Errorf("branch = %q, want %q", mgr.worktreeBranch, wantBranch)
	}
}

// SetupWorktree in a non-git directory should return an error
func TestSetupWorktree_NonGitDirErrors(t *testing.T) {
	tmp := t.TempDir()
	mgr := newRepoForTest(Config{ProjectDir: tmp, RalphDir: filepath.Join(tmp, ".ralph"), BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}))

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
	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	firstWorkDir := mgr.workDir
	firstBranch := mgr.worktreeBranch

	// Second run: resume
	mgr2 := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log, Resume: true}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("resume SetupWorktree: %v", err)
	}

	if mgr2.workDir != firstWorkDir {
		t.Errorf("resumed WorkDir = %q, want %q", mgr2.workDir, firstWorkDir)
	}
	if mgr2.worktreeBranch != firstBranch {
		t.Errorf("resumed branch = %q, want %q", mgr2.worktreeBranch, firstBranch)
	}
}

// Resume log must not leak the old branch name to avoid confusion with the
// current task — the branch gets rebased and renamed shortly after resume.
func TestSetupWorktree_ResumeLogSuppressesBranchName(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	log := &testLog{}

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("old task name", "")
	oldBranch := mgr.worktreeBranch

	log.messages = nil

	mgr2 := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log, Resume: true}, nil, withRunner(&execRunner{}), withState(state))
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

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("first task", "ralph-aaa")
	mgr.PrepareForNextTask("ralph-bbb", "")
	mgr.RenameBranchForTask("second task", "ralph-bbb")

	if mgr.prevBranch != "" {
		t.Errorf("PrevBranch = %q, want empty (set by setStackHead, not rename)", mgr.prevBranch)
	}
}

// PrepareForNextTask creates a fresh wip branch so the next task doesn't
// reuse the previous task's branch name.
func TestPrepareForNextTask_CreatesFreshBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("first task", "ralph-aaa")
	firstBranch := mgr.worktreeBranch
	if firstBranch == "ralph/next" {
		t.Fatal("branch should have been renamed from ralph/next")
	}

	mgr.PrepareForNextTask("ralph-bbb", "")

	if mgr.worktreeBranch != "ralph/next" {
		t.Errorf("after PrepareForNextTask, branch = %q, want ralph/next", mgr.worktreeBranch)
	}
	if mgr.branchRenamed {
		t.Error("BranchRenamed should be false after PrepareForNextTask")
	}

	mgr.RenameBranchForTask("second task", "ralph-bbb")
	if mgr.worktreeBranch == firstBranch {
		t.Errorf("second task reused first task's branch %q", firstBranch)
	}
	if mgr.worktreeBranch == "ralph/next" {
		t.Error("second task should have been renamed from ralph/next")
	}
}

// PrepareForNextTask discards uncommitted changes so dirty files from a
// previous task don't carry over into the next task's branch.
func TestPrepareForNextTask_DiscardsUncommittedChanges(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("first task", "ralph-aaa")

	// Simulate dirty state: tracked modification and untracked file.
	trackedFile := filepath.Join(mgr.workDir, "tracked.txt")
	if err := os.WriteFile(trackedFile, []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", mgr.workDir, "add", "tracked.txt")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "add tracked")

	// Modify the tracked file without committing.
	if err := os.WriteFile(trackedFile, []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create an untracked file.
	untrackedFile := filepath.Join(mgr.workDir, "untracked.txt")
	if err := os.WriteFile(untrackedFile, []byte("untracked"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr.PrepareForNextTask("ralph-bbb", "")

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
// placeholder when the task's commits are merged into main — the common
// post-completion cleanup path. Completed task branches don't accumulate
// locally.
func TestPrepareForNextTask_DeletesOldBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("task to clean up", "ralph-del1")
	taskBranch := mgr.worktreeBranch

	// Commit and then fast-forward main to the task branch, simulating a merged PR.
	writeFile(t, mgr.workDir, "work.txt", "done\n")
	run(t, "git", "-C", mgr.workDir, "add", "work.txt")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "task work")
	run(t, "git", "-C", project, "merge", "--ff-only", taskBranch)

	mgr.PrepareForNextTask("ralph-next1", "")

	// Worktree must be on the placeholder branch.
	if mgr.worktreeBranch != WipBranchName() {
		t.Errorf("WorktreeBranch = %q, want %q", mgr.worktreeBranch, WipBranchName())
	}

	// Old task branch must no longer exist — its commits are now on main.
	branches := gitOutput(project, "branch", "--list")
	if strings.Contains(branches, taskBranch) {
		t.Errorf("old task branch %q should be deleted after PrepareForNextTask, still in: %s", taskBranch, branches)
	}
}

// PrepareForNextTask preserves a task branch when it has unmerged local
// commits. This protects in-progress work from prior sessions when the loop
// resumes and picks a different next task — force-deleting the branch would
// silently drop commits that represent open work. Regression: see
// log excerpt where ralph/tabi-6p4-ralph-command-tests-playwright was
// deleted during resume of an unrelated task.
func TestPrepareForNextTask_PreservesUnmergedTaskBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	log := &testLog{}
	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("in progress task", "ralph-wip1")
	taskBranch := mgr.worktreeBranch

	// Commit local work but do NOT merge into main — simulates an in-progress
	// task whose PR is not yet merged.
	writeFile(t, mgr.workDir, "work.txt", "pending\n")
	run(t, "git", "-C", mgr.workDir, "add", "work.txt")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "in-progress work")
	commitSHA := strings.TrimSpace(gitOutput(mgr.workDir, "rev-parse", "HEAD"))

	mgr.PrepareForNextTask("ralph-different", "")

	// Worktree must be on the placeholder branch.
	if mgr.worktreeBranch != WipBranchName() {
		t.Errorf("WorktreeBranch = %q, want %q", mgr.worktreeBranch, WipBranchName())
	}

	// Old task branch must still exist — it holds unmerged work.
	branches := gitOutput(project, "branch", "--list")
	if !strings.Contains(branches, taskBranch) {
		t.Errorf("task branch %q with unmerged work should be preserved, branches: %s", taskBranch, branches)
	}

	// The branch must still point at the original commit (not reset).
	got := strings.TrimSpace(gitOutput(project, "rev-parse", taskBranch))
	if got != commitSHA {
		t.Errorf("task branch %q tip = %q, want preserved commit %q", taskBranch, got, commitSHA)
	}

	// A warning must be logged explaining the preservation.
	foundWarning := false
	for _, msg := range log.messages {
		if strings.Contains(msg, "Preserving local branch") && strings.Contains(msg, taskBranch) {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Errorf("expected 'Preserving local branch' log for unmerged branch %q, got: %v", taskBranch, log.messages)
	}
}

// ResetToDefaultBranch preserves local commits instead of force-resetting.
// When the worktree is on a branch with unpushed work (e.g. after loop
// interruption), resume must not destroy those commits — EnsureUpToDate is
// responsible for safely rebasing or aborting.
func TestResetToDefaultBranch_PreservesLocalCommits(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	log := &testLog{}
	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("in progress task", "ralph-wip2")

	// Commit local work without merging into main.
	writeFile(t, mgr.workDir, "work.txt", "pending\n")
	run(t, "git", "-C", mgr.workDir, "add", "work.txt")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "in-progress work")
	commitSHA := strings.TrimSpace(gitOutput(mgr.workDir, "rev-parse", "HEAD"))

	mgr.ResetToDefaultBranch()

	// HEAD must still point at the local commit — reset must have been skipped.
	got := strings.TrimSpace(gitOutput(mgr.workDir, "rev-parse", "HEAD"))
	if got != commitSHA {
		t.Errorf("HEAD = %q after ResetToDefaultBranch, want preserved commit %q (local work was destroyed)", got, commitSHA)
	}

	// A preservation log must be emitted so operators see why the reset no-op'd.
	foundLog := false
	for _, msg := range log.messages {
		if strings.Contains(msg, "Preserving") && strings.Contains(msg, "deferring to rebase") {
			foundLog = true
			break
		}
	}
	if !foundLog {
		t.Errorf("expected preservation log, got: %v", log.messages)
	}
}

// PrepareForNextTask discards uncommitted changes when switching to a different
// task (last_task_id differs from nextTaskID), so stale files don't leak across tasks.
func TestPrepareForNextTask_CleansOnTaskSwitch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	_ = state.Write("last_task_id", "ralph-aaa")

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("first task", "ralph-aaa")

	trackedFile := filepath.Join(mgr.workDir, "tracked.txt")
	if err := os.WriteFile(trackedFile, []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", mgr.workDir, "add", "tracked.txt")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "add tracked")
	if err := os.WriteFile(trackedFile, []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	untrackedFile := filepath.Join(mgr.workDir, "untracked.txt")
	if err := os.WriteFile(untrackedFile, []byte("untracked"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr.PrepareForNextTask("ralph-bbb", "")

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

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("first task", "ralph-aaa")

	trackedFile := filepath.Join(mgr.workDir, "tracked.txt")
	if err := os.WriteFile(trackedFile, []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", mgr.workDir, "add", "tracked.txt")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "add tracked")
	if err := os.WriteFile(trackedFile, []byte("in-progress"), 0o644); err != nil {
		t.Fatal(err)
	}
	untrackedFile := filepath.Join(mgr.workDir, "wip.txt")
	if err := os.WriteFile(untrackedFile, []byte("work in progress"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr.PrepareForNextTask("ralph-aaa", "")

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

// BranchForTask anchors the new wip branch at origin/main when CompletedBranches
// is empty, even when HEAD has commits from a previous task. Proves that
// PrepareForNextTask uses the resolved base (origin/main) rather than HEAD,
// so previous-task commits cannot leak into the next task's branch.
func TestBranchForTask_AnchorsAtOriginMain_NoPreviousCommits(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := newRepoForTest(Config{
		ProjectDir: project,
		RalphDir:   ralphDir,
		BaseBranch: "main",
		Logger:     &testLog{},
	}, nil, withRunner(&execRunner{}), withState(newMemState()))

	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// Simulate previous-task work: rename to a task branch and make a commit.
	mgr.RenameBranchForTask("prev task", "ralph-prev")
	writeFile(t, mgr.workDir, "prev-work.txt", "previous task work\n")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "previous task commit")

	// Call BranchForTask for a new task with no completed branches.
	// setStackHead finds prevBranch="" (no candidates), so baseRef="origin/main".
	// The new wip branch must start from origin/main, not from the stale HEAD.
	if _, err := mgr.BranchForTask(context.Background(), "ralph-new", "new task", BranchTaskMeta{}); err != nil {
		t.Fatalf("BranchForTask: %v", err)
	}

	// The new branch must have 0 commits ahead of origin/main — previous-task
	// commit must not have leaked into the new wip.
	countStr := strings.TrimSpace(gitOutput(mgr.workDir, "rev-list", "--count", "origin/main..HEAD"))
	if countStr != "0" {
		t.Errorf("new wip branch has %s commit(s) ahead of origin/main — previous task's commits leaked in", countStr)
	}
}

// BranchForTask anchors the new wip branch at the tip of the stack parent
// branch when CompletedBranches contains a pushed branch still ahead of main.
// Proves setStackHead detects the parent before PrepareForNextTask creates the
// new branch — if the order were reversed, baseRef would default to origin/main.
func TestBranchForTask_AnchorsAtStackParent(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	// Create and push the stack-parent branch (task A) with one commit.
	stackBranch := "ralph/ralph-a-task-a"
	run(t, "git", "-C", project, "checkout", "-b", stackBranch)
	writeFile(t, project, "task-a.txt", "task A work\n")
	run(t, "git", "-C", project, "commit", "-m", "task A commit")
	run(t, "git", "-C", project, "push", "origin", stackBranch)
	parentTip := strings.TrimSpace(gitOutput(project, "rev-parse", stackBranch))
	run(t, "git", "-C", project, "checkout", "main")

	mgr := newRepoForTest(Config{
		ProjectDir: project,
		RalphDir:   ralphDir,
		BaseBranch: "main",
		Logger:     &testLog{},
	}, nil, withRunner(&execRunner{}), withState(newMemState()))

	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	// BranchForTask with CompletedBranches containing the stack parent.
	// setStackHead detects stackBranch as head; baseRef="origin/stackBranch".
	if _, err := mgr.BranchForTask(context.Background(), "ralph-b", "task b", BranchTaskMeta{
		CompletedBranches: []string{stackBranch},
	}); err != nil {
		t.Fatalf("BranchForTask: %v", err)
	}

	// The new wip branch must be exactly at the stack parent's tip.
	tip := strings.TrimSpace(gitOutput(mgr.workDir, "rev-parse", "HEAD"))
	if tip != parentTip {
		t.Errorf("new wip branch tip = %q, want %q (tip of %s)", tip, parentTip, stackBranch)
	}

	// Exactly the stack parent's commit must be ahead of origin/main.
	count := strings.TrimSpace(gitOutput(mgr.workDir, "rev-list", "--count", "origin/main..HEAD"))
	if count != "1" {
		t.Errorf("new wip branch has %s commit(s) ahead of origin/main, want 1 (stack parent's)", count)
	}
}

// SetPrevBranch explicitly sets PrevBranch and persists to state.
func TestSetPrevBranch(t *testing.T) {
	state := newMemState()
	mgr := newRepoForTest(Config{Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	mgr.SetPrevBranch("ralph/ralph-abc-task")
	if mgr.prevBranch != "ralph/ralph-abc-task" {
		t.Errorf("PrevBranch = %q, want ralph/ralph-abc-task", mgr.prevBranch)
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

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	mgr.RenameBranchForTask("in progress work", "")
	writeFile(t, mgr.workDir, "wip.txt", "work in progress\n")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "wip commit")
	branchBefore := mgr.worktreeBranch

	// Resume without squash-merging — branch should be preserved
	mgr2 := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log, Resume: true}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("resume SetupWorktree: %v", err)
	}

	if mgr2.worktreeBranch != branchBefore {
		t.Errorf("branch = %q, want %q (valid branch should be preserved)", mgr2.worktreeBranch, branchBefore)
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

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	if err := mgr.RenameBranchForTask("Fix auth bug", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantBranch := "ralph/fix-auth-bug"
	if mgr.worktreeBranch != wantBranch {
		t.Errorf("branch = %q, want %q", mgr.worktreeBranch, wantBranch)
	}
	if !mgr.branchRenamed {
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

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	if err := mgr.RenameBranchForTask("Fix auth bug", "ralph-abc1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantBranch := "ralph/ralph-abc1-fix-auth-bug"
	if mgr.worktreeBranch != wantBranch {
		t.Errorf("branch = %q, want %q", mgr.worktreeBranch, wantBranch)
	}
}

// RenameBranchForTask only renames once per iteration (idempotent guard)
func TestRenameBranchForTask_OnlyRenamesOnce(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	if err := mgr.RenameBranchForTask("First task", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	firstBranch := mgr.worktreeBranch

	if err := mgr.RenameBranchForTask("Second task", ""); err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	if mgr.worktreeBranch != firstBranch {
		t.Errorf("branch changed on second call: %q → %q", firstBranch, mgr.worktreeBranch)
	}
}

// RenameBranchForTask is a no-op when running without a worktree
func TestRenameBranchForTask_NoOpWithoutWorktree(t *testing.T) {
	mgr := newRepoForTest(Config{ProjectDir: "/some/dir", WorkDir: "/some/dir", BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}))
	if err := mgr.RenameBranchForTask("anything", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mgr.branchRenamed {
		t.Error("should not rename when WorkDir == ProjectDir")
	}
}


// Worktree path contains ralph-YYYYMMDD-01 (bats test 1)
func TestWorktreeDirUsesDateBasedName(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	today := time.Now().Format("20060102")
	expected := fmt.Sprintf("ralph-%s-01", today)
	if !strings.Contains(mgr.workDir, "/worktrees/"+expected) {
		t.Errorf("WorkDir = %q, want it to contain %q", mgr.workDir, expected)
	}
}

// Existing -01 directory causes -02 suffix (bats test 2)
func TestSecondRunSameDayIncrementsSuffix(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	today := time.Now().Format("20060102")
	os.MkdirAll(filepath.Join(ralphDir, "worktrees", "ralph-"+today+"-01"), 0o755)

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	expected := fmt.Sprintf("ralph-%s-02", today)
	if !strings.Contains(mgr.workDir, "/worktrees/"+expected) {
		t.Errorf("WorkDir = %q, want it to contain %q", mgr.workDir, expected)
	}
}

// Stale branches don't affect new branch naming (bats test 6)
func TestBranchNamingIgnoresStale(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	run(t, "git", "-C", project, "branch", "ralph/project/old-stale")

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("First task", "")

	want := "ralph/first-task"
	if mgr.worktreeBranch != want {
		t.Errorf("branch = %q, want %q", mgr.worktreeBranch, want)
	}
}


// Removed worktree directory is pruned and fresh setup succeeds (bats test 9)
func TestStaleWorktreeBranchCleanedUpViaPrune(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	firstWorkDir := mgr.workDir

	os.RemoveAll(firstWorkDir)

	mgr2 := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("second SetupWorktree after prune: %v", err)
	}
	if _, err := os.Stat(mgr2.workDir); err != nil {
		t.Errorf("new worktree dir should exist: %v", err)
	}
}

// Live ralph worktree is force-removed when branch conflicts (bats test 10)
func TestLiveRalphWorktreeRemovedWhenBranchExists(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	firstWorkDir := mgr.workDir

	mgr2 := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("second SetupWorktree: %v", err)
	}

	if _, err := os.Stat(mgr2.workDir); err != nil {
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

	preSetupMgr := newRepoForTest(Config{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}))
	preSetupMgr.EnsureGitignored(".ralph")
	run(t, "git", "-C", project, "push", "origin", "main", "-q")

	state := newMemState()
	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(mgr.workDir, ".gitignore"))
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

	mgr := newRepoForTest(Config{ProjectDir: project, WorkDir: project, Logger: &testLog{}, BaseBranch: "main"}, nil, withRunner(&execRunner{}))
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

	dirtyMgr := newRepoForTest(Config{ProjectDir: project, WorkDir: project, Logger: &testLog{}, BaseBranch: "main"}, nil, withRunner(&execRunner{}))
	if !dirtyMgr.HasUncommittedChanges() {
		t.Error("should detect uncommitted changes")
	}
}

// RenameBranchForTask renames the branch and sets BranchRenamed.
func TestRenameBranchForTask_RenamesAndSetsFlag(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := newMemState()

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(st))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	origBranch := mgr.worktreeBranch
	mgr.RenameBranchForTask("First task", "")

	if mgr.worktreeBranch == origBranch {
		t.Error("should rename the branch")
	}
	if !mgr.branchRenamed {
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

	mgr := newRepoForTest(Config{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, Logger: log, BaseBranch: "main"}, nil, withRunner(&execRunner{}))
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

	mgr := newRepoForTest(Config{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, Logger: log, BaseBranch: "main"}, nil, withRunner(&execRunner{}))
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

	mgr := newRepoForTest(Config{ProjectDir: project, WorkDir: project, RalphDir: ralphDir, Logger: log, BaseBranch: "main"}, nil, withRunner(&execRunner{}))
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