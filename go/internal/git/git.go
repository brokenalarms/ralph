package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// StateStore abstracts reading and writing ralph JSON state.
type StateStore interface {
	Read(key string) (string, error)
	Write(key, value string) error
}

// Log is the logging interface used by Manager, matching the subset of
// logging.Logger that git operations need.
type Log interface {
	Log(format string, args ...any)
	Warn(format string, args ...any)
	Error(format string, args ...any)
}

// Manager handles git worktree creation, branch rotation, and renaming.
type Manager struct {
	ProjectDir     string
	RalphDir       string
	ProjectName    string
	WorkDir        string
	WorktreeBranch string
	UseWorktree    bool
	Resume         bool
	TaskSeq        int
	BranchRenamed  bool
	State          StateStore
	Logger         Log
}

// TempBranch returns the temporary branch name for the current project.
func (m *Manager) TempBranch() string {
	return "ralph/" + m.ProjectName + "/next"
}

// SetupWorktree creates (or resumes) a git worktree for isolated work.
// Mirrors lib/git.sh setup_worktree.
func (m *Manager) SetupWorktree() error {
	m.WorkDir = m.ProjectDir

	if !m.UseWorktree {
		return nil
	}

	if !isGitRepo(m.ProjectDir) {
		return fmt.Errorf("not a git repo — ralph requires git. Use --no-worktree to run without git isolation")
	}

	if m.Resume {
		if err := m.tryResumeWorktree(); err == nil {
			return nil
		}
	}

	m.ProjectName = filepath.Base(m.ProjectDir)

	today := time.Now().Format("20060102")
	runSeq := 1
	worktreeRoot := filepath.Join(m.RalphDir, "worktrees")
	if entries, err := os.ReadDir(worktreeRoot); err == nil {
		prefix := "ralph-" + today + "-"
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
				runSeq++
			}
		}
	}

	m.WorktreeBranch = m.TempBranch()
	m.WorkDir = filepath.Join(m.RalphDir, "worktrees", fmt.Sprintf("ralph-%s-%02d", today, runSeq))

	if err := os.MkdirAll(filepath.Join(m.RalphDir, "worktrees"), 0o755); err != nil {
		return fmt.Errorf("creating worktrees dir: %w", err)
	}

	// Prune stale worktree bookkeeping
	gitCmd(m.ProjectDir, "worktree", "prune")

	// Clean up temp branch if it already exists
	if err := m.cleanTempBranch(); err != nil {
		return err
	}

	defaultBranch := detectDefaultBranch(m.ProjectDir)
	gitCmd(m.ProjectDir, "fetch", "origin", defaultBranch)

	// Push main if remote is empty
	if !refExists(m.ProjectDir, "origin/"+defaultBranch) &&
		refExists(m.ProjectDir, "HEAD") &&
		remoteExists(m.ProjectDir) {
		m.Logger.Log("Pushing %s to origin (empty remote — ensures correct default branch)", defaultBranch)
		gitCmd(m.ProjectDir, "push", "-u", "origin", defaultBranch)
	}

	// Create worktree, preferring origin/default, falling back to HEAD
	if err := gitCmdErr(m.ProjectDir, "worktree", "add", "-b", m.WorktreeBranch, m.WorkDir, "origin/"+defaultBranch); err != nil {
		if err := gitCmdErr(m.ProjectDir, "worktree", "add", "-b", m.WorktreeBranch, m.WorkDir, "HEAD"); err != nil {
			return fmt.Errorf("failed to create worktree: %w", err)
		}
	}

	gitCmd(m.WorkDir, "config", "rebase.updateRefs", "true")
	m.Logger.Log("Worktree: %s (branch: %s)", m.WorkDir, m.WorktreeBranch)

	if m.State != nil {
		_ = m.State.Write("worktree_dir", m.WorkDir)
		_ = m.State.Write("worktree_branch", m.WorktreeBranch)
	}

	return nil
}

// tryResumeWorktree attempts to reuse a stored worktree from a previous run.
func (m *Manager) tryResumeWorktree() error {
	if m.State == nil {
		return fmt.Errorf("no state store")
	}
	stored, err := m.State.Read("worktree_dir")
	if err != nil || stored == "" || stored == "null" {
		return fmt.Errorf("no stored worktree")
	}
	info, err := os.Stat(stored)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("stored worktree missing")
	}

	m.WorkDir = stored
	branch, _ := m.State.Read("worktree_branch")
	m.WorktreeBranch = branch
	m.ProjectName = filepath.Base(m.ProjectDir)

	namedCount := countNamedBranches(m.ProjectDir, m.ProjectName)
	m.TaskSeq = namedCount

	m.Logger.Log("Resuming in worktree: %s (branch: %s)", m.WorkDir, m.WorktreeBranch)
	return nil
}

// cleanTempBranch removes the temp branch, force-removing a stale worktree
// if necessary. Returns an error only if the branch is checked out in a
// non-ralph worktree.
func (m *Manager) cleanTempBranch() error {
	if !refExists(m.ProjectDir, m.WorktreeBranch) {
		return nil
	}
	if err := gitCmdErr(m.ProjectDir, "branch", "-D", m.WorktreeBranch); err == nil {
		return nil
	}

	existingWt := findWorktreeForBranch(m.ProjectDir, m.WorktreeBranch)
	if existingWt != "" && strings.Contains(existingWt, "/.ralph/worktrees/") {
		m.Logger.Warn("Removing stale ralph worktree: %s", existingWt)
		gitCmd(m.ProjectDir, "worktree", "remove", "--force", existingWt)
		gitCmd(m.ProjectDir, "branch", "-D", m.WorktreeBranch)
		return nil
	}

	return fmt.Errorf("cannot delete branch '%s' — it is checked out in a non-ralph worktree: %s",
		m.WorktreeBranch, existingWt)
}

// RenameWorktreeForTheme renames the worktree directory to include a
// slugified theme description. Mirrors lib/git.sh rename_worktree_for_theme.
func (m *Manager) RenameWorktreeForTheme(themeDesc string) {
	if themeDesc == "" || m.WorkDir == m.ProjectDir {
		return
	}

	slug := Slugify(themeDesc)
	if slug == "" {
		return
	}

	today := time.Now().Format("20060102")
	newDir := filepath.Join(m.RalphDir, "worktrees", "ralph-"+today+"-"+slug)

	if m.WorkDir == newDir {
		return
	}

	// Avoid collision
	if _, err := os.Stat(newDir); err == nil {
		i := 2
		for {
			candidate := fmt.Sprintf("%s-%d", newDir, i)
			if _, err := os.Stat(candidate); err != nil {
				newDir = candidate
				break
			}
			i++
		}
	}

	if err := gitCmdErr(m.ProjectDir, "worktree", "move", m.WorkDir, newDir); err != nil {
		m.Logger.Warn("Could not rename worktree (continuing with current name)")
		return
	}

	m.WorkDir = newDir
	if m.State != nil {
		_ = m.State.Write("worktree_dir", m.WorkDir)
	}
	m.Logger.Log("Worktree renamed: %s", newDir)
}

// RenameBranchForTask renames the current branch to include a task slug.
// Each call increments TaskSeq. Only renames once per iteration (tracked by
// BranchRenamed). Mirrors lib/git.sh rename_branch_for_task.
func (m *Manager) RenameBranchForTask(taskDesc string) {
	if m.BranchRenamed || m.WorktreeBranch == "" || taskDesc == "" {
		return
	}
	if m.WorkDir == m.ProjectDir {
		return
	}

	slug := Slugify(taskDesc)
	if slug == "" {
		return
	}

	m.TaskSeq++
	newBranch := fmt.Sprintf("ralph/%s/%02d-%s", m.ProjectName, m.TaskSeq, slug)
	if err := gitCmdErr(m.WorkDir, "branch", "-m", m.WorktreeBranch, newBranch); err == nil {
		m.WorktreeBranch = newBranch
		if m.State != nil {
			_ = m.State.Write("worktree_branch", m.WorktreeBranch)
		}
		m.BranchRenamed = true
	}
}

// RotateBranch creates a fresh temp branch from the current HEAD, ready for
// the next iteration. Mirrors lib/git.sh rotate_branch.
func (m *Manager) RotateBranch() {
	if m.WorktreeBranch == "" || m.WorkDir == m.ProjectDir {
		return
	}

	m.WorktreeBranch = m.TempBranch()
	gitCmd(m.WorkDir, "branch", "-D", m.WorktreeBranch)
	if err := gitCmdErr(m.WorkDir, "checkout", "-b", m.WorktreeBranch); err == nil {
		if m.State != nil {
			_ = m.State.Write("worktree_branch", m.WorktreeBranch)
		}
		m.BranchRenamed = false
		m.Logger.Log("Branch: %s (from previous iteration)", m.WorktreeBranch)
	} else {
		m.Logger.Warn("Branch rotation failed, continuing on %s", m.WorktreeBranch)
	}
}

// RebaseOntoDefaultBranch rebases the worktree onto origin's default branch,
// detecting and skipping squash-merged branches when a naive rebase conflicts.
// Mirrors lib/git.sh rebase_onto_default_branch.
func (m *Manager) RebaseOntoDefaultBranch() error {
	defaultBranch := detectDefaultBranch(m.ProjectDir)
	gitCmd(m.WorkDir, "fetch", "origin", defaultBranch)

	// Skip if remote branch doesn't exist (e.g. repo never pushed)
	if !refExists(m.WorkDir, "origin/"+defaultBranch) {
		m.Logger.Log("No remote branch origin/%s — skipping rebase", defaultBranch)
		return nil
	}

	// Already up to date
	if gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", "HEAD", "origin/"+defaultBranch) == nil {
		m.Logger.Log("Already up to date with origin/%s", defaultBranch)
		return nil
	}

	// Try simple rebase
	if gitCmdErr(m.WorkDir, "rebase", "--update-refs", "origin/"+defaultBranch) == nil {
		m.Logger.Log("Rebased onto origin/%s", defaultBranch)
		return nil
	}

	gitCmd(m.WorkDir, "rebase", "--abort")
	m.Logger.Warn("Rebase failed, checking for squash-merged branches...")

	// Find squash-merged branches by checking if their changes are already
	// absorbed into origin/default via reverse-apply detection.
	lastMerged := m.findLastSquashMergedBranch(defaultBranch)

	if lastMerged == "" {
		m.Logger.Error("Rebase onto %s failed with real conflicts — halting", defaultBranch)
		return fmt.Errorf("rebase onto %s failed with real conflicts", defaultBranch)
	}

	m.Logger.Log("Detected squash-merged branch: %s", lastMerged)

	if err := gitCmdErr(m.WorkDir, "rebase", "--update-refs", "--onto", "origin/"+defaultBranch, lastMerged, "HEAD"); err != nil {
		gitCmd(m.WorkDir, "rebase", "--abort")
		m.Logger.Error("Rebase onto %s past squash-merged branches failed — halting", defaultBranch)
		return fmt.Errorf("rebase onto %s past squash-merged branches failed", defaultBranch)
	}

	m.Logger.Log("Rebased onto origin/%s (skipped squash-merged branches)", defaultBranch)

	// Update TaskSeq from remaining branches, then delete the merged branch
	m.TaskSeq = ParseTaskSeqFromBranches(m.ProjectDir, m.ProjectName)
	gitCmd(m.ProjectDir, "branch", "-D", lastMerged)

	return nil
}

// findLastSquashMergedBranch iterates ralph/<project>/* branches (sorted by
// refname, skipping */next) and returns the last one whose changes are already
// absorbed into origin/defaultBranch. Detection uses a temporary git index:
// read-tree origin/default, then reverse-apply the branch's diff. If that
// succeeds, main already contains the branch's changes (squash-merged).
func (m *Manager) findLastSquashMergedBranch(defaultBranch string) string {
	branches := listProjectBranches(m.ProjectDir, m.ProjectName)
	lastMerged := ""

	for _, branch := range branches {
		if strings.HasSuffix(branch, "/next") {
			continue
		}

		mergeBase := gitOutput(m.WorkDir, "merge-base", "origin/"+defaultBranch, branch)
		if mergeBase == "" {
			continue
		}

		// Skip branches with no changes
		branchFiles := gitOutput(m.WorkDir, "diff", "--name-only", mergeBase, branch)
		if branchFiles == "" {
			continue
		}

		if m.isSquashMerged(defaultBranch, mergeBase, branch) {
			lastMerged = branch
		}
	}

	return lastMerged
}

// isSquashMerged checks whether branch's changes (relative to mergeBase) are
// already present in origin/defaultBranch by reverse-applying the diff against
// a temporary index loaded with origin/default's tree.
func (m *Manager) isSquashMerged(defaultBranch, mergeBase, branch string) bool {
	tmpIndex, err := os.CreateTemp("", "ralph_squash_check.*")
	if err != nil {
		return false
	}
	tmpPath := tmpIndex.Name()
	tmpIndex.Close()
	defer os.Remove(tmpPath)

	// Load origin/default's tree into the temp index
	readTreeCmd := exec.Command("git", "-C", m.WorkDir, "read-tree", "origin/"+defaultBranch)
	readTreeCmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+tmpPath)
	if readTreeCmd.Run() != nil {
		return false
	}

	// Generate the diff between merge-base and branch
	diffCmd := exec.Command("git", "-C", m.WorkDir, "diff", mergeBase, branch)
	diffOut, err := diffCmd.Output()
	if err != nil || len(diffOut) == 0 {
		return false
	}

	// Reverse-apply the diff against the temp index — if it succeeds,
	// origin/default already contains these changes.
	applyCmd := exec.Command("git", "-C", m.WorkDir, "apply", "--cached", "--reverse", "--check", "-C0")
	applyCmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+tmpPath)
	applyCmd.Stdin = strings.NewReader(string(diffOut))
	return applyCmd.Run() == nil
}

// listProjectBranches returns ralph/<project>/* branches sorted by refname.
func listProjectBranches(dir, projectName string) []string {
	out := gitOutput(dir, "branch", "--list", "ralph/"+projectName+"/*", "--sort=refname")
	if out == "" {
		return nil
	}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "* ")
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches
}

// --- Slugify ---

var (
	nonAlnum  = regexp.MustCompile(`[^a-z0-9]+`)
	dashTrim  = regexp.MustCompile(`^-|-$`)
)

// Slugify converts a description to a URL/branch-safe slug, matching
// ralph.sh's slugify function. Max 50 characters.
func Slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlnum.ReplaceAllString(s, "-")
	s = dashTrim.ReplaceAllString(s, "")
	if len(s) > 50 {
		s = s[:50]
		// Trim trailing dash after truncation
		s = strings.TrimRight(s, "-")
	}
	return s
}

// --- Git helpers ---

func gitCmd(dir string, args ...string) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	_ = cmd.Run()
}

func gitCmdErr(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func isGitRepo(dir string) bool {
	return gitCmdErr(dir, "rev-parse", "--git-dir") == nil
}

func refExists(dir, ref string) bool {
	return gitCmdErr(dir, "rev-parse", "--verify", ref) == nil
}

func remoteExists(dir string) bool {
	return gitOutput(dir, "remote", "get-url", "origin") != ""
}

func detectDefaultBranch(dir string) string {
	ref := gitOutput(dir, "symbolic-ref", "refs/remotes/origin/HEAD")
	if ref != "" {
		return strings.TrimPrefix(ref, "refs/remotes/origin/")
	}
	return "main"
}

// countNamedBranches counts ralph/<project>/* branches (used for task seq on resume).
func countNamedBranches(dir, projectName string) int {
	out := gitOutput(dir, "branch", "--list", "ralph/"+projectName+"/*")
	if out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

// findWorktreeForBranch finds the worktree path that has the given branch
// checked out, using git worktree list --porcelain.
func findWorktreeForBranch(dir, branch string) string {
	out := gitOutput(dir, "worktree", "list", "--porcelain")
	if out == "" {
		return ""
	}

	target := "branch refs/heads/" + branch
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == target {
			// Walk backwards to find the "worktree" line
			for j := i - 1; j >= 0; j-- {
				if strings.HasPrefix(lines[j], "worktree ") {
					return strings.TrimPrefix(lines[j], "worktree ")
				}
			}
		}
	}
	return ""
}

// CountNamedBranchesForSeq returns the count to seed TaskSeq from tests.
func CountNamedBranchesForSeq(dir, projectName string) int {
	out := gitOutput(dir, "branch", "--list", "ralph/"+projectName+"/*")
	if out == "" {
		return 0
	}
	count := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			count++
		}
	}
	return count
}

// DefaultBranch returns the detected default branch for tests.
func DefaultBranch(dir string) string {
	return detectDefaultBranch(dir)
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
		if m := seqRe.FindStringSubmatch(line); len(m) > 1 {
			if n, err := strconv.Atoi(m[1]); err == nil && n > maxSeq {
				maxSeq = n
			}
		}
	}
	return maxSeq
}
