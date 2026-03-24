package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ValidateRemoteBranch checks that the given branch exists on the remote.
// Called before state initialization so a failed check doesn't leave
// stale state that causes false resumes.
func ValidateRemoteBranch(ctx context.Context, projectDir, baseBranch string) error {
	branch := detectDefaultBranch(projectDir, baseBranch)
	gitCmdCtx(ctx, projectDir, "fetch", "origin", branch)
	if !refExists(projectDir, "origin/"+branch) {
		return fmt.Errorf("base branch %q does not exist on remote — create it or set --base-branch", branch)
	}
	return nil
}

var (
	nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
	dashTrim = regexp.MustCompile(`^-|-$`)
)

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

// ListProjectBranches returns ralph/<project>/* branches sorted by refname.
func ListProjectBranches(dir, projectName string) []string {
	out := gitOutput(dir, "branch", "--list", "ralph/"+projectName+"/*", "--sort=refname")
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

// IsGitRepo returns true if dir is inside a git repository.
func IsGitRepo(dir string) bool {
	return gitCmdErr(dir, "rev-parse", "--git-dir") == nil
}

// EnsureGitignored adds an entry to .gitignore if it's not already present,
// then commits the change. Preserves existing content.
func EnsureGitignored(projectDir, entry string) {
	gitignorePath := filepath.Join(projectDir, ".gitignore")
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

	if IsGitRepo(projectDir) {
		gitCmd(projectDir, "add", ".gitignore")
		gitCmd(projectDir, "commit", "-m", "Add "+entry+" to .gitignore")
	}
}

// HasUncommittedChanges returns true if the working tree has staged or
// unstaged changes (git diff --quiet fails).
func HasUncommittedChanges(dir string) bool {
	return gitCmdErr(dir, "diff", "--quiet") != nil ||
		gitCmdErr(dir, "diff", "--cached", "--quiet") != nil
}

// HeadRev returns the current HEAD commit hash, or empty string on error.
func HeadRev(dir string) string {
	return gitOutput(dir, "rev-parse", "HEAD")
}

// HasDiff returns true if the worktree has staged or unstaged changes.
func HasDiff(dir string) bool {
	if gitOutput(dir, "diff", "--stat") != "" {
		return true
	}
	return gitOutput(dir, "diff", "--cached", "--stat") != ""
}

// ChangedFiles returns a deduplicated list of files changed in the worktree
// (staged + unstaged), and optionally between two commits.
func ChangedFiles(dir, headBefore, headAfter string) []string {
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

	add(gitOutput(dir, "diff", "--name-only"))
	add(gitOutput(dir, "diff", "--cached", "--name-only"))

	if headBefore != "" && headAfter != "" && headBefore != headAfter {
		add(gitOutput(dir, "diff", "--name-only", headBefore+"..."+headAfter))
	}

	return result
}

// DiffStatRange returns the --stat summary between two commits.
// Returns empty string if the commits are equal or missing.
func DiffStatRange(dir, from, to string) string {
	if from == "" || to == "" || from == to {
		return ""
	}
	return gitOutput(dir, "diff", "--stat", from, to)
}

// DiffFull returns the full diff between two commits.
func DiffFull(dir, from, to string) string {
	return gitOutput(dir, "diff", from+".."+to)
}

// LogOneline returns the oneline log between two commits.
func LogOneline(dir, from, to string) string {
	return gitOutput(dir, "log", "--oneline", from+".."+to)
}

// RecentChangedFiles returns files changed in the last N commits.
func RecentChangedFiles(dir string, n int) string {
	return gitOutput(dir, "diff", "--name-only", fmt.Sprintf("HEAD~%d", n), "HEAD")
}

// PruneOrphanedWorktrees removes worktree directories under ralphDir/worktrees
// that are no longer tracked by git. It first runs `git worktree prune` to
// clean up stale bookkeeping, then removes any leftover directories that git
// no longer knows about.
func PruneOrphanedWorktrees(projectDir, ralphDir string, logger Log) {
	worktreeRoot := filepath.Join(ralphDir, "worktrees")
	entries, err := os.ReadDir(worktreeRoot)
	if err != nil {
		return
	}
	if len(entries) == 0 {
		return
	}

	gitCmd(projectDir, "worktree", "prune")

	tracked := trackedWorktreePaths(projectDir)

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
		if logger != nil {
			logger.Log("git", "Removing orphaned worktree directory: %s", dirPath)
		}
		os.RemoveAll(dirPath)
	}
}

// trackedWorktreePaths returns the set of worktree paths that git currently
// tracks, parsed from `git worktree list --porcelain`.
func trackedWorktreePaths(projectDir string) map[string]bool {
	out := gitOutput(projectDir, "worktree", "list", "--porcelain")
	paths := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			paths[strings.TrimPrefix(line, "worktree ")] = true
		}
	}
	return paths
}

// ParseTaskSeqFromBranches scans ralph/<project>/* branches and returns the
// highest sequence number found.
func ParseTaskSeqFromBranches(dir, projectName string) int {
	out := gitOutput(dir, "branch", "--list", "ralph/"+projectName+"/*", "--sort=refname")
	if out == "" {
		return 0
	}
	seqRe := regexp.MustCompile(`/(\d+)-`)
	maxSeq := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "* ")
		line = strings.TrimPrefix(line, "+ ")
		if m := seqRe.FindStringSubmatch(line); len(m) > 1 {
			if n, err := strconv.Atoi(m[1]); err == nil && n > maxSeq {
				maxSeq = n
			}
		}
	}
	return maxSeq
}
