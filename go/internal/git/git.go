package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/git/rebasecontinue"
	"github.com/brokenalarms/ralph/internal/logging"
)

// Log is the logging interface used by repo and its helpers. A single Emit
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
	NoCIGracePeriod             time.Duration
	CopilotGatedTimeout         time.Duration
	CopilotOpportunisticTimeout time.Duration
	CodeRabbitTimeout           time.Duration
	AdminMergeOnCIInfraFailure  bool
	ConfigVerify                string
	TestTimeout                 time.Duration
}

// repo implements Ops. Constructed by New(); tests in the git package
// construct it directly for stub injection.
type repo struct {
	projectDir     string
	workDir        string
	worktreeBranch string
	prevBranch     string
	branchRenamed  bool

	// stackHeadResolved is set by SyncWorktreeBase after calling setStackHead
	// at startup. BranchForTask clears and skips setStackHead on the first task
	// so that the two calls don't emit duplicate log lines.
	stackHeadResolved bool

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
	noCIGracePeriod             time.Duration
	copilotGatedTimeout         time.Duration
	copilotOpportunisticTimeout time.Duration
	codeRabbitTimeout           time.Duration
	adminMergeOnCIInfraFailure  bool
	configVerify                string
	testTimeout                 time.Duration
}

// New creates a git module from data-only Config. Constructs all internal
// sub-modules (GitHub CLI wrapper, state store, runner) from Config data.
// Returns the Ops interface — the only public API surface.
func New(cfg Config) Ops {
	projectDir := cfg.ProjectDir
	if projectDir == "" {
		projectDir = cfg.WorkDir
	}
	return &repo{
		projectDir:                  projectDir,
		workDir:                     cfg.WorkDir,
		ralphDir:                    cfg.RalphDir,
		baseBranch:                  cfg.BaseBranch,
		resume:                      cfg.Resume,
		logger:                      cfg.Logger,
		compileCheckTimeout:         cfg.CompileCheckTimeout,
		ciPollTimeout:               cfg.CIPollTimeout,
		noCIGracePeriod:             cfg.NoCIGracePeriod,
		copilotGatedTimeout:         cfg.CopilotGatedTimeout,
		copilotOpportunisticTimeout: cfg.CopilotOpportunisticTimeout,
		codeRabbitTimeout:           cfg.CodeRabbitTimeout,
		adminMergeOnCIInfraFailure:  cfg.AdminMergeOnCIInfraFailure,
		configVerify:                cfg.ConfigVerify,
		testTimeout:                 cfg.TestTimeout,
		github: &ghCLI{
			logger:                      cfg.Logger,
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
func (r *repo) SetKnownPRNumber(n int) {
	r.knownPRNumber = n
}

// Init runs the git pre-flight checks and worktree setup that must
// complete before the orchestrator can use this repo. Bundles the
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
// exercise worktree setup can skip it and use the constructed repo
// directly.
func (r *repo) Init(ctx context.Context) error {
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
	return r.assertWorktreeReady()
}

// SetupWorktree creates (or resumes) a git worktree for isolated work.
//
// On success, r.workDir is set to a fresh or resumed worktree path distinct
// from r.projectDir. On error, r.workDir is left unchanged — callers MUST
// treat any error from this function as fatal (do not read GetWorkDir() to
// look for a fallback path). Init's post-condition check enforces this.
func (r *repo) SetupWorktree(ctx context.Context) error {
	if !IsGitRepo(r.projectDir) {
		return fmt.Errorf("not a git repo — ralph requires git")
	}

	if r.resume {
		if err := r.tryResumeWorktree(); err == nil {
			return nil
		}
	}

	worktreeRoot, err := r.resolveWorktreeRoot()
	if err != nil {
		return fmt.Errorf("resolving worktree root: %w", err)
	}

	today := time.Now().Format("20060102")
	runSeq := 1
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
	r.workDir = filepath.Join(worktreeRoot, fmt.Sprintf("ralph-%s-%02d", today, runSeq))

	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		return fmt.Errorf("creating worktrees dir: %w", err)
	}

	r.gitCmd(r.projectDir, "worktree", "prune")

	if _, err := os.Stat(r.workDir); err == nil {
		os.RemoveAll(r.workDir)
	}

	// Clean up leftover wip branch from a previous run.
	if r.refExists(r.projectDir, r.worktreeBranch) {
		if wt := r.findWorktreeForBranch(r.projectDir, r.worktreeBranch); wt != "" && hasPathPrefix(wt, worktreeRoot) {
			r.gitCmd(r.projectDir, "worktree", "remove", "--force", wt)
		}
		_ = r.gitCmdErr(r.projectDir, "branch", "-D", r.worktreeBranch)
	}

	defaultBranch := r.baseBranch
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
	// Set tracking explicitly so git pull --rebase in post-task hooks works
	// regardless of branch.autoSetupMerge in the user's environment.
	_ = r.gitCmdErr(r.projectDir, "branch", "--set-upstream-to", "origin/"+r.resolveBaseBranch(), r.worktreeBranch)

	r.gitCmd(r.workDir, "config", "rebase.updateRefs", "true")
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Worktree: %s (branch: %s)", r.workDir, r.worktreeBranch)

	if r.state != nil {
		_ = r.state.Write("worktree_dir", r.workDir)
		_ = r.state.Write("worktree_branch", r.worktreeBranch)
	}

	return nil
}

// InitTask is the worktree-setup entry point for `ralph task` — the
// interactive triage session. It mirrors Init's pre-flight checks but
// installs a per-instance worktree on a unique branch so it does NOT
// share state with the loop's `ralph/next` wip branch and does NOT
// destroy other concurrent ralph processes' worktrees.
//
// Steps:
//  1. ValidateRemoteBranch — same as Init.
//  2. EnsureGitignored(".ralph") — same as Init.
//  3. PruneOrphanedWorktrees — best-effort cleanup of stale dirs.
//  4. SetupTaskWorktree — creates a fresh worktree on a unique
//     `ralph/task/YYYYMMDD-NN` branch under
//     `.ralph/worktrees/ralph-task-YYYYMMDD-NN`.
//
// Unlike Init: skips the dirty-tree check (the task manager doesn't
// modify the project checkout) and does not write worktree state to
// .ralph/state.json (task worktrees are ephemeral and must not
// contaminate the loop's resume state).
func (r *repo) InitTask(ctx context.Context) error {
	if err := r.ValidateRemoteBranch(ctx); err != nil {
		return err
	}
	r.EnsureGitignored(".ralph")
	r.PruneOrphanedWorktrees()
	if err := r.SetupTaskWorktree(ctx); err != nil {
		return fmt.Errorf("task worktree setup failed: %w", err)
	}
	return r.assertWorktreeReady()
}

// assertWorktreeReady is the single enforcement point for the worktree
// invariant: workDir must be non-empty and distinct from projectDir. Called
// by Init and InitTask after setup — all operational methods trust the
// invariant and do not re-check it.
func (r *repo) assertWorktreeReady() error {
	if r.workDir == "" || r.workDir == r.projectDir {
		return fmt.Errorf("worktree setup post-condition failed: workDir=%q projectDir=%q", r.workDir, r.projectDir)
	}
	return nil
}

// SetupTaskWorktree creates a per-instance git worktree for `ralph task`.
// Uses a unique branch (`ralph/task/YYYYMMDD-NN`) and dir
// (`.ralph/worktrees/ralph-task-YYYYMMDD-NN`) per invocation so it can
// run alongside the loop or another task manager without collision.
//
// On error, r.workDir is left unchanged. Callers (subcommands.handleTask,
// handleReview) MUST treat any error from this function as fatal — the
// previous "silently fall back to project dir" behavior was the recurring
// source of worktree contents leaking into main.
//
// Does NOT persist worktree_dir / worktree_branch to state — task
// manager worktrees are ephemeral and must not interfere with the
// loop's resume state.
func (r *repo) SetupTaskWorktree(ctx context.Context) error {
	if !IsGitRepo(r.projectDir) {
		return fmt.Errorf("not a git repo — ralph requires git")
	}

	worktreeRoot, err := r.resolveWorktreeRoot()
	if err != nil {
		return fmt.Errorf("resolving worktree root: %w", err)
	}

	today := time.Now().Format("20060102")
	runSeq := 1
	if entries, err := os.ReadDir(worktreeRoot); err == nil {
		prefix := "ralph-task-" + today + "-"
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
				runSeq++
			}
		}
	}

	candidateBranch := TaskBranchName(today, runSeq)
	candidateDir := filepath.Join(worktreeRoot, fmt.Sprintf("ralph-task-%s-%02d", today, runSeq))

	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		return fmt.Errorf("creating worktrees dir: %w", err)
	}

	r.gitCmd(r.projectDir, "worktree", "prune")

	// If a previous task-manager session crashed and left this exact
	// candidate dir behind, clear the directory and any stale branch
	// reference for THIS branch only. Never touch other worktrees.
	if _, err := os.Stat(candidateDir); err == nil {
		os.RemoveAll(candidateDir)
	}
	if r.refExists(r.projectDir, candidateBranch) {
		// Only delete the branch if no live worktree currently has it
		// checked out. If something does have it checked out, that's a
		// concurrent run with a colliding seq — bump runSeq instead.
		if wt := r.findWorktreeForBranch(r.projectDir, candidateBranch); wt == "" {
			_ = r.gitCmdErr(r.projectDir, "branch", "-D", candidateBranch)
		} else {
			runSeq++
			candidateBranch = TaskBranchName(today, runSeq)
			candidateDir = filepath.Join(worktreeRoot, fmt.Sprintf("ralph-task-%s-%02d", today, runSeq))
		}
	}

	defaultBranch := r.baseBranch
	r.gitCmdCtx(ctx, r.projectDir, "fetch", "origin", defaultBranch)

	if !r.refExists(r.projectDir, "origin/"+defaultBranch) &&
		r.refExists(r.projectDir, "HEAD") &&
		r.remoteExists() {
		r.gitCmdCtx(ctx, r.projectDir, "push", "-u", "origin", defaultBranch)
	}

	if err := r.gitCmdErr(r.projectDir, "worktree", "add", "-b", candidateBranch, candidateDir, "origin/"+defaultBranch); err != nil {
		if err := r.gitCmdErr(r.projectDir, "worktree", "add", "-b", candidateBranch, candidateDir, "HEAD"); err != nil {
			// Leave r.workDir unchanged — caller must treat this as fatal.
			return fmt.Errorf("failed to create task worktree: %w", err)
		}
	}

	r.workDir = candidateDir
	r.worktreeBranch = candidateBranch
	r.gitCmd(r.workDir, "config", "rebase.updateRefs", "true")
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Task worktree: %s (branch: %s)", r.workDir, r.worktreeBranch)

	// Intentionally do NOT write worktree_dir / worktree_branch to state.
	// Task manager worktrees are ephemeral and must not contaminate the
	// loop's resume state.

	return nil
}

// tryResumeWorktree resumes a worktree from a mid-task interrupt by reading
// the worktree_dir marker from state.json. It succeeds only when the marker is
// non-empty AND the directory exists on disk — no branch-name inference, no
// /next suffix checks, no commit-count heuristics. RemoveWorktree clears the
// marker at task completion, so a completed task never resumes here; only a
// genuinely interrupted mid-task run has the marker present. The actual branch
// setup happens later in checkoutExistingBranch once the task is known.
func (r *repo) tryResumeWorktree() error {
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
	if renamed, _ := r.state.Read("branch_renamed"); renamed == "true" {
		r.branchRenamed = true
	}

	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Resuming worktree: %s", r.workDir)
	return nil
}

// withStash stashes any uncommitted changes, runs fn, then reapplies the stash.
// Used by EnsureUpToDate and RebaseOntoDefaultBranch to avoid duplicating
// the stash/pop logic.
func (r *repo) withStash(stashMsg string, fn func()) {
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

// validateStackParent re-verifies that r.prevBranch still corresponds to a
// live remote branch with an open PR. The parent PR can merge and get its
// branch auto-deleted between setStackHead (at iteration start) and a later
// Ship/CreatePR call — GitHub then rejects CreatePR with HTTP 422
// base=invalid because the local remote-tracking ref is stale.
//
// Checks, in order:
//  1. `git fetch origin <prevBranch>` — if this errors, the remote branch
//     is gone (auto-deleted after merge). Clear prevBranch.
//  2. BranchIsAncestorOfMain(prevBranch) — if the branch's tip is in main's
//     history, its work has landed via a regular (non-squash) merge. Clear.
//     Squash-merged branches do NOT trigger this check (their original tip
//     is not on main); they're caught by the GitHub check below.
//  3. ListOpenPRBranches excludes prevBranch (only checked when gh is
//     available, signaled by a non-nil non-empty list) — the branch exists
//     on remote but its PR was closed/merged. Clear.
//
// Diverged branches that still hold unmerged work (e.g. after a pre-push
// rebase failure) are NOT cleared by check (2): main is not an ancestor of
// them, but neither is their tip an ancestor of main. The branch is still a
// valid stack parent — the next task chains onto it, and the merge pipeline
// re-aligns the chain via --update-refs.
//
// On a confirmed vanish, prevBranch is cleared so subsequent rebases and
// CreatePR calls target the default branch. On ambiguous signals (gh
// unavailable, transient fetch error with local state intact), prevBranch
// is left alone — better to retry with possibly-stale base than to wipe
// valid stack state on a transient failure.
func (r *repo) validateStackParent(ctx context.Context) {
	if r.prevBranch == "" {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		return
	}
	parent := r.prevBranch

	// A vanished parent is expected in a healthy stacked workflow (parent
	// merges, subsequent PRs advance to main) — log at info level so it
	// shows up in the narrative without looking like a failure.

	// (1) Confirm the remote branch still exists by fetching it.
	if err := r.FetchBranch(parent); err != nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git},
			"Stack parent %s no longer on remote — falling back to %s", parent, r.baseBranch)
		r.SetPrevBranch("")
		return
	}

	// (2) If the branch's tip is an ancestor of main, its work has landed
	// via a regular merge — the branch is redundant as a stack parent.
	if r.BranchIsAncestorOfMain(parent) {
		r.logger.Emit(logging.Opts{Domain: logging.Git},
			"Stack parent %s has landed on main — falling back to %s", parent, r.baseBranch)
		r.SetPrevBranch("")
		return
	}

	// (3) Final check against GitHub's PR state, when gh is available.
	// An empty/nil result means gh is unavailable — don't act on that.
	// This is the path that catches squash-merged parents (their PR is
	// closed; topology alone can't distinguish squash-merge from a diverged
	// branch with unmerged work).
	openBranches, err := r.ListOpenPRBranches(ctx)
	if err != nil || len(openBranches) == 0 {
		return
	}
	for _, b := range openBranches {
		if b == parent {
			return
		}
	}
	r.logger.Emit(logging.Opts{Domain: logging.Git},
		"Stack parent %s has no open PR — falling back to %s", parent, r.baseBranch)
	r.SetPrevBranch("")
}

// EnsureUpToDate fetches the latest base branch, stashes any uncommitted
// changes, rebases onto origin, and reapplies the stash. If rebase fails
// after auto-resolve, it aborts and returns an error — the caller decides
// what to do (e.g. push anyway and let GitHub handle merge conflicts).
func (r *repo) EnsureUpToDate(ctx context.Context) error {
	// Re-validate the stack parent before every sync. The parent PR may have
	// merged and been deleted since we last checked — see validateStackParent.
	r.validateStackParent(ctx)

	// Rebase onto stack head if set, otherwise the default branch.
	baseBranch := r.baseBranch
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
			baseBranch = r.baseBranch
			if err2 := r.gitCmdErrCtx(ctx, r.workDir, "fetch", "origin", baseBranch); err2 != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to fetch origin/%s: %v", baseBranch, err2)
				if isFetchTransportErr(err2) {
					return &TransportError{Op: "fetch", Err: err2}
				}
				return nil
			}
		} else {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to fetch origin/%s: %v", baseBranch, err)
			if isFetchTransportErr(err) {
				return &TransportError{Op: "fetch", Err: err}
			}
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

		err := rebasecontinue.Run(r.workDir, rebasecontinue.Options{
			Auto:       true,
			OntoTarget: "origin/" + baseBranch,
		})
		if err == nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Branch: baseBranch}, "Rebased onto origin/%s", baseBranch)
			return
		}

		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Rebase conflict with local work — stack diverged, continuing")
		r.gitCmd(r.workDir, "rebase", "--abort")
		result = &LocalRebaseConflictError{Branch: r.worktreeBranch, Base: baseBranch}
	})
	return result
}


// RenameBranchForTask renames the current branch to include a task slug.
// Records the previous branch name for stacked PR targeting.
// Only renames once per task (tracked by BranchRenamed).
// Returns an error if the rename fails — callers must abort the iteration.
func (r *repo) RenameBranchForTask(taskDesc, taskID string) error {
	if r.branchRenamed || r.worktreeBranch == "" || taskDesc == "" {
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
func (r *repo) RenameBranchTo(name string) {
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

// ResetToDefaultBranch resets the worktree to origin's default branch when
// the current branch has no local commits to lose. If local commits exist
// (in-progress work on a task branch), the reset is skipped so EnsureUpToDate
// can rebase them safely — preserving work across loop restarts.
// No-ops silently when the worktree is already at the target ref.
func (r *repo) ResetToDefaultBranch() {
	defaultBranch := r.baseBranch
	_ = r.gitCmdErr(r.workDir, "fetch", "origin", defaultBranch)
	target := "origin/" + defaultBranch
	if !r.refExists(r.workDir, target) {
		return
	}
	if r.gitOutput(r.workDir, "rev-parse", "HEAD") == r.gitOutput(r.workDir, "rev-parse", target) {
		return
	}
	// Preserve local work — let EnsureUpToDate rebase or abort safely.
	if localCommits := r.gitOutput(r.workDir, "rev-list", "--count", target+"..HEAD"); localCommits != "" && localCommits != "0" {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Branch: r.worktreeBranch}, "Preserving %s local commit(s) on %s — deferring to rebase", localCommits, r.worktreeBranch)
		return
	}
	r.gitCmd(r.workDir, "reset", "--hard", target)
	r.branchRenamed = false
	if r.state != nil {
		_ = r.state.Write("branch_renamed", "false")
	}
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Reset worktree to %s", target)
}

// forceResetToDefaultBranch hard-resets the worktree to origin/<default>,
// discarding any local commits ahead of main. Uncommitted working-tree
// changes are captured via `git stash create` (a dangling commit that does
// NOT appear in any worktree's `git stash list`) and re-applied after the
// reset. On apply conflict, the worktree is reset again to discard the WIP
// cleanly; the dangling stash commit is left for git gc.
//
// Used by SyncWorktreeBase when setStackHead returns prevBranch="" — the
// local commits are known ghosts from a drained stack and must be discarded,
// not preserved. All other callers must use ResetToDefaultBranch.
func (r *repo) forceResetToDefaultBranch() {
	defaultBranch := r.baseBranch
	_ = r.gitCmdErr(r.workDir, "fetch", "origin", defaultBranch)
	target := "origin/" + defaultBranch
	if !r.refExists(r.workDir, target) {
		return
	}
	if r.gitOutput(r.workDir, "rev-parse", "HEAD") == r.gitOutput(r.workDir, "rev-parse", target) {
		return
	}

	// Capture dirty state as a dangling commit — never mutates .git/refs/stash.
	stashSHA := r.gitOutput(r.workDir, "stash", "create")

	r.gitCmd(r.workDir, "reset", "--hard", target)
	r.branchRenamed = false
	if r.state != nil {
		_ = r.state.Write("branch_renamed", "false")
	}
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Reset worktree to %s", target)

	if stashSHA == "" {
		return
	}

	if err := r.gitCmdErr(r.workDir, "stash", "apply", stashSHA); err != nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
			"WIP could not be re-applied onto %s (stash %s) — discarded: %v", target, stashSHA, err)
		r.gitCmd(r.workDir, "reset", "--hard", target)
		return
	}
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Re-applied WIP from stash %s", stashSHA)
}

// SetPrevBranch sets the previous branch for stacked PR targeting and
// persists it to state so it survives process restarts.
func (r *repo) SetPrevBranch(branch string) {
	r.prevBranch = branch
	if r.state != nil {
		_ = r.state.Write("prev_branch", branch)
	}
}

// branchSafeToDelete returns true when the branch has no commits beyond the
// default branch (local or origin's), meaning all its work is already merged
// and the ref can be dropped without data loss. Unlike `git branch -d`, this
// ignores the upstream-tracking config (which is preserved across renames and
// gives false negatives when a task branch inherits origin/main as upstream).
func (r *repo) branchSafeToDelete(branch string) bool {
	defaultBranch := r.baseBranch
	for _, ref := range []string{"origin/" + defaultBranch, defaultBranch} {
		if !r.refExists(r.projectDir, ref) {
			continue
		}
		if r.gitCmdErr(r.projectDir, "merge-base", "--is-ancestor", branch, ref) == nil {
			return true
		}
	}
	return false
}

// PrepareForNextTask creates a fresh wip branch anchored at baseRef so the
// next task starts from the correct stack base. If baseRef is empty, the branch
// is created at the current HEAD (preserving prior behaviour for call sites
// that do not need stack-aware anchoring). RenameBranchForTask will rename the
// placeholder to a task-specific name before the first commit.
//
// Uncommitted changes are discarded only when switching to a different task;
// resuming the same task preserves in-progress work. When resuming the same
// task and the branch already has commits ahead of baseRef, the checkout is
// skipped entirely — the existing branch is kept as-is.
func (r *repo) PrepareForNextTask(nextTaskID, baseRef string) {
	r.branchRenamed = false
	if r.state != nil {
		_ = r.state.Write("branch_renamed", "false")
	}

	if r.worktreeBranch == "" {
		return
	}

	lastTaskID := ""
	if r.state != nil {
		lastTaskID, _ = r.state.Read("current_task_id")
	}
	if nextTaskID == "" || lastTaskID == "" || nextTaskID != lastTaskID {
		r.gitCmdErr(r.workDir, "checkout", ".")
		r.gitCmdErr(r.workDir, "clean", "-fd", "--exclude=.ralph/")
	}

	// Same-task resume: if the branch already has local commits ahead of
	// baseRef, keep the current branch — EnsureUpToDate will rebase them.
	if nextTaskID != "" && lastTaskID != "" && nextTaskID == lastTaskID && baseRef != "" {
		countStr := r.gitOutput(r.workDir, "rev-list", "--count", baseRef+"..HEAD")
		var count int
		fmt.Sscanf(countStr, "%d", &count)
		if count > 0 {
			return
		}
	}

	// Fetch the base so the subsequent checkout can reference it.
	if baseRef != "" {
		baseBranch := strings.TrimPrefix(baseRef, "origin/")
		_ = r.gitCmdErr(r.projectDir, "fetch", "origin", baseBranch)
	}

	newBranch := WipBranchName()
	oldBranch := r.worktreeBranch
	if oldBranch == newBranch {
		return
	}

	var checkoutErr error
	if baseRef != "" {
		checkoutErr = r.gitCmdErr(r.workDir, "checkout", "-B", newBranch, baseRef)
	} else {
		checkoutErr = r.gitCmdErr(r.workDir, "checkout", "-B", newBranch)
	}

	if checkoutErr == nil {
		r.worktreeBranch = newBranch
		if r.state != nil {
			_ = r.state.Write("worktree_branch", newBranch)
		}
		// Set tracking explicitly so git pull --rebase in post-task hooks
		// works regardless of branch.autoSetupMerge in the user's environment.
		_ = r.gitCmdErr(r.projectDir, "branch", "--set-upstream-to", "origin/"+r.resolveBaseBranch(), newBranch)
		// Only delete if oldBranch has no commits beyond the default branch —
		// i.e. all its work is already merged. Otherwise preserve it so
		// in-progress work from a prior session isn't silently destroyed.
		if r.branchSafeToDelete(oldBranch) {
			if err := r.gitCmdErr(r.projectDir, "branch", "-D", oldBranch); err == nil {
				r.logger.Emit(logging.Opts{Domain: logging.Git}, "Deleted local branch %s", oldBranch)
			}
		} else {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Preserving local branch %s — unmerged commits ahead of %s", oldBranch, r.baseBranch)
		}
	}
}

// isFetchTransportErr returns true when err is a transient remote-transport
// failure from a git fetch — indicated by exit status 128, which git uses for
// network errors, auth failures, and unreachable remotes. Local errors (bad
// ref, wrong branch name) use other exit codes.
func isFetchTransportErr(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 128
}

// SquashToOneCommit squashes all commits since baseSHA into a single commit
// with the given message. No-op if there is already exactly one commit
// ahead of base. Returns an error if there are no commits to squash.
func (r *repo) SquashToOneCommit(baseSHA, message string) error {
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

// RemoveWorktree force-removes the loop's worktree, deletes its branch, and
// clears the worktree state markers so the next startup does not resume a
// gone directory.
func (r *repo) RemoveWorktree() {
	r.gitCmd(r.projectDir, "worktree", "remove", "--force", r.workDir)
	r.gitCmd(r.projectDir, "branch", "-D", r.worktreeBranch)
	if r.state != nil {
		_ = r.state.Write("worktree_dir", "")
		_ = r.state.Write("worktree_branch", "")
		_ = r.state.Write("branch_renamed", "false")
	}
	r.workDir = r.projectDir
	r.worktreeBranch = ""
	r.branchRenamed = false
}

// RemoveWorktreeForBranch removes the local worktree directory checked out on
// branch and deletes the local branch ref. Best-effort: logs a warning on
// failure and never aborts the caller. r.projectDir's own worktree is never
// removed.
func (r *repo) RemoveWorktreeForBranch(branch string) {
	worktreeDir := r.findWorktreeForBranch(r.projectDir, branch)
	if worktreeDir != "" && worktreeDir != r.projectDir {
		if err := r.gitCmdErr(r.projectDir, "worktree", "remove", "--force", worktreeDir); err != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
				"Failed to remove worktree %s for branch %s: %v", worktreeDir, branch, err)
		}
	}
	if err := r.gitCmdErr(r.projectDir, "branch", "-D", branch); err != nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
			"Failed to delete local branch %s: %v", branch, err)
	}
}

// TagTaskStart creates a lightweight git tag marking the start of a task iteration.
// The tag name is task/{taskID}/start when a backend ID is available,
// or task/{seq}-{slug}/start derived from the current branch name.
func (r *repo) TagTaskStart(taskID string) {
	tag := r.taskTag(taskID, "start")
	if tag == "" {
		return
	}
	if err := r.gitCmdErr(r.workDir, "tag", "-f", tag); err == nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Tag: %s", tag)
	}
}

// TagTaskEnd creates a lightweight git tag marking the end of a task iteration.
func (r *repo) TagTaskEnd(taskID string) {
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
func (r *repo) taskTag(taskID, suffix string) string {
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
