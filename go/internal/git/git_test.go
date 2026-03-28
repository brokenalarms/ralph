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

func cmdOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v failed: %v", name, args, err)
	}
	return string(out)
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
	want := "ralph/wip"
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

	// Branch name must be exactly ralph/<projectName>/wip
	wantBranch := "ralph/wip"
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


// RenameBranchForTask does not set PrevBranch — that's controlled by
// setStackHead in the loop via SetPrevBranch. Rename only changes the
// branch name.
func TestRenameBranchForTask_DoesNotSetPrevBranch(t *testing.T) {
	project, _ := initBareRepo(t)
	ralphDir := filepath.Join(project, ".ralph")
	state := newMemState()
	log := &testLog{}

	mgr := &Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      state,
		Logger:     log,
	}
	if err := mgr.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	mgr.RenameBranchForTask("first task", "")
	mgr.PrepareForNextTask()
	mgr.RenameBranchForTask("second task", "")

	if mgr.PrevBranch != "" {
		t.Errorf("PrevBranch = %q, want empty (set by setStackHead, not rename)", mgr.PrevBranch)
	}
}

// SetPrevBranch explicitly sets PrevBranch and persists to state.
func TestSetPrevBranch(t *testing.T) {
	state := newMemState()
	mgr := &Manager{State: state, Logger: &testLog{}}
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

// Stale branches don't affect new branch naming (bats test 6)
func TestBranchNamingIgnoresStale(t *testing.T) {
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

// RenameBranchForTask renames the branch and sets BranchRenamed.
func TestRenameBranchForTask_RenamesAndSetsFlag(t *testing.T) {
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

