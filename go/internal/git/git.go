package git

import (
	"context"
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
	BranchRenamed bool
	BaseBranch    string
	MergeAdmin    bool
	GitHub         GitHub
	Runner         Runner
	State          StateStore
	Logger         Log
}

func (m *Manager) gh() GitHub {
	if m.GitHub != nil {
		return m.GitHub
	}
	return &ghCLI{}
}

// TempBranch returns the temporary branch name for the current project.
func (m *Manager) TempBranch() string {
	return "ralph/" + m.ProjectName + "/next"
}

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

// SetupWorktree creates (or resumes) a git worktree for isolated work.
// Mirrors lib/git.sh setup_worktree.
func (m *Manager) SetupWorktree(ctx context.Context) error {
	m.WorkDir = m.ProjectDir

	if !m.UseWorktree {
		return nil
	}

	if !IsGitRepo(m.ProjectDir) {
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
	m.gitCmd(m.ProjectDir, "worktree", "prune")

	// Remove leftover worktree directory from a previous run (git prune
	// cleans the bookkeeping but leaves the directory on disk).
	if _, err := os.Stat(m.WorkDir); err == nil {
		os.RemoveAll(m.WorkDir)
	}

	// Clean up temp branch if it already exists
	if err := m.cleanTempBranch(); err != nil {
		return err
	}

	defaultBranch := m.detectDefaultBranch()
	m.gitCmdCtx(ctx, m.ProjectDir, "fetch", "origin", defaultBranch)

	// Push main if remote is empty
	if !m.refExists(m.ProjectDir, "origin/"+defaultBranch) &&
		m.refExists(m.ProjectDir, "HEAD") &&
		m.remoteExists() {
		m.Logger.Log("Pushing %s to origin (empty remote — ensures correct default branch)", defaultBranch)
		m.gitCmdCtx(ctx, m.ProjectDir, "push", "-u", "origin", defaultBranch)
	}

	// Create worktree, preferring origin/default, falling back to HEAD
	if err := m.gitCmdErr(m.ProjectDir, "worktree", "add", "-b", m.WorktreeBranch, m.WorkDir, "origin/"+defaultBranch); err != nil {
		if err := m.gitCmdErr(m.ProjectDir, "worktree", "add", "-b", m.WorktreeBranch, m.WorkDir, "HEAD"); err != nil {
			return fmt.Errorf("failed to create worktree: %w", err)
		}
	}

	m.gitCmd(m.WorkDir, "config", "rebase.updateRefs", "true")
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

	if seqStr, _ := m.State.Read("task_seq"); seqStr != "" {
		if n, err := strconv.Atoi(seqStr); err == nil {
			m.TaskSeq = n
		}
	}

	m.Logger.Log("Resuming worktree: %s", m.WorkDir)

	defaultBranch := m.detectDefaultBranch()
	if err := m.gitCmdErr(m.WorkDir, "fetch", "origin", defaultBranch); err != nil {
		m.Logger.Warn("Failed to fetch origin/%s on resume: %v", defaultBranch, err)
	}

	if m.resumedBranchIsStale(defaultBranch) {
		m.Logger.Log("Stale branch detected on resume — resetting to origin/%s", defaultBranch)
		return m.resetResumedWorktree(defaultBranch)
	}

	m.EnsureUpToDate(context.Background())
	return nil
}

// EnsureUpToDate fetches the latest base branch, stashes any uncommitted
// changes, rebases onto origin, and reapplies the stash. This is the single
// sync point that all git operations (push, merge, resume) go through to
// guarantee the worktree has the latest upstream changes.
func (m *Manager) EnsureUpToDate(ctx context.Context) {
	if m.WorkDir == "" || m.WorkDir == m.ProjectDir {
		return
	}

	defaultBranch := m.detectDefaultBranch()

	if err := m.gitCmdErrCtx(ctx, m.WorkDir, "fetch", "origin", defaultBranch); err != nil {
		m.Logger.Warn("Failed to fetch origin/%s: %v", defaultBranch, err)
		return
	}

	if !m.refExists(m.WorkDir, "origin/"+defaultBranch) {
		return
	}

	// Already up to date?
	if m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", "origin/"+defaultBranch, "HEAD") == nil {
		return
	}

	dirty := gitCmdErr(m.WorkDir, "diff", "--quiet") != nil ||
		m.gitCmdErr(m.WorkDir, "diff", "--cached", "--quiet") != nil
	if dirty {
		m.Logger.Log("Stashing uncommitted changes before rebase...")
		m.gitCmd(m.WorkDir, "stash", "push", "-m", "ralph-autostash")
	}

	if err := m.gitCmdErrCtx(ctx, m.WorkDir, "rebase", "origin/"+defaultBranch); err != nil {
		if resolved := m.autoResolveAndContinue(ctx, defaultBranch); !resolved {
			m.Logger.Warn("Rebase onto %s failed with unresolvable conflicts — aborting", defaultBranch)
			m.gitCmd(m.WorkDir, "rebase", "--abort")
		}
	}

	if dirty {
		if err := m.gitCmdErr(m.WorkDir, "stash", "pop"); err != nil {
			m.Logger.Warn("Stash pop conflict — committing stash as WIP")
			m.gitCmd(m.WorkDir, "checkout", "--theirs", ".")
			m.gitCmd(m.WorkDir, "add", "-A")
			m.gitCmd(m.WorkDir, "commit", "-m", "WIP: reapply stashed changes after rebase (may need review)")
		}
	}
}

// cleanTempBranch removes the temp branch, force-removing a stale worktree
// if necessary. Returns an error only if the branch is checked out in a
// non-ralph worktree.
func (m *Manager) cleanTempBranch() error {
	if !m.refExists(m.ProjectDir, m.WorktreeBranch) {
		return nil
	}
	if err := m.gitCmdErr(m.ProjectDir, "branch", "-D", m.WorktreeBranch); err == nil {
		return nil
	}

	existingWt := m.findWorktreeForBranch(m.ProjectDir, m.WorktreeBranch)
	if existingWt != "" && strings.Contains(existingWt, "/.ralph/worktrees/") {
		m.Logger.Warn("Removing stale ralph worktree: %s", existingWt)
		m.gitCmd(m.ProjectDir, "worktree", "remove", "--force", existingWt)
		m.gitCmd(m.ProjectDir, "branch", "-D", m.WorktreeBranch)
		return nil
	}

	return fmt.Errorf("cannot delete branch '%s' — it is checked out in a non-ralph worktree: %s",
		m.WorktreeBranch, existingWt)
}

// RenameBranchForTask renames the current branch to include a task slug.
// Each call increments TaskSeq. Only renames once per iteration (tracked by
// BranchRenamed). When taskID is provided, it is included in the branch name
// for traceability.
func (m *Manager) RenameBranchForTask(taskDesc, taskID string) {
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
	var newBranch string
	if taskID != "" {
		newBranch = fmt.Sprintf("ralph/%s/%02d-%s-%s", m.ProjectName, m.TaskSeq, taskID, slug)
	} else {
		newBranch = fmt.Sprintf("ralph/%s/%02d-%s", m.ProjectName, m.TaskSeq, slug)
	}
	if err := m.gitCmdErr(m.WorkDir, "branch", "-m", m.WorktreeBranch, newBranch); err == nil {
		m.WorktreeBranch = newBranch
		if m.State != nil {
			_ = m.State.Write("worktree_branch", m.WorktreeBranch)
			_ = m.State.Write("task_seq", fmt.Sprintf("%d", m.TaskSeq))
		}
		m.BranchRenamed = true
	}
}

// RotateBranch creates a fresh temp branch from the current HEAD, ready for
// the next iteration.
func (m *Manager) RotateBranch() {
	if m.WorktreeBranch == "" || m.WorkDir == m.ProjectDir {
		return
	}

	newBranch := m.TempBranch()

	// Already on the temp branch (e.g. after PostMergeReset)
	if m.WorktreeBranch == newBranch {
		m.BranchRenamed = false
		return
	}

	if err := m.gitCmdErr(m.WorkDir, "checkout", "-B", newBranch); err == nil {
		m.WorktreeBranch = newBranch
		if m.State != nil {
			_ = m.State.Write("worktree_branch", m.WorktreeBranch)
		}
		m.BranchRenamed = false
		m.Logger.Log("Branch: %s (from previous iteration)", m.WorktreeBranch)
	} else {
		m.Logger.Warn("Branch rotation failed, continuing on %s", m.WorktreeBranch)
	}
}

// RemoveWorktree force-removes a worktree and deletes its branch.
func (m *Manager) RemoveWorktree() {
	m.gitCmd(m.ProjectDir, "worktree", "remove", "--force", m.WorkDir)
	m.gitCmd(m.ProjectDir, "branch", "-D", m.WorktreeBranch)
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


var (
	nonAlnum  = regexp.MustCompile(`[^a-z0-9]+`)
	dashTrim  = regexp.MustCompile(`^-|-$`)
)

// Slugify converts a description to a URL/branch-safe slug.
// Limited to 4 words and 50 characters to keep branch names short.
func Slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlnum.ReplaceAllString(s, "-")
	s = dashTrim.ReplaceAllString(s, "")

	// Limit to 4 words
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


func gitCmd(dir string, args ...string) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	_ = cmd.Run()
}

func gitCmdCtx(ctx context.Context, dir string, args ...string) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
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

func gitCmdErrCtx(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
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

// IsGitRepo returns true if dir is inside a git repository.
func IsGitRepo(dir string) bool {
	return gitCmdErr(dir, "rev-parse", "--git-dir") == nil
}

func refExists(dir, ref string) bool {
	return gitCmdErr(dir, "rev-parse", "--verify", ref) == nil
}

func remoteExists(dir string) bool {
	return gitOutput(dir, "remote", "get-url", "origin") != ""
}


// findWorktreeForBranch finds the worktree path that has the given branch
// checked out, using git worktree list --porcelain. This standalone version
// uses the default exec runner; Manager methods use m.findWorktreeForBranch
// which routes through the injected Runner.
func findWorktreeForBranch(dir, branch string) string {
	out := gitOutput(dir, "worktree", "list", "--porcelain")
	return parseWorktreeForBranch(out, branch)
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

// RecentChangedFiles returns files changed in the last N commits.
func RecentChangedFiles(dir string, n int) string {
	return gitOutput(dir, "diff", "--name-only", fmt.Sprintf("HEAD~%d", n), "HEAD")
}

// TagTaskStart creates a lightweight git tag marking the start of a task iteration.
// The tag name is task/{taskID}/start when a backend ID is available,
// or task/{seq}-{slug}/start derived from the current branch name.
func (m *Manager) TagTaskStart(taskID string) {
	tag := m.taskTag(taskID, "start")
	if tag == "" {
		return
	}
	// Force-create to handle reruns of the same task
	if err := m.gitCmdErr(m.WorkDir, "tag", "-f", tag); err == nil {
		m.Logger.Log("Tag: %s", tag)
	}
}

// TagTaskEnd creates a lightweight git tag marking the end of a task iteration.
func (m *Manager) TagTaskEnd(taskID string) {
	tag := m.taskTag(taskID, "end")
	if tag == "" {
		return
	}
	if err := m.gitCmdErr(m.WorkDir, "tag", "-f", tag); err == nil {
		m.Logger.Log("Tag: %s", tag)
	}
}

// taskTag builds a tag name like task/{id}/{suffix}. Returns empty
// string if there's not enough info to build a meaningful tag.
func (m *Manager) taskTag(taskID, suffix string) string {
	if m.WorkDir == "" || m.WorkDir == m.ProjectDir {
		return ""
	}
	if taskID != "" {
		return fmt.Sprintf("task/%s/%s", taskID, suffix)
	}
	// Fall back to seq-slug extracted from the current branch name
	seqSlug := extractSeqSlug(m.WorktreeBranch)
	if seqSlug == "" {
		return ""
	}
	return fmt.Sprintf("task/%s/%s", seqSlug, suffix)
}

// extractSeqSlug pulls the "NN-slug" portion from a branch like
// "ralph/project/01-my-task".
func extractSeqSlug(branch string) string {
	parts := strings.SplitN(branch, "/", 3)
	if len(parts) < 3 {
		return ""
	}
	seg := parts[2]
	if seg == "next" || seg == "" {
		return ""
	}
	return seg
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
			logger.Log("Removing orphaned worktree directory: %s", dirPath)
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
