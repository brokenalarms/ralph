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

// projectDir working-tree mutation audit — all sites in the git package that
// pass r.projectDir as workdir, classified as allowed or violation:
//
// runner.go
//   remote get-url origin          read-only          allowed
//   fetch --prune origin           fetch              allowed
//   branch -r --list ...           read-only          allowed
//
// git.go (Init / SetupWorktree)
//   diff --quiet / diff --cached   read-only          allowed
//   worktree prune                 worktree mgmt      allowed
//   worktree remove                worktree mgmt      allowed
//   worktree add                   worktree mgmt      allowed
//   branch -D                      branch delete      allowed
//   fetch origin <branch>          fetch              allowed
//   push -u origin <branch>        initial-push-only  allowed (one-time setup)
//   merge-base --is-ancestor       read-only          allowed
//
// git_helpers.go (ValidateRemoteBranch)
//   fetch origin <branch>          fetch              allowed
//
// git_helpers.go (EnsureGitignored)
//   add .gitignore / commit        intentional edit   allowed (gitignore management)
//
// git_helpers.go (PruneOrphanedWorktrees)
//   worktree prune / list          worktree mgmt      allowed
//
// git_merge.go (PostMergeUpdateMain / FlushUnpushedWork)
//   fetch origin <branch>          fetch              allowed
//   branch -D                      branch delete      allowed
//
// git_stack.go (RebaseStack)
//   worktree remove / prune / add  worktree mgmt      allowed
//   branch -D                      branch delete      allowed
//   fetch origin <branch>          fetch              allowed
//
// git_stack.go (RebaseBranchOntoRemote) — FIXED: all checkout/rebase/push
//   now run in a temp worktree; only fetch and worktree/branch ops remain in projectDir
//
// git_stack.go (ResetBranchToRemote) — FIXED: checkout + reset replaced with
//   fetch + update-ref (plumbing only, no working-tree mutation)

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
func (r *repo) RebaseStack(ctx context.Context, opts RebaseStackOpts) error {
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
			r.gitCmdCtx(ctx, r.projectDir, "worktree", "remove", "--force", wtDir)
			r.gitCmdCtx(ctx, r.projectDir, "worktree", "prune")
		} else {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Resuming from existing worktree: %s", wtDir)
			worktreeReady = true
		}
	}

	if !worktreeReady {
		os.RemoveAll(wtDir)
		r.gitCmdCtx(ctx, r.projectDir, "worktree", "prune")
		r.gitCmdCtx(ctx, r.projectDir, "branch", "-D", tmpBranch)

		// Fetch main and all stack branches so --update-refs has current refs.
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Fetching %s and %d stack branches...", opts.BaseBranch, len(opts.AllBranches))
		r.gitCmdCtx(ctx, r.projectDir, "fetch", "origin", opts.BaseBranch)
		for _, b := range opts.AllBranches {
			r.gitCmdCtx(ctx, r.projectDir, "fetch", "origin", b)
		}

		// Create worktree on the top branch.
		out, err := r.run().Run(ctx, r.projectDir, "worktree", "add", "-b", tmpBranch, wtDir, "origin/"+opts.TopBranch)
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
		r.gitCmdCtx(ctx, r.projectDir, "worktree", "remove", "--force", wtDir)
		r.gitCmdCtx(ctx, r.projectDir, "branch", "-D", tmpBranch)
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

// RebaseBranchOntoRemote fetches branch and baseBranch from origin, then
// creates a temp worktree under ralphDir/worktrees/rebase-<slug> to perform
// the rebase and force-push. projectDir's working tree is never touched.
// Attempts auto-resolution of mechanical conflicts before returning an error.
// The temp worktree is removed on success and on failure.
func (r *repo) RebaseBranchOntoRemote(ctx context.Context, branch, baseBranch string) error {
	r.gitCmdCtx(ctx, r.projectDir, "fetch", "origin", baseBranch)
	r.gitCmdCtx(ctx, r.projectDir, "fetch", "origin", branch)

	slug := strings.ReplaceAll(branch, "/", "-")
	wtDir := filepath.Join(r.ralphDir, "worktrees", "rebase-"+slug)
	tmpBranch := "ralph-rebase/" + slug

	os.RemoveAll(wtDir)
	r.gitCmdCtx(ctx, r.projectDir, "worktree", "prune")
	r.gitCmdCtx(ctx, r.projectDir, "branch", "-D", tmpBranch)

	out, err := r.run().Run(ctx, r.projectDir, "worktree", "add", "-b", tmpBranch, wtDir, "origin/"+branch)
	if err != nil {
		return fmt.Errorf("worktree setup failed: %s", out)
	}

	cleanup := func() {
		r.gitCmdCtx(ctx, r.projectDir, "worktree", "remove", "--force", wtDir)
		r.gitCmdCtx(ctx, r.projectDir, "branch", "-D", tmpBranch)
	}

	if r.gitCmdErrCtx(ctx, wtDir, "rebase", "origin/"+baseBranch) != nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Rebase conflict on %s — attempting auto-resolve...", branch)
		if autoErr := rebasecontinue.Run(wtDir, rebasecontinue.Options{Auto: true}); autoErr != nil {
			cleanup()
			return fmt.Errorf("rebase conflicts on %s: %w", branch, autoErr)
		}
	}

	if _, pushErr := r.run().Run(ctx, wtDir, "push", "--force-with-lease", "origin", "HEAD:"+branch); pushErr != nil {
		cleanup()
		return fmt.Errorf("force-push failed for %s: %w", branch, pushErr)
	}

	cleanup()
	return nil
}

// MergeStackPR merges the PR with the given number using opts and returns the
// full result — merged / conflict / blocked / failed / message.
func (r *repo) MergeStackPR(ctx context.Context, prNumber int, opts MergeOpts) MergeResult {
	return r.github.MergePR(ctx, prNumber, r.RemoteURL(), opts)
}

// ResetBranchToRemote fetches origin/<branch> and updates the local branch ref
// via git update-ref — no checkout, no working-tree mutation in projectDir.
// Used after each PR merge to sync the local default branch with the remote state.
func (r *repo) ResetBranchToRemote(ctx context.Context, branch string) {
	r.gitCmdCtx(ctx, r.projectDir, "fetch", "origin", branch)
	r.gitCmdCtx(ctx, r.projectDir, "update-ref", "refs/heads/"+branch, "origin/"+branch)
}
