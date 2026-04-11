package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/git/rebasecontinue"
	"github.com/brokenalarms/ralph/internal/logging"
)

// RebaseStackOpts configures a RebaseStack call.
type RebaseStackOpts struct {
	TopBranch   string   // the top PR's branch name
	BaseBranch  string   // the base branch to rebase onto (e.g. "main")
	TopPR       int      // top PR number, used in error messages for manual resolution
	AllBranches []string // all branch names in the stack, bottom to top
}

// RebaseStack creates a temp worktree on TopBranch, rebases the full stack
// with --update-refs onto BaseBranch, then force-pushes all AllBranches.
// If a worktree from a previous conflict resolution already exists and the
// branches are already rebased, skips the rebase and goes straight to push.
func (r *Repo) RebaseStack(ctx context.Context, opts RebaseStackOpts) error {
	slug := strings.ReplaceAll(opts.TopBranch, "/", "-")
	wtDir := filepath.Join(r.ralphDir, "worktrees", "merge-"+slug)
	tmpBranch := "ralph-merge/" + slug

	// Check if worktree from a previous run exists. Verify branches are
	// already rebased — stale worktrees from interrupted runs must be recreated.
	worktreeReady := false
	if _, err := os.Stat(filepath.Join(wtDir, ".git")); err == nil {
		bottomBranch := opts.AllBranches[0]
		if r.gitCmdErrCtx(ctx, wtDir, "merge-base", "--is-ancestor", "origin/"+opts.BaseBranch, bottomBranch) != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
				"Stale worktree found — branches not rebased onto %s, recreating", opts.BaseBranch)
			r.gitCmdCtx(ctx, r.ProjectDir, "worktree", "remove", "--force", wtDir)
			r.gitCmdCtx(ctx, r.ProjectDir, "worktree", "prune")
		} else {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Resuming from existing worktree: %s", wtDir)
			worktreeReady = true
		}
	}

	if !worktreeReady {
		os.RemoveAll(wtDir)
		r.gitCmdCtx(ctx, r.ProjectDir, "worktree", "prune")
		r.gitCmdCtx(ctx, r.ProjectDir, "branch", "-D", tmpBranch)

		// Fetch main and all stack branches so --update-refs has current refs.
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Fetching %s and %d stack branches...", opts.BaseBranch, len(opts.AllBranches))
		r.gitCmdCtx(ctx, r.ProjectDir, "fetch", "origin", opts.BaseBranch)
		for _, b := range opts.AllBranches {
			r.gitCmdCtx(ctx, r.ProjectDir, "fetch", "origin", b)
		}

		// Create worktree on the top branch.
		out, err := r.run().Run(ctx, r.ProjectDir, "worktree", "add", "-b", tmpBranch, wtDir, "origin/"+opts.TopBranch)
		if err != nil {
			return fmt.Errorf("worktree setup failed: %s", out)
		}

		// Set up local tracking branches for --update-refs.
		for _, b := range opts.AllBranches {
			r.gitCmdCtx(ctx, wtDir, "branch", "-f", b, "origin/"+b)
		}

		// Rebase with --update-refs onto base branch.
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Rebasing with --update-refs onto origin/%s...", opts.BaseBranch)
		if r.gitCmdErrCtx(ctx, wtDir, "rebase", "--update-refs", "origin/"+opts.BaseBranch) != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Rebase conflict — attempting auto-resolve...")
			if autoErr := rebasecontinue.Run(wtDir, rebasecontinue.Options{Auto: true}); autoErr != nil {
				r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error},
					"Rebase has conflicts — resolve manually in:\n  %s", wtDir)
				r.logger.Emit(logging.Opts{Domain: logging.Git},
					"Then run: cd %s && git-rebase-continue", wtDir)
				r.logger.Emit(logging.Opts{Domain: logging.Git},
					"Then re-run: ralph merge %d", opts.TopPR)
				r.logger.Emit(logging.Opts{}, "\n%s", autoErr.Error())
				return fmt.Errorf("rebase conflicts in %s: %w", wtDir, autoErr)
			}
		}
	}

	cleanup := func() {
		r.gitCmdCtx(ctx, r.ProjectDir, "worktree", "remove", "--force", wtDir)
		r.gitCmdCtx(ctx, r.ProjectDir, "branch", "-D", tmpBranch)
	}

	// Force-push all branches.
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Force-pushing %d branches...", len(opts.AllBranches))
	for _, b := range opts.AllBranches {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "  Pushing %s", b)
		if err := r.gitCmdErrCtx(ctx, wtDir, "push", "--force", "origin", b); err != nil {
			cleanup()
			return fmt.Errorf("push failed for %s: %w", b, err)
		}
	}

	cleanup()
	r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Success}, "All branches rebased and pushed")
	return nil
}

// RebaseBranchOntoRemote fetches branch and baseBranch from origin, checks out
// origin/branch in detached HEAD state, rebases onto origin/baseBranch, and
// force-pushes the result back to branch with --force-with-lease.
// Attempts auto-resolution of mechanical conflicts before returning an error.
// Checks out baseBranch before returning.
func (r *Repo) RebaseBranchOntoRemote(ctx context.Context, branch, baseBranch string) error {
	r.gitCmdCtx(ctx, r.ProjectDir, "fetch", "origin", baseBranch)
	r.gitCmdCtx(ctx, r.ProjectDir, "fetch", "origin", branch)
	r.gitCmdCtx(ctx, r.ProjectDir, "checkout", "origin/"+branch)

	if r.gitCmdErrCtx(ctx, r.ProjectDir, "rebase", "origin/"+baseBranch) != nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Rebase conflict on %s — attempting auto-resolve...", branch)
		if autoErr := rebasecontinue.Run(r.ProjectDir, rebasecontinue.Options{Auto: true}); autoErr != nil {
			return fmt.Errorf("rebase conflicts on %s: %w", branch, autoErr)
		}
	}

	if _, pushErr := r.run().Run(ctx, r.ProjectDir, "push", "--force-with-lease", "origin", "HEAD:"+branch); pushErr != nil {
		return fmt.Errorf("force-push failed for %s: %w", branch, pushErr)
	}
	r.gitCmdCtx(ctx, r.ProjectDir, "checkout", baseBranch)
	return nil
}

// MergeStackPR merges the PR with the given number using opts and returns the
// full result — merged / conflict / blocked / failed / message.
func (r *Repo) MergeStackPR(prNumber int, opts MergeOpts) MergeResult {
	return r.gh().MergePR(prNumber, r.RemoteURL(), opts)
}

// ResetBranchToRemote fetches origin/<branch>, checks out branch, and resets
// --hard to origin/<branch>. Used after each PR merge to sync the local default
// branch with the updated remote state.
func (r *Repo) ResetBranchToRemote(ctx context.Context, branch string) {
	r.gitCmdCtx(ctx, r.ProjectDir, "fetch", "origin", branch)
	r.gitCmdCtx(ctx, r.ProjectDir, "checkout", branch)
	r.gitCmdCtx(ctx, r.ProjectDir, "reset", "--hard", "origin/"+branch)
}
