package loop

import (
	"context"
	"fmt"
	"strings"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// tryFixCI spawns a fix agent to address CI failures, force-pushes the
// new commits, and returns a CIFixResult:
//   - CIFixApplied:   fix was pushed, ready for merge retry
//   - CIFixNoCommits: agent ran but made no commits (infrastructure failure)
//   - CIFixFailed:    agent error or push failure
func tryFixCI(ctx context.Context, g git.Ops, v *Verifier, logger *logging.Logger, ciErr *git.CIFailureError, nextTask, workDir, rawLogPath string) git.CIFixResult {
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
func tryFixConflict(ctx context.Context, g git.Ops, v *Verifier, logger *logging.Logger, backend tasks.Backend, taskID, nextTask, workDir, rawLogPath string) bool {
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

// filterActionableComments returns only comments that propose concrete changes.
// Purely informational comments (e.g. "This PR does X" summaries) are excluded.
func filterActionableComments(comments []git.ReviewComment) []git.ReviewComment {
	var out []git.ReviewComment
	for _, c := range comments {
		if isActionableComment(c) {
			out = append(out, c)
		}
	}
	return out
}

// isActionableComment returns true when a review comment proposes a concrete
// change rather than just describing what the code does. Heuristics:
//   - markdown suggestion blocks (```suggestion) always propose a change
//   - code blocks on file-scoped comments propose a change
//   - comments containing issue-signalling keywords are actionable
func isActionableComment(c git.ReviewComment) bool {
	body := strings.TrimSpace(c.Body)
	if body == "" {
		return false
	}
	if strings.Contains(body, "```suggestion") {
		return true
	}
	if c.Path != "" && strings.Contains(body, "```") {
		return true
	}
	lower := strings.ToLower(body)
	for _, kw := range []string{"bug", "nil check", "incorrect", "missing", "should ", "must ", "fix ", "issue", "problem", "error", "panic", "crash", "leak"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// formatReviewContext formats actionable review comments as structured agent
// context with file paths and line numbers, attributed to the given reviewer.
func formatReviewContext(reviewerName string, prNumber int, comments []git.ReviewComment) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s Review Feedback\nThe following review comments were left on PR #%d. Address each one:\n\n", reviewerName, prNumber)
	for _, c := range comments {
		fmt.Fprintf(&sb, "### %s:%d\n%s\n\n", c.Path, c.Line, c.Body)
	}
	return sb.String()
}

// tryFixReviewComments filters actionable comments from the review, spawns a
// fix agent to address them, and force-pushes the result. Returns true when
// the fix was committed and pushed successfully.
func tryFixReviewComments(ctx context.Context, g git.Ops, v *Verifier, logger *logging.Logger, reviewerName string, review *git.AutoReview, prNumber int, nextTask, workDir, rawLogPath string) bool {
	actionable := filterActionableComments(review.Comments)
	if len(actionable) == 0 {
		logger.Emit(logging.Opts{Domain: logging.Git}, "%s review: no actionable comments — proceeding to merge", reviewerName)
		return false
	}

	logger.Emit(logging.Opts{Domain: logging.Git}, "%s review: %d actionable comment(s) — spawning fix agent", reviewerName, len(actionable))
	for _, c := range actionable {
		firstLine := c.Body
		if i := strings.IndexByte(c.Body, '\n'); i >= 0 {
			firstLine = c.Body[:i]
		}
		logger.Emit(logging.Opts{Domain: logging.Git}, "%s: %s:%d — %s", reviewerName, c.Path, c.Line, firstLine)
	}
	reviewCtx := formatReviewContext(reviewerName, prNumber, actionable)
	headBefore := g.HeadRev()

	if !v.TryCopilotFix(ctx, reviewCtx, nextTask, workDir, rawLogPath) {
		return false
	}

	if g.HasUncommittedChanges() {
		logger.Emit(logging.Opts{Domain: logging.Git}, "Fix agent left uncommitted changes — auto-committing")
		g.CommitAll("fix: address " + reviewerName + " review feedback")
	}

	if g.HeadRev() == headBefore {
		logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "%s fix agent made no new commits — proceeding to merge anyway", reviewerName)
		return false
	}

	logger.Emit(logging.Opts{Domain: logging.Git}, "%s fix committed — pushing", reviewerName)
	if err := g.Push(ctx); err != nil {
		logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Push after %s fix failed: %v", reviewerName, err)
		return false
	}
	return true
}
