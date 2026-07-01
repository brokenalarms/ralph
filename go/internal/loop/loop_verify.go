package loop

import (
	"context"
	"fmt"
	"strings"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/verifier"
)

// fixLoopResult is the tri-state outcome of a fix-and-push cycle. The CI
// caller is the only one that distinguishes between "fix applied" and
// "no commits — likely infrastructure failure"; conflict and review fixes
// collapse no-commits into the failure case at their wrapper layer.
type fixLoopResult int

const (
	fixApplied fixLoopResult = iota
	fixNoCommits
	fixFailed
)

// fixLoopSpec parameterizes one cycle of "spawn a fix agent, observe whether
// it made commits, push if it did". Each tryFix* wrapper supplies its
// fix-specific spawn callback and (optionally) a post-push hook (e.g. for
// review's resolve-comments call).
type fixLoopSpec struct {
	// spawn invokes the fix agent. The result's SignalDetected gates the
	// rest of the loop.
	spawn func() verifier.FixAgentResult

	// fixName tags log lines (e.g. "CI", "Conflict", "<reviewer>").
	fixName string

	// autoCommitMsg, when non-empty, instructs the helper to commit any
	// uncommitted changes the fix agent left behind, with the given
	// commit message. CI and review use this; conflict does not.
	autoCommitMsg string

	// noCommitsMsg overrides the default "fix agent made no new commits"
	// log line — the three callers each phrase it slightly differently.
	noCommitsMsg string

	// onPushed runs after a successful push. Used by the review wrapper to
	// reply-and-resolve review threads. Errors are logged but not surfaced
	// (the fix was already pushed).
	onPushed func() error
}

// fixLoop owns the shared "spawn → check commits → push" scaffolding for
// the three fix flows. The two HeadRev() calls in this function are the
// only HeadRev() reads in the file; the per-fix wrappers compose the
// scaffold via fixLoopSpec callbacks rather than re-reading state.
func (l *Loop) fixLoop(ctx context.Context, opts fixLoopSpec) fixLoopResult {
	headBefore := l.git.HeadRev()
	fixResult := opts.spawn()
	if !fixResult.SignalDetected {
		return fixFailed
	}

	if opts.autoCommitMsg != "" && l.git.HasUncommittedChanges() {
		l.logger.Emit(logging.Opts{Domain: logging.Git}, "Fix agent left uncommitted changes — auto-committing")
		l.git.CommitAll(opts.autoCommitMsg)
	}

	headAfter := l.git.HeadRev()
	if headBefore == headAfter {
		msg := opts.noCommitsMsg
		if msg == "" {
			msg = fmt.Sprintf("%s fix agent made no new commits", opts.fixName)
		}
		l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "%s", msg)
		return fixNoCommits
	}

	l.logger.Emit(logging.Opts{Domain: logging.Git}, "%s fix committed — pushing", opts.fixName)
	if err := l.git.Push(ctx); err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Push after %s fix failed: %v", opts.fixName, err)
		return fixFailed
	}

	if opts.onPushed != nil {
		if err := opts.onPushed(); err != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "%s post-push hook: %v", opts.fixName, err)
		}
	}
	return fixApplied
}

// tryFixCI spawns a fix agent to address CI failures, force-pushes the
// new commits, and returns a CIFixResult:
//   - CIFixApplied:   fix was pushed, ready for merge retry
//   - CIFixNoCommits: agent ran but made no commits (infrastructure failure)
//   - CIFixFailed:    agent error or push failure
func (l *Loop) tryFixCI(ctx context.Context, ciErr *git.CIFailureError, nextTask, workDir, rawLogPath string) git.CIFixResult {
	ciLog := l.git.GetCIFailureLog(ctx, ciErr.PRNumber)

	// Pre-filter required vs optional check failures. Verifier does not
	// import the git package, so Loop flattens the typed git.CheckFailure
	// slice into plain string slices before handing off.
	var requiredNames, optionalNames []string
	for _, f := range git.RequiredFailedChecks(ciErr.Failures) {
		requiredNames = append(requiredNames, f.Name)
	}
	for _, f := range ciErr.Failures {
		if !f.IsRequired {
			optionalNames = append(optionalNames, f.Name)
		}
	}

	if len(optionalNames) > 0 {
		l.logger.Emit(logging.Opts{Domain: logging.CI}, "Ignoring optional/deploy check failures: %s", strings.Join(optionalNames, ", "))
	}
	if len(requiredNames) == 0 {
		l.logger.Emit(logging.Opts{Domain: logging.CI}, "Only optional checks failed on PR #%d — skipping fix agent", ciErr.PRNumber)
		return git.CIFixFailed
	}

	l.logger.Emit(logging.Opts{Domain: logging.CI}, "CI failed on PR #%d — spawning fix agent for required checks", ciErr.PRNumber)

	result := l.fixLoop(ctx, fixLoopSpec{
		fixName:       "CI",
		autoCommitMsg: "fix: auto-commit CI fix agent changes",
		noCommitsMsg:  "Fix agent made no new commits — likely infrastructure failure",
		spawn: func() verifier.FixAgentResult {
			return l.verifier.SpawnFixAgent(verifier.FixAgentInput{
				Ctx:      ctx,
				Template: "verify-ci.md",
				Vars: map[string]string{
					"{{TASK_TITLE}}":    nextTask,
					"{{FAILED_CHECKS}}": strings.Join(requiredNames, ", "),
					"{{CI_LOG}}":        ciLog,
				},
				Attempt:     1,
				WorkDir:     workDir,
				RawLogPath:  rawLogPath,
				Description: "CI failures",
			})
		},
	})

	switch result {
	case fixApplied:
		return git.CIFixApplied
	case fixNoCommits:
		return git.CIFixNoCommits
	default:
		return git.CIFixFailed
	}
}

// tryFixConflict spawns a conflict resolution agent, force-pushes the
// resolved commits, and returns true if the fix was pushed (ready for
// merge retry). Mirrors tryFixCI.
func (l *Loop) tryFixConflict(ctx context.Context, taskID, nextTask, workDir, rawLogPath string) bool {
	conflictDiff := l.git.ConflictDiff()
	taskDesc := l.taskDescription(taskID)

	l.logger.Emit(logging.Opts{Domain: logging.Git}, "Unresolvable merge conflict — spawning conflict resolution agent")

	result := l.fixLoop(ctx, fixLoopSpec{
		fixName:      "Conflict",
		noCommitsMsg: "Conflict agent made no new commits — nothing to push",
		spawn: func() verifier.FixAgentResult {
			return l.verifier.SpawnFixAgent(verifier.FixAgentInput{
				Ctx:      ctx,
				Template: "resolve-conflict.md",
				Vars: map[string]string{
					"{{TASK_TITLE}}":       nextTask,
					"{{TASK_DESCRIPTION}}": taskDesc,
					"{{CONFLICT_DIFF}}":    conflictDiff,
				},
				Attempt:     1,
				WorkDir:     workDir,
				RawLogPath:  rawLogPath,
				Description: "conflict resolution",
			})
		},
	})
	return result == fixApplied
}

// taskDescription fetches the task description from the configured backend,
// returning empty string when no description exists or the call fails.
// Reads l.taskBackend via the receiver.
func (l *Loop) taskDescription(taskID string) string {
	if taskID == "" || l.taskBackend == nil {
		return ""
	}
	desc, err := l.taskBackend.GetDescription(taskID)
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
func (l *Loop) tryFixReviewComments(ctx context.Context, reviewerName string, review *git.AutoReview, prNumber int, nextTask, workDir, rawLogPath string) bool {
	actionable := filterActionableComments(review.Comments)
	if len(actionable) == 0 {
		l.logger.Emit(logging.Opts{Domain: logging.Git}, "%s review: no actionable comments — proceeding to merge", reviewerName)
		return false
	}

	l.logger.Emit(logging.Opts{Domain: logging.Git}, "%s review: %d actionable comment(s) — spawning fix agent", reviewerName, len(actionable))
	for _, c := range actionable {
		firstLine := c.Body
		if i := strings.IndexByte(c.Body, '\n'); i >= 0 {
			firstLine = c.Body[:i]
		}
		l.logger.Emit(logging.Opts{Domain: logging.Git}, "%s: %s:%d — %s", reviewerName, c.Path, c.Line, firstLine)
	}
	reviewCtx := formatReviewContext(reviewerName, prNumber, actionable)

	l.logger.Emit(logging.Opts{Domain: logging.Git}, "Spawning %s review fix agent", reviewerName)

	result := l.fixLoop(ctx, fixLoopSpec{
		fixName:       reviewerName,
		autoCommitMsg: "fix: address " + reviewerName + " review feedback",
		noCommitsMsg:  fmt.Sprintf("%s fix agent made no new commits — proceeding to merge anyway", reviewerName),
		spawn: func() verifier.FixAgentResult {
			return l.verifier.SpawnFixAgent(verifier.FixAgentInput{
				Ctx:      ctx,
				Template: "verify-copilot-review.md",
				Vars: map[string]string{
					"{{TASK_TITLE}}":      nextTask,
					"{{REVIEW_FEEDBACK}}": reviewCtx,
				},
				Attempt:     1,
				WorkDir:     workDir,
				RawLogPath:  rawLogPath,
				Description: "Copilot review feedback",
			})
		},
		onPushed: func() error {
			return l.git.ReplyToAndResolveComments(ctx, prNumber, actionable)
		},
	})
	return result == fixApplied
}
