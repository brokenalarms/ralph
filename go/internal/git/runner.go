package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner abstracts git command execution for testability. Production code
// uses execRunner (which shells out to git); tests inject stubs to avoid
// spawning real processes.
type Runner interface {
	Run(ctx context.Context, dir string, args ...string) (output string, err error)
}

// execRunner is the production Runner that shells out to the git binary.
type execRunner struct{}

func (r *execRunner) Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// defaultRunner is the package-level runner used by IsGitRepo and test helpers.
var defaultRunner Runner = &execRunner{}

func gitCmd(dir string, args ...string) {
	defaultRunner.Run(context.Background(), dir, args...)
}

func gitCmdErr(dir string, args ...string) error {
	_, err := defaultRunner.Run(context.Background(), dir, args...)
	return err
}

func gitOutput(dir string, args ...string) string {
	out, _ := defaultRunner.Run(context.Background(), dir, args...)
	return out
}

func refExists(dir, ref string) bool {
	return gitCmdErr(dir, "rev-parse", "--verify", ref) == nil
}

// repo-level git command wrappers that delegate to the injected Runner.
// These mirror the standalone wrappers above but route through r.run()
// so tests can intercept all git calls.

func (r *repo) run() Runner {
	if r.runner != nil {
		return r.runner
	}
	return defaultRunner
}

// GetRunner returns the injected Runner, or the package default if none is set.
// Used by cmd/ralph functions that need to route git commands through the same
// runner as the repo for testability.
func (r *repo) GetRunner() Runner {
	return r.run()
}

func (r *repo) gitCmd(dir string, args ...string) {
	r.run().Run(context.Background(), dir, args...)
}

func (r *repo) gitCmdCtx(ctx context.Context, dir string, args ...string) {
	r.run().Run(ctx, dir, args...)
}

func (r *repo) gitCmdErr(dir string, args ...string) error {
	_, err := r.run().Run(context.Background(), dir, args...)
	return err
}

func (r *repo) gitCmdErrCtx(ctx context.Context, dir string, args ...string) error {
	_, err := r.run().Run(ctx, dir, args...)
	return err
}

func (r *repo) gitOutput(dir string, args ...string) string {
	out, _ := r.run().Run(context.Background(), dir, args...)
	return out
}

func (r *repo) refExists(dir, ref string) bool {
	return r.gitCmdErr(dir, "rev-parse", "--verify", ref) == nil
}

// DetectDefaultBranch returns the configured base branch. The base is the
// single source of truth: resolved once at startup into cfg.BaseBranch,
// threaded into git.New as r.baseBranch, and never re-derived from git.
func (r *repo) DetectDefaultBranch() string {
	return r.baseBranch
}

// OriginRev returns the commit hash of origin/<branch>.
func (r *repo) OriginRev(branch string) string {
	return r.gitOutput(r.workDir, "rev-parse", "origin/"+branch)
}

func (r *repo) remoteExists() bool {
	return r.gitOutput(r.projectDir, "remote", "get-url", "origin") != ""
}

// FindRemoteBranchForTask searches remote branches for one containing the
// given task/bead ID. Returns the branch name or empty string.
func (r *repo) FindRemoteBranchForTask(taskID string) string {
	if taskID == "" {
		return ""
	}
	_ = r.gitCmdErr(r.projectDir, "fetch", "--prune", "origin")
	out := r.gitOutput(r.projectDir, "branch", "-r", "--list", "origin/"+BranchListPattern()+taskID+"*")
	for _, line := range strings.Split(out, "\n") {
		branch := strings.TrimSpace(line)
		branch = strings.TrimPrefix(branch, "origin/")
		if branch != "" {
			return branch
		}
	}
	return ""
}

// CheckoutRemoteBranch checks out a remote branch in the worktree,
// creating a local tracking branch.
func (r *repo) CheckoutRemoteBranch(branch string) {
	_ = r.gitCmdErr(r.workDir, "fetch", "origin", branch)
	_ = r.gitCmdErr(r.workDir, "checkout", "-B", branch, "origin/"+branch)
	r.worktreeBranch = branch
	r.branchRenamed = true
	if r.state != nil {
		_ = r.state.Write("worktree_branch", branch)
		_ = r.state.Write("branch_renamed", "true")
	}
}

// LocalBranchHasCommits returns true when refs/heads/<branch> exists locally
// and has at least one commit ahead of origin/<default>. Used to detect
// unpushed task work before deciding whether to rename over the local branch.
func (r *repo) LocalBranchHasCommits(branch string) bool {
	if !r.refExists(r.workDir, "refs/heads/"+branch) {
		return false
	}
	defaultBranch := r.baseBranch
	count := r.gitOutput(r.workDir, "rev-list", "--count", "origin/"+defaultBranch+".."+branch)
	return count != "" && count != "0"
}

// CheckoutLocalBranch checks out an existing local branch in the worktree
// without fetching from origin. Used to resume unpushed task work where the
// remote has no corresponding branch.
func (r *repo) CheckoutLocalBranch(branch string) {
	_ = r.gitCmdErr(r.workDir, "checkout", "-B", branch, branch)
	r.worktreeBranch = branch
	r.branchRenamed = true
	if r.state != nil {
		_ = r.state.Write("worktree_branch", branch)
		_ = r.state.Write("branch_renamed", "true")
	}
}

// DeleteRemoteBranchByName deletes a specific remote branch.
func (r *repo) DeleteRemoteBranchByName(branch string) error {
	return r.gitCmdErr(r.workDir, "push", "origin", "--delete", branch)
}

// RemoteBranchDiffFromMain returns the diff stat between origin/<branch> and
// origin/<defaultBranch>. Empty means no difference (work is on main).
func (r *repo) RemoteBranchDiffFromMain(branch, defaultBranch string) string {
	return r.gitOutput(r.workDir, "diff", "--stat", "origin/"+defaultBranch, "origin/"+branch)
}

// RemoteURL returns the origin remote URL.
func (r *repo) RemoteURL() string {
	return r.gitOutput(r.projectDir, "remote", "get-url", "origin")
}

// FetchBranch fetches a specific branch from origin.
func (r *repo) FetchBranch(branch string) error {
	return r.gitCmdErr(r.workDir, "fetch", "origin", branch)
}

// RemoteBranchHasCommits checks if origin/<branch> has commits beyond the default branch.
func (r *repo) RemoteBranchHasCommits(branch string) bool {
	remote := "origin/" + branch
	if !r.refExists(r.workDir, remote) {
		return false
	}
	defaultBranch := r.baseBranch
	count := r.gitOutput(r.workDir, "rev-list", "--count", "origin/"+defaultBranch+".."+remote)
	return count != "" && count != "0"
}

// RemoteBranchIsOnMain returns true if a remote branch is a descendant of
// origin's default branch (i.e., main is an ancestor). Returns false if the
// branch has diverged — caller should treat it as stale and start fresh.
func (r *repo) RemoteBranchIsOnMain(branch string) bool {
	defaultBranch := r.baseBranch
	remote := "origin/" + branch

	// Main is ancestor of branch — branch is cleanly ahead.
	if r.gitCmdErr(r.workDir, "merge-base", "--is-ancestor", "origin/"+defaultBranch, remote) == nil {
		return true
	}

	// Branch is ancestor of main — work already landed.
	if r.gitCmdErr(r.workDir, "merge-base", "--is-ancestor", remote, "origin/"+defaultBranch) == nil {
		return true
	}

	return false
}

// BranchIsAncestorOfMain returns true if a remote branch's tip is an
// ancestor of origin's default branch — meaning its work has already
// landed on main (via merge or squash-merge).
func (r *repo) BranchIsAncestorOfMain(branch string) bool {
	defaultBranch := r.baseBranch
	remote := "origin/" + branch
	return r.gitCmdErr(r.workDir, "merge-base", "--is-ancestor", remote, "origin/"+defaultBranch) == nil
}

// IsCommitAncestorOf returns true when sha is an ancestor of (or equal to) ref.
func (r *repo) IsCommitAncestorOf(sha, ref string) bool {
	return r.gitCmdErr(r.workDir, "merge-base", "--is-ancestor", sha, ref) == nil
}

// BranchIsAheadOfMain returns true if origin's default branch is an
// ancestor of the remote branch — meaning the branch is cleanly ahead
// of main with unmerged work. Returns false for landed branches (equal
// to or behind main) and diverged branches (neither is ancestor).
func (r *repo) BranchIsAheadOfMain(branch string) bool {
	defaultBranch := r.baseBranch
	remote := "origin/" + branch
	return r.gitCmdErr(r.workDir, "merge-base", "--is-ancestor", "origin/"+defaultBranch, remote) == nil
}

// BranchHasUnmergedWork returns true if the remote branch has any commits
// that are not on origin's default branch — i.e. git rev-list --count
// origin/<default>..<branch> > 0. This is a superset of BranchIsAheadOfMain
// that also returns true for diverged branches (branch has commits ahead of
// main AND main has commits not on branch). Use this in Ship to avoid
// skipping PR creation for diverged branches with real unmerged work.
func (r *repo) BranchHasUnmergedWork(branch string) bool {
	defaultBranch := r.baseBranch
	out := r.gitOutput(r.workDir, "rev-list", "--count", "origin/"+defaultBranch+"..origin/"+branch)
	return strings.TrimSpace(out) != "0" && strings.TrimSpace(out) != ""
}

func (r *repo) mRebaseInProgress() bool {
	gitDir := r.gitOutput(r.workDir, "rev-parse", "--git-dir")
	if gitDir == "" {
		return false
	}
	_, errMerge := os.Stat(filepath.Join(gitDir, "rebase-merge"))
	_, errApply := os.Stat(filepath.Join(gitDir, "rebase-apply"))
	return errMerge == nil || errApply == nil
}

func (r *repo) findWorktreeForBranch(dir, branch string) string {
	out := r.gitOutput(dir, "worktree", "list", "--porcelain")
	return parseWorktreeForBranch(out, branch)
}

// parseWorktreeForBranch extracts the worktree path for a branch from
// git worktree list --porcelain output.
func parseWorktreeForBranch(porcelainOutput, branch string) string {
	if porcelainOutput == "" {
		return ""
	}
	target := "branch refs/heads/" + branch
	lines := strings.Split(porcelainOutput, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == target {
			for j := i - 1; j >= 0; j-- {
				if strings.HasPrefix(lines[j], "worktree ") {
					return strings.TrimPrefix(lines[j], "worktree ")
				}
			}
		}
	}
	return ""
}
