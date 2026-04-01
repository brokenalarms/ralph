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

// Manager methods that mirror the package-level helpers but route
// through the Manager's injected Runner.

func (m *Manager) HasDiff() bool {
	if m.gitOutput(m.WorkDir, "diff", "--stat") != "" {
		return true
	}
	return m.gitOutput(m.WorkDir, "diff", "--cached", "--stat") != ""
}

func (m *Manager) HeadRev() string {
	return m.gitOutput(m.WorkDir, "rev-parse", "HEAD")
}

func (m *Manager) HasUncommittedChanges() bool {
	return m.gitCmdErr(m.WorkDir, "diff", "--quiet") != nil ||
		m.gitCmdErr(m.WorkDir, "diff", "--cached", "--quiet") != nil
}

func (m *Manager) CommitAll(message string) {
	m.gitCmd(m.WorkDir, "add", "-A")
	_ = m.gitCmdErr(m.WorkDir, "commit", "-m", message)
}

func (m *Manager) ChangedFiles(headBefore, headAfter string) []string {
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

	add(m.gitOutput(m.WorkDir, "diff", "--name-only"))
	add(m.gitOutput(m.WorkDir, "diff", "--cached", "--name-only"))

	if headBefore != "" && headAfter != "" && headBefore != headAfter {
		add(m.gitOutput(m.WorkDir, "diff", "--name-only", headBefore+"..."+headAfter))
	}

	return result
}

func (m *Manager) DiffStatRange(from, to string) string {
	if from == "" || to == "" || from == to {
		return ""
	}
	return m.gitOutput(m.WorkDir, "diff", "--stat", from, to)
}

// WorktreeMatchesMain returns true if the worktree tree is identical to
// origin's default branch. Uses tree diff so squash-merge hash mismatches
// don't cause false negatives.
func (m *Manager) WorktreeMatchesMain() bool {
	defaultBranch := m.detectDefaultBranch()
	_ = m.gitCmdErr(m.WorkDir, "fetch", "origin", defaultBranch)
	return m.gitOutput(m.WorkDir, "diff", "--stat", "HEAD", "origin/"+defaultBranch) == ""
}

func (m *Manager) DiffFull(from, to string) string {
	return m.gitOutput(m.WorkDir, "diff", from+".."+to)
}

// ConflictDiff returns the three-way merge diff between HEAD and the base
// branch (or the default branch), showing what diverges. Used to give a
// conflict resolution agent context about the conflicting changes.
func (m *Manager) ConflictDiff() string {
	baseBranch := m.detectDefaultBranch()
	if m.PrevBranch != "" {
		baseBranch = m.PrevBranch
	}
	remote := "origin/" + baseBranch
	files := m.gitOutput(m.WorkDir, "diff", "--name-only", remote+"...HEAD")
	diff := m.gitOutput(m.WorkDir, "diff", remote+"...HEAD")
	if files == "" && diff == "" {
		return ""
	}
	return "Conflicting files:\n" + files + "\n\nDiff (ours vs base):\n" + diff
}

func (m *Manager) LogOneline(from, to string) string {
	return m.gitOutput(m.WorkDir, "log", "--oneline", from+".."+to)
}

func (m *Manager) RecentChangedFiles(n int) string {
	return m.gitOutput(m.WorkDir, "diff", "--name-only", fmt.Sprintf("HEAD~%d", n), "HEAD")
}

func (m *Manager) ListProjectBranches() []string {
	out := m.gitOutput(m.ProjectDir, "branch", "--list", BranchListPattern(), "--sort=refname")
	return parseBranchList(out)
}


func (m *Manager) ValidateRemoteBranch(ctx context.Context) error {
	branch := m.detectDefaultBranch()
	m.gitCmdCtx(ctx, m.ProjectDir, "fetch", "origin", branch)
	if !m.refExists(m.ProjectDir, "origin/"+branch) {
		return fmt.Errorf("base branch %q does not exist on remote — create it or set --base-branch", branch)
	}
	return nil
}

func (m *Manager) EnsureGitignored(entry string) {
	gitignorePath := filepath.Join(m.ProjectDir, ".gitignore")
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

	if IsGitRepo(m.ProjectDir) {
		m.gitCmd(m.ProjectDir, "add", ".gitignore")
		m.gitCmd(m.ProjectDir, "commit", "-m", "Add "+entry+" to .gitignore")
	}
}

func (m *Manager) PruneOrphanedWorktrees() {
	worktreeRoot := filepath.Join(m.RalphDir, "worktrees")
	entries, err := os.ReadDir(worktreeRoot)
	if err != nil {
		return
	}
	if len(entries) == 0 {
		return
	}

	m.gitCmd(m.ProjectDir, "worktree", "prune")

	out := m.gitOutput(m.ProjectDir, "worktree", "list", "--porcelain")
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
		if m.Logger != nil {
			m.Logger.Emit(logging.Opts{Domain: logging.Git}, "Removing orphaned worktree directory: %s", dirPath)
		}
		os.RemoveAll(dirPath)
	}
}

