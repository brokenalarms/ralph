package loop

import (
	"context"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// tryFixCI spawns a fix agent to address CI failures, force-pushes the
// new commits, and returns a CIFixResult:
//   - CIFixApplied:   fix was pushed, ready for merge retry
//   - CIFixNoCommits: agent ran but made no commits (infrastructure failure)
//   - CIFixFailed:    agent error or push failure
func tryFixCI(ctx context.Context, g git.GitOps, v *Verifier, logger *logging.Logger, ciErr *git.CIFailureError, nextTask, workDir, rawLogPath string) git.CIFixResult {
	ciLog := g.GetCIFailureLog(ciErr.PRNumber)
	headBefore := g.HeadRev()
	if !v.TryFixCI(ctx, ciLog, ciErr, nextTask, workDir, rawLogPath) {
		return git.CIFixFailed
	}

	// Fix agent may leave uncommitted changes — commit them before checking HEAD.
	if g.HasUncommittedChanges() {
		logger.Emit(logging.Opts{Domain: logging.Git}, "Fix agent left uncommitted changes — auto-committing")
		g.CommitAll("fix: auto-commit CI fix agent changes")
	}

	headAfter := g.HeadRev()
	if headBefore == headAfter {
		logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Fix agent made no new commits — likely infrastructure failure")
		return git.CIFixNoCommits
	}

	logger.Emit(logging.Opts{Domain: logging.Git}, "Fix agent committed — pushing")
	if err := g.Push(ctx); err != nil {
		logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Push after CI fix failed: %v", err)
		return git.CIFixFailed
	}
	return git.CIFixApplied
}

// tryFixConflict spawns a conflict resolution agent, force-pushes the
// resolved commits, and returns true if the fix was pushed (ready for
// merge retry). Mirrors tryFixCI.
func tryFixConflict(ctx context.Context, g git.GitOps, v *Verifier, logger *logging.Logger, backend tasks.Backend, taskID, nextTask, workDir, rawLogPath string) bool {
	conflictDiff := g.ConflictDiff()
	beadDesc := getBeadDescription(backend, taskID)
	headBefore := g.HeadRev()
	if !v.TryFixConflict(ctx, conflictDiff, beadDesc, nextTask, workDir, rawLogPath) {
		return false
	}

	headAfter := g.HeadRev()
	if headBefore == headAfter {
		logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Conflict agent made no new commits — nothing to push")
		return false
	}

	if err := g.Push(ctx); err != nil {
		logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Push after conflict resolution failed: %v", err)
		return false
	}
	return true
}

func getBeadDescription(backend tasks.Backend, taskID string) string {
	if taskID == "" || backend == nil {
		return ""
	}
	desc, err := backend.GetDescription(taskID)
	if err != nil {
		return ""
	}
	return desc
}
