package loop

import (
	"context"

	"github.com/brokenalarms/ralph/internal/agent"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/verify"
)

// onSignal delegates to the Verifier for post-signal verification.
func (l *Loop) onSignal(p signalParams) bool {
	return l.verifier.OnSignal(p)
}

// verifyCompletion delegates to the Verifier for the legacy/fallback path.
// Test overrides via verifyFunc bypass the Verifier entirely.
func (l *Loop) verifyCompletion(ctx context.Context, headBefore string) (bool, string) {
	if l.verifyFunc != nil {
		return l.verifyFunc(ctx, l.git.GetWorkDir(), headBefore)
	}
	return l.verifier.VerifyCompletion(ctx, l.git.GetWorkDir(), headBefore)
}

// runPreIterationTests delegates to the Verifier.
func (l *Loop) runPreIterationTests(ctx context.Context) string {
	return l.verifier.RunPreIterationTests(ctx)
}

// tryFixCI spawns a fix agent to address CI failures, force-pushes the
// new commits, and returns a CIFixResult:
//   - CIFixApplied:   fix was pushed, ready for merge retry
//   - CIFixNoCommits: agent ran but made no commits (infrastructure failure)
//   - CIFixFailed:    agent error or push failure
func (l *Loop) tryFixCI(ctx context.Context, ciErr *git.CIFailureError, taskID, nextTask, workDir, rawLogPath string) git.CIFixResult {
	ciLog := l.getCIFailureLog(ciErr.PRNumber)
	headBefore := l.git.HeadRev()
	if !l.verifier.TryFixCI(ctx, ciLog, ciErr, nextTask, workDir, rawLogPath) {
		return git.CIFixFailed
	}

	// Fix agent may leave uncommitted changes — commit them before checking HEAD.
	if l.git.HasUncommittedChanges() {
		l.logger.Log("git", "Fix agent left uncommitted changes — auto-committing")
		l.git.CommitAll("fix: auto-commit CI fix agent changes")
	}

	headAfter := l.git.HeadRev()
	if headBefore == headAfter {
		l.logger.Warn("git", "Fix agent made no new commits — likely infrastructure failure")
		return git.CIFixNoCommits
	}

	l.logger.Log("git", "Fix agent committed — pushing")
	if err := l.git.Push(ctx); err != nil {
		l.logger.Warn("git", "Push after CI fix failed: %v", err)
		return git.CIFixFailed
	}
	return git.CIFixApplied
}

// tryFixConflict spawns a conflict resolution agent, force-pushes the
// resolved commits, and returns true if the fix was pushed (ready for
// merge retry). Mirrors tryFixCI.
func (l *Loop) tryFixConflict(ctx context.Context, conflictErr *git.UnresolvedConflictError, taskID, nextTask, workDir, rawLogPath string) bool {
	conflictDiff := l.git.ConflictDiff()
	beadDesc := getBeadDescription(l.cfg.TaskBackend, taskID)
	headBefore := l.git.HeadRev()
	if !l.verifier.TryFixConflict(ctx, conflictDiff, beadDesc, nextTask, workDir, rawLogPath) {
		return false
	}

	headAfter := l.git.HeadRev()
	if headBefore == headAfter {
		l.logger.Warn("git", "Conflict agent made no new commits — nothing to push")
		return false
	}

	if err := l.git.Push(ctx); err != nil {
		l.logger.Warn("git", "Push after conflict resolution failed: %v", err)
		return false
	}
	return true
}

// newRunner returns a claudeRunner for spawning sub-agents.
func (l *Loop) newRunner() claudeRunner {
	if l.newRunnerFunc != nil {
		return l.newRunnerFunc()
	}
	return agent.New(l.logger)
}

// queryFunc returns the Query method from the centralized agent runner.
func (l *Loop) queryFunc() verify.QueryFunc {
	if l.agentRunner != nil {
		return l.agentRunner.Query
	}
	return nil
}

// getCIFailureLog retrieves the failed CI run's log output for the given PR.
func (l *Loop) getCIFailureLog(prNumber string) string {
	return l.git.GetCIFailureLog(prNumber)
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

// findPRInfo looks up the PR number, title, and URL for the current branch.
func (l *Loop) findPRInfo(workDir string) (number, title, url string) {
	if l.findPRInfoFunc != nil {
		n, t := l.findPRInfoFunc(workDir)
		return n, t, ""
	}
	gh := l.git.GH()
	if gh == nil {
		return "", "", ""
	}
	num, t, u, err := gh.FindPR(l.git.GetWorktreeBranch(), workDir)
	if err != nil {
		return "", "", ""
	}
	return num, t, u
}
