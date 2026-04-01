package main_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	. "github.com/brokenalarms/ralph/cmd/ralph"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
)

// mergeStubRunner records all git command invocations and returns canned
// responses. Tests configure it via errOn (arg prefix → error) and
// use calledWith/neverCalledWith to assert the git command sequence.
type mergeStubRunner struct {
	mu    sync.Mutex
	calls [][]string
	errOn map[string]error
}

func newMergeStubRunner() *mergeStubRunner {
	return &mergeStubRunner{errOn: map[string]error{}}
}

func (r *mergeStubRunner) Run(_ context.Context, dir string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, args)
	for n := len(args); n > 0; n-- {
		key := strings.Join(args[:n], " ")
		if err, ok := r.errOn[key]; ok {
			return "", err
		}
	}
	return "", nil
}

func (r *mergeStubRunner) errOnArgs(key string, err error) *mergeStubRunner {
	r.errOn[key] = err
	return r
}

func (r *mergeStubRunner) calledWith(args ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if len(call) >= len(args) {
			match := true
			for i, a := range args {
				if call[i] != a {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

func (r *mergeStubRunner) neverCalledWith(args ...string) bool {
	return !r.calledWith(args...)
}

// Compile-time check that mergeStubRunner satisfies git.Runner.
var _ git.Runner = (*mergeStubRunner)(nil)

// trackingGH wraps StubGitHub and records which PR numbers GetPRHeadSHA is called for.
type trackingGH struct {
	*git.StubGitHub
	mu           sync.Mutex
	headSHACalls []string
}

func (g *trackingGH) GetPRHeadSHA(workDir, prNumber string) (string, error) {
	g.mu.Lock()
	g.headSHACalls = append(g.headSHACalls, prNumber)
	g.mu.Unlock()
	return "sha-" + prNumber, nil
}

func (g *trackingGH) calledForPR(prNumber string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, id := range g.headSHACalls {
		if id == prNumber {
			return true
		}
	}
	return false
}

// buildGM creates a git.Manager with the given stubs for merge tests.
// Uses a temp dir for RalphDir so worktree paths are predictable.
func buildGM(t *testing.T, runner git.Runner, gh git.GitHub) (*git.Manager, string) {
	t.Helper()
	tmp := t.TempDir()
	ralphDir := filepath.Join(tmp, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	return &git.Manager{
		ProjectDir: tmp,
		WorkDir:    tmp,
		RalphDir:   ralphDir,
		State:      state.NewStore(filepath.Join(ralphDir, "state.json")),
		Logger:     logging.New(nil),
		Runner:     runner,
		GitHub:     gh,
		BaseBranch: "main",
	}, tmp
}

// mergeRunCmd runs a command and fails the test on error (for integration tests).
func mergeRunCmd(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}

// ── collectStack tests ────────────────────────────────────────────────────────

// Proves: collectStack walks a chain of PRs from the top PR down to main,
// returning them in bottom-up order with the correct baseBranch.
func TestCollectStack_BottomUpOrder(t *testing.T) {
	// PR 460 → 459 → 452 → main
	gh := &git.StubGitHub{
		AllPRs: []git.PRInfo{
			{Number: 452, Head: "feature/a", Base: "main", State: "OPEN"},
			{Number: 459, Head: "feature/b", Base: "feature/a", State: "OPEN"},
			{Number: 460, Head: "feature/c", Base: "feature/b", State: "OPEN"},
		},
	}

	result := CollectStack(gh, "/any", "460", logging.New(nil))
	prs := StackResultPRs(result)

	if len(prs) != 3 {
		t.Fatalf("expected 3 PRs, got %d", len(prs))
	}
	if StackPRNumber(prs[0]) != "452" {
		t.Errorf("expected prs[0]=452 (bottom), got %s", StackPRNumber(prs[0]))
	}
	if StackPRNumber(prs[1]) != "459" {
		t.Errorf("expected prs[1]=459, got %s", StackPRNumber(prs[1]))
	}
	if StackPRNumber(prs[2]) != "460" {
		t.Errorf("expected prs[2]=460 (top), got %s", StackPRNumber(prs[2]))
	}
	if StackResultBaseBranch(result) != "main" {
		t.Errorf("expected baseBranch=main, got %s", StackResultBaseBranch(result))
	}
}

// Proves: collectStack skips CLOSED PRs — only OPEN PRs appear in the
// returned stack (closed ones are still used as chain links).
func TestCollectStack_SkipsClosedPRs(t *testing.T) {
	// PR 459 is CLOSED (already merged), 460 is still open on top.
	gh := &git.StubGitHub{
		AllPRs: []git.PRInfo{
			{Number: 452, Head: "feature/a", Base: "main", State: "OPEN"},
			{Number: 459, Head: "feature/b", Base: "feature/a", State: "CLOSED"},
			{Number: 460, Head: "feature/c", Base: "feature/b", State: "OPEN"},
		},
	}

	result := CollectStack(gh, "/any", "460", logging.New(nil))
	prs := StackResultPRs(result)

	if len(prs) != 2 {
		t.Fatalf("expected 2 PRs (CLOSED skipped), got %d", len(prs))
	}
	if StackPRNumber(prs[0]) != "452" {
		t.Errorf("expected prs[0]=452, got %s", StackPRNumber(prs[0]))
	}
	if StackPRNumber(prs[1]) != "460" {
		t.Errorf("expected prs[1]=460, got %s", StackPRNumber(prs[1]))
	}
}

// Proves: when the bottom PR targets a non-main branch (e.g. 'develop'),
// baseBranch reflects that target, not 'main'.
func TestCollectStack_NonMainBaseBranch(t *testing.T) {
	gh := &git.StubGitHub{
		AllPRs: []git.PRInfo{
			{Number: 100, Head: "feature/x", Base: "develop", State: "OPEN"},
		},
	}

	result := CollectStack(gh, "/any", "100", logging.New(nil))

	if StackResultBaseBranch(result) != "develop" {
		t.Errorf("expected baseBranch=develop, got %s", StackResultBaseBranch(result))
	}
	prs := StackResultPRs(result)
	if len(prs) != 1 || StackPRNumber(prs[0]) != "100" {
		t.Errorf("unexpected prs: %+v", prs)
	}
}

// ── rebaseStackAndPush tests ─────────────────────────────────────────────────

// Proves: when a worktree directory exists but the bottom branch is not
// rebased onto origin/main (merge-base --is-ancestor fails), rebaseStackAndPush
// removes the stale worktree and recreates it (worktree remove then worktree add).
func TestRebaseStackAndPush_StaleWorktreeRecreated(t *testing.T) {
	runner := newMergeStubRunner()
	// merge-base --is-ancestor fails → stale worktree
	runner.errOnArgs("merge-base --is-ancestor", fmt.Errorf("not an ancestor"))
	// worktree add succeeds; rebase succeeds
	gm, tmp := buildGM(t, runner, &git.StubGitHub{})
	gm.RalphDir = filepath.Join(tmp, ".ralph")

	// Create a fake .git inside the worktree dir to simulate an existing worktree.
	wtDir := filepath.Join(gm.RalphDir, "worktrees", "merge-pr2")
	os.MkdirAll(filepath.Join(wtDir, ".git"), 0o755)

	RebaseStackAndPush(context.Background(), runner, tmp, "main", "pr2", "999", []string{"pr1", "pr2"}, gm, logging.New(nil))

	if !runner.calledWith("worktree", "remove", "--force", wtDir) {
		t.Error("expected 'git worktree remove --force' to be called for stale worktree")
	}
	if !runner.calledWith("worktree", "add") {
		t.Error("expected 'git worktree add' to be called after removing stale worktree")
	}
}

// Proves: on the fresh path, rebaseStackAndPush fetches origin/main AND
// every individual stack branch before creating the worktree.
func TestRebaseStackAndPush_FetchesAllBranches(t *testing.T) {
	runner := newMergeStubRunner()
	gm, tmp := buildGM(t, runner, &git.StubGitHub{})

	RebaseStackAndPush(context.Background(), runner, tmp, "main", "pr3", "123", []string{"pr1", "pr2", "pr3"}, gm, logging.New(nil))

	if !runner.calledWith("fetch", "origin", "main") {
		t.Error("expected fetch of origin/main")
	}
	for _, br := range []string{"pr1", "pr2", "pr3"} {
		if !runner.calledWith("fetch", "origin", br) {
			t.Errorf("expected fetch of origin/%s", br)
		}
	}
}

// Proves: when rebase succeeds, rebaseStackAndPush force-pushes every branch
// in the stack.
func TestRebaseStackAndPush_PushesAllBranchesOnSuccess(t *testing.T) {
	runner := newMergeStubRunner()
	gm, tmp := buildGM(t, runner, &git.StubGitHub{})

	code := RebaseStackAndPush(context.Background(), runner, tmp, "main", "pr2", "42", []string{"pr1", "pr2"}, gm, logging.New(nil))

	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	for _, br := range []string{"pr1", "pr2"} {
		if !runner.calledWith("push", "--force", "origin", br) {
			t.Errorf("expected force-push of %s", br)
		}
	}
}

// Proves: when rebase fails and auto-resolve also fails, rebaseStackAndPush
// returns non-zero and does NOT attempt to push any branch.
func TestRebaseStackAndPush_RebaseConflictNoPush(t *testing.T) {
	runner := newMergeStubRunner()
	runner.errOnArgs("rebase --update-refs", fmt.Errorf("conflict"))
	gm, tmp := buildGM(t, runner, &git.StubGitHub{})

	code := RebaseStackAndPush(context.Background(), runner, tmp, "main", "pr1", "7", []string{"pr1"}, gm, logging.New(nil))

	if code == 0 {
		t.Error("expected non-zero exit when rebase conflicts")
	}
	if !runner.neverCalledWith("push", "--force") {
		t.Error("expected no push when rebase fails")
	}
}

// Proves: the worktree directory is created under .ralph/worktrees/ (not the
// project root or some other temp location).
func TestRebaseStackAndPush_WorktreeUnderRalphDir(t *testing.T) {
	runner := newMergeStubRunner()
	gm, tmp := buildGM(t, runner, &git.StubGitHub{})

	RebaseStackAndPush(context.Background(), runner, tmp, "main", "pr1", "5", []string{"pr1"}, gm, logging.New(nil))

	expectedPrefix := filepath.Join(gm.RalphDir, "worktrees") + string(filepath.Separator)
	// Find the worktree add call and verify its path argument.
	found := false
	for _, call := range runner.calls {
		if len(call) >= 2 && call[0] == "worktree" && call[1] == "add" {
			found = true
			for _, arg := range call[2:] {
				if filepath.IsAbs(arg) && strings.Contains(arg, "merge-") {
					if !strings.HasPrefix(arg, expectedPrefix) {
						t.Errorf("worktree add path %q is not under .ralph/worktrees/", arg)
					}
				}
			}
		}
	}
	if !found {
		t.Error("expected 'git worktree add' to be called")
	}
}

// ── runMerge tests (stub-based) ───────────────────────────────────────────────

// Proves: when CI fails on a PR, runMerge returns non-zero and does not
// attempt to merge any subsequent PRs.
func TestRunMerge_CIFailureStops(t *testing.T) {
	runner := newMergeStubRunner()
	gh := &git.StubGitHub{
		IsAvailable: true,
		Checks:      []git.CICheckResult{{Bucket: "check", State: "FAILURE"}},
		MergeResult: git.MergeResult{Merged: true},
	}
	gm, tmp := buildGM(t, runner, gh)

	prs := []StackPR{
		NewStackPR("1", "pr1"),
		NewStackPR("2", "pr2"),
	}
	code := RunMerge(context.Background(), prs, tmp, "main", gm, false, logging.New(nil))

	if code == 0 {
		t.Errorf("expected non-zero exit when CI fails, got 0")
	}
	if gh.MergeCalls > 0 {
		t.Errorf("expected no MergePR calls when CI fails, got %d", gh.MergeCalls)
	}
}

// Proves: when MergePR returns Conflict, runMerge returns non-zero with a
// conflict-specific message rather than a generic failure.
func TestRunMerge_MergeConflictLogsMessage(t *testing.T) {
	runner := newMergeStubRunner()
	var logBuf bytes.Buffer
	gh := &git.StubGitHub{
		IsAvailable: true,
		Checks:      []git.CICheckResult{{Bucket: "pass", State: "SUCCESS"}},
		MergeResult: git.MergeResult{Conflict: true},
	}
	gm, tmp := buildGM(t, runner, gh)
	gm.Logger = logging.NewWithWriter(&logBuf)

	code := RunMerge(context.Background(), []StackPR{NewStackPR("1", "pr1")}, tmp, "main", gm, false, logging.NewWithWriter(&logBuf))

	if code == 0 {
		t.Error("expected non-zero exit on Conflict")
	}
	if !strings.Contains(logBuf.String(), "merge conflicts") {
		t.Errorf("expected conflict message in output, got: %s", logBuf.String())
	}
}

// Proves: after merging PR N, runMerge fetches origin/main and origin/PR(N+1),
// rebases PR(N+1) onto main, and force-pushes before waiting for CI.
// This ensures stale CI results from the original base are never used.
func TestRunMerge_SecondPRRebased(t *testing.T) {
	runner := newMergeStubRunner()
	gh := &trackingGH{
		StubGitHub: &git.StubGitHub{
			IsAvailable: true,
			Checks:      []git.CICheckResult{{Bucket: "pass", State: "SUCCESS"}},
			MergeResult: git.MergeResult{Merged: true},
		},
	}
	gm, tmp := buildGM(t, runner, gh)

	prs := []StackPR{
		NewStackPR("1", "pr1"),
		NewStackPR("2", "pr2"),
	}
	code := RunMerge(context.Background(), prs, tmp, "main", gm, false, logging.New(nil))

	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}

	// After merging PR 1, must fetch origin/main and origin/pr2.
	if !runner.calledWith("fetch", "origin", "main") {
		t.Error("expected fetch of origin/main after first merge")
	}
	if !runner.calledWith("fetch", "origin", "pr2") {
		t.Error("expected fetch of origin/pr2 before rebasing")
	}

	// Must rebase pr2 onto origin/main.
	if !runner.calledWith("rebase", "origin/main") {
		t.Error("expected rebase of pr2 onto origin/main")
	}

	// Must force-push pr2 before CI wait.
	if !runner.calledWith("push", "--force-with-lease", "origin", "HEAD:pr2") {
		t.Error("expected force-with-lease push of pr2")
	}

	// GetPRHeadSHA must be called for PR 2 to get the fresh HEAD after push.
	if !gh.calledForPR("2") {
		t.Error("GetPRHeadSHA not called for PR 2 — fresh CI was not awaited after rebase")
	}
}

// ── integration tests (real git repos) ───────────────────────────────────────

// setupStackRepo creates a bare "remote" and a working clone with two stacked
// branches: pr1 (one commit on main) and pr2 (one commit on top of pr1).
// Returns (workDir, bareDir).
func setupStackRepo(t *testing.T) (string, string) {
	t.Helper()
	bareDir := filepath.Join(t.TempDir(), "origin.git")
	workDir := filepath.Join(t.TempDir(), "work")

	mergeRunCmd(t, "git", "init", "--bare", "-b", "main", bareDir)
	mergeRunCmd(t, "git", "clone", bareDir, workDir)

	// Initial commit on main.
	os.WriteFile(filepath.Join(workDir, "init.txt"), []byte("init"), 0o644)
	mergeRunCmd(t, "git", "-C", workDir, "add", "init.txt")
	mergeRunCmd(t, "git", "-C", workDir, "commit", "-m", "init")
	mergeRunCmd(t, "git", "-C", workDir, "push", "origin", "main")

	// pr1 branch: one commit on top of main.
	mergeRunCmd(t, "git", "-C", workDir, "checkout", "-b", "pr1")
	os.WriteFile(filepath.Join(workDir, "pr1.txt"), []byte("pr1"), 0o644)
	mergeRunCmd(t, "git", "-C", workDir, "add", "pr1.txt")
	mergeRunCmd(t, "git", "-C", workDir, "commit", "-m", "pr1 commit")
	mergeRunCmd(t, "git", "-C", workDir, "push", "origin", "pr1")

	// pr2 branch: one commit on top of pr1.
	mergeRunCmd(t, "git", "-C", workDir, "checkout", "-b", "pr2")
	os.WriteFile(filepath.Join(workDir, "pr2.txt"), []byte("pr2"), 0o644)
	mergeRunCmd(t, "git", "-C", workDir, "add", "pr2.txt")
	mergeRunCmd(t, "git", "-C", workDir, "commit", "-m", "pr2 commit")
	mergeRunCmd(t, "git", "-C", workDir, "push", "origin", "pr2")

	// Return to main.
	mergeRunCmd(t, "git", "-C", workDir, "checkout", "main")
	return workDir, bareDir
}

// mergeCmdOutputDir runs a command in a directory and returns its output.
func mergeCmdOutputDir(dir, name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, _ := cmd.Output()
	return string(out)
}

// Proves: when a two-PR stack is merged, the second PR's branch is rebased
// onto the updated main and fresh CI is awaited (GetPRHeadSHA is called for
// the second PR) before it is merged. This ensures stale CI results from the
// original base branch are never used to gate the merge.
func TestRunMerge_SecondPRWaitsForFreshCI(t *testing.T) {
	workDir, bareDir := setupStackRepo(t)

	// Record main's SHA before any merges so we can verify the rebase later.
	mainSHABefore := strings.TrimSpace(mergeCmdOutputDir(workDir, "git", "rev-parse", "origin/main"))

	ghStub := &trackingGH{
		StubGitHub: &git.StubGitHub{
			IsAvailable: true,
			MergeResult: git.MergeResult{Merged: true},
			Checks:      []git.CICheckResult{{Bucket: "pass", State: "SUCCESS"}},
		},
	}

	// Simulate GitHub advancing main when PR 1 is merged: fast-forward
	// main on the bare remote to include pr1's commit. This is what
	// GitHub does on squash-merge. Without this, the rebase of pr2 onto
	// origin/main would be a no-op (same base), hiding the bug.
	mergeCount := 0
	ghStub.OnMerge = func() {
		mergeCount++
		if mergeCount == 1 {
			// Advance main on the bare remote to include pr1.
			exec.Command("git", "-C", bareDir, "update-ref", "refs/heads/main",
				strings.TrimSpace(mergeCmdOutputDir(bareDir, "git", "rev-parse", "pr1"))).Run()
		}
	}

	ralphDir := filepath.Join(workDir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	gm := &git.Manager{
		ProjectDir: workDir,
		WorkDir:    workDir,
		RalphDir:   ralphDir,
		State:      state.NewStore(filepath.Join(ralphDir, "state.json")),
		Logger:     logging.New(nil),
		GitHub:     ghStub,
		BaseBranch: "main",
	}

	prs := []StackPR{
		NewStackPR("1", "pr1"),
		NewStackPR("2", "pr2"),
	}

	code := RunMerge(context.Background(), prs, workDir, "main", gm, false, logging.New(nil))
	if code != 0 {
		t.Errorf("runMerge returned %d, expected 0 (success)", code)
	}

	// Both PRs should have been merged.
	if ghStub.MergeCalls != 2 {
		t.Errorf("expected 2 MergePR calls, got %d", ghStub.MergeCalls)
	}

	// GetPRHeadSHA must be called for PR 2 to get the fresh HEAD after rebase.
	if !ghStub.calledForPR("2") {
		t.Error("GetPRHeadSHA was not called for PR 2 — fresh CI was not awaited after rebase")
	}

	// Verify pr2 was actually rebased onto the updated main (not the old main).
	// After the rebase, pr2 on the remote must have the new main (which includes
	// pr1) as an ancestor, not just the original main.
	pr2SHA := strings.TrimSpace(mergeCmdOutputDir(bareDir, "git", "rev-parse", "pr2"))
	newMainSHA := strings.TrimSpace(mergeCmdOutputDir(bareDir, "git", "rev-parse", "main"))

	// main must have advanced past its original position (pr1 was merged).
	if newMainSHA == mainSHABefore {
		t.Error("main did not advance after PR 1 merge — OnMerge simulation failed")
	}

	// pr2 on the remote must be a descendant of the new main (post-pr1-merge),
	// proving the rebase onto updated main actually happened.
	cmd := exec.Command("git", "-C", bareDir, "merge-base", "--is-ancestor", newMainSHA, pr2SHA)
	if err := cmd.Run(); err != nil {
		t.Error("PR 2 was not rebased onto updated main — its branch does not descend from the new main SHA")
	}
}

// Proves: a single-PR stack completes successfully, preserving the existing
// single-PR merge path unchanged.
func TestRunMerge_SinglePRMergesSuccessfully(t *testing.T) {
	workDir, _ := setupStackRepo(t)

	ghStub := &trackingGH{
		StubGitHub: &git.StubGitHub{
			IsAvailable: true,
			MergeResult: git.MergeResult{Merged: true},
			Checks:      []git.CICheckResult{{Bucket: "pass", State: "SUCCESS"}},
		},
	}

	ralphDir := filepath.Join(workDir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	gm := &git.Manager{
		ProjectDir: workDir,
		WorkDir:    workDir,
		RalphDir:   ralphDir,
		State:      state.NewStore(filepath.Join(ralphDir, "state.json")),
		Logger:     logging.New(nil),
		GitHub:     ghStub,
		BaseBranch: "main",
	}

	code := RunMerge(context.Background(), []StackPR{NewStackPR("1", "pr1")}, workDir, "main", gm, false, logging.New(nil))
	if code != 0 {
		t.Errorf("runMerge returned %d, expected 0", code)
	}
	if ghStub.MergeCalls != 1 {
		t.Errorf("expected 1 MergePR call, got %d", ghStub.MergeCalls)
	}
}

// Proves: --bypass-rules sets Admin=true on each MergePR call, allowing
// branch protection to be bypassed when the flag is explicitly passed.
func TestRunMerge_BypassRulesSetsAdminOnMergeOpts(t *testing.T) {
	workDir, _ := setupStackRepo(t)

	ghStub := &trackingGH{
		StubGitHub: &git.StubGitHub{
			IsAvailable: true,
			MergeResult: git.MergeResult{Merged: true},
			Checks:      []git.CICheckResult{{Bucket: "pass", State: "SUCCESS"}},
		},
	}

	ralphDir := filepath.Join(workDir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	gm := &git.Manager{
		ProjectDir: workDir,
		WorkDir:    workDir,
		RalphDir:   ralphDir,
		State:      state.NewStore(filepath.Join(ralphDir, "state.json")),
		Logger:     logging.New(nil),
		GitHub:     ghStub,
		BaseBranch: "main",
	}

	code := RunMerge(context.Background(), []StackPR{NewStackPR("1", "pr1")}, workDir, "main", gm, true, logging.New(nil))
	if code != 0 {
		t.Errorf("runMerge with bypassRules returned %d, expected 0", code)
	}
	if !ghStub.LastMergeOpts.Admin {
		t.Error("expected MergeOpts.Admin=true when --bypass-rules is set")
	}
}

// Proves: --bypass-rules with REST returning 405 triggers the admin fallback
// and still merges successfully (end-to-end through runMerge).
func TestRunMerge_BypassRulesAdminFallbackOn405(t *testing.T) {
	workDir, _ := setupStackRepo(t)

	ghStub := &trackingGH{
		StubGitHub: &git.StubGitHub{
			IsAvailable: true,
			MergeResult: git.MergeResult{Blocked: true, Message: "branch protection"},
			Checks:      []git.CICheckResult{{Bucket: "pass", State: "SUCCESS"}},
		},
	}

	ralphDir := filepath.Join(workDir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	gm := &git.Manager{
		ProjectDir: workDir,
		WorkDir:    workDir,
		RalphDir:   ralphDir,
		State:      state.NewStore(filepath.Join(ralphDir, "state.json")),
		Logger:     logging.New(nil),
		GitHub:     ghStub,
		BaseBranch: "main",
	}

	code := RunMerge(context.Background(), []StackPR{NewStackPR("1", "pr1")}, workDir, "main", gm, true, logging.New(nil))
	if code != 0 {
		t.Errorf("runMerge with bypassRules+Blocked returned %d, expected 0 (admin fallback)", code)
	}
}

// Proves: when MergeResult.Blocked=true without --bypass-rules, runMerge returns
// non-zero and logs the block message so the user knows why the merge stopped.
func TestRunMerge_BlockedWithoutBypassLogs(t *testing.T) {
	workDir, _ := setupStackRepo(t)

	var logBuf bytes.Buffer
	ghStub := &trackingGH{
		StubGitHub: &git.StubGitHub{
			IsAvailable: true,
			MergeResult: git.MergeResult{Blocked: true, Message: "requires admin approval"},
			Checks:      []git.CICheckResult{{Bucket: "pass", State: "SUCCESS"}},
		},
	}

	ralphDir := filepath.Join(workDir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	gm := &git.Manager{
		ProjectDir: workDir,
		WorkDir:    workDir,
		RalphDir:   ralphDir,
		State:      state.NewStore(filepath.Join(ralphDir, "state.json")),
		Logger:     logging.NewWithWriter(&logBuf),
		GitHub:     ghStub,
		BaseBranch: "main",
	}

	code := RunMerge(context.Background(), []StackPR{NewStackPR("1", "pr1")}, workDir, "main", gm, false, logging.NewWithWriter(&logBuf))
	if code == 0 {
		t.Errorf("runMerge with Blocked should return non-zero, got 0")
	}
	if !strings.Contains(logBuf.String(), "requires admin approval") {
		t.Errorf("expected Blocked message in output, got: %s", logBuf.String())
	}
}

// Proves: when MergeResult.Conflict=true, runMerge returns non-zero with a
// conflict-specific error message rather than a generic merge failure.
func TestRunMerge_ConflictLogsDistinctMessage(t *testing.T) {
	workDir, _ := setupStackRepo(t)

	var logBuf bytes.Buffer
	ghStub := &trackingGH{
		StubGitHub: &git.StubGitHub{
			IsAvailable: true,
			MergeResult: git.MergeResult{Conflict: true},
			Checks:      []git.CICheckResult{{Bucket: "pass", State: "SUCCESS"}},
		},
	}

	ralphDir := filepath.Join(workDir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	gm := &git.Manager{
		ProjectDir: workDir,
		WorkDir:    workDir,
		RalphDir:   ralphDir,
		State:      state.NewStore(filepath.Join(ralphDir, "state.json")),
		Logger:     logging.NewWithWriter(&logBuf),
		GitHub:     ghStub,
		BaseBranch: "main",
	}

	code := RunMerge(context.Background(), []StackPR{NewStackPR("1", "pr1")}, workDir, "main", gm, false, logging.NewWithWriter(&logBuf))
	if code == 0 {
		t.Errorf("runMerge with Conflict should return non-zero, got 0")
	}
	if !strings.Contains(logBuf.String(), "merge conflicts") {
		t.Errorf("expected conflict-specific message in output, got: %s", logBuf.String())
	}
}
