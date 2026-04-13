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

// WipBranchName returns the placeholder branch used between tasks.
func WipBranchName() string {
	return normalizeBranch("next")
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

// RepoRoot returns the absolute path of the root of the git repository
// containing dir. Returns an error if dir is not inside a git repository.
func RepoRoot(dir string) (string, error) {
	out, err := defaultRunner.Run(context.Background(), dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s", dir)
	}
	return out, nil
}

func parseBranchList(out string) []string {
	if out == "" {
		return nil
	}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "* ")
		line = strings.TrimPrefix(line, "+ ")
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches
}

// Repo methods that mirror the package-level helpers but route
// through the Repo's injected Runner.

func (r *Repo) HasDiff() bool {
	if r.gitOutput(r.workDir, "diff", "--stat") != "" {
		return true
	}
	return r.gitOutput(r.workDir, "diff", "--cached", "--stat") != ""
}

func (r *Repo) HeadRev() string {
	return r.gitOutput(r.workDir, "rev-parse", "HEAD")
}

func (r *Repo) HasUncommittedChanges() bool {
	return r.hasUncommittedChangesIn(r.workDir)
}

// hasUncommittedChangesIn returns true when the given dir has unstaged or
// staged changes. Init calls this with r.projectDir explicitly, because
// Init runs the dirty-tree check before SetupWorktree has had a chance to
// move WorkDir to a worktree subdirectory — so checking WorkDir would be
// the wrong question.
func (r *Repo) hasUncommittedChangesIn(dir string) bool {
	return r.gitCmdErr(dir, "diff", "--quiet") != nil ||
		r.gitCmdErr(dir, "diff", "--cached", "--quiet") != nil
}

func (r *Repo) CommitAll(message string) {
	r.gitCmd(r.workDir, "add", "-A")
	_ = r.gitCmdErr(r.workDir, "commit", "-m", message)
}

// RevertFilesToRef restores files to their state in the given ref and amends
// the current commit. Used to undo out-of-scope changes made by fix agents.
func (r *Repo) RevertFilesToRef(files []string, ref string) {
	for _, f := range files {
		_ = r.gitCmdErr(r.workDir, "checkout", ref, "--", f)
	}
	r.gitCmd(r.workDir, "add", "-A")
	_ = r.gitCmdErr(r.workDir, "commit", "--amend", "--no-edit")
}

func (r *Repo) ChangedFiles(headBefore, headAfter string) []string {
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

// DiffFilesBetween returns the list of files modified between two refs.
// Unlike ChangedFiles, this does not include uncommitted or cached changes.
func (r *Repo) DiffFilesBetween(from, to string) []string {
	if from == "" || to == "" || from == to {
		return nil
	}
	out := r.gitOutput(r.workDir, "diff", "--name-only", from, to)
	var files []string
	for _, f := range strings.Split(out, "\n") {
		f = strings.TrimSpace(f)
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

func (r *Repo) DiffStatRange(from, to string) string {
	if from == "" || to == "" || from == to {
		return ""
	}
	return r.gitOutput(r.workDir, "diff", "--stat", from, to)
}

// WorktreeMatchesMain returns true if the worktree tree is identical to
// origin's default branch. Uses tree diff so squash-merge hash mismatches
// don't cause false negatives.
func (r *Repo) WorktreeMatchesMain() bool {
	defaultBranch := r.detectDefaultBranch()
	_ = r.gitCmdErr(r.workDir, "fetch", "origin", defaultBranch)
	return r.gitOutput(r.workDir, "diff", "--stat", "HEAD", "origin/"+defaultBranch) == ""
}

func (r *Repo) DiffFull(from, to string) string {
	return r.gitOutput(r.workDir, "diff", from+".."+to)
}

// ConflictDiff returns the three-way merge diff between HEAD and the base
// branch (or the default branch), showing what diverges. Used to give a
// conflict resolution agent context about the conflicting changes.
func (r *Repo) ConflictDiff() string {
	baseBranch := r.detectDefaultBranch()
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

func (r *Repo) LogOneline(from, to string) string {
	return r.gitOutput(r.workDir, "log", "--oneline", from+".."+to)
}

func (r *Repo) RecentChangedFiles(n int) string {
	return r.gitOutput(r.workDir, "diff", "--name-only", fmt.Sprintf("HEAD~%d", n), "HEAD")
}

func (r *Repo) ListProjectBranches() []string {
	out := r.gitOutput(r.projectDir, "branch", "--list", BranchListPattern(), "--sort=refname")
	return parseBranchList(out)
}


func (r *Repo) ValidateRemoteBranch(ctx context.Context) error {
	branch := r.detectDefaultBranch()
	r.gitCmdCtx(ctx, r.projectDir, "fetch", "origin", branch)
	if !r.refExists(r.projectDir, "origin/"+branch) {
		return fmt.Errorf("base branch %q does not exist on remote — create it or set --base-branch", branch)
	}
	return nil
}

func (r *Repo) EnsureGitignored(entry string) {
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

func (r *Repo) PruneOrphanedWorktrees() {
	worktreeRoot := filepath.Join(r.ralphDir, "worktrees")
	entries, err := os.ReadDir(worktreeRoot)
	if err != nil {
		return
	}
	if len(entries) == 0 {
		return
	}

	r.gitCmd(r.projectDir, "worktree", "prune")

	out := r.gitOutput(r.projectDir, "worktree", "list", "--porcelain")
	tracked := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			tracked[strings.TrimPrefix(line, "worktree ")] = true
		}
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dirPath := filepath.Join(worktreeRoot, e.Name())
		resolved, err := filepath.EvalSymlinks(dirPath)
		if err != nil {
			resolved = dirPath
		}
		if tracked[dirPath] || tracked[resolved] {
			continue
		}
		if r.logger != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Removing orphaned worktree directory: %s", dirPath)
		}
		os.RemoveAll(dirPath)
	}
}

