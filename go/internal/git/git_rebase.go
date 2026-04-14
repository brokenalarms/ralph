package git

import "context"

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

// RebaseOntoDefaultBranch delegates to EnsureUpToDate, which is the single
// sync point for all rebase operations.
func (r *repo) RebaseOntoDefaultBranch(ctx context.Context) error {
	return r.EnsureUpToDate(ctx)
}
