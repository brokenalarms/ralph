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

// Log is the logging interface used by Repo and its helpers. A single Emit
// method handles full lines and in-place partial lines via logging.Opts.Append.
// *logging.Logger satisfies this; tests provide stubs.
type Log interface {
	Emit(o logging.Opts, format string, args ...any)
}

// stateStore abstracts reading and writing ralph JSON state. Unexported —
// git constructs the production implementation internally from ralphDir.
// Tests use memState via same-package construction.
type stateStore interface {
	Read(key string) (string, error)
	Write(key, value string) error
}


// Config bundles the construction-time data inputs for git.New. Pure data
// plus the Logger (Rule 5 exception). No module interfaces, no function
// fields. The orchestrator builds one of these once and passes it in.
type Config struct {
	ProjectDir                  string
	WorkDir                     string
	RalphDir                    string
	BaseBranch                  string
	Resume                      bool
	Logger                      Log
	CompileCheckTimeout         time.Duration
	CIPollTimeout               time.Duration
	CopilotGatedTimeout         time.Duration
	CopilotOpportunisticTimeout time.Duration
	CodeRabbitTimeout           time.Duration
}

// Repo implements Ops. Constructed by New(); tests in the git package
// construct it directly for stub injection.
type Repo struct {
	projectDir     string
	projectName    string
	workDir        string
	worktreeBranch string
	prevBranch     string
	branchRenamed  bool

	knownPRNumber int
	runner           Runner

	ralphDir                    string
	baseBranch                  string
	resume                      bool
	github                      gitHub
	state                       stateStore
	logger                      Log
	compileCheckTimeout         time.Duration
	ciPollTimeout               time.Duration
	copilotGatedTimeout         time.Duration
	copilotOpportunisticTimeout time.Duration
	codeRabbitTimeout           time.Duration
}

// New creates a git module from data-only Config. Constructs all internal
// sub-modules (GitHub CLI wrapper, state store, runner) from Config data.
// Returns the Ops interface — the only public API surface.
func New(cfg Config) Ops {
	projectDir := cfg.ProjectDir
	if projectDir == "" {
		projectDir = cfg.WorkDir
	}
	return &Repo{
		projectDir:                  projectDir,
		workDir:                     cfg.WorkDir,
		ralphDir:                    cfg.RalphDir,
		baseBranch:                  cfg.BaseBranch,
		resume:                      cfg.Resume,
		logger:                      cfg.Logger,
		compileCheckTimeout:         cfg.CompileCheckTimeout,
		ciPollTimeout:               cfg.CIPollTimeout,
		copilotGatedTimeout:         cfg.CopilotGatedTimeout,
		copilotOpportunisticTimeout: cfg.CopilotOpportunisticTimeout,
		codeRabbitTimeout:           cfg.CodeRabbitTimeout,
		github: &ghCLI{
			CopilotGatedTimeout:         cfg.CopilotGatedTimeout,
			CopilotOpportunisticTimeout: cfg.CopilotOpportunisticTimeout,
			CodeRabbitTimeout:           cfg.CodeRabbitTimeout,
		},
		state: newStateStore(cfg.RalphDir),
	}
}

// newStateStore returns a file-backed state store when ralphDir is set,
// or nil when empty (disabling state persistence).
func newStateStore(ralphDir string) stateStore {
	if ralphDir == "" {
		return nil
	}
	return newFileStateStore(ralphDir)
}


// SetKnownPRNumber stores a PR number discovered earlier (e.g. during Ship)
// so that AutoMergeCurrentBranch and FlushUnpushedWork can skip the FindOpenPR lookup.
func (r *Repo) SetKnownPRNumber(n int) {
	r.knownPRNumber = n
}

// Init runs the git pre-flight checks and worktree setup that must
// complete before the orchestrator can use this Repo. Bundles the
// individual operations so callers don't have to know the right
// sequence:
//
//   1. ValidateRemoteBranch — checks the configured base branch exists
//      on the remote. Returns an error on failure (init aborts).
//   2. Dirty-tree check (only on fresh runs, not on resume) — refuses to
//      start with a dirty working tree in the project repo, so the
//      .gitignore commit below doesn't sweep in unrelated staged work.
//      Returns an error on failure (init aborts). This explicitly
//      checks r.projectDir, not r.workDir, because Init runs before
//      SetupWorktree has moved WorkDir to a worktree subdirectory.
//   3. EnsureGitignored — adds .ralph to .gitignore. Best-effort: any
//      filesystem failure is silently swallowed by the helper, so this
//      step never returns an error from Init.
//   4. PruneOrphanedWorktrees — cleans up stale worktrees from previous
//      runs. Best-effort: any cleanup failure is silently swallowed.
//   5. SetupWorktree — creates (or resumes) the iteration worktree.
//      Returns an error on failure (init aborts).
//
// Init should be called once immediately after New, before any task
// execution. Production callers (cmd/ralph) call this; tests that don't
// exercise worktree setup can skip it and use the constructed Repo
// directly.
func (r *Repo) Init(ctx context.Context) error {
	if err := r.ValidateRemoteBranch(ctx); err != nil {
		return err
	}
	if !r.resume {
		if IsGitRepo(r.projectDir) && r.hasUncommittedChangesIn(r.projectDir) {
			return fmt.Errorf("uncommitted changes in %s — please commit or stash before running ralph.", r.projectDir)
		}
	}
	r.EnsureGitignored(".ralph")
	r.PruneOrphanedWorktrees()
	if err := r.SetupWorktree(ctx); err != nil {
		return fmt.Errorf("worktree setup failed: %w", err)
	}
	return nil
}

// SetupWorktree creates (or resumes) a git worktree for isolated work.
func (r *Repo) SetupWorktree(ctx context.Context) error {
	r.workDir = r.projectDir

	if !IsGitRepo(r.projectDir) {
		return fmt.Errorf("not a git repo — ralph requires git")
	}

	if r.resume {
		if err := r.tryResumeWorktree(); err == nil {
			return nil
		}
	}

	r.projectName = filepath.Base(r.projectDir)

	today := time.Now().Format("20060102")
	runSeq := 1
	worktreeRoot := filepath.Join(r.ralphDir, "worktrees")
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
	r.worktreeBranch = WipBranchName()
	r.workDir = filepath.Join(r.ralphDir, "worktrees", fmt.Sprintf("ralph-%s-%02d", today, runSeq))

	if err := os.MkdirAll(filepath.Join(r.ralphDir, "worktrees"), 0o755); err != nil {
		return fmt.Errorf("creating worktrees dir: %w", err)
	}

	r.gitCmd(r.projectDir, "worktree", "prune")

	if _, err := os.Stat(r.workDir); err == nil {
		os.RemoveAll(r.workDir)
	}

	// Clean up leftover wip branch from a previous run.
	if r.refExists(r.projectDir, r.worktreeBranch) {
		if wt := r.findWorktreeForBranch(r.projectDir, r.worktreeBranch); wt != "" && strings.Contains(wt, "/.ralph/worktrees/") {
			r.gitCmd(r.projectDir, "worktree", "remove", "--force", wt)
		}
		_ = r.gitCmdErr(r.projectDir, "branch", "-D", r.worktreeBranch)
	}

	defaultBranch := r.detectDefaultBranch()
	r.gitCmdCtx(ctx, r.projectDir, "fetch", "origin", defaultBranch)

	if !r.refExists(r.projectDir, "origin/"+defaultBranch) &&
		r.refExists(r.projectDir, "HEAD") &&
		r.remoteExists() {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Pushing %s to origin (empty remote — ensures correct default branch)", defaultBranch)
		r.gitCmdCtx(ctx, r.projectDir, "push", "-u", "origin", defaultBranch)
	}

	if err := r.gitCmdErr(r.projectDir, "worktree", "add", "-b", r.worktreeBranch, r.workDir, "origin/"+defaultBranch); err != nil {
		if err := r.gitCmdErr(r.projectDir, "worktree", "add", "-b", r.worktreeBranch, r.workDir, "HEAD"); err != nil {
			return fmt.Errorf("failed to create worktree: %w", err)
		}
	}

	r.gitCmd(r.workDir, "config", "rebase.updateRefs", "true")
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Worktree: %s (branch: %s)", r.workDir, r.worktreeBranch)

	if r.state != nil {
		_ = r.state.Write("worktree_dir", r.workDir)
		_ = r.state.Write("worktree_branch", r.worktreeBranch)
	}

	return nil
}

// tryResumeWorktree attempts to reuse a stored worktree from a previous run.
// Only restores the worktree path and state — no rebase or reset. The actual
// branch setup happens later in checkoutExistingBranch once the task is known
// and the correct base branch can be determined.
func (r *Repo) tryResumeWorktree() error {
	if r.state == nil {
		return fmt.Errorf("no state store")
	}
	stored, err := r.state.Read("worktree_dir")
	if err != nil || stored == "" || stored == "null" {
		return fmt.Errorf("no stored worktree")
	}
	info, err := os.Stat(stored)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("stored worktree missing")
	}

	r.workDir = stored
	branch, _ := r.state.Read("worktree_branch")
	r.worktreeBranch = branch
	r.projectName = filepath.Base(r.projectDir)
	if renamed, _ := r.state.Read("branch_renamed"); renamed == "true" {
		r.branchRenamed = true
	}

	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Resuming worktree: %s", r.workDir)
	return nil
}

// withStash stashes any uncommitted changes, runs fn, then reapplies the stash.
// Used by EnsureUpToDate and RebaseOntoDefaultBranch to avoid duplicating
// the stash/pop logic.
func (r *Repo) withStash(stashMsg string, fn func()) {
	dirty := r.gitCmdErr(r.workDir, "diff", "--quiet") != nil ||
		r.gitCmdErr(r.workDir, "diff", "--cached", "--quiet") != nil
	if dirty {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Stashing uncommitted changes before rebase...")
		r.gitCmd(r.workDir, "stash", "push", "-m", stashMsg)
	}

	fn()

	if dirty {
		if err := r.gitCmdErr(r.workDir, "stash", "pop"); err != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Stash pop conflict — committing stash as WIP")
			r.gitCmd(r.workDir, "checkout", "--theirs", ".")
			r.gitCmd(r.workDir, "add", "-A")
			r.gitCmd(r.workDir, "commit", "-m", "WIP: reapply stashed changes after rebase (may need review)")
		} else {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Re-applied stashed changes")
		}
	}
}

// EnsureUpToDate fetches the latest base branch, stashes any uncommitted
// changes, rebases onto origin, and reapplies the stash. If rebase fails
// after auto-resolve, it aborts and returns an error — the caller decides
// what to do (e.g. push anyway and let GitHub handle merge conflicts).
func (r *Repo) EnsureUpToDate(ctx context.Context) error {
	if r.workDir == "" || r.workDir == r.projectDir {
		return nil
	}

	// Rebase onto stack head if set, otherwise the default branch.
	baseBranch := r.detectDefaultBranch()
	if r.prevBranch != "" {
		baseBranch = r.prevBranch
	}

	if err := r.gitCmdErrCtx(ctx, r.workDir, "fetch", "origin", baseBranch); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if r.prevBranch != "" {
			// Stack head branch missing from remote — likely merged and deleted.
			// Fall back to the default branch silently.
			r.SetPrevBranch("")
			baseBranch = r.detectDefaultBranch()
			if err2 := r.gitCmdErrCtx(ctx, r.workDir, "fetch", "origin", baseBranch); err2 != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to fetch origin/%s: %v", baseBranch, err2)
				return nil
			}
		} else {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to fetch origin/%s: %v", baseBranch, err)
			return nil
		}
	}

	if !r.refExists(r.workDir, "origin/"+baseBranch) {
		return nil
	}

	if r.gitCmdErr(r.workDir, "merge-base", "--is-ancestor", "origin/"+baseBranch, "HEAD") == nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Branch: baseBranch}, "Already up to date with origin/%s", baseBranch)
		return nil
	}

	// No local commits ahead of base → safe to force-reset (fresh start).
	localCommits := r.gitOutput(r.workDir, "rev-list", "--count", "origin/"+baseBranch+"..HEAD")
	if localCommits == "" || localCommits == "0" {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Branch: baseBranch}, "Resetting to origin/%s (no local work)", baseBranch)
		r.gitCmd(r.workDir, "reset", "--hard", "origin/"+baseBranch)
		return nil
	}

	// Local commits exist — try to rebase them onto latest base.
	var result error
	r.withStash("ralph-autostash", func() {
		if ctx.Err() != nil {
			result = ctx.Err()
			return
		}

		// 1. Fast-forward rebase
		if r.tryRebase(ctx, baseBranch) {
			return
		}

		// 2. Auto-resolve mechanical conflicts
		if r.tryAutoResolve(ctx, baseBranch) {
			return
		}

		// Stack diverges — abort rebase, keep local commits, let merge handle it.
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Rebase conflict with local work — stack diverged, continuing")
		r.gitCmd(r.workDir, "rebase", "--abort")
		result = nil // not an error — diverged stack is expected
	})
	return result
}

func (r *Repo) tryRebase(ctx context.Context, defaultBranch string) bool {
	if r.gitCmdErrCtx(ctx, r.workDir, "rebase", "--update-refs", "origin/"+defaultBranch) == nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Branch: defaultBranch}, "Rebased onto origin/%s", defaultBranch)
		return true
	}
	return false
}

func (r *Repo) tryAutoResolve(ctx context.Context, defaultBranch string) bool {
	if r.autoResolveAndContinue(ctx, defaultBranch) {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Branch: defaultBranch}, "Rebased onto origin/%s (auto-resolved)", defaultBranch)
		return true
	}
	r.gitCmd(r.workDir, "rebase", "--abort")
	return false
}


// RenameBranchForTask renames the current branch to include a task slug.
// Records the previous branch name for stacked PR targeting.
// Only renames once per task (tracked by BranchRenamed).
// Returns an error if the rename fails — callers must abort the iteration.
func (r *Repo) RenameBranchForTask(taskDesc, taskID string) error {
	if r.branchRenamed || r.worktreeBranch == "" || taskDesc == "" {
		return nil
	}
	if r.workDir == r.projectDir {
		return nil
	}

	slug := Slugify(taskDesc)
	if slug == "" {
		return nil
	}

	newBranch := BranchName(taskID, slug)
	if err := r.gitCmdErr(r.workDir, "branch", "-m", r.worktreeBranch, newBranch); err != nil {
		_ = r.gitCmdErr(r.workDir, "branch", "-D", newBranch)
		if retryErr := r.gitCmdErr(r.workDir, "branch", "-m", r.worktreeBranch, newBranch); retryErr != nil {
			return fmt.Errorf("rename branch %s → %s: %w", r.worktreeBranch, newBranch, retryErr)
		}
	}
	r.worktreeBranch = newBranch
	r.branchRenamed = true
	if r.state != nil {
		_ = r.state.Write("worktree_branch", r.worktreeBranch)
		_ = r.state.Write("branch_renamed", "true")
	}
	return nil
}

// RenameBranchTo renames the current worktree branch to a specific name.
// Used when the bead already has a stored branch name from a previous run.
func (r *Repo) RenameBranchTo(name string) {
	if r.branchRenamed || r.worktreeBranch == "" || name == "" {
		return
	}
	if r.worktreeBranch == name {
		r.branchRenamed = true
		if r.state != nil {
			_ = r.state.Write("branch_renamed", "true")
		}
		return
	}
	if err := r.gitCmdErr(r.workDir, "branch", "-m", r.worktreeBranch, name); err != nil {
		// Target branch may exist locally as a stale leftover. Delete it
		// and retry — the local branch is expendable since work is on the remote.
		_ = r.gitCmdErr(r.workDir, "branch", "-D", name)
		if retryErr := r.gitCmdErr(r.workDir, "branch", "-m", r.worktreeBranch, name); retryErr != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to rename branch %s → %s: %v", r.worktreeBranch, name, retryErr)
			return
		}
	}
	r.worktreeBranch = name
	r.branchRenamed = true
	if r.state != nil {
		_ = r.state.Write("worktree_branch", r.worktreeBranch)
		_ = r.state.Write("branch_renamed", "true")
	}
}

// ResetToDefaultBranch resets the worktree to origin's default branch.
// Used on resume when no stack exists — stale local commits are discarded.
// No-ops silently when the worktree is already at the target ref.
func (r *Repo) ResetToDefaultBranch() {
	defaultBranch := r.detectDefaultBranch()
	_ = r.gitCmdErr(r.workDir, "fetch", "origin", defaultBranch)
	target := "origin/" + defaultBranch
	if r.refExists(r.workDir, target) && r.gitOutput(r.workDir, "rev-parse", "HEAD") == r.gitOutput(r.workDir, "rev-parse", target) {
		return
	}
	r.gitCmd(r.workDir, "reset", "--hard", target)
	r.branchRenamed = false
	if r.state != nil {
		_ = r.state.Write("branch_renamed", "false")
	}
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Reset worktree to %s", target)
}

// SetPrevBranch sets the previous branch for stacked PR targeting and
// persists it to state so it survives process restarts.
func (r *Repo) SetPrevBranch(branch string) {
	r.prevBranch = branch
	if r.state != nil {
		_ = r.state.Write("prev_branch", branch)
	}
}

// PrepareForNextTask creates a fresh wip branch from HEAD so the next task
// gets its own branch. RenameBranchForTask will rename it to a task-specific
// name before the first commit. Uncommitted changes are discarded only when
// switching to a different task; resuming the same task preserves in-progress work.
func (r *Repo) PrepareForNextTask(nextTaskID string) {
	r.branchRenamed = false
	if r.state != nil {
		_ = r.state.Write("branch_renamed", "false")
	}

	if r.workDir == r.projectDir || r.worktreeBranch == "" {
		return
	}

	lastTaskID := ""
	if r.state != nil {
		lastTaskID, _ = r.state.Read("last_task_id")
	}
	if nextTaskID == "" || lastTaskID == "" || nextTaskID != lastTaskID {
		r.gitCmdErr(r.workDir, "checkout", ".")
		r.gitCmdErr(r.workDir, "clean", "-fd", "--exclude=.ralph/")
	}

	newBranch := WipBranchName()
	oldBranch := r.worktreeBranch
	if oldBranch == newBranch {
		return
	}
	if err := r.gitCmdErr(r.workDir, "checkout", "-B", newBranch); err == nil {
		r.worktreeBranch = newBranch
		if r.state != nil {
			_ = r.state.Write("worktree_branch", newBranch)
		}
		if err := r.gitCmdErr(r.projectDir, "branch", "-D", oldBranch); err == nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Deleted local branch %s", oldBranch)
		}
	}
}

// SquashToOneCommit squashes all commits since baseSHA into a single commit
// with the given message. No-op if there is already exactly one commit
// ahead of base. Returns an error if there are no commits to squash.
func (r *Repo) SquashToOneCommit(baseSHA, message string) error {
	countStr := r.gitOutput(r.workDir, "rev-list", "--count", baseSHA+"..HEAD")
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
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Squashing %d commits into one", count)
	r.gitCmd(r.workDir, "reset", "--soft", baseSHA)
	return r.gitCmdErr(r.workDir, "commit", "-m", message)
}

// RemoveWorktree force-removes a worktree and deletes its branch.
func (r *Repo) RemoveWorktree() {
	r.gitCmd(r.projectDir, "worktree", "remove", "--force", r.workDir)
	r.gitCmd(r.projectDir, "branch", "-D", r.worktreeBranch)
}

// TagTaskStart creates a lightweight git tag marking the start of a task iteration.
// The tag name is task/{taskID}/start when a backend ID is available,
// or task/{seq}-{slug}/start derived from the current branch name.
func (r *Repo) TagTaskStart(taskID string) {
	tag := r.taskTag(taskID, "start")
	if tag == "" {
		return
	}
	if err := r.gitCmdErr(r.workDir, "tag", "-f", tag); err == nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Tag: %s", tag)
	}
}

// TagTaskEnd creates a lightweight git tag marking the end of a task iteration.
func (r *Repo) TagTaskEnd(taskID string) {
	tag := r.taskTag(taskID, "end")
	if tag == "" {
		return
	}
	if err := r.gitCmdErr(r.workDir, "tag", "-f", tag); err == nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Tag: %s", tag)
	}
}

// taskTag builds a tag name like task/{id}/{suffix}. Returns empty
// string if there's not enough info to build a meaningful tag.
func (r *Repo) taskTag(taskID, suffix string) string {
	if r.workDir == "" || r.workDir == r.projectDir {
		return ""
	}
	if taskID != "" {
		return fmt.Sprintf("task/%s/%s", taskID, suffix)
	}
	seqSlug := extractSeqSlug(r.worktreeBranch)
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
