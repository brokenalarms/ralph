package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
)

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

// setupStackRepo creates a bare "remote" and a working clone with two stacked
// branches: pr1 (one commit on main) and pr2 (one commit on top of pr1).
// Returns (workDir, bareDir).
func setupStackRepo(t *testing.T) (string, string) {
	t.Helper()
	bareDir := filepath.Join(t.TempDir(), "origin.git")
	workDir := filepath.Join(t.TempDir(), "work")

	runCmd(t, "git", "init", "--bare", "-b", "main", bareDir)
	runCmd(t, "git", "clone", bareDir, workDir)

	// Initial commit on main.
	os.WriteFile(filepath.Join(workDir, "init.txt"), []byte("init"), 0o644)
	runCmd(t, "git", "-C", workDir, "add", "init.txt")
	runCmd(t, "git", "-C", workDir, "commit", "-m", "init")
	runCmd(t, "git", "-C", workDir, "push", "origin", "main")

	// pr1 branch: one commit on top of main.
	runCmd(t, "git", "-C", workDir, "checkout", "-b", "pr1")
	os.WriteFile(filepath.Join(workDir, "pr1.txt"), []byte("pr1"), 0o644)
	runCmd(t, "git", "-C", workDir, "add", "pr1.txt")
	runCmd(t, "git", "-C", workDir, "commit", "-m", "pr1 commit")
	runCmd(t, "git", "-C", workDir, "push", "origin", "pr1")

	// pr2 branch: one commit on top of pr1.
	runCmd(t, "git", "-C", workDir, "checkout", "-b", "pr2")
	os.WriteFile(filepath.Join(workDir, "pr2.txt"), []byte("pr2"), 0o644)
	runCmd(t, "git", "-C", workDir, "add", "pr2.txt")
	runCmd(t, "git", "-C", workDir, "commit", "-m", "pr2 commit")
	runCmd(t, "git", "-C", workDir, "push", "origin", "pr2")

	// Return to main.
	runCmd(t, "git", "-C", workDir, "checkout", "main")
	return workDir, bareDir
}

// Proves: when a two-PR stack is merged, the second PR's branch is rebased
// onto the updated main and fresh CI is awaited (GetPRHeadSHA is called for
// the second PR) before it is merged. This ensures stale CI results from the
// original base branch are never used to gate the merge.
func TestRunMerge_SecondPRWaitsForFreshCI(t *testing.T) {
	workDir, bareDir := setupStackRepo(t)

	// Record main's SHA before any merges so we can verify the rebase later.
	mainSHABefore := strings.TrimSpace(cmdOutputDir(workDir, "git", "rev-parse", "origin/main"))

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
				strings.TrimSpace(cmdOutputDir(bareDir, "git", "rev-parse", "pr1"))).Run()
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

	prs := []stackPR{
		{number: "1", head: "pr1"},
		{number: "2", head: "pr2"},
	}

	code := runMerge(context.Background(), prs, workDir, "main", gm, false, logging.New(nil))
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
	pr2SHA := strings.TrimSpace(cmdOutputDir(bareDir, "git", "rev-parse", "pr2"))
	newMainSHA := strings.TrimSpace(cmdOutputDir(bareDir, "git", "rev-parse", "main"))

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

	code := runMerge(context.Background(), []stackPR{{number: "1", head: "pr1"}}, workDir, "main", gm, false, logging.New(nil))
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

	code := runMerge(context.Background(), []stackPR{{number: "1", head: "pr1"}}, workDir, "main", gm, true, logging.New(nil))
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

	code := runMerge(context.Background(), []stackPR{{number: "1", head: "pr1"}}, workDir, "main", gm, true, logging.New(nil))
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

	code := runMerge(context.Background(), []stackPR{{number: "1", head: "pr1"}}, workDir, "main", gm, false, logging.NewWithWriter(&logBuf))
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

	code := runMerge(context.Background(), []stackPR{{number: "1", head: "pr1"}}, workDir, "main", gm, false, logging.NewWithWriter(&logBuf))
	if code == 0 {
		t.Errorf("runMerge with Conflict should return non-zero, got 0")
	}
	if !strings.Contains(logBuf.String(), "merge conflicts") {
		t.Errorf("expected conflict-specific message in output, got: %s", logBuf.String())
	}
}
