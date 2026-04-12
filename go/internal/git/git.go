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
func (r *Repo) GH() GitHub {
	if r.github != nil {
		return r.github
	}
	return &ghCLI{
		CopilotGatedTimeout:         r.copilotGatedTimeout,
		CopilotOpportunisticTimeout: r.copilotOpportunisticTimeout,
		CodeRabbitTimeout:           r.codeRabbitTimeout,
	}
}

func (r *Repo) gh() GitHub {
	return r.GH()
}

func (r *Repo) SetLocalTestsPassed(v bool) {
	r.LocalTestsPassed = v
}

// SetKnownPRNumber stores a PR number discovered earlier (e.g. during Ship)
// so that AutoMergeCurrentBranch and FlushUnpushedWork can skip the FindOpenPR lookup.
func (r *Repo) SetKnownPRNumber(n int) {
	r.KnownPRNumber = n
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
//      checks r.ProjectDir, not r.WorkDir, because Init runs before
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
		if IsGitRepo(r.ProjectDir) && r.hasUncommittedChangesIn(r.ProjectDir) {
			return fmt.Errorf("uncommitted changes in %s — please commit or stash before running ralph.", r.ProjectDir)
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
	r.WorkDir = r.ProjectDir

	if !IsGitRepo(r.ProjectDir) {
		return fmt.Errorf("not a git repo — ralph requires git")
	}

	if r.resume {
		if err := r.tryResumeWorktree(); err == nil {
			return nil
		}
	}

	r.ProjectName = filepath.Base(r.ProjectDir)

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
	r.WorktreeBranch = WipBranchName()
	r.WorkDir = filepath.Join(r.ralphDir, "worktrees", fmt.Sprintf("ralph-%s-%02d", today, runSeq))

	if err := os.MkdirAll(filepath.Join(r.ralphDir, "worktrees"), 0o755); err != nil {
		return fmt.Errorf("creating worktrees dir: %w", err)
	}

	r.gitCmd(r.ProjectDir, "worktree", "prune")

	if _, err := os.Stat(r.WorkDir); err == nil {
		os.RemoveAll(r.WorkDir)
	}

	// Clean up leftover wip branch from a previous run.
	if r.refExists(r.ProjectDir, r.WorktreeBranch) {
		if wt := r.findWorktreeForBranch(r.ProjectDir, r.WorktreeBranch); wt != "" && strings.Contains(wt, "/.ralph/worktrees/") {
			r.gitCmd(r.ProjectDir, "worktree", "remove", "--force", wt)
		}
		_ = r.gitCmdErr(r.ProjectDir, "branch", "-D", r.WorktreeBranch)
	}

	defaultBranch := r.detectDefaultBranch()
	r.gitCmdCtx(ctx, r.ProjectDir, "fetch", "origin", defaultBranch)

	if !r.refExists(r.ProjectDir, "origin/"+defaultBranch) &&
		r.refExists(r.ProjectDir, "HEAD") &&
		r.remoteExists() {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Pushing %s to origin (empty remote — ensures correct default branch)", defaultBranch)
		r.gitCmdCtx(ctx, r.ProjectDir, "push", "-u", "origin", defaultBranch)
	}

	if err := r.gitCmdErr(r.ProjectDir, "worktree", "add", "-b", r.WorktreeBranch, r.WorkDir, "origin/"+defaultBranch); err != nil {
		if err := r.gitCmdErr(r.ProjectDir, "worktree", "add", "-b", r.WorktreeBranch, r.WorkDir, "HEAD"); err != nil {
			return fmt.Errorf("failed to create worktree: %w", err)
		}
	}

	r.gitCmd(r.WorkDir, "config", "rebase.updateRefs", "true")
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Worktree: %s (branch: %s)", r.WorkDir, r.WorktreeBranch)

	if r.state != nil {
		_ = r.state.Write("worktree_dir", r.WorkDir)
		_ = r.state.Write("worktree_branch", r.WorktreeBranch)
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

	r.WorkDir = stored
	branch, _ := r.state.Read("worktree_branch")
	r.WorktreeBranch = branch
	r.ProjectName = filepath.Base(r.ProjectDir)
	if renamed, _ := r.state.Read("branch_renamed"); renamed == "true" {
		r.BranchRenamed = true
	}

	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Resuming worktree: %s", r.WorkDir)
	return nil
}

// withStash stashes any uncommitted changes, runs fn, then reapplies the stash.
// Used by EnsureUpToDate and RebaseOntoDefaultBranch to avoid duplicating
// the stash/pop logic.
func (r *Repo) withStash(stashMsg string, fn func()) {
	dirty := r.gitCmdErr(r.WorkDir, "diff", "--quiet") != nil ||
		r.gitCmdErr(r.WorkDir, "diff", "--cached", "--quiet") != nil
	if dirty {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Stashing uncommitted changes before rebase...")
		r.gitCmd(r.WorkDir, "stash", "push", "-m", stashMsg)
	}

	fn()

	if dirty {
		if err := r.gitCmdErr(r.WorkDir, "stash", "pop"); err != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Stash pop conflict — committing stash as WIP")
			r.gitCmd(r.WorkDir, "checkout", "--theirs", ".")
			r.gitCmd(r.WorkDir, "add", "-A")
			r.gitCmd(r.WorkDir, "commit", "-m", "WIP: reapply stashed changes after rebase (may need review)")
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
	if r.WorkDir == "" || r.WorkDir == r.ProjectDir {
		return nil
	}

	// Rebase onto stack head if set, otherwise the default branch.
	baseBranch := r.detectDefaultBranch()
	if r.PrevBranch != "" {
		baseBranch = r.PrevBranch
	}

	if err := r.gitCmdErrCtx(ctx, r.WorkDir, "fetch", "origin", baseBranch); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if r.PrevBranch != "" {
			// Stack head branch missing from remote — likely merged and deleted.
			// Fall back to the default branch silently.
			r.SetPrevBranch("")
			baseBranch = r.detectDefaultBranch()
			if err2 := r.gitCmdErrCtx(ctx, r.WorkDir, "fetch", "origin", baseBranch); err2 != nil {
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

	if !r.refExists(r.WorkDir, "origin/"+baseBranch) {
		return nil
	}

	if r.gitCmdErr(r.WorkDir, "merge-base", "--is-ancestor", "origin/"+baseBranch, "HEAD") == nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Branch: baseBranch}, "Already up to date with origin/%s", baseBranch)
		return nil
	}

	// No local commits ahead of base → safe to force-reset (fresh start).
	localCommits := r.gitOutput(r.WorkDir, "rev-list", "--count", "origin/"+baseBranch+"..HEAD")
	if localCommits == "" || localCommits == "0" {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Branch: baseBranch}, "Resetting to origin/%s (no local work)", baseBranch)
		r.gitCmd(r.WorkDir, "reset", "--hard", "origin/"+baseBranch)
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
		r.gitCmd(r.WorkDir, "rebase", "--abort")
		result = nil // not an error — diverged stack is expected
	})
	return result
}

func (r *Repo) tryRebase(ctx context.Context, defaultBranch string) bool {
	if r.gitCmdErrCtx(ctx, r.WorkDir, "rebase", "--update-refs", "origin/"+defaultBranch) == nil {
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
	r.gitCmd(r.WorkDir, "rebase", "--abort")
	return false
}


// RenameBranchForTask renames the current branch to include a task slug.
// Records the previous branch name for stacked PR targeting.
// Only renames once per task (tracked by BranchRenamed).
// Returns an error if the rename fails — callers must abort the iteration.
func (r *Repo) RenameBranchForTask(taskDesc, taskID string) error {
	if r.BranchRenamed || r.WorktreeBranch == "" || taskDesc == "" {
		return nil
	}
	if r.WorkDir == r.ProjectDir {
		return nil
	}

	slug := Slugify(taskDesc)
	if slug == "" {
		return nil
	}

	newBranch := BranchName(taskID, slug)
	if err := r.gitCmdErr(r.WorkDir, "branch", "-m", r.WorktreeBranch, newBranch); err != nil {
		_ = r.gitCmdErr(r.WorkDir, "branch", "-D", newBranch)
		if retryErr := r.gitCmdErr(r.WorkDir, "branch", "-m", r.WorktreeBranch, newBranch); retryErr != nil {
			return fmt.Errorf("rename branch %s → %s: %w", r.WorktreeBranch, newBranch, retryErr)
		}
	}
	r.WorktreeBranch = newBranch
	r.BranchRenamed = true
	if r.state != nil {
		_ = r.state.Write("worktree_branch", r.WorktreeBranch)
		_ = r.state.Write("branch_renamed", "true")
	}
	return nil
}

// RenameBranchTo renames the current worktree branch to a specific name.
// Used when the bead already has a stored branch name from a previous run.
func (r *Repo) RenameBranchTo(name string) {
	if r.BranchRenamed || r.WorktreeBranch == "" || name == "" {
		return
	}
	if r.WorktreeBranch == name {
		r.BranchRenamed = true
		if r.state != nil {
			_ = r.state.Write("branch_renamed", "true")
		}
		return
	}
	if err := r.gitCmdErr(r.WorkDir, "branch", "-m", r.WorktreeBranch, name); err != nil {
		// Target branch may exist locally as a stale leftover. Delete it
		// and retry — the local branch is expendable since work is on the remote.
		_ = r.gitCmdErr(r.WorkDir, "branch", "-D", name)
		if retryErr := r.gitCmdErr(r.WorkDir, "branch", "-m", r.WorktreeBranch, name); retryErr != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to rename branch %s → %s: %v", r.WorktreeBranch, name, retryErr)
			return
		}
	}
	r.WorktreeBranch = name
	r.BranchRenamed = true
	if r.state != nil {
		_ = r.state.Write("worktree_branch", r.WorktreeBranch)
		_ = r.state.Write("branch_renamed", "true")
	}
}

// ResetToDefaultBranch resets the worktree to origin's default branch.
// Used on resume when no stack exists — stale local commits are discarded.
// No-ops silently when the worktree is already at the target ref.
func (r *Repo) ResetToDefaultBranch() {
	defaultBranch := r.detectDefaultBranch()
	_ = r.gitCmdErr(r.WorkDir, "fetch", "origin", defaultBranch)
	target := "origin/" + defaultBranch
	if r.refExists(r.WorkDir, target) && r.gitOutput(r.WorkDir, "rev-parse", "HEAD") == r.gitOutput(r.WorkDir, "rev-parse", target) {
		return
	}
	r.gitCmd(r.WorkDir, "reset", "--hard", target)
	r.BranchRenamed = false
	if r.state != nil {
		_ = r.state.Write("branch_renamed", "false")
	}
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Reset worktree to %s", target)
}

// SetPrevBranch sets the previous branch for stacked PR targeting and
// persists it to state so it survives process restarts.
func (r *Repo) SetPrevBranch(branch string) {
	r.PrevBranch = branch
	if r.state != nil {
		_ = r.state.Write("prev_branch", branch)
	}
}

// PrepareForNextTask creates a fresh wip branch from HEAD so the next task
// gets its own branch. RenameBranchForTask will rename it to a task-specific
// name before the first commit. Uncommitted changes are discarded only when
// switching to a different task; resuming the same task preserves in-progress work.
func (r *Repo) PrepareForNextTask(nextTaskID string) {
	r.BranchRenamed = false
	if r.state != nil {
		_ = r.state.Write("branch_renamed", "false")
	}

	if r.WorkDir == r.ProjectDir || r.WorktreeBranch == "" {
		return
	}

	lastTaskID := ""
	if r.state != nil {
		lastTaskID, _ = r.state.Read("last_task_id")
	}
	if nextTaskID == "" || lastTaskID == "" || nextTaskID != lastTaskID {
		r.gitCmdErr(r.WorkDir, "checkout", ".")
		r.gitCmdErr(r.WorkDir, "clean", "-fd", "--exclude=.ralph/")
	}

	newBranch := WipBranchName()
	oldBranch := r.WorktreeBranch
	if oldBranch == newBranch {
		return
	}
	if err := r.gitCmdErr(r.WorkDir, "checkout", "-B", newBranch); err == nil {
		r.WorktreeBranch = newBranch
		if r.state != nil {
			_ = r.state.Write("worktree_branch", newBranch)
		}
		if err := r.gitCmdErr(r.ProjectDir, "branch", "-D", oldBranch); err == nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Deleted local branch %s", oldBranch)
		}
	}
}

// SquashToOneCommit squashes all commits since baseSHA into a single commit
// with the given message. No-op if there is already exactly one commit
// ahead of base. Returns an error if there are no commits to squash.
func (r *Repo) SquashToOneCommit(baseSHA, message string) error {
	countStr := r.gitOutput(r.WorkDir, "rev-list", "--count", baseSHA+"..HEAD")
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
	r.gitCmd(r.WorkDir, "reset", "--soft", baseSHA)
	return r.gitCmdErr(r.WorkDir, "commit", "-m", message)
}

// RemoveWorktree force-removes a worktree and deletes its branch.
func (r *Repo) RemoveWorktree() {
	r.gitCmd(r.ProjectDir, "worktree", "remove", "--force", r.WorkDir)
	r.gitCmd(r.ProjectDir, "branch", "-D", r.WorktreeBranch)
}

// TagTaskStart creates a lightweight git tag marking the start of a task iteration.
// The tag name is task/{taskID}/start when a backend ID is available,
// or task/{seq}-{slug}/start derived from the current branch name.
func (r *Repo) TagTaskStart(taskID string) {
	tag := r.taskTag(taskID, "start")
	if tag == "" {
		return
	}
	if err := r.gitCmdErr(r.WorkDir, "tag", "-f", tag); err == nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Tag: %s", tag)
	}
}

// TagTaskEnd creates a lightweight git tag marking the end of a task iteration.
func (r *Repo) TagTaskEnd(taskID string) {
	tag := r.taskTag(taskID, "end")
	if tag == "" {
		return
	}
	if err := r.gitCmdErr(r.WorkDir, "tag", "-f", tag); err == nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Tag: %s", tag)
	}
}

// taskTag builds a tag name like task/{id}/{suffix}. Returns empty
// string if there's not enough info to build a meaningful tag.
func (r *Repo) taskTag(taskID, suffix string) string {
	if r.WorkDir == "" || r.WorkDir == r.ProjectDir {
		return ""
	}
	if taskID != "" {
		return fmt.Sprintf("task/%s/%s", taskID, suffix)
	}
	seqSlug := extractSeqSlug(r.WorktreeBranch)
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
