package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
	dashTrim = regexp.MustCompile(`^-|-$`)
)

const branchPrefix = "ralph/"

// BranchName returns the canonical branch name for a task.
// With a taskID: ralph/<taskID>-<slug>
// Without:      ralph/<project>/<slug>
func BranchName(projectName, taskID, slug string) string {
	if taskID != "" {
		return branchPrefix + taskID + "-" + slug
	}
	return branchPrefix + projectName + "/" + slug
}

// WipBranchName returns the placeholder branch used before a task is assigned.
func WipBranchName(projectName string) string {
	return branchPrefix + projectName + "/wip"
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

func (m *Manager) DiffFull(from, to string) string {
	return m.gitOutput(m.WorkDir, "diff", from+".."+to)
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
			m.Logger.Log("git", "Removing orphaned worktree directory: %s", dirPath)
		}
		os.RemoveAll(dirPath)
	}
}

