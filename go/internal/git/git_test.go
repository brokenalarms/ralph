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

func TestTaskBranchName(t *testing.T) {
	cases := []struct {
		date string
		seq  int
		want string
	}{
		{"20260429", 1, "ralph/task/20260429-01"},
		{"20260429", 2, "ralph/task/20260429-02"},
		{"20260429", 12, "ralph/task/20260429-12"},
	}
	for _, tc := range cases {
		got := TaskBranchName(tc.date, tc.seq)
		if got != tc.want {
			t.Errorf("TaskBranchName(%q, %d) = %q, want %q", tc.date, tc.seq, got, tc.want)
		}
	}
}

// TaskBranchName must NOT collide with WipBranchName under any seq —
// the whole point of the fix is namespace separation between
// `ralph task` and the loop's `ralph/next` wip branch.
func TestTaskBranchName_DistinctFromWipBranch(t *testing.T) {
	wip := WipBranchName()
	for seq := 1; seq <= 99; seq++ {
		got := TaskBranchName("20260429", seq)
		if got == wip {
			t.Errorf("TaskBranchName(%d) = %q collides with WipBranchName %q", seq, got, wip)
		}
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

// GitHub deletes a merged PR's head branch on the remote (delete_branch_on_merge),
// but plain `git fetch origin <base>` never prunes the now-dangling local
// origin/<branch> tracking ref for it. SetupWorktree's routine fetch must be
// prune-aware so that ref disappears on the next fetch, while a genuinely
// live remote branch's tracking ref survives.
//
// The branch deletion is done directly on the bare "remote" repo (not via a
// push --delete from project) because deleting through the same clone that
// holds the tracking ref updates that ref as a side effect of the push
// itself — that wouldn't reproduce the real staleness GitHub's independent
// deletion leaves behind.
func TestSetupWorktree_PrunesStaleRemoteTrackingRefs(t *testing.T) {
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

	run(t, "git", "-C", project, "checkout", "-b", "ralph/merged-and-deleted")
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "merged work")
	run(t, "git", "-C", project, "push", "-u", "origin", "ralph/merged-and-deleted")
	run(t, "git", "-C", project, "checkout", "-b", "ralph/still-live")
	run(t, "git", "-C", project, "commit", "--allow-empty", "-m", "live work")
	run(t, "git", "-C", project, "push", "-u", "origin", "ralph/still-live")
	run(t, "git", "-C", project, "checkout", "main")

	if !refExistsForTest(t, project, "refs/remotes/origin/ralph/merged-and-deleted") {
		t.Fatal("setup: origin/ralph/merged-and-deleted tracking ref should exist before delete")
	}

	// GitHub auto-deletes the merged branch on the remote; the live one stays.
	run(t, "git", "-C", bare, "branch", "-D", "ralph/merged-and-deleted")

	ralphDir := filepath.Join(project, ".ralph")
	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	if refExistsForTest(t, project, "refs/remotes/origin/ralph/merged-and-deleted") {
		t.Error("origin/ralph/merged-and-deleted tracking ref should be pruned after SetupWorktree's fetch, but still exists")
	}
	if !refExistsForTest(t, project, "refs/remotes/origin/ralph/still-live") {
		t.Error("origin/ralph/still-live tracking ref should survive — it is still live on the remote")
	}
}

func refExistsForTest(t *testing.T, dir, ref string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--verify", ref)
	return cmd.Run() == nil
}

// SetupWorktree in a non-git directory should return an error
func TestSetupWorktree_NonGitDirErrors(t *testing.T) {
	tmp := t.TempDir()
	mgr := newRepoForTest(Config{ProjectDir: tmp, RalphDir: filepath.Join(tmp, ".ralph"), BaseBranch: "main", Logger: &testLog{}}, nil)

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
	project := t.TempDir()
	run(t, "git", "init", "-b", "main", project)
	worktreeDir := filepath.Join(t.TempDir(), "wt")
	os.MkdirAll(worktreeDir, 0o755)

	oldBranch := "ralph/old-task-name"
	state := newMemState()
	state.Write("worktree_dir", worktreeDir)
	state.Write("worktree_branch", oldBranch)
	state.Write("branch_renamed", "true")

	log := &testLog{}
	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: filepath.Join(project, ".ralph"), BaseBranch: "main", Logger: log, Resume: true}, nil, withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("resume SetupWorktree: %v", err)
	}

	for _, msg := range log.messages {
		if strings.Contains(msg, oldBranch) {
			t.Errorf("resume log should not contain old branch name %q, got %q", oldBranch, msg)
		}
	}
}

// SetupTaskWorktree creates a per-instance worktree on a unique
// `ralph/task/YYYYMMDD-NN` branch, never on `ralph/next`.
func TestSetupTaskWorktree_UsesUniqueBranchNotRalphNext(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))

	if err := mgr.SetupTaskWorktree(context.Background()); err != nil {
		t.Fatalf("SetupTaskWorktree: %v", err)
	}

	if mgr.worktreeBranch == "ralph/next" {
		t.Errorf("task worktree branch must NOT be ralph/next (loop's wip branch); got %q", mgr.worktreeBranch)
	}
	if !strings.HasPrefix(mgr.worktreeBranch, "ralph/task/") {
		t.Errorf("task worktree branch must start with ralph/task/; got %q", mgr.worktreeBranch)
	}
	if _, err := os.Stat(mgr.workDir); err != nil {
		t.Errorf("task worktree dir does not exist: %v", err)
	}
}

// On a fresh (non-resume) start, SetupWorktree removes the previous run's
// worktree recorded in state.json — even after RenameBranchForTask has moved
// it off the wip placeholder branch — so a killed run never leaves a
// registered worktree behind forever (PruneOrphanedWorktrees only cleans up
// unregistered directories).
func TestSetupWorktree_RemovesPreviousRunWorktreeOnFreshStart(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	log := &testLog{}

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	if err := mgr.RenameBranchForTask("some task", "task-1"); err != nil {
		t.Fatalf("RenameBranchForTask: %v", err)
	}
	oldDir := mgr.workDir
	oldBranch := mgr.worktreeBranch

	if _, err := os.Stat(oldDir); err != nil {
		t.Fatalf("setup: old worktree should exist: %v", err)
	}
	if oldBranch == "ralph/next" {
		t.Fatalf("setup: branch should have been renamed off the wip placeholder, got %q", oldBranch)
	}

	mgr2 := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr2.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("second SetupWorktree: %v", err)
	}

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("previous run's worktree dir should be removed, stat err=%v", err)
	}
	if refExistsForTest(t, project, "refs/heads/"+oldBranch) {
		t.Errorf("previous run's branch %q should be deleted", oldBranch)
	}
	out, err := exec.Command("git", "-C", project, "worktree", "list").CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	if strings.Contains(string(out), oldDir) {
		t.Errorf("git worktree list should not show removed dir %q: %s", oldDir, out)
	}

	if mgr2.workDir == oldDir {
		t.Fatalf("new worktree dir should differ from the removed one: %q", mgr2.workDir)
	}
	if _, err := os.Stat(mgr2.workDir); err != nil {
		t.Errorf("new worktree dir should exist: %v", err)
	}
}

// A worktree_dir recorded outside the worktree root must never be touched —
// it isn't a ralph-managed worktree, so removing it would delete something
// SetupWorktree doesn't own.
func TestSetupWorktree_LeavesOutOfRootWorktreeUntouched(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	outside := filepath.Join(t.TempDir(), "outside-worktree")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(outside, "marker.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	state := newMemState()
	state.Write("worktree_dir", outside)
	state.Write("worktree_branch", "ralph/some-other-branch")
	log := &testLog{}

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("out-of-root worktree_dir should be left untouched, marker file gone: %v", err)
	}
	if log.contains("Removing previous run's worktree") {
		t.Error("should not log removal for an out-of-root worktree_dir")
	}
}

// SetupTaskWorktree must NOT write worktree_dir / worktree_branch to
// state.json — task worktrees are ephemeral and must not contaminate
// the loop's resume state.
func TestSetupTaskWorktree_DoesNotWriteState(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	// Pre-populate with sentinel loop state so we can detect contamination.
	state.Write("worktree_dir", "/loop/preserved/path")
	state.Write("worktree_branch", "ralph/next")

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))

	if err := mgr.SetupTaskWorktree(context.Background()); err != nil {
		t.Fatalf("SetupTaskWorktree: %v", err)
	}

	if got, _ := state.Read("worktree_dir"); got != "/loop/preserved/path" {
		t.Errorf("state worktree_dir was overwritten by task setup: got %q, want /loop/preserved/path", got)
	}
	if got, _ := state.Read("worktree_branch"); got != "ralph/next" {
		t.Errorf("state worktree_branch was overwritten by task setup: got %q, want ralph/next", got)
	}
}

// Two SetupTaskWorktree calls back-to-back must produce two distinct
// worktrees on distinct branches, with both still on disk after the
// second call. This is the regression test for the data-loss bug
// where a second `ralph task` was force-removing the first.
func TestSetupTaskWorktree_DoesNotDestroyConcurrentWorktree(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr1 := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr1.SetupTaskWorktree(context.Background()); err != nil {
		t.Fatalf("first SetupTaskWorktree: %v", err)
	}
	firstDir := mgr1.workDir
	firstBranch := mgr1.worktreeBranch

	// Drop a sentinel file in the first worktree to detect destruction.
	sentinel := filepath.Join(firstDir, "uncommitted-edit.txt")
	if err := os.WriteFile(sentinel, []byte("important work"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	mgr2 := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr2.SetupTaskWorktree(context.Background()); err != nil {
		t.Fatalf("second SetupTaskWorktree: %v", err)
	}
	secondDir := mgr2.workDir
	secondBranch := mgr2.worktreeBranch

	if firstDir == secondDir {
		t.Errorf("two task worktrees got the same dir: %q", firstDir)
	}
	if firstBranch == secondBranch {
		t.Errorf("two task worktrees got the same branch: %q", firstBranch)
	}
	if _, err := os.Stat(firstDir); err != nil {
		t.Errorf("first worktree was destroyed by second SetupTaskWorktree: %v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil {
		t.Errorf("sentinel file was deleted (uncommitted work would be lost): %v", err)
	} else if string(data) != "important work" {
		t.Errorf("sentinel file was modified: got %q", string(data))
	}
	if _, err := os.Stat(secondDir); err != nil {
		t.Errorf("second worktree does not exist: %v", err)
	}
}

// SetupTaskWorktree must not touch a pre-existing `ralph/next` worktree
// (the loop's). Set one up via SetupWorktree, then run a task setup,
// then verify the loop's worktree and branch are unmodified.
func TestSetupTaskWorktree_LeavesLoopWorktreeAlone(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	loopMgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := loopMgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("loop SetupWorktree: %v", err)
	}
	loopDir := loopMgr.workDir
	loopBranch := loopMgr.worktreeBranch
	if loopBranch != "ralph/next" {
		t.Fatalf("expected loop branch ralph/next, got %q", loopBranch)
	}

	// Drop a sentinel into the loop worktree.
	sentinel := filepath.Join(loopDir, "loop-uncommitted.txt")
	if err := os.WriteFile(sentinel, []byte("loop work"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	taskMgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := taskMgr.SetupTaskWorktree(context.Background()); err != nil {
		t.Fatalf("task SetupTaskWorktree: %v", err)
	}

	if _, err := os.Stat(loopDir); err != nil {
		t.Errorf("loop worktree dir was destroyed by task setup: %v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil {
		t.Errorf("loop sentinel deleted: %v", err)
	} else if string(data) != "loop work" {
		t.Errorf("loop sentinel modified: got %q", string(data))
	}

	// ralph/next branch must still exist and still be checked out by the loop's worktree.
	if !loopMgr.refExists(project, "ralph/next") {
		t.Errorf("ralph/next branch was deleted by task setup")
	}
	gotWT := loopMgr.findWorktreeForBranch(project, "ralph/next")
	gotResolved, _ := filepath.EvalSymlinks(gotWT)
	wantResolved, _ := filepath.EvalSymlinks(loopDir)
	if gotResolved != wantResolved {
		t.Errorf("ralph/next no longer attached to loop worktree: got %q, want %q", gotWT, loopDir)
	}
}

// On a non-git directory SetupTaskWorktree must error AND must NOT set
// r.workDir to anything that would resolve to the project root. The
// previous "fall back to project dir" behavior was the recurring source
// of worktree contents leaking into main; callers now must treat any
// error from SetupTaskWorktree as fatal.
func TestSetupTaskWorktree_ErrorDoesNotSetWorkDirToProject(t *testing.T) {
	tmp := t.TempDir()
	mgr := newRepoForTest(Config{ProjectDir: tmp, RalphDir: filepath.Join(tmp, ".ralph"), BaseBranch: "main", Logger: &testLog{}}, nil)

	err := mgr.SetupTaskWorktree(context.Background())
	if err == nil {
		t.Fatal("expected error for non-git dir")
	}
	if mgr.workDir == tmp {
		t.Errorf("workDir on error must not equal projectDir (%q) — that was the bug", tmp)
	}
}


// SetupTaskWorktree must skip seq N when a ralph/task/YYYYMMDD-N branch exists
// but the matching directory does not (e.g. the directory was cleaned but the
// branch was preserved for claude --resume). The new invocation must land on
// the next free slot without "branch already exists" errors.
func TestSetupTaskWorktree_SkipsSeqWithExistingBranchNoDir(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	worktreeRoot := filepath.Join(ralphDir, "worktrees")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("mkdir worktreeRoot: %v", err)
	}

	today := time.Now().Format("20060102")
	// Pre-create the seq-01 branch with no worktree dir to simulate a quit
	// session whose dir was cleaned but whose branch survives.
	seq01Branch := TaskBranchName(today, 1)
	run(t, "git", "-C", project, "branch", seq01Branch, "main")

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupTaskWorktree(context.Background()); err != nil {
		t.Fatalf("SetupTaskWorktree: %v", err)
	}

	// Must have chosen seq 02, not seq 01.
	if mgr.worktreeBranch == seq01Branch {
		t.Errorf("new invocation reused the existing branch %q instead of skipping to a free slot", seq01Branch)
	}
	seq02Branch := TaskBranchName(today, 2)
	if mgr.worktreeBranch != seq02Branch {
		t.Errorf("expected branch %q, got %q", seq02Branch, mgr.worktreeBranch)
	}
	if _, err := os.Stat(mgr.workDir); err != nil {
		t.Errorf("new worktree dir does not exist: %v", err)
	}
}

// SetupTaskWorktree must never RemoveAll a preserved worktree directory even
// when a sequence-number gap causes the old dir-count heuristic to land on an
// occupied slot. Scenario: seq-01 has no dir (gap), seq-02 has both a branch
// and a dir (preserved quit session). A new invocation counting only one dir
// would compute runSeq=2 and RemoveAll seq-02's dir with the old code. The fix
// must detect the seq-02 branch, skip the slot, and use the first fully-free
// slot instead.
func TestSetupTaskWorktree_DoesNotRemoveAllPreservedDirOnSeqGap(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	worktreeRoot := filepath.Join(ralphDir, "worktrees")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("mkdir worktreeRoot: %v", err)
	}

	today := time.Now().Format("20060102")

	// Simulate a preserved quit session at seq-02: branch exists, dir exists,
	// but no git worktree registration (the registration was pruned when the
	// user quit). seq-01 has neither dir nor branch — it's the gap.
	seq02Branch := TaskBranchName(today, 2)
	seq02Dir := filepath.Join(worktreeRoot, fmt.Sprintf("ralph-task-%s-02", today))
	run(t, "git", "-C", project, "branch", seq02Branch, "main")
	if err := os.MkdirAll(seq02Dir, 0o755); err != nil {
		t.Fatalf("mkdir seq02Dir: %v", err)
	}
	sentinel := filepath.Join(seq02Dir, "preserved-work.txt")
	if err := os.WriteFile(sentinel, []byte("must survive"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	// With the buggy code: dir count=1 (seq02Dir) → runSeq=2 → RemoveAll(seq02Dir)!
	// With the fix: seq-01 is fully free → use seq-01, never touch seq-02.
	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupTaskWorktree(context.Background()); err != nil {
		t.Fatalf("SetupTaskWorktree: %v", err)
	}

	// New invocation must not have used seq-02 (the preserved slot).
	if mgr.worktreeBranch == seq02Branch {
		t.Errorf("new invocation reused/clobbered preserved branch %q", seq02Branch)
	}
	if mgr.workDir == seq02Dir {
		t.Errorf("new invocation reused/clobbered preserved dir %q", seq02Dir)
	}

	// Preserved dir, sentinel, and branch must all still be intact.
	if _, err := os.Stat(seq02Dir); err != nil {
		t.Errorf("preserved worktree dir was deleted: %v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil {
		t.Errorf("sentinel deleted (preserved work would be lost): %v", err)
	} else if string(data) != "must survive" {
		t.Errorf("sentinel modified: got %q", string(data))
	}
	r := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil)
	if !r.refExists(project, seq02Branch) {
		t.Errorf("preserved branch %q was deleted", seq02Branch)
	}
}

// SetupTaskWorktree must never RemoveAll a slot dir that is a registered
// worktree by PATH, even when its branch was renamed away and the original
// slot branch no longer exists. Regression test for the incident where a
// renamed session's worktree was judged a dead leftover (branch absent,
// findWorktreeForBranch(candidateBranch)=="" because registration moved to
// the new branch name) and os.RemoveAll'd — a live, registered worktree.
func TestSetupTaskWorktree_SkipsPathRegisteredDirAfterBranchRename(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	worktreeRoot := filepath.Join(ralphDir, "worktrees")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("mkdir worktreeRoot: %v", err)
	}

	today := time.Now().Format("20060102")
	seq01Branch := TaskBranchName(today, 1)
	seq01Dir := filepath.Join(worktreeRoot, fmt.Sprintf("ralph-task-%s-01", today))

	// Register a live worktree at the slot-01 dir on the slot-01 branch, then
	// rename the branch away (git branch -m renames+deletes the old ref in
	// one step, matching save-worktree's end state: the worktree stays
	// registered at seq01Dir, but now under a different branch name — the
	// slot-01 branch no longer exists at all).
	run(t, "git", "-C", project, "worktree", "add", "-b", seq01Branch, seq01Dir, "main")
	renamedBranch := "ralph/save/renamed-topic"
	run(t, "git", "-C", seq01Dir, "branch", "-m", seq01Branch, renamedBranch)

	sentinel := filepath.Join(seq01Dir, "preserved-work.txt")
	if err := os.WriteFile(sentinel, []byte("must survive"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupTaskWorktree(context.Background()); err != nil {
		t.Fatalf("SetupTaskWorktree: %v", err)
	}

	// The path-registered dir must be untouched.
	if _, err := os.Stat(seq01Dir); err != nil {
		t.Errorf("path-registered worktree dir was deleted: %v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil {
		t.Errorf("sentinel deleted (renamed session's work would be lost): %v", err)
	} else if string(data) != "must survive" {
		t.Errorf("sentinel modified: got %q", string(data))
	}
	if !mgr.refExists(project, renamedBranch) {
		t.Errorf("renamed branch %q was deleted", renamedBranch)
	}

	// The new invocation must have claimed the next free slot, not seq-01.
	seq02Branch := TaskBranchName(today, 2)
	if mgr.worktreeBranch != seq02Branch {
		t.Errorf("expected next free slot %q, got %q", seq02Branch, mgr.worktreeBranch)
	}
	if mgr.workDir == seq01Dir {
		t.Errorf("new invocation reused the path-registered dir %q", seq01Dir)
	}

	// No leftover ralph/task branch should have been created at the
	// deleted slot-01 branch name.
	if mgr.refExists(project, seq01Branch) {
		t.Errorf("leftover branch %q was recreated", seq01Branch)
	}
}

// RenameBranchForTask does not set PrevBranch — that's controlled by
// setStackHead in the loop via SetPrevBranch. Rename only changes the
// branch name.
func TestRenameBranchForTask_DoesNotSetPrevBranch(t *testing.T) {
	dir := t.TempDir()
	runner := newStubRunner()
	runner.On("branch -m", "", nil)
	runner.On("checkout", "", nil)
	runner.On("clean", "", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", fmt.Errorf("not found"))
	runner.On("merge-base --is-ancestor", "", fmt.Errorf("not ancestor"))

	mgr := newRepoForTest(Config{ProjectDir: dir, WorkDir: filepath.Join(dir, "wt"), BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner), withWorktreeBranch("ralph/next"))

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
	dir := t.TempDir()
	runner := newStubRunner()
	runner.On("branch -m", "", nil)
	runner.On("checkout", "", nil)
	runner.On("clean", "", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", fmt.Errorf("not found"))
	runner.On("merge-base --is-ancestor", "", fmt.Errorf("not ancestor"))

	mgr := newRepoForTest(Config{ProjectDir: dir, WorkDir: filepath.Join(dir, "wt"), BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner), withWorktreeBranch("ralph/next"))

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
	dir := t.TempDir()
	runner := newStubRunner()
	runner.On("branch -m", "", nil)
	runner.On("checkout", "", nil)
	runner.On("clean", "", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "abc123", nil)
	runner.On("merge-base --is-ancestor", "", nil)
	runner.On("branch -D", "", nil)

	log := &testLog{}
	mgr := newRepoForTest(Config{ProjectDir: dir, WorkDir: filepath.Join(dir, "wt"), BaseBranch: "main", Logger: log}, nil, withRunner(runner), withWorktreeBranch("ralph/next"))

	mgr.RenameBranchForTask("task to clean up", "ralph-del1")
	taskBranch := mgr.worktreeBranch

	mgr.PrepareForNextTask("ralph-next1", "")

	if mgr.worktreeBranch != WipBranchName() {
		t.Errorf("WorktreeBranch = %q, want %q", mgr.worktreeBranch, WipBranchName())
	}

	if !runner.CalledWith("branch", "-D", taskBranch) {
		t.Errorf("expected branch -D %s to be called", taskBranch)
	}

	if log.contains("Deleted local branch "+taskBranch) {
		t.Errorf("expected bare 'Deleted local branch' emit to be replaced with a previous-session framing, got messages: %v", log.messages)
	}
	wantMsg := fmt.Sprintf("Cleaned up previous-session branch %s — fully merged into %s", taskBranch, mgr.baseBranch)
	if !log.contains(wantMsg) {
		t.Errorf("expected log to contain %q, got messages: %v", wantMsg, log.messages)
	}
}

// PrepareForNextTask preserves a task branch when it has unmerged local
// commits. This protects in-progress work from prior sessions when the loop
// resumes and picks a different next task — force-deleting the branch would
// silently drop commits that represent open work. Regression: see
// log excerpt where ralph/tabi-6p4-ralph-command-tests-playwright was
// deleted during resume of an unrelated task.
func TestPrepareForNextTask_PreservesUnmergedTaskBranch(t *testing.T) {
	dir := t.TempDir()
	runner := newStubRunner()
	runner.On("branch -m", "", nil)
	runner.On("checkout", "", nil)
	runner.On("clean", "", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "abc123", nil)
	runner.On("merge-base --is-ancestor", "", fmt.Errorf("not ancestor"))

	log := &testLog{}
	mgr := newRepoForTest(Config{ProjectDir: dir, WorkDir: filepath.Join(dir, "wt"), BaseBranch: "main", Logger: log}, nil, withRunner(runner), withWorktreeBranch("ralph/next"))

	mgr.RenameBranchForTask("in progress task", "ralph-wip1")
	taskBranch := mgr.worktreeBranch

	mgr.PrepareForNextTask("ralph-different", "")

	if mgr.worktreeBranch != WipBranchName() {
		t.Errorf("WorktreeBranch = %q, want %q", mgr.worktreeBranch, WipBranchName())
	}

	if runner.CalledWith("branch", "-D", taskBranch) {
		t.Errorf("branch -D %s should NOT have been called for unmerged branch", taskBranch)
	}

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

// PrepareForNextTask deletes a squash-merged task branch even though its
// commits never became ancestors of the base branch — the case Ralph hits
// on every completed task, since it squash-merges every PR. Real git
// throughout: the branch is squash-merged into main exactly as a merged PR
// lands, so the deletion has to follow from git's own view of the content.
func TestPrepareForNextTask_DeletesSquashMergedBranch(t *testing.T) {
	project, _ := initBareRepo(t)

	taskBranch := "ralph/ralph-sq1-squash-merged-task"
	run(t, "git", "-C", project, "checkout", "-b", taskBranch)
	writeAndCommit(t, project, "feature.txt", "part one\n", "feature part 1")
	writeAndCommit(t, project, "feature.txt", "part one\npart two\n", "feature part 2")

	run(t, "git", "-C", project, "checkout", "main")
	run(t, "git", "-C", project, "merge", "--squash", taskBranch)
	run(t, "git", "-C", project, "commit", "-m", "squashed feature (#1)")
	run(t, "git", "-C", project, "push", "origin", "main")
	run(t, "git", "-C", project, "checkout", taskBranch)

	if isAncestorForTest(t, project, taskBranch, "origin/main") {
		t.Fatal("setup is wrong: a squash-merged branch must not be an ancestor of origin/main")
	}

	log := &testLog{}
	mgr := newRepoForTest(Config{ProjectDir: project, WorkDir: project, BaseBranch: "main", Logger: log}, nil, withRunner(&execRunner{}), withWorktreeBranch(taskBranch))

	mgr.PrepareForNextTask("ralph-next2", "")

	if mgr.worktreeBranch != WipBranchName() {
		t.Errorf("WorktreeBranch = %q, want %q", mgr.worktreeBranch, WipBranchName())
	}

	if refExistsForTest(t, project, "refs/heads/"+taskBranch) {
		t.Errorf("squash-merged branch %s should have been deleted", taskBranch)
	}

	wantMsg := fmt.Sprintf("Cleaned up previous-session branch %s — fully merged into %s", taskBranch, mgr.baseBranch)
	if !log.contains(wantMsg) {
		t.Errorf("expected log to contain %q, got messages: %v", wantMsg, log.messages)
	}
}

// branchSafeToDelete decides deletion from git's own view of the branch:
// ancestry when history proves the merge, and a merge-tree content comparison
// when history cannot (squash merge). Anything main does not already
// contain — an extra commit, or a divergent edit that conflicts — must be
// preserved. Real git, so each case is the shape it claims to be.
func TestBranchSafeToDelete_RealGit(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, project, branch string)
		want  bool
	}{
		{
			name: "merged by fast-forward, branch is an ancestor of main",
			setup: func(t *testing.T, project, branch string) {
				run(t, "git", "-C", project, "checkout", "-b", branch)
				writeAndCommit(t, project, "feature.txt", "landed work\n", "feature work")
				run(t, "git", "-C", project, "checkout", "main")
				run(t, "git", "-C", project, "merge", branch)
				run(t, "git", "-C", project, "push", "origin", "main")
			},
			want: true,
		},
		{
			name: "squash-merged, content landed but ancestry broken",
			setup: func(t *testing.T, project, branch string) {
				run(t, "git", "-C", project, "checkout", "-b", branch)
				writeAndCommit(t, project, "feature.txt", "part one\n", "feature part 1")
				writeAndCommit(t, project, "feature.txt", "part one\npart two\n", "feature part 2")
				run(t, "git", "-C", project, "checkout", "main")
				run(t, "git", "-C", project, "merge", "--squash", branch)
				run(t, "git", "-C", project, "commit", "-m", "squashed feature (#1)")
				run(t, "git", "-C", project, "push", "origin", "main")
			},
			want: true,
		},
		{
			name: "never merged, content absent from main",
			setup: func(t *testing.T, project, branch string) {
				run(t, "git", "-C", project, "checkout", "-b", branch)
				writeAndCommit(t, project, "feature.txt", "unlanded work\n", "feature work")
				run(t, "git", "-C", project, "checkout", "main")
			},
			want: false,
		},
		{
			name: "squash-merged then extended with work that never landed",
			setup: func(t *testing.T, project, branch string) {
				run(t, "git", "-C", project, "checkout", "-b", branch)
				writeAndCommit(t, project, "feature.txt", "shipped\n", "feature work")
				run(t, "git", "-C", project, "checkout", "main")
				run(t, "git", "-C", project, "merge", "--squash", branch)
				run(t, "git", "-C", project, "commit", "-m", "squashed feature (#1)")
				run(t, "git", "-C", project, "push", "origin", "main")
				run(t, "git", "-C", project, "checkout", branch)
				writeAndCommit(t, project, "extra.txt", "written after the PR merged\n", "follow-up work")
				run(t, "git", "-C", project, "checkout", "main")
			},
			want: false,
		},
		{
			name: "diverged from main with a conflicting edit",
			setup: func(t *testing.T, project, branch string) {
				writeAndCommit(t, project, "shared.txt", "base\n", "shared base")
				run(t, "git", "-C", project, "push", "origin", "main")
				run(t, "git", "-C", project, "checkout", "-b", branch)
				writeAndCommit(t, project, "shared.txt", "branch version\n", "branch edit")
				run(t, "git", "-C", project, "checkout", "main")
				writeAndCommit(t, project, "shared.txt", "main version\n", "main edit")
				run(t, "git", "-C", project, "push", "origin", "main")
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project, _ := initBareRepo(t)
			branch := "ralph/ralph-abc-task"
			tt.setup(t, project, branch)

			mgr := newRepoForTest(Config{ProjectDir: project, WorkDir: project, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}))

			if got := mgr.branchSafeToDelete(branch); got != tt.want {
				t.Errorf("branchSafeToDelete(%s) = %v, want %v", branch, got, tt.want)
			}
		})
	}
}

// Ancestry is the cheap check and answers first: when it proves the merge,
// the merge-tree fallback never runs.
func TestBranchSafeToDelete_AncestrySkipsMergeTree(t *testing.T) {
	runner := newStubRunner()
	runner.On("rev-parse --verify", "abc123", nil)
	runner.On("merge-base --is-ancestor", "", nil)

	mgr := newRepoForTest(Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner))

	if !mgr.branchSafeToDelete("ralph/task") {
		t.Error("branch that is an ancestor of main should be safe to delete")
	}
	if runner.CalledWith("merge-tree") {
		t.Error("merge-tree should not run when ancestry already proves the merge")
	}
}

// An inconclusive merge-tree — a git old enough to lack --write-tree, or any
// other failure — preserves the branch rather than guessing.
func TestBranchSafeToDelete_MergeTreeUnsupportedPreserves(t *testing.T) {
	runner := newStubRunner()
	runner.On("rev-parse --verify", "abc123", nil)
	runner.On("merge-base --is-ancestor", "", fmt.Errorf("not an ancestor"))
	runner.On("merge-tree", "", fmt.Errorf("error: unknown option `write-tree'"))

	mgr := newRepoForTest(Config{ProjectDir: t.TempDir(), BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner))

	if mgr.branchSafeToDelete("ralph/task") {
		t.Error("branch should be preserved when merge-tree cannot answer")
	}
}

func writeAndCommit(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	run(t, "git", "-C", dir, "add", name)
	run(t, "git", "-C", dir, "commit", "-m", msg)
}

func isAncestorForTest(t *testing.T, dir, ancestor, descendant string) bool {
	t.Helper()
	return exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", ancestor, descendant).Run() == nil
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

// SyncWorktreeBase force-resets the worktree to origin/<default> when the
// stack is drained (prevBranch==""). Local commits from the prior stack are
// ghosts — they cannot rebase cleanly onto an advanced main and must be
// discarded rather than preserved. This is the regression from ralph-pf1n.
func TestSyncWorktreeBase_StackDrain_DiscardsLocalCommits(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	log := &testLog{}
	ralphDir := filepath.Join(project, ".ralph")
	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	run(t, "git", "-C", mgr.workDir, "remote", "set-url", "origin", bare)
	run(t, "git", "-C", mgr.workDir, "fetch", "origin")
	run(t, "git", "-C", mgr.workDir, "config", "user.name", "test")
	run(t, "git", "-C", mgr.workDir, "config", "user.email", "test@test")

	mgr.RenameBranchForTask("ghost task", "ralph-ghost")

	// Commit 3 ghost commits that must be discarded.
	for i := 0; i < 3; i++ {
		writeFile(t, mgr.workDir, fmt.Sprintf("ghost%d.txt", i), fmt.Sprintf("ghost %d\n", i))
		run(t, "git", "-C", mgr.workDir, "commit", "-m", fmt.Sprintf("ghost %d", i))
	}

	originHead := strings.TrimSpace(gitOutput(mgr.workDir, "rev-parse", "origin/main"))

	// Non-empty completedBranches with default stub gh (no open PRs) triggers
	// setStackHead's "no stacked parents — top has no open PR" drain path.
	if err := mgr.SyncWorktreeBase(context.Background(), []string{"ralph/prior-drained"}); err != nil {
		t.Fatalf("SyncWorktreeBase: %v", err)
	}

	got := strings.TrimSpace(gitOutput(mgr.workDir, "rev-parse", "HEAD"))
	if got != originHead {
		t.Errorf("HEAD = %q after SyncWorktreeBase, want origin/main = %q (ghost commits not discarded)", got, originHead)
	}
	ahead := strings.TrimSpace(gitOutput(mgr.workDir, "rev-list", "--count", "origin/main..HEAD"))
	if ahead != "0" {
		t.Errorf("HEAD still %s commits ahead of origin/main, want 0", ahead)
	}
	for _, msg := range log.messages {
		if strings.Contains(msg, "Preserving") && strings.Contains(msg, "deferring to rebase") {
			t.Errorf("force-reset path must not emit preservation log, got: %s", msg)
		}
	}
}

// SyncWorktreeBase on a drained stack captures dirty WIP via `git stash
// create` (dangling commit, not on the shared stash stack), hard-resets to
// origin/<default>, then re-applies the WIP. The shared stash list is never
// mutated.
func TestSyncWorktreeBase_StackDrain_CleanWIPReapplied(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	run(t, "git", "-C", mgr.workDir, "remote", "set-url", "origin", bare)
	run(t, "git", "-C", mgr.workDir, "fetch", "origin")
	run(t, "git", "-C", mgr.workDir, "config", "user.name", "test")
	run(t, "git", "-C", mgr.workDir, "config", "user.email", "test@test")

	mgr.RenameBranchForTask("ghost task", "ralph-ghost2")

	// A ghost commit so HEAD diverges from origin/main (triggers the force-reset).
	writeFile(t, mgr.workDir, "ghost.txt", "ghost\n")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "ghost")

	// Dirty WIP that cleanly re-applies onto origin/main: add a new tracked file.
	writeFile(t, mgr.workDir, "wip.txt", "wip content\n")

	stashesBefore := strings.TrimSpace(gitOutput(project, "stash", "list"))

	if err := mgr.SyncWorktreeBase(context.Background(), []string{"ralph/prior-drained"}); err != nil {
		t.Fatalf("SyncWorktreeBase: %v", err)
	}

	originHead := strings.TrimSpace(gitOutput(mgr.workDir, "rev-parse", "origin/main"))
	got := strings.TrimSpace(gitOutput(mgr.workDir, "rev-parse", "HEAD"))
	if got != originHead {
		t.Errorf("HEAD = %q, want origin/main = %q", got, originHead)
	}
	wip, err := os.ReadFile(filepath.Join(mgr.workDir, "wip.txt"))
	if err != nil {
		t.Fatalf("wip.txt missing after force-reset: %v", err)
	}
	if string(wip) != "wip content\n" {
		t.Errorf("wip.txt = %q, want 'wip content\\n' — WIP not re-applied", string(wip))
	}
	stashesAfter := strings.TrimSpace(gitOutput(project, "stash", "list"))
	if stashesAfter != stashesBefore {
		t.Errorf("git stash list mutated: before=%q after=%q — shared stash stack must not change", stashesBefore, stashesAfter)
	}
}

// SyncWorktreeBase on a drained stack with conflicting WIP: the stash-apply
// fails, the worktree is hard-reset a second time to a clean origin/<default>,
// a log line explains what happened, and the shared stash stack is untouched.
func TestSyncWorktreeBase_StackDrain_ConflictingWIPDiscarded(t *testing.T) {
	project, bare := initBareRepoWithOrigin(t)
	log := &testLog{}
	ralphDir := filepath.Join(project, ".ralph")
	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: log}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	run(t, "git", "-C", mgr.workDir, "remote", "set-url", "origin", bare)
	run(t, "git", "-C", mgr.workDir, "fetch", "origin")
	run(t, "git", "-C", mgr.workDir, "config", "user.name", "test")
	run(t, "git", "-C", mgr.workDir, "config", "user.email", "test@test")

	mgr.RenameBranchForTask("ghost task", "ralph-ghost3")

	// Ghost commit adds a file with content "B".
	writeFile(t, mgr.workDir, "conflict.txt", "B\n")
	run(t, "git", "-C", mgr.workDir, "commit", "-m", "ghost B")

	// Dirty WIP changes the file to "C". Stash captures the diff B→C.
	// After reset to origin/main (no conflict.txt), apply stash tries to
	// modify a file whose base version "B" doesn't exist → conflict.
	if err := os.WriteFile(filepath.Join(mgr.workDir, "conflict.txt"), []byte("C\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stashesBefore := strings.TrimSpace(gitOutput(project, "stash", "list"))

	if err := mgr.SyncWorktreeBase(context.Background(), []string{"ralph/prior-drained"}); err != nil {
		t.Fatalf("SyncWorktreeBase: %v", err)
	}

	originHead := strings.TrimSpace(gitOutput(mgr.workDir, "rev-parse", "origin/main"))
	got := strings.TrimSpace(gitOutput(mgr.workDir, "rev-parse", "HEAD"))
	if got != originHead {
		t.Errorf("HEAD = %q, want origin/main = %q", got, originHead)
	}
	// Working tree must be clean (no conflict markers, no stray file).
	status := strings.TrimSpace(gitOutput(mgr.workDir, "status", "--porcelain"))
	if status != "" {
		t.Errorf("worktree not clean after conflicting WIP discard: %q", status)
	}
	if _, err := os.Stat(filepath.Join(mgr.workDir, "conflict.txt")); !os.IsNotExist(err) {
		t.Errorf("conflict.txt should not exist on origin/main, stat err: %v", err)
	}
	found := false
	for _, msg := range log.messages {
		if strings.Contains(msg, "WIP could not be re-applied") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'WIP could not be re-applied' log, got: %v", log.messages)
	}
	stashesAfter := strings.TrimSpace(gitOutput(project, "stash", "list"))
	if stashesAfter != stashesBefore {
		t.Errorf("git stash list mutated: before=%q after=%q — shared stash stack must not change", stashesBefore, stashesAfter)
	}
}

// PrepareForNextTask discards uncommitted changes when switching to a different
// task (current_task_id differs from nextTaskID), so stale files don't leak across tasks.
func TestPrepareForNextTask_CleansOnTaskSwitch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	_ = state.Write("current_task_id", "ralph-aaa")

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
// (current_task_id matches nextTaskID), so in-progress work survives crash/restart.
func TestPrepareForNextTask_PreservesChangesWhenResumingSameTask(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	_ = state.Write("current_task_id", "ralph-aaa")

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
		ProjectDir:   project,
		RalphDir:     ralphDir,
		BaseBranch:   "main",
		Logger:       &testLog{},
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

	// setStackHead now requires an open PR for the top completed branch.
	gh := newStubGitHub(StubGitHubConfig{
		Available: true,
		PRs:       []StubPR{{Number: 1, Branch: stackBranch}},
	})
	mgr := newRepoForTest(Config{
		ProjectDir:   project,
		RalphDir:     ralphDir,
		BaseBranch:   "main",
		Logger:       &testLog{},
	}, gh, withRunner(&execRunner{}), withState(newMemState()))

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
	mgr := newRepoForTest(Config{Logger: &testLog{}}, nil, withState(state))
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
	dir := t.TempDir()
	state := newMemState()
	runner := newStubRunner()
	runner.On("branch -m", "", nil)

	mgr := newRepoForTest(Config{ProjectDir: dir, WorkDir: filepath.Join(dir, "wt"), BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner), withState(state), withWorktreeBranch("ralph/next"))

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
	state := newMemState()
	runner := newStubRunner()
	workDir := t.TempDir()

	mgr := newRepoForTest(Config{ProjectDir: "/project", WorkDir: workDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner), withState(state), withWorktreeBranch("ralph/wip"))

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
	workDir := t.TempDir()
	mgr := newRepoForTest(Config{ProjectDir: "/project", WorkDir: workDir, BaseBranch: "main", Logger: &testLog{}}, nil, withWorktreeBranch("ralph/wip"))

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
	mgr := newRepoForTest(Config{ProjectDir: "/some/dir", WorkDir: "/some/dir", BaseBranch: "main", Logger: &testLog{}}, nil)
	if err := mgr.RenameBranchForTask("anything", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mgr.branchRenamed {
		t.Error("should not rename when WorkDir == ProjectDir")
	}
}


// Worktree path contains ralph-YYYYMMDD-01 (bats test 1)
func TestWorktreeDirUsesDateBasedName(t *testing.T) {
	project := t.TempDir()
	run(t, "git", "init", "-b", "main", project)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	runner := newStubRunner()
	runner.On("rev-parse --verify", "", fmt.Errorf("not found"))
	runner.On("rev-parse", "abc123", nil)
	runner.On("worktree", "", nil)
	runner.On("fetch", "", nil)
	runner.On("config", "", nil)

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	worktreeRoot := filepath.Join(ralphDir, "worktrees")
	today := time.Now().Format("20060102")
	expected := fmt.Sprintf("ralph-%s-01", today)
	if filepath.Base(mgr.workDir) != expected || filepath.Dir(mgr.workDir) != worktreeRoot {
		t.Errorf("WorkDir = %q, want %q under %q", mgr.workDir, expected, worktreeRoot)
	}
}

// Existing -01 directory causes -02 suffix (bats test 2)
func TestSecondRunSameDayIncrementsSuffix(t *testing.T) {
	project := t.TempDir()
	run(t, "git", "init", "-b", "main", project)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	runner := newStubRunner()
	runner.On("rev-parse --verify", "", fmt.Errorf("not found"))
	runner.On("rev-parse", "abc123", nil)
	runner.On("worktree", "", nil)
	runner.On("fetch", "", nil)
	runner.On("config", "", nil)

	worktreeRoot := filepath.Join(ralphDir, "worktrees")
	today := time.Now().Format("20060102")
	os.MkdirAll(filepath.Join(worktreeRoot, "ralph-"+today+"-01"), 0o755)

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	expected := fmt.Sprintf("ralph-%s-02", today)
	if filepath.Base(mgr.workDir) != expected || filepath.Dir(mgr.workDir) != worktreeRoot {
		t.Errorf("WorkDir = %q, want %q under %q", mgr.workDir, expected, worktreeRoot)
	}
}

// Worktree is created under <projectDir>/.ralph/worktrees/.
func TestWorktreeIsUnderRalphDir(t *testing.T) {
	project := t.TempDir()
	run(t, "git", "init", "-b", "main", project)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	runner := newStubRunner()
	runner.On("rev-parse --verify", "", fmt.Errorf("not found"))
	runner.On("rev-parse", "abc123", nil)
	runner.On("worktree", "", nil)
	runner.On("fetch", "", nil)
	runner.On("config", "", nil)

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	wantPrefix := filepath.Join(ralphDir, "worktrees")
	if !strings.HasPrefix(mgr.workDir, wantPrefix) {
		t.Errorf("WorkDir %q should be under %q", mgr.workDir, wantPrefix)
	}
}

// Stale branches don't affect new branch naming (bats test 6)
func TestBranchNamingIgnoresStale(t *testing.T) {
	workDir := t.TempDir()
	mgr := newRepoForTest(Config{ProjectDir: "/project", WorkDir: workDir, BaseBranch: "main", Logger: &testLog{}}, nil, withWorktreeBranch("ralph/wip"))

	mgr.RenameBranchForTask("First task", "")

	want := "ralph/first-task"
	if mgr.worktreeBranch != want {
		t.Errorf("branch = %q, want %q", mgr.worktreeBranch, want)
	}
}


// Removed worktree directory is pruned and fresh setup succeeds (bats test 9)
func TestStaleWorktreeBranchCleanedUpViaPrune(t *testing.T) {
	project := t.TempDir()
	run(t, "git", "init", "-b", "main", project)
	ralphDir := filepath.Join(project, ".ralph")

	runner := newStubRunner()
	runner.On("rev-parse --verify", "", fmt.Errorf("not found"))
	runner.On("rev-parse", "abc123", nil)
	runner.On("worktree", "", nil)
	runner.On("fetch", "", nil)
	runner.On("config", "", nil)

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	if !runner.CalledWith("worktree", "prune") {
		t.Error("expected worktree prune to be called during SetupWorktree")
	}
}

// Live ralph worktree is force-removed when branch conflicts (bats test 10)
func TestLiveRalphWorktreeRemovedWhenBranchExists(t *testing.T) {
	project := t.TempDir()
	run(t, "git", "init", "-b", "main", project)
	ralphDir := filepath.Join(project, ".ralph")
	worktreeRoot := filepath.Join(ralphDir, "worktrees")
	oldWt := filepath.Join(worktreeRoot, "ralph-old")
	os.MkdirAll(oldWt, 0o755)

	wipBranch := WipBranchName()
	porcelain := fmt.Sprintf("worktree %s\nbranch refs/heads/%s\n\n", oldWt, wipBranch)

	runner := newStubRunner()
	runner.On("rev-parse --verify", "", nil)
	runner.On("worktree list --porcelain", porcelain, nil)
	runner.On("worktree remove", "", nil)
	runner.On("worktree prune", "", nil)
	runner.On("worktree add", "", nil)
	runner.On("branch -D", "", nil)
	runner.On("fetch", "", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("config", "", nil)
	runner.On("rev-parse", "abc123", nil)

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	if !runner.CalledWith("worktree", "remove", "--force", oldWt) {
		t.Error("expected worktree remove --force to be called for old worktree")
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
	project := t.TempDir()
	run(t, "git", "init", "-b", "main", project)

	os.WriteFile(filepath.Join(project, ".gitignore"), []byte("node_modules\n"), 0o644)

	runner := newStubRunner()
	runner.On("add", "", nil)
	runner.On("commit", "", nil)

	mgr := newRepoForTest(Config{ProjectDir: project, WorkDir: project, Logger: &testLog{}, BaseBranch: "main"}, nil, withRunner(runner))
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
	workDir := t.TempDir()
	st := newMemState()
	mgr := newRepoForTest(Config{ProjectDir: "/project", WorkDir: workDir, BaseBranch: "main", Logger: &testLog{}}, nil, withState(st), withWorktreeBranch("ralph/wip"))

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
	log := &testLog{}

	mgr := newRepoForTest(Config{ProjectDir: "/project", WorkDir: "/project", RalphDir: filepath.Join(t.TempDir(), "nonexistent-ralph"), Logger: log, BaseBranch: "main"}, nil)
	mgr.PruneOrphanedWorktrees()

	if len(log.messages) > 0 {
		t.Errorf("expected no log messages, got %v", log.messages)
	}
}

// PruneOrphanedWorktrees leaves non-directory files alone
func TestPruneOrphanedWorktrees_IgnoresFiles(t *testing.T) {
	ralphDir := filepath.Join(t.TempDir(), ".ralph")
	worktreeRoot := filepath.Join(ralphDir, "worktrees")
	os.MkdirAll(worktreeRoot, 0o755)
	log := &testLog{}

	filePath := filepath.Join(worktreeRoot, "some-file.txt")
	os.WriteFile(filePath, []byte("keep"), 0o644)

	runner := newStubRunner()
	runner.On("worktree prune", "", nil)
	runner.On("worktree list --porcelain", "", nil)

	mgr := newRepoForTest(Config{ProjectDir: "/project", WorkDir: "/project", RalphDir: ralphDir, Logger: log, BaseBranch: "main"}, nil, withRunner(runner))
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

// SetupWorktree sets upstream tracking on ralph/next so git pull --rebase
// works in post-task hooks without manual git branch --set-upstream-to.
func TestSetupWorktree_SetsUpstreamTracking(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	remote := strings.TrimSpace(cmdOutput(t, "git", "-C", project, "config", "branch.ralph/next.remote"))
	if remote != "origin" {
		t.Errorf("branch.ralph/next.remote = %q, want origin", remote)
	}
	merge := strings.TrimSpace(cmdOutput(t, "git", "-C", project, "config", "branch.ralph/next.merge"))
	if merge != "refs/heads/main" {
		t.Errorf("branch.ralph/next.merge = %q, want refs/heads/main", merge)
	}
}

// SetupWorktree with a custom base branch sets tracking to origin/<base>,
// not origin/main, so post-task git pull works on non-default base branches.
func TestSetupWorktree_SetsUpstreamTracking_CustomBase(t *testing.T) {
	project, _ := initBareRepoWithBranch(t, "develop")
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "develop", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(state))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	remote := strings.TrimSpace(cmdOutput(t, "git", "-C", project, "config", "branch.ralph/next.remote"))
	if remote != "origin" {
		t.Errorf("branch.ralph/next.remote = %q, want origin", remote)
	}
	merge := strings.TrimSpace(cmdOutput(t, "git", "-C", project, "config", "branch.ralph/next.merge"))
	if merge != "refs/heads/develop" {
		t.Errorf("branch.ralph/next.merge = %q, want refs/heads/develop", merge)
	}
}

// SetupWorktree HEAD-fallback path also sets upstream tracking on ralph/next.
// Verified via stub runner to confirm the branch --set-upstream-to call
// happens even when origin/<base> is unavailable.
func TestSetupWorktree_FallbackPath_SetsUpstreamTracking(t *testing.T) {
	project := t.TempDir()
	run(t, "git", "init", "-b", "main", project)
	ralphDir := filepath.Join(project, ".ralph")

	runner := newStubRunner()
	runner.On("rev-parse --verify", "", fmt.Errorf("branch not found"))
	runner.OnSequence("worktree add", []stubResponse{
		{Err: fmt.Errorf("origin/main not found")},
		{Err: nil},
	})
	runner.On("worktree", "", nil)
	runner.On("fetch", "", nil)
	runner.On("config", "", nil)
	runner.On("rev-parse", "abc123", nil)

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	if !runner.CalledWith("branch", "--set-upstream-to", "origin/main", WipBranchName()) {
		t.Errorf("expected branch --set-upstream-to origin/main %s after fallback path", WipBranchName())
	}
}

// PrepareForNextTask calls branch --set-upstream-to explicitly so tracking is
// set regardless of branch.autoSetupMerge. Verified via stub runner so the
// test fails in any environment before the fix.
func TestPrepareForNextTask_CallsSetUpstreamExplicitly(t *testing.T) {
	dir := t.TempDir()
	runner := newStubRunner()
	runner.On("branch -m", "", nil)
	runner.On("checkout", "", nil)
	runner.On("clean", "", nil)
	runner.On("fetch", "", nil)
	runner.On("symbolic-ref", "refs/remotes/origin/main", nil)
	runner.On("rev-parse --verify", "", fmt.Errorf("not found"))
	runner.On("merge-base --is-ancestor", "", fmt.Errorf("not ancestor"))
	runner.On("rev-list --count", "0", nil)

	mgr := newRepoForTest(Config{ProjectDir: dir, WorkDir: filepath.Join(dir, "wt"), BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(runner), withWorktreeBranch("ralph/next"))

	mgr.RenameBranchForTask("first task", "ralph-aaa")
	mgr.PrepareForNextTask("ralph-bbb", "")

	if !runner.CalledWith("branch", "--set-upstream-to", "origin/main", WipBranchName()) {
		t.Errorf("expected branch --set-upstream-to origin/main %s to be called after PrepareForNextTask", WipBranchName())
	}
}

// PrepareForNextTask sets upstream tracking on the new ralph/next branch so
// git pull --rebase works in post-task hooks after every merge. Uses empty
// baseRef (the PostMergeUpdateMain path) which does not rely on autoSetupMerge.
func TestPrepareForNextTask_SetsUpstreamTracking(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "main", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("some task", "ralph-abc")
	// Empty baseRef matches PostMergeUpdateMain: git checkout -B ralph/next (no
	// start point) does not set tracking — only an explicit set-upstream call will.
	mgr.PrepareForNextTask("ralph-next", "")

	remote := strings.TrimSpace(cmdOutput(t, "git", "-C", project, "config", "branch.ralph/next.remote"))
	if remote != "origin" {
		t.Errorf("branch.ralph/next.remote = %q, want origin", remote)
	}
	merge := strings.TrimSpace(cmdOutput(t, "git", "-C", project, "config", "branch.ralph/next.merge"))
	if merge != "refs/heads/main" {
		t.Errorf("branch.ralph/next.merge = %q, want refs/heads/main", merge)
	}
}

// PrepareForNextTask with a custom base branch sets tracking to origin/<base>,
// not origin/main, honoring --base-branch override. Uses empty baseRef to
// reproduce PostMergeUpdateMain which does not pass a start point.
func TestPrepareForNextTask_SetsUpstreamTracking_CustomBase(t *testing.T) {
	project, _ := initBareRepoWithBranch(t, "develop")
	ralphDir := filepath.Join(project, ".ralph")

	mgr := newRepoForTest(Config{ProjectDir: project, RalphDir: ralphDir, BaseBranch: "develop", Logger: &testLog{}}, nil, withRunner(&execRunner{}), withState(newMemState()))
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("some task", "ralph-abc")
	mgr.PrepareForNextTask("ralph-next", "")

	remote := strings.TrimSpace(cmdOutput(t, "git", "-C", project, "config", "branch.ralph/next.remote"))
	if remote != "origin" {
		t.Errorf("branch.ralph/next.remote = %q, want origin", remote)
	}
	merge := strings.TrimSpace(cmdOutput(t, "git", "-C", project, "config", "branch.ralph/next.merge"))
	if merge != "refs/heads/develop" {
		t.Errorf("branch.ralph/next.merge = %q, want refs/heads/develop", merge)
	}
}

// Proves assertWorktreeReady is the single enforcement point for the worktree
// invariant. Init and InitTask call it; operational methods trust the invariant
// and do not re-check workDir == projectDir.
func TestAssertWorktreeReady_EnforcesInvariant(t *testing.T) {
	dir := t.TempDir()

	t.Run("workDir empty", func(t *testing.T) {
		r := newRepoForTest(Config{ProjectDir: dir, WorkDir: "", Logger: &testLog{}}, nil)
		if err := r.assertWorktreeReady(); err == nil {
			t.Error("expected error when workDir is empty")
		}
	})

	t.Run("workDir equals projectDir", func(t *testing.T) {
		r := newRepoForTest(Config{ProjectDir: dir, WorkDir: dir, Logger: &testLog{}}, nil)
		if err := r.assertWorktreeReady(); err == nil {
			t.Error("expected error when workDir == projectDir")
		}
	})

	t.Run("valid worktree", func(t *testing.T) {
		r := newRepoForTest(Config{ProjectDir: dir, WorkDir: dir + "/worktree", Logger: &testLog{}}, nil)
		if err := r.assertWorktreeReady(); err != nil {
			t.Errorf("expected no error for valid worktree, got: %v", err)
		}
	})
}
