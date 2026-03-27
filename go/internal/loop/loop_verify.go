package loop

import (
	"context"

	"github.com/brokenalarms/ralph/internal/agent"
	"github.com/brokenalarms/ralph/internal/git"
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
		return l.verifyFunc(ctx, l.git.WorkDir, headBefore)
	}
	return l.verifier.VerifyCompletion(ctx, l.git.WorkDir, headBefore)
}

// runPreIterationTests delegates to the Verifier.
func (l *Loop) runPreIterationTests(ctx context.Context) string {
	return l.verifier.RunPreIterationTests(ctx)
}

// tryFixCI spawns a fix agent to address CI failures, pushes the fix,
// and returns true if the fix was applied (ready for merge retry).
func (l *Loop) tryFixCI(ctx context.Context, ciErr *git.CIFailureError, taskID, nextTask, workDir, rawLogPath string) bool {
	ciLog := l.getCIFailureLog(ciErr.PRNumber)
	if !l.verifier.TryFixCI(ctx, ciLog, ciErr, nextTask, workDir, rawLogPath) {
		return false
	}

	if _, pushErr := l.pushAndCreatePR(ctx, taskID, nextTask, ""); pushErr != nil {
		l.logger.Warn("git", "Push after CI fix failed: %v", pushErr)
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

// getBeadDescription retrieves the bead description for LLM verification.
func (l *Loop) getBeadDescription(taskID string) string {
	if taskID == "" || l.cfg.TaskBackend == nil {
		return ""
	}
	desc, err := l.cfg.TaskBackend.GetDescription(taskID)
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
	gh := l.git.GitHub
	if gh == nil {
		return "", "", ""
	}
	num, t, u, err := gh.FindPR(l.git.WorktreeBranch, workDir)
	if err != nil {
		return "", "", ""
	}
	return num, t, u
}
