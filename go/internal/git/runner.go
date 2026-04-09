package git

import (
	"context"
	"io"
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
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
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

// Repo-level git command wrappers that delegate to the injected Runner.
// These mirror the standalone wrappers above but route through m.run()
// so tests can intercept all git calls.

func (m *Repo) run() Runner {
	if m.Runner != nil {
		return m.Runner
	}
	return defaultRunner
}

// GetRunner returns the injected Runner, or the package default if none is set.
// Used by cmd/ralph functions that need to route git commands through the same
// runner as the Repo for testability.
func (m *Repo) GetRunner() Runner {
	return m.run()
}

func (m *Repo) gitCmd(dir string, args ...string) {
	m.run().Run(context.Background(), dir, args...)
}

func (m *Repo) gitCmdCtx(ctx context.Context, dir string, args ...string) {
	m.run().Run(ctx, dir, args...)
}

func (m *Repo) gitCmdErr(dir string, args ...string) error {
	_, err := m.run().Run(context.Background(), dir, args...)
	return err
}

func (m *Repo) gitCmdErrCtx(ctx context.Context, dir string, args ...string) error {
	_, err := m.run().Run(ctx, dir, args...)
	return err
}

func (m *Repo) gitOutput(dir string, args ...string) string {
	out, _ := m.run().Run(context.Background(), dir, args...)
	return out
}

func (m *Repo) refExists(dir, ref string) bool {
	return m.gitCmdErr(dir, "rev-parse", "--verify", ref) == nil
}

// DetectDefaultBranch returns the resolved default branch name.
func (m *Repo) DetectDefaultBranch() string {
	return m.detectDefaultBranch()
}

func (m *Repo) detectDefaultBranch() string {
	return detectDefaultBranch(m.ProjectDir, m.BaseBranch, m.run())
}

// OriginRev returns the commit hash of origin/<branch>.
func (m *Repo) OriginRev(branch string) string {
	return m.gitOutput(m.WorkDir, "rev-parse", "origin/"+branch)
}

func (m *Repo) remoteExists() bool {
	return m.gitOutput(m.ProjectDir, "remote", "get-url", "origin") != ""
}

// FindRemoteBranchForTask searches remote branches for one containing the
// given task/bead ID. Returns the branch name or empty string.
func (m *Repo) FindRemoteBranchForTask(taskID string) string {
	if taskID == "" {
		return ""
	}
	_ = m.gitCmdErr(m.ProjectDir, "fetch", "--prune", "origin")
	out := m.gitOutput(m.ProjectDir, "branch", "-r", "--list", "origin/"+BranchListPattern()+taskID+"*")
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
func (m *Repo) CheckoutRemoteBranch(branch string) {
	_ = m.gitCmdErr(m.WorkDir, "fetch", "origin", branch)
	_ = m.gitCmdErr(m.WorkDir, "checkout", "-B", branch, "origin/"+branch)
	m.WorktreeBranch = branch
	m.BranchRenamed = true
	if m.State != nil {
		_ = m.State.Write("worktree_branch", branch)
		_ = m.State.Write("branch_renamed", "true")
	}
}

// DeleteRemoteBranchByName deletes a specific remote branch.
func (m *Repo) DeleteRemoteBranchByName(branch string) error {
	return m.gitCmdErr(m.WorkDir, "push", "origin", "--delete", branch)
}

// RemoteBranchDiffFromMain returns the diff stat between origin/<branch> and
// origin/<defaultBranch>. Empty means no difference (work is on main).
func (m *Repo) RemoteBranchDiffFromMain(branch, defaultBranch string) string {
	return m.gitOutput(m.WorkDir, "diff", "--stat", "origin/"+defaultBranch, "origin/"+branch)
}

// RemoteURL returns the origin remote URL.
func (m *Repo) RemoteURL() string {
	return m.gitOutput(m.ProjectDir, "remote", "get-url", "origin")
}

// FetchBranch fetches a specific branch from origin.
func (m *Repo) FetchBranch(branch string) error {
	return m.gitCmdErr(m.WorkDir, "fetch", "origin", branch)
}

// RemoteBranchHasCommits checks if origin/<branch> has commits beyond the default branch.
func (m *Repo) RemoteBranchHasCommits(branch string) bool {
	remote := "origin/" + branch
	if !m.refExists(m.WorkDir, remote) {
		return false
	}
	defaultBranch := m.detectDefaultBranch()
	count := m.gitOutput(m.WorkDir, "rev-list", "--count", "origin/"+defaultBranch+".."+remote)
	return count != "" && count != "0"
}

// RemoteBranchIsOnMain returns true if a remote branch is a descendant of
// origin's default branch (i.e., main is an ancestor). Returns false if the
// branch has diverged — caller should treat it as stale and start fresh.
func (m *Repo) RemoteBranchIsOnMain(branch string) bool {
	defaultBranch := m.detectDefaultBranch()
	remote := "origin/" + branch

	// Main is ancestor of branch — branch is cleanly ahead.
	if m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", "origin/"+defaultBranch, remote) == nil {
		return true
	}

	// Branch is ancestor of main — work already landed.
	if m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", remote, "origin/"+defaultBranch) == nil {
		return true
	}

	return false
}

// BranchIsAncestorOfMain returns true if a remote branch's tip is an
// ancestor of origin's default branch — meaning its work has already
// landed on main (via merge or squash-merge).
func (m *Repo) BranchIsAncestorOfMain(branch string) bool {
	defaultBranch := m.detectDefaultBranch()
	remote := "origin/" + branch
	return m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", remote, "origin/"+defaultBranch) == nil
}

// BranchIsAheadOfMain returns true if origin's default branch is an
// ancestor of the remote branch — meaning the branch is cleanly ahead
// of main with unmerged work. Returns false for landed branches (equal
// to or behind main) and diverged branches (neither is ancestor).
func (m *Repo) BranchIsAheadOfMain(branch string) bool {
	defaultBranch := m.detectDefaultBranch()
	remote := "origin/" + branch
	return m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", "origin/"+defaultBranch, remote) == nil
}

func (m *Repo) mRebaseInProgress() bool {
	gitDir := m.gitOutput(m.WorkDir, "rev-parse", "--git-dir")
	if gitDir == "" {
		return false
	}
	_, errMerge := os.Stat(filepath.Join(gitDir, "rebase-merge"))
	_, errApply := os.Stat(filepath.Join(gitDir, "rebase-apply"))
	return errMerge == nil || errApply == nil
}

func (m *Repo) findWorktreeForBranch(dir, branch string) string {
	out := m.gitOutput(dir, "worktree", "list", "--porcelain")
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

// detectDefaultBranch returns the configured base branch.
// BaseBranch is always set after config parsing (--base-branch defaults to "develop"),
// so no fallback to git symbolic-ref or hardcoded values is needed.
func detectDefaultBranch(_, override string, _ ...Runner) string {
	return override
}
