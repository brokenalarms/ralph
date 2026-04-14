package git

import (
	"context"
	"strings"

	"github.com/brokenalarms/ralph/internal/logging"
)

// RebaseRecovery represents the user's chosen recovery action when rebase
// fails due to squash-merged branches.
type RebaseRecovery int

const (
	RebaseAbort         RebaseRecovery = iota
	RebaseFreshWorktree
	RebaseManualResolve
)

// RebaseConflictError signals that a rebase failed and can potentially be
// recovered by recreating the worktree from main. Cause describes what
// went wrong (squash-merge conflicts vs real conflicts).
type RebaseConflictError struct {
	Cause string
}

func (e *RebaseConflictError) Error() string {
	return e.Cause
}

// autoResolveAndContinue attempts to resolve rebase conflicts mechanically
// and continue. For each conflicted file: if only one side changed from base,
// take that side. If both changed but ours is a subset of theirs, take theirs.
// Returns true if the rebase completed successfully.
func (r *repo) autoResolveAndContinue(ctx context.Context, defaultBranch string) bool {
	for i := 0; i < 50; i++ { // max steps to prevent infinite loop
		conflicted := r.gitOutput(r.workDir, "diff", "--name-only", "--diff-filter=U")
		if conflicted == "" {
			// No conflicts — try to continue
			if err := r.gitCmdErrCtx(ctx, r.workDir, "rebase", "--continue"); err == nil {
				return true
			}
			// Might be done
			if !r.mRebaseInProgress() {
				return true
			}
			continue
		}

		resolvedAny := false
		for _, f := range strings.Split(conflicted, "\n") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}

			ours := r.gitOutput(r.workDir, "show", ":2:"+f)
			theirs := r.gitOutput(r.workDir, "show", ":3:"+f)
			base := r.gitOutput(r.workDir, "show", ":1:"+f)

			if ours == theirs {
				r.logger.Emit(logging.Opts{Domain: logging.Git}, "Auto-resolved (identical): %s", f)
				r.gitCmd(r.workDir, "checkout", "--theirs", f)
				r.gitCmd(r.workDir, "add", f)
				resolvedAny = true
			} else if ours == base {
				r.logger.Emit(logging.Opts{Domain: logging.Git}, "Auto-resolved (only theirs changed): %s", f)
				r.gitCmd(r.workDir, "checkout", "--theirs", f)
				r.gitCmd(r.workDir, "add", f)
				resolvedAny = true
			} else if theirs == base {
				r.logger.Emit(logging.Opts{Domain: logging.Git}, "Auto-resolved (only ours changed): %s", f)
				r.gitCmd(r.workDir, "checkout", "--ours", f)
				r.gitCmd(r.workDir, "add", f)
				resolvedAny = true
			} else {
				// Both changed — check if ours is subset of theirs
				if strings.Contains(theirs, ours) || isSubsetByLines(base, ours, theirs) {
					r.logger.Emit(logging.Opts{Domain: logging.Git}, "Auto-resolved (ours is subset of theirs): %s", f)
					r.gitCmd(r.workDir, "checkout", "--theirs", f)
					r.gitCmd(r.workDir, "add", f)
					resolvedAny = true
				} else {
					r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Real conflict in %s — cannot auto-resolve", f)
					return false
				}
			}
		}

		if !resolvedAny {
			return false
		}

		// Continue the rebase
		if err := r.gitCmdErrCtx(ctx, r.workDir, "rebase", "--continue"); err != nil {
			if !r.mRebaseInProgress() {
				return true
			}
			// Another step with conflicts — loop again
			continue
		}
		return true
	}
	return false
}

// isSubsetByLines checks if every line ours added (relative to base) is
// present in theirs. If so, theirs is a superset and wins.
func isSubsetByLines(base, ours, theirs string) bool {
	baseLines := make(map[string]bool)
	for _, line := range strings.Split(base, "\n") {
		baseLines[line] = true
	}

	theirsLines := make(map[string]bool)
	for _, line := range strings.Split(theirs, "\n") {
		theirsLines[line] = true
	}

	for _, line := range strings.Split(ours, "\n") {
		if !baseLines[line] && !theirsLines[line] {
			return false
		}
	}
	return true
}

// RebaseOntoDefaultBranch delegates to EnsureUpToDate, which is the single
// sync point for all rebase operations.
func (r *repo) RebaseOntoDefaultBranch(ctx context.Context) error {
	return r.EnsureUpToDate(ctx)
}
