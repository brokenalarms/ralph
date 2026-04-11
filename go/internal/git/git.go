package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// StateStore abstracts reading and writing ralph JSON state. Defined locally
// in the git package so git holds no peer-module references — production
// passes *state.Store; tests pass stub stores.
type StateStore interface {
	Read(key string) (string, error)
	Write(key, value string) error
}

// Log is the logging interface used by Repo. Defined locally for the same
// reason as StateStore.
type Log interface {
	Emit(o logging.Opts, format string, args ...any)
	// EmitInPlace writes the first segment of an in-place line in append mode —
	// no carriage return, no trailing newline. Writes to stdout and the log file.
	EmitInPlace(o logging.Opts, format string, args ...any)
	// EmitAppend appends raw text to the current in-place line — no tag, no
	// carriage return, no trailing newline. Writes to stdout and the log file.
	EmitAppend(format string, args ...any)
	// EmitFinalInPlace closes the current in-place line with a newline.
	EmitFinalInPlace()
}

// PrePusher is git's local interface for the pre-push hook. The
// orchestrator (cmd/ralph) provides an implementation that runs the
// project's compile check before each push. Defined here as an interface
// so git holds no module reference and no function field — production
// wires a real implementation; tests provide stubs. The workDir argument
// is the current worktree directory at push time, so implementations
// don't need a back-reference to *Repo.
type PrePusher interface {
	PrePush(ctx context.Context, workDir string) error
}

// Config bundles the construction-time inputs for git.New. Pure data plus
// locally-defined interface dependencies (StateStore, Log, GitHub,
// PrePusher) — no peer-module references, no function fields. The
// orchestrator (cmd/ralph) builds one of these once and passes it in;
// nothing on Repo is mutated after construction.
type Config struct {
	// ProjectDir is the original project root. WorkDir starts equal to it
	// and may later become a worktree subdirectory after SetupWorktree.
	// When ProjectDir is empty, it defaults to WorkDir.
	ProjectDir                  string
	WorkDir                     string
	RalphDir                    string
	BaseBranch                  string
	Resume                      bool
	GitHub                      GitHub    // optional; nil falls back to ghCLI
	State                       StateStore
	Logger                      Log
	PrePush                     PrePusher // optional; nil disables the pre-push hook
	CIPollTimeout               time.Duration
	CopilotGatedTimeout         time.Duration
	CopilotOpportunisticTimeout time.Duration
	CodeRabbitTimeout           time.Duration
}

// Repo handles git worktree creation, branch naming, and sync. The fields
// below split into two groups:
//
//   - Construction-time inputs: set once via New(Config{...}), never mutated
//     externally. These are unexported.
//   - Runtime state: ProjectName, WorkDir, WorktreeBranch, PrevBranch, etc.
//     are still exported because the package's own helpers and tests mutate
//     them as worktree setup progresses. Cleanup of those is a separate
//     commit.
type Repo struct {
	// Runtime state — still exported pending a separate cleanup commit.
	ProjectDir     string
	ProjectName    string
	WorkDir        string
	WorktreeBranch string
	PrevBranch     string
	BranchRenamed  bool

	LocalTestsPassed bool
	KnownPRNumber    int
	Runner           Runner

	// Construction-time inputs — set once via Config, never mutated.
	ralphDir                    string
	baseBranch                  string
	resume                      bool
	github                      GitHub
	state                       StateStore
	logger                      Log
	prePush                     PrePusher
	ciPollTimeout               time.Duration
	copilotGatedTimeout         time.Duration
	copilotOpportunisticTimeout time.Duration
	codeRabbitTimeout           time.Duration
}

// New creates a Repo from a Config. The orchestrator (cmd/ralph) calls
// this once with all construction-time inputs; no field is mutated
// externally afterward. Pass a non-nil cfg.GitHub to inject a stub for
// testing; nil falls back to the default gh CLI wrapper.
func New(cfg Config) *Repo {
	projectDir := cfg.ProjectDir
	if projectDir == "" {
		projectDir = cfg.WorkDir
	}
	return &Repo{
		ProjectDir:                  projectDir,
		WorkDir:                     cfg.WorkDir,
		ralphDir:                    cfg.RalphDir,
		baseBranch:                  cfg.BaseBranch,
		resume:                      cfg.Resume,
		github:                      cfg.GitHub,
		state:                       cfg.State,
		logger:                      cfg.Logger,
		prePush:                     cfg.PrePush,
		ciPollTimeout:               cfg.CIPollTimeout,
		copilotGatedTimeout:         cfg.CopilotGatedTimeout,
		copilotOpportunisticTimeout: cfg.CopilotOpportunisticTimeout,
		codeRabbitTimeout:           cfg.CodeRabbitTimeout,
	}
}

// GH returns the GitHub interface, using the injected stub if set (tests)
// or a live ghCLI wrapper in production.
func (m *Repo) GH() GitHub {
	if m.github != nil {
		return m.github
	}
	return &ghCLI{
		CopilotGatedTimeout:         m.copilotGatedTimeout,
		CopilotOpportunisticTimeout: m.copilotOpportunisticTimeout,
		CodeRabbitTimeout:           m.codeRabbitTimeout,
	}
}

func (m *Repo) gh() GitHub {
	return m.GH()
}

func (m *Repo) SetLocalTestsPassed(v bool) {
	m.LocalTestsPassed = v
}

// SetKnownPRNumber stores a PR number discovered earlier (e.g. during Ship)
// so that AutoMergeCurrentBranch and FlushUnpushedWork can skip the FindOpenPR lookup.
func (m *Repo) SetKnownPRNumber(n int) {
	m.KnownPRNumber = n
}


// SetupWorktree creates (or resumes) a git worktree for isolated work.
func (m *Repo) SetupWorktree(ctx context.Context) error {
	m.WorkDir = m.ProjectDir

	if !IsGitRepo(m.ProjectDir) {
		return fmt.Errorf("not a git repo — ralph requires git")
	}

	if m.resume {
		if err := m.tryResumeWorktree(); err == nil {
			return nil
		}
	}

	m.ProjectName = filepath.Base(m.ProjectDir)

	today := time.Now().Format("20060102")
	runSeq := 1
	worktreeRoot := filepath.Join(m.ralphDir, "worktrees")
	if entries, err := os.ReadDir(worktreeRoot); err == nil {
		prefix := "ralph-" + today + "-"
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
				runSeq++
			}
		}
	}

	// Use a placeholder branch name; RenameBranchForTask will rename it
	// to a proper task branch before the first commit.
	m.WorktreeBranch = WipBranchName()
	m.WorkDir = filepath.Join(m.ralphDir, "worktrees", fmt.Sprintf("ralph-%s-%02d", today, runSeq))

	if err := os.MkdirAll(filepath.Join(m.ralphDir, "worktrees"), 0o755); err != nil {
		return fmt.Errorf("creating worktrees dir: %w", err)
	}

	m.gitCmd(m.ProjectDir, "worktree", "prune")

	if _, err := os.Stat(m.WorkDir); err == nil {
		os.RemoveAll(m.WorkDir)
	}

	// Clean up leftover wip branch from a previous run.
	if m.refExists(m.ProjectDir, m.WorktreeBranch) {
		if wt := m.findWorktreeForBranch(m.ProjectDir, m.WorktreeBranch); wt != "" && strings.Contains(wt, "/.ralph/worktrees/") {
			m.gitCmd(m.ProjectDir, "worktree", "remove", "--force", wt)
		}
		_ = m.gitCmdErr(m.ProjectDir, "branch", "-D", m.WorktreeBranch)
	}

	defaultBranch := m.detectDefaultBranch()
	m.gitCmdCtx(ctx, m.ProjectDir, "fetch", "origin", defaultBranch)

	if !m.refExists(m.ProjectDir, "origin/"+defaultBranch) &&
		m.refExists(m.ProjectDir, "HEAD") &&
		m.remoteExists() {
		m.logger.Emit(logging.Opts{Domain: logging.Git}, "Pushing %s to origin (empty remote — ensures correct default branch)", defaultBranch)
		m.gitCmdCtx(ctx, m.ProjectDir, "push", "-u", "origin", defaultBranch)
	}

	if err := m.gitCmdErr(m.ProjectDir, "worktree", "add", "-b", m.WorktreeBranch, m.WorkDir, "origin/"+defaultBranch); err != nil {
		if err := m.gitCmdErr(m.ProjectDir, "worktree", "add", "-b", m.WorktreeBranch, m.WorkDir, "HEAD"); err != nil {
			return fmt.Errorf("failed to create worktree: %w", err)
		}
	}

	m.gitCmd(m.WorkDir, "config", "rebase.updateRefs", "true")
	m.logger.Emit(logging.Opts{Domain: logging.Git}, "Worktree: %s (branch: %s)", m.WorkDir, m.WorktreeBranch)

	if m.state != nil {
		_ = m.state.Write("worktree_dir", m.WorkDir)
		_ = m.state.Write("worktree_branch", m.WorktreeBranch)
	}

	return nil
}

// tryResumeWorktree attempts to reuse a stored worktree from a previous run.
// Only restores the worktree path and state — no rebase or reset. The actual
// branch setup happens later in checkoutExistingBranch once the task is known
// and the correct base branch can be determined.
func (m *Repo) tryResumeWorktree() error {
	if m.state == nil {
		return fmt.Errorf("no state store")
	}
	stored, err := m.state.Read("worktree_dir")
	if err != nil || stored == "" || stored == "null" {
		return fmt.Errorf("no stored worktree")
	}
	info, err := os.Stat(stored)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("stored worktree missing")
	}

	m.WorkDir = stored
	branch, _ := m.state.Read("worktree_branch")
	m.WorktreeBranch = branch
	m.ProjectName = filepath.Base(m.ProjectDir)
	if renamed, _ := m.state.Read("branch_renamed"); renamed == "true" {
		m.BranchRenamed = true
	}

	m.logger.Emit(logging.Opts{Domain: logging.Git}, "Resuming worktree: %s", m.WorkDir)
	return nil
}

// withStash stashes any uncommitted changes, runs fn, then reapplies the stash.
// Used by EnsureUpToDate and RebaseOntoDefaultBranch to avoid duplicating
// the stash/pop logic.
func (m *Repo) withStash(stashMsg string, fn func()) {
	dirty := m.gitCmdErr(m.WorkDir, "diff", "--quiet") != nil ||
		m.gitCmdErr(m.WorkDir, "diff", "--cached", "--quiet") != nil
	if dirty {
		m.logger.Emit(logging.Opts{Domain: logging.Git}, "Stashing uncommitted changes before rebase...")
		m.gitCmd(m.WorkDir, "stash", "push", "-m", stashMsg)
	}

	fn()

	if dirty {
		if err := m.gitCmdErr(m.WorkDir, "stash", "pop"); err != nil {
			m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Stash pop conflict — committing stash as WIP")
			m.gitCmd(m.WorkDir, "checkout", "--theirs", ".")
			m.gitCmd(m.WorkDir, "add", "-A")
			m.gitCmd(m.WorkDir, "commit", "-m", "WIP: reapply stashed changes after rebase (may need review)")
		} else {
			m.logger.Emit(logging.Opts{Domain: logging.Git}, "Re-applied stashed changes")
		}
	}
}

// EnsureUpToDate fetches the latest base branch, stashes any uncommitted
// changes, rebases onto origin, and reapplies the stash. If rebase fails
// after auto-resolve, it aborts and returns an error — the caller decides
// what to do (e.g. push anyway and let GitHub handle merge conflicts).
func (m *Repo) EnsureUpToDate(ctx context.Context) error {
	if m.WorkDir == "" || m.WorkDir == m.ProjectDir {
		return nil
	}

	// Rebase onto stack head if set, otherwise the default branch.
	baseBranch := m.detectDefaultBranch()
	if m.PrevBranch != "" {
		baseBranch = m.PrevBranch
	}

	if err := m.gitCmdErrCtx(ctx, m.WorkDir, "fetch", "origin", baseBranch); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if m.PrevBranch != "" {
			// Stack head branch missing from remote — likely merged and deleted.
			// Fall back to the default branch silently.
			m.SetPrevBranch("")
			baseBranch = m.detectDefaultBranch()
			if err2 := m.gitCmdErrCtx(ctx, m.WorkDir, "fetch", "origin", baseBranch); err2 != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to fetch origin/%s: %v", baseBranch, err2)
				return nil
			}
		} else {
			m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to fetch origin/%s: %v", baseBranch, err)
			return nil
		}
	}

	if !m.refExists(m.WorkDir, "origin/"+baseBranch) {
		return nil
	}

	if m.gitCmdErr(m.WorkDir, "merge-base", "--is-ancestor", "origin/"+baseBranch, "HEAD") == nil {
		m.logger.Emit(logging.Opts{Domain: logging.Git, Branch: baseBranch}, "Already up to date with origin/%s", baseBranch)
		return nil
	}

	// No local commits ahead of base → safe to force-reset (fresh start).
	localCommits := m.gitOutput(m.WorkDir, "rev-list", "--count", "origin/"+baseBranch+"..HEAD")
	if localCommits == "" || localCommits == "0" {
		m.logger.Emit(logging.Opts{Domain: logging.Git, Branch: baseBranch}, "Resetting to origin/%s (no local work)", baseBranch)
		m.gitCmd(m.WorkDir, "reset", "--hard", "origin/"+baseBranch)
		return nil
	}

	// Local commits exist — try to rebase them onto latest base.
	var result error
	m.withStash("ralph-autostash", func() {
		if ctx.Err() != nil {
			result = ctx.Err()
			return
		}

		// 1. Fast-forward rebase
		if m.tryRebase(ctx, baseBranch) {
			return
		}

		// 2. Auto-resolve mechanical conflicts
		if m.tryAutoResolve(ctx, baseBranch) {
			return
		}

		// Stack diverges — abort rebase, keep local commits, let merge handle it.
		m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Rebase conflict with local work — stack diverged, continuing")
		m.gitCmd(m.WorkDir, "rebase", "--abort")
		result = nil // not an error — diverged stack is expected
	})
	return result
}

func (m *Repo) tryRebase(ctx context.Context, defaultBranch string) bool {
	if m.gitCmdErrCtx(ctx, m.WorkDir, "rebase", "--update-refs", "origin/"+defaultBranch) == nil {
		m.logger.Emit(logging.Opts{Domain: logging.Git, Branch: defaultBranch}, "Rebased onto origin/%s", defaultBranch)
		return true
	}
	return false
}

func (m *Repo) tryAutoResolve(ctx context.Context, defaultBranch string) bool {
	if m.autoResolveAndContinue(ctx, defaultBranch) {
		m.logger.Emit(logging.Opts{Domain: logging.Git, Branch: defaultBranch}, "Rebased onto origin/%s (auto-resolved)", defaultBranch)
		return true
	}
	m.gitCmd(m.WorkDir, "rebase", "--abort")
	return false
}


// RenameBranchForTask renames the current branch to include a task slug.
// Records the previous branch name for stacked PR targeting.
// Only renames once per task (tracked by BranchRenamed).
// Returns an error if the rename fails — callers must abort the iteration.
func (m *Repo) RenameBranchForTask(taskDesc, taskID string) error {
	if m.BranchRenamed || m.WorktreeBranch == "" || taskDesc == "" {
		return nil
	}
	if m.WorkDir == m.ProjectDir {
		return nil
	}

	slug := Slugify(taskDesc)
	if slug == "" {
		return nil
	}

	newBranch := BranchName(taskID, slug)
	if err := m.gitCmdErr(m.WorkDir, "branch", "-m", m.WorktreeBranch, newBranch); err != nil {
		_ = m.gitCmdErr(m.WorkDir, "branch", "-D", newBranch)
		if retryErr := m.gitCmdErr(m.WorkDir, "branch", "-m", m.WorktreeBranch, newBranch); retryErr != nil {
			return fmt.Errorf("rename branch %s → %s: %w", m.WorktreeBranch, newBranch, retryErr)
		}
	}
	m.WorktreeBranch = newBranch
	m.BranchRenamed = true
	if m.state != nil {
		_ = m.state.Write("worktree_branch", m.WorktreeBranch)
		_ = m.state.Write("branch_renamed", "true")
	}
	return nil
}

// RenameBranchTo renames the current worktree branch to a specific name.
// Used when the bead already has a stored branch name from a previous run.
func (m *Repo) RenameBranchTo(name string) {
	if m.BranchRenamed || m.WorktreeBranch == "" || name == "" {
		return
	}
	if m.WorktreeBranch == name {
		m.BranchRenamed = true
		if m.state != nil {
			_ = m.state.Write("branch_renamed", "true")
		}
		return
	}
	if err := m.gitCmdErr(m.WorkDir, "branch", "-m", m.WorktreeBranch, name); err != nil {
		// Target branch may exist locally as a stale leftover. Delete it
		// and retry — the local branch is expendable since work is on the remote.
		_ = m.gitCmdErr(m.WorkDir, "branch", "-D", name)
		if retryErr := m.gitCmdErr(m.WorkDir, "branch", "-m", m.WorktreeBranch, name); retryErr != nil {
			m.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to rename branch %s → %s: %v", m.WorktreeBranch, name, retryErr)
			return
		}
	}
	m.WorktreeBranch = name
	m.BranchRenamed = true
	if m.state != nil {
		_ = m.state.Write("worktree_branch", m.WorktreeBranch)
		_ = m.state.Write("branch_renamed", "true")
	}
}

// ResetToDefaultBranch resets the worktree to origin's default branch.
// Used on resume when no stack exists — stale local commits are discarded.
// No-ops silently when the worktree is already at the target ref.
func (m *Repo) ResetToDefaultBranch() {
	defaultBranch := m.detectDefaultBranch()
	_ = m.gitCmdErr(m.WorkDir, "fetch", "origin", defaultBranch)
	target := "origin/" + defaultBranch
	if m.refExists(m.WorkDir, target) && m.gitOutput(m.WorkDir, "rev-parse", "HEAD") == m.gitOutput(m.WorkDir, "rev-parse", target) {
		return
	}
	m.gitCmd(m.WorkDir, "reset", "--hard", target)
	m.BranchRenamed = false
	if m.state != nil {
		_ = m.state.Write("branch_renamed", "false")
	}
	m.logger.Emit(logging.Opts{Domain: logging.Git}, "Reset worktree to %s", target)
}

// SetPrevBranch sets the previous branch for stacked PR targeting and
// persists it to state so it survives process restarts.
func (m *Repo) SetPrevBranch(branch string) {
	m.PrevBranch = branch
	if m.state != nil {
		_ = m.state.Write("prev_branch", branch)
	}
}

// PrepareForNextTask creates a fresh wip branch from HEAD so the next task
// gets its own branch. RenameBranchForTask will rename it to a task-specific
// name before the first commit. Uncommitted changes are discarded only when
// switching to a different task; resuming the same task preserves in-progress work.
func (m *Repo) PrepareForNextTask(nextTaskID string) {
	m.BranchRenamed = false
	if m.state != nil {
		_ = m.state.Write("branch_renamed", "false")
	}

	if m.WorkDir == m.ProjectDir || m.WorktreeBranch == "" {
		return
	}

	lastTaskID := ""
	if m.state != nil {
		lastTaskID, _ = m.state.Read("last_task_id")
	}
	if nextTaskID == "" || lastTaskID == "" || nextTaskID != lastTaskID {
		m.gitCmdErr(m.WorkDir, "checkout", ".")
		m.gitCmdErr(m.WorkDir, "clean", "-fd", "--exclude=.ralph/")
	}

	newBranch := WipBranchName()
	oldBranch := m.WorktreeBranch
	if oldBranch == newBranch {
		return
	}
	if err := m.gitCmdErr(m.WorkDir, "checkout", "-B", newBranch); err == nil {
		m.WorktreeBranch = newBranch
		if m.state != nil {
			_ = m.state.Write("worktree_branch", newBranch)
		}
		if err := m.gitCmdErr(m.ProjectDir, "branch", "-D", oldBranch); err == nil {
			m.logger.Emit(logging.Opts{Domain: logging.Git}, "Deleted local branch %s", oldBranch)
		}
	}
}

// SquashToOneCommit squashes all commits since baseSHA into a single commit
// with the given message. No-op if there is already exactly one commit
// ahead of base. Returns an error if there are no commits to squash.
func (m *Repo) SquashToOneCommit(baseSHA, message string) error {
	countStr := m.gitOutput(m.WorkDir, "rev-list", "--count", baseSHA+"..HEAD")
	count := 0
	if countStr != "" {
		fmt.Sscanf(countStr, "%d", &count)
	}
	if count == 0 {
		return fmt.Errorf("no commits ahead of %s", baseSHA)
	}
	if count == 1 {
		return nil
	}
	m.logger.Emit(logging.Opts{Domain: logging.Git}, "Squashing %d commits into one", count)
	m.gitCmd(m.WorkDir, "reset", "--soft", baseSHA)
	return m.gitCmdErr(m.WorkDir, "commit", "-m", message)
}

// RemoveWorktree force-removes a worktree and deletes its branch.
func (m *Repo) RemoveWorktree() {
	m.gitCmd(m.ProjectDir, "worktree", "remove", "--force", m.WorkDir)
	m.gitCmd(m.ProjectDir, "branch", "-D", m.WorktreeBranch)
}

// TagTaskStart creates a lightweight git tag marking the start of a task iteration.
// The tag name is task/{taskID}/start when a backend ID is available,
// or task/{seq}-{slug}/start derived from the current branch name.
func (m *Repo) TagTaskStart(taskID string) {
	tag := m.taskTag(taskID, "start")
	if tag == "" {
		return
	}
	if err := m.gitCmdErr(m.WorkDir, "tag", "-f", tag); err == nil {
		m.logger.Emit(logging.Opts{Domain: logging.Git}, "Tag: %s", tag)
	}
}

// TagTaskEnd creates a lightweight git tag marking the end of a task iteration.
func (m *Repo) TagTaskEnd(taskID string) {
	tag := m.taskTag(taskID, "end")
	if tag == "" {
		return
	}
	if err := m.gitCmdErr(m.WorkDir, "tag", "-f", tag); err == nil {
		m.logger.Emit(logging.Opts{Domain: logging.Git}, "Tag: %s", tag)
	}
}

// taskTag builds a tag name like task/{id}/{suffix}. Returns empty
// string if there's not enough info to build a meaningful tag.
func (m *Repo) taskTag(taskID, suffix string) string {
	if m.WorkDir == "" || m.WorkDir == m.ProjectDir {
		return ""
	}
	if taskID != "" {
		return fmt.Sprintf("task/%s/%s", taskID, suffix)
	}
	seqSlug := extractSeqSlug(m.WorktreeBranch)
	if seqSlug == "" {
		return ""
	}
	return fmt.Sprintf("task/%s/%s", seqSlug, suffix)
}

// extractSeqSlug pulls the slug portion from a branch like "ralph/my-task"
// or legacy "ralph/project/01-my-task".
func extractSeqSlug(branch string) string {
	stripped := strings.TrimPrefix(branch, branchPrefix)
	if stripped == branch || stripped == "" {
		return ""
	}
	// Handle legacy format with extra path segment.
	if idx := strings.LastIndex(stripped, "/"); idx >= 0 {
		stripped = stripped[idx+1:]
	}
	if stripped == "next" || stripped == "wip" || stripped == "" {
		return ""
	}
	return stripped
}
