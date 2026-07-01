package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/brokenalarms/ralph/internal/logging"
)

var (
	nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
	dashTrim = regexp.MustCompile(`^-|-$`)
)

const branchPrefix = "ralph/"

// normalizeBranch ensures a branch name has exactly one "ralph/" prefix.
// All branch name constructors go through this to prevent duplication.
func normalizeBranch(name string) string {
	// Strip any existing prefix(es) before adding the canonical one.
	for strings.HasPrefix(name, branchPrefix) {
		name = strings.TrimPrefix(name, branchPrefix)
	}
	return branchPrefix + name
}

// BranchName returns the canonical branch name for a task.
// With a beadID: ralph/<beadID>-<slug>
// Without:       ralph/<slug>
func BranchName(beadID, slug string) string {
	if beadID != "" {
		return normalizeBranch(beadID + "-" + slug)
	}
	return normalizeBranch(slug)
}

// WipBranchName returns the placeholder branch used between tasks by the loop.
// ralph task does NOT use this — it has its own per-instance branch via
// TaskBranchName so it never collides with the loop's worktree.
func WipBranchName() string {
	return normalizeBranch("next")
}

// TaskBranchName returns the per-instance branch used by an interactive
// `ralph task` worktree. Each invocation gets a unique date+seq suffix so
// concurrent task managers (or a task manager running alongside the loop)
// never share a branch and never tear down each other's worktrees.
func TaskBranchName(date string, seq int) string {
	return normalizeBranch(fmt.Sprintf("task/%s-%02d", date, seq))
}

// BranchListPattern returns the glob pattern for listing ralph branches.
func BranchListPattern() string {
	return branchPrefix + "*"
}

// Slugify converts a description to a URL/branch-safe slug.
// Limited to 4 words and 50 characters to keep branch names short.
func Slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlnum.ReplaceAllString(s, "-")
	s = dashTrim.ReplaceAllString(s, "")

	parts := strings.SplitN(s, "-", 5)
	if len(parts) > 4 {
		parts = parts[:4]
	}
	s = strings.Join(parts, "-")

	if len(s) > 50 {
		s = s[:50]
		s = strings.TrimRight(s, "-")
	}
	return s
}

// IsGitRepo returns true if dir is inside a git repository.
func IsGitRepo(dir string) bool {
	return gitCmdErr(dir, "rev-parse", "--git-dir") == nil
}

// RemoteDefaultBranch reads the repository's default branch from the local
// git remote HEAD (refs/remotes/origin/HEAD). It falls back to "main" when the
// symbolic ref is unset. Unlike the loop's commit/PR-creation path — which
// requires an explicit --base-branch and must never guess a target — ralph
// merge only pulls already-merged PRs into the local default branch, so
// detecting that branch from git is the correct, flag-free behavior.
func RemoteDefaultBranch(dir string) string {
	out := gitOutput(dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	out = strings.TrimSpace(strings.TrimPrefix(out, "origin/"))
	if out == "" {
		return "main"
	}
	return out
}

// RepoRoot returns the absolute path of the root of the git repository
// containing dir. Returns an error if dir is not inside a git repository.
func RepoRoot(dir string) (string, error) {
	out, err := defaultRunner.Run(context.Background(), dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s", dir)
	}
	return out, nil
}

// repo methods that mirror the package-level helpers but route
// through the repo's injected Runner.

func (r *repo) HasDiff() bool {
	if r.gitOutput(r.workDir, "diff", "--stat") != "" {
		return true
	}
	return r.gitOutput(r.workDir, "diff", "--cached", "--stat") != ""
}

func (r *repo) HeadRev() string {
	return r.gitOutput(r.workDir, "rev-parse", "HEAD")
}

func (r *repo) HasUncommittedChanges() bool {
	return r.hasUncommittedChangesIn(r.workDir)
}

// hasUncommittedChangesIn returns true when the given dir has unstaged or
// staged changes. Init calls this with r.projectDir explicitly, because
// Init runs the dirty-tree check before SetupWorktree has had a chance to
// move WorkDir to a worktree subdirectory — so checking WorkDir would be
// the wrong question.
func (r *repo) hasUncommittedChangesIn(dir string) bool {
	return r.gitCmdErr(dir, "diff", "--quiet") != nil ||
		r.gitCmdErr(dir, "diff", "--cached", "--quiet") != nil
}

func (r *repo) CommitAll(message string) {
	r.gitCmd(r.workDir, "add", "-A")
	_ = r.gitCmdErr(r.workDir, "commit", "-m", message)
}

func (r *repo) EmptyCommit(message string) {
	_ = r.gitCmdErr(r.workDir, "commit", "--allow-empty", "-m", message)
}

func (r *repo) ChangedFiles(headBefore, headAfter string) []string {
	seen := make(map[string]bool)
	var result []string

	add := func(out string) {
		for _, f := range strings.Split(out, "\n") {
			f = strings.TrimSpace(f)
			if f != "" && !seen[f] {
				seen[f] = true
				result = append(result, f)
			}
		}
	}

	add(r.gitOutput(r.workDir, "diff", "--name-only"))
	add(r.gitOutput(r.workDir, "diff", "--cached", "--name-only"))

	if headBefore != "" && headAfter != "" && headBefore != headAfter {
		add(r.gitOutput(r.workDir, "diff", "--name-only", headBefore+"..."+headAfter))
	}

	return result
}

func (r *repo) DiffStatRange(from, to string) string {
	if from == "" || to == "" || from == to {
		return ""
	}
	return r.gitOutput(r.workDir, "diff", "--stat", from, to)
}

func (r *repo) DiffFull(from, to string) string {
	return r.gitOutput(r.workDir, "diff", from+".."+to)
}

// DiffFromBase returns the three-dot diff of HEAD against the merge-base with
// the default branch (git diff origin/<base>...HEAD). Unlike DiffFull with an
// iteration-local headBefore, this captures every commit the task branch has
// made relative to the base regardless of which iteration produced it — the
// same coverage a PR diff gives. Used by the verifier when no PR exists yet.
func (r *repo) DiffFromBase() string {
	remote := "origin/" + r.baseBranch
	return r.gitOutput(r.workDir, "diff", remote+"...HEAD")
}

// ConflictDiff returns the three-way merge diff between HEAD and the base
// branch (or the default branch), showing what diverges. Used to give a
// conflict resolution agent context about the conflicting changes.
func (r *repo) ConflictDiff() string {
	baseBranch := r.baseBranch
	if r.prevBranch != "" {
		baseBranch = r.prevBranch
	}
	remote := "origin/" + baseBranch
	files := r.gitOutput(r.workDir, "diff", "--name-only", remote+"...HEAD")
	diff := r.gitOutput(r.workDir, "diff", remote+"...HEAD")
	if files == "" && diff == "" {
		return ""
	}
	return "Conflicting files:\n" + files + "\n\nDiff (ours vs base):\n" + diff
}

func (r *repo) LogOneline(from, to string) string {
	return r.gitOutput(r.workDir, "log", "--oneline", from+".."+to)
}

func (r *repo) RecentChangedFiles(n int) string {
	return r.gitOutput(r.workDir, "diff", "--name-only", fmt.Sprintf("HEAD~%d", n), "HEAD")
}

func (r *repo) ValidateRemoteBranch(ctx context.Context) error {
	branch := r.baseBranch
	r.gitCmdCtx(ctx, r.projectDir, "fetch", "origin", branch)
	if !r.refExists(r.projectDir, "origin/"+branch) {
		return fmt.Errorf("base branch %q does not exist on remote — create it or set --base-branch", branch)
	}
	return nil
}

func (r *repo) EnsureGitignored(entry string) {
	gitignorePath := filepath.Join(r.projectDir, ".gitignore")
	existing := ""
	if data, err := os.ReadFile(gitignorePath); err == nil {
		existing = string(data)
	}

	found := false
	for _, line := range strings.Split(existing, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == entry || trimmed == entry+"/" || trimmed == entry+"/*" {
			found = true
			break
		}
	}
	if found {
		return
	}

	existing += entry + "\n"
	os.WriteFile(gitignorePath, []byte(existing), 0o644)

	if IsGitRepo(r.projectDir) {
		r.gitCmd(r.projectDir, "add", ".gitignore")
		r.gitCmd(r.projectDir, "commit", "-m", "Add "+entry+" to .gitignore")
	}
}

// hasPathPrefix reports whether path is under prefix, resolving symlinks on
// both sides so macOS /var → /private/var aliases don't cause false negatives.
func hasPathPrefix(path, prefix string) bool {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if resolved, err := filepath.EvalSymlinks(prefix); err == nil {
		prefix = resolved
	}
	return strings.HasPrefix(path, prefix)
}

// resolveWorktreeRoot returns the worktree root directory under ralphDir.
func (r *repo) resolveWorktreeRoot() (string, error) {
	if r.ralphDir == "" {
		return "", fmt.Errorf("ralphDir is required for worktree root")
	}
	return filepath.Join(r.ralphDir, "worktrees"), nil
}

func (r *repo) PruneOrphanedWorktrees() {
	worktreeRoot, err := r.resolveWorktreeRoot()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(worktreeRoot)
	if err != nil {
		return
	}
	if len(entries) == 0 {
		return
	}

	r.gitCmd(r.projectDir, "worktree", "prune")

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dirPath := filepath.Join(worktreeRoot, e.Name())
		// Re-verify immediately before removal instead of trusting a snapshot
		// taken earlier in this call: a concurrent process may have registered
		// dirPath as a live worktree in the meantime (TOCTOU race).
		if r.isLiveWorktree(dirPath) {
			continue
		}
		// Last-instant re-check: isLiveWorktree's own `git worktree list`
		// query can itself be the moment a concurrent registration lands, so
		// check the gitdir link one more time right at the point of no return.
		if hasValidGitdirLink(dirPath) {
			continue
		}
		if r.logger != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Removing orphaned worktree directory: %s", dirPath)
		}
		os.RemoveAll(dirPath)
	}
}

// isLiveWorktree reports whether dirPath is currently a registered git
// worktree. It is called immediately before a removal decision, so it never
// trusts a stale snapshot — it either finds a valid `.git` gitdir link
// written by `git worktree add` (checked first since it can't lag behind a
// concurrent registration the way a cached `git worktree list` would) or
// falls back to a fresh `git worktree list --porcelain` query.
func (r *repo) isLiveWorktree(dirPath string) bool {
	if hasValidGitdirLink(dirPath) {
		return true
	}
	resolved, err := filepath.EvalSymlinks(dirPath)
	if err != nil {
		resolved = dirPath
	}
	out := r.gitOutput(r.projectDir, "worktree", "list", "--porcelain")
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		tracked := strings.TrimPrefix(line, "worktree ")
		if tracked == dirPath || tracked == resolved {
			return true
		}
	}
	return false
}

// hasValidGitdirLink reports whether dirPath contains a `.git` file (as
// git worktrees do, in contrast to a full `.git` directory) whose `gitdir:`
// target still exists as a worktree admin entry.
func hasValidGitdirLink(dirPath string) bool {
	gitFile := filepath.Join(dirPath, ".git")
	info, err := os.Lstat(gitFile)
	if err != nil || info.IsDir() {
		return false
	}
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return false
	}
	const prefix = "gitdir:"
	content := strings.TrimSpace(string(data))
	if !strings.HasPrefix(content, prefix) {
		return false
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(content, prefix))
	_, err = os.Stat(gitdir)
	return err == nil
}
