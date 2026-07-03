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
// git_stack.go (rebaseInTempWorktree, shared by RebaseStack and
// RebaseBranchOntoRemote)
//   worktree remove / prune / add  worktree mgmt      allowed
//   branch -D                      branch delete      allowed
//   fetch origin <branch>          fetch              allowed
//   all checkout/rebase/push run in the temp worktree; only fetch and
//   worktree/branch ops touch projectDir
//
// git_stack.go (ResetBranchToRemote) — checkout-aware: uses merge --ff-only
//   when branch is checked out (worktree update), update-ref otherwise

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
	return r.rebaseInTempWorktree(ctx, rebaseSpec{
		kind:         "merge",
		branch:       opts.TopBranch,
		baseBranch:   opts.BaseBranch,
		pushBranches: opts.AllBranches,
		updateRefs:   true,
		allowResume:  true,
		topPR:        opts.TopPR,
	})
}

// RebaseBranchOntoRemote fetches branch and baseBranch from origin, then
// creates a temp worktree under ralphDir/worktrees/rebase-<slug> to perform
// the rebase and force-push. projectDir's working tree is never touched.
// Attempts auto-resolution of mechanical conflicts before returning an error.
// The temp worktree is removed on success and on failure.
func (r *repo) RebaseBranchOntoRemote(ctx context.Context, branch, baseBranch string) error {
	return r.rebaseInTempWorktree(ctx, rebaseSpec{
		kind:           "rebase",
		branch:         branch,
		baseBranch:     baseBranch,
		pushBranches:   []string{branch},
		forceWithLease: true,
	})
}

// rebaseSpec configures a single rebaseInTempWorktree run. branch is checked
// out into the temp worktree; pushBranches is both the set fetched from
// origin before the rebase and the set force-pushed after it.
type rebaseSpec struct {
	kind           string   // "merge" or "rebase" — worktree dir / temp branch naming
	branch         string   // branch checked out into the temp worktree
	baseBranch     string   // base branch to rebase onto
	pushBranches   []string // branches fetched before, and force-pushed after, the rebase
	updateRefs     bool     // pass --update-refs to git rebase and pre-seed local tracking branches for each pushBranch
	forceWithLease bool     // push --force-with-lease origin HEAD:<branch> for the single pushBranch, instead of --force origin <branch> per pushBranch
	allowResume    bool     // resume an existing worktree when pushBranches[0] is already rebased onto baseBranch
	topPR          int      // when nonzero, log stack-specific manual-resolution guidance referencing this PR on unresolved conflict
}

// rebaseInTempWorktree owns the full temp-worktree lifecycle shared by
// RebaseStack and RebaseBranchOntoRemote: derive the worktree dir and temp
// branch from spec.branch, clean up any prior state, fetch spec.baseBranch
// and spec.pushBranches from origin, create the worktree, rebase (with
// rebasecontinue auto-resolve on conflict), force-push spec.pushBranches,
// then remove the temp worktree and branch. projectDir's working tree is
// never touched — all checkout/rebase/push run inside the temp worktree.
func (r *repo) rebaseInTempWorktree(ctx context.Context, spec rebaseSpec) error {
	slug := strings.ReplaceAll(spec.branch, "/", "-")
	wtDir := filepath.Join(r.ralphDir, "worktrees", spec.kind+"-"+slug)
	tmpBranch := "ralph-" + spec.kind + "/" + slug

	cleanup := func() {
		r.gitCmdCtx(ctx, r.projectDir, "worktree", "remove", "--force", wtDir)
		r.gitCmdCtx(ctx, r.projectDir, "branch", "-D", tmpBranch)
	}

	worktreeReady := false
	if spec.allowResume {
		// Check if a worktree from a previous run exists. Verify branches
		// are already rebased — stale worktrees from interrupted runs must
		// be recreated.
		if _, err := os.Stat(filepath.Join(wtDir, ".git")); err == nil {
			bottomBranch := spec.pushBranches[0]
			if !r.isAncestorCtx(ctx, wtDir, "origin/"+spec.baseBranch, bottomBranch) {
				r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
					"Stale worktree found — branches not rebased onto %s, recreating", spec.baseBranch)
				r.gitCmdCtx(ctx, r.projectDir, "worktree", "remove", "--force", wtDir)
				r.gitCmdCtx(ctx, r.projectDir, "worktree", "prune")
			} else {
				r.logger.Emit(logging.Opts{Domain: logging.Git}, "Resuming from existing worktree: %s", wtDir)
				worktreeReady = true
			}
		}
	}

	if !worktreeReady {
		os.RemoveAll(wtDir)
		r.gitCmdCtx(ctx, r.projectDir, "worktree", "prune")
		r.gitCmdCtx(ctx, r.projectDir, "branch", "-D", tmpBranch)

		// Fetch the base and all push branches so the rebase (and
		// --update-refs, when requested) sees current refs.
		if spec.updateRefs {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Fetching %s and %d stack branches...", spec.baseBranch, len(spec.pushBranches))
		}
		r.gitCmdCtx(ctx, r.projectDir, "fetch", "origin", spec.baseBranch)
		for _, b := range spec.pushBranches {
			r.gitCmdCtx(ctx, r.projectDir, "fetch", "origin", b)
		}

		out, err := r.run().Run(ctx, r.projectDir, "worktree", "add", "-b", tmpBranch, wtDir, "origin/"+spec.branch)
		if err != nil {
			return fmt.Errorf("worktree setup failed: %s", out)
		}

		rebaseArgs := []string{"rebase"}
		if spec.updateRefs {
			// Set up local tracking branches for --update-refs.
			for _, b := range spec.pushBranches {
				r.gitCmdCtx(ctx, wtDir, "branch", "-f", b, "origin/"+b)
			}
			rebaseArgs = append(rebaseArgs, "--update-refs")
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Rebasing with --update-refs onto origin/%s...", spec.baseBranch)
		}
		rebaseArgs = append(rebaseArgs, "origin/"+spec.baseBranch)

		if r.gitCmdErrCtx(ctx, wtDir, rebaseArgs...) != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Rebase conflict on %s — attempting auto-resolve...", spec.branch)
			if autoErr := rebasecontinue.Run(wtDir, rebasecontinue.Options{Auto: true}); autoErr != nil {
				if spec.topPR != 0 {
					// Resumable case: leave the worktree in place so the
					// conflict can be resolved manually and the run resumed.
					r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error},
						"Rebase has conflicts — resolve manually in:\n  %s", wtDir)
					r.logger.Emit(logging.Opts{Domain: logging.Git},
						"Then run: cd %s && git-rebase-continue", wtDir)
					r.logger.Emit(logging.Opts{Domain: logging.Git},
						"Then re-run: ralph merge %d", spec.topPR)
					r.logger.Emit(logging.Opts{}, "\n%s", autoErr.Error())
				} else {
					cleanup()
				}
				return fmt.Errorf("rebase conflicts on %s: %w", spec.branch, autoErr)
			}
		}
	}

	if spec.forceWithLease {
		branch := spec.pushBranches[0]
		if _, err := r.run().Run(ctx, wtDir, "push", "--force-with-lease", "origin", "HEAD:"+branch); err != nil {
			cleanup()
			return fmt.Errorf("push failed for %s: %w", branch, err)
		}
	} else {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Force-pushing %d branches...", len(spec.pushBranches))
		for _, b := range spec.pushBranches {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "  Pushing %s", b)
			if err := r.gitCmdErrCtx(ctx, wtDir, "push", "--force", "origin", b); err != nil {
				cleanup()
				return fmt.Errorf("push failed for %s: %w", b, err)
			}
		}
	}

	cleanup()
	if !spec.forceWithLease {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Success}, "All branches rebased and pushed")
	}
	return nil
}

// MergeStackPR merges the PR with the given number using opts and returns the
// full result — merged / conflict / blocked / failed / message.
func (r *repo) MergeStackPR(ctx context.Context, prNumber int, opts MergeOpts) MergeResult {
	return r.github.MergePR(ctx, prNumber, r.RemoteURL(), opts)
}

// ResetBranchToRemote fetches origin/<branch> and syncs the local branch ref.
// When <branch> is currently checked out in projectDir, it fast-forwards the
// working tree via merge --ff-only to keep HEAD, index, and worktree in sync.
// When not checked out, it updates only the ref via update-ref (no worktree
// mutation). If fast-forward is not possible (diverged branch), a warning is
// logged and projectDir is left untouched.
func (r *repo) ResetBranchToRemote(ctx context.Context, branch string) {
	r.gitCmdCtx(ctx, r.projectDir, "fetch", "origin", branch)
	current, _ := r.run().Run(ctx, r.projectDir, "symbolic-ref", "--short", "HEAD")
	if current == branch {
		if err := r.gitCmdErrCtx(ctx, r.projectDir, "merge", "--ff-only", "origin/"+branch); err != nil {
			r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
				"ResetBranchToRemote: cannot fast-forward %s to origin/%s — leaving projectDir untouched", branch, branch)
		}
		return
	}
	r.gitCmdCtx(ctx, r.projectDir, "update-ref", "refs/heads/"+branch, "origin/"+branch)
}
