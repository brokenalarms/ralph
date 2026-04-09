package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/attempts"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// completeTaskParams bundles the signal data and callbacks for the post-signal
// pipeline. All module interactions are callbacks — no module references are
// held as fields; only data, func types, and *logging.Logger are permitted.
type completeTaskParams struct {
	// signal data
	result     claude.Result
	headBefore string
	workDir    string
	rawLogPath string
	diffStat   string
	taskID     string
	nextTask   string
	// config
	postSignalTimeout time.Duration
	autoMerge         bool
	evolve            bool
	notify            bool
	ralphDir          string
	// cross-cutting
	logger *logging.Logger
	// verification
	verifyFn func(ctx context.Context, headBefore string) (bool, string)
	// git callbacks
	headRevFn        func() string
	worktreeBranchFn func() string
	tagTaskEndFn     func(taskID string)
	getPRStateFn     func(prNum int) (git.PRState, error)
	findExistingPRFn func(taskID, branch string) (int, bool)
	// backend callbacks
	getStateFn       func(taskID, key string) (string, error)
	setStateFn       func(taskID, key, value, reason string) error
	closeTaskFn      func(taskID, reason string) error
	getExternalRefFn func(taskID string) (string, error)
	// state callbacks
	getSkippedTasksFn  func() ([]string, error)
	recordCompletedFn  func(taskID, nextTask string)
	persistCompletedFn func(taskID string, merged bool)
	touchPlanFlashFn   func()
	writeStateFn       func(key, value string)
	// attempts callbacks
	recordAttemptFn func(taskID, nextTask, reason, diffStat, note string)
	clearAttemptsFn func(taskID, nextTask string)
	// skip
	skipTaskFn func(taskID, reason string)
	// ship + finalize + post-task
	shipFn        func(ctx context.Context, taskID, title, summary string) (prNumber int, prURL string)
	finalizePRFn  func(ctx context.Context, taskID, nextTask string, prNumber int, prState git.PRState, prURL, workDir, rawLogPath string) finalizePRResult
	buildCTFn     func(taskID, nextTask, summary string, prNumber int) CompletedTask
	runPostTaskFn func(ctx context.Context, taskID string, prNumber int, merged bool)
}

// completeTaskOut carries the results of completeTask back to Run().
type completeTaskOut struct {
	action postSignalAction
	ct     *CompletedTask
	merged bool
}

// postSignalAction describes the outcome of post-signal processing.
type postSignalAction int

const (
	signalComplete postSignalAction = iota // task done, fall through to tagTaskEnd
	signalRetry                            // verification failed, skip tagTaskEnd
	signalSkipped                          // no new commits, already handled
	signalEvolve                           // evolve restart, caller returns nil
)

// postSignalParams bundles the context needed for post-signal processing.
type postSignalParams struct {
	ctx        context.Context
	result     claude.Result
	headBefore string
	workDir    string
	rawLogPath string
	taskID     string
	nextTask   string
	diffStat   string
}

// completeTask runs after the agent signals completion: verifies the work,
// ships a PR, merges if configured, and closes the bead.
func completeTask(ctx context.Context, p completeTaskParams) completeTaskOut {
	if p.postSignalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.postSignalTimeout)
		defer cancel()
	}

	// Watch for feedback file and cancel context when it appears.
	if p.ralphDir != "" {
		feedbackFile := filepath.Join(p.ralphDir, "feedback")
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		ticker := time.NewTicker(500 * time.Millisecond)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := os.Stat(feedbackFile); err == nil {
						os.Remove(feedbackFile)
						p.logger.Emit(logging.Opts{Domain: logging.Git}, "Feedback signal detected during post-signal pipeline — cancelling")
						cancel()
						return
					}
				}
			}
		}()
		defer func() {
			cancel()
			ticker.Stop()
			<-done
		}()
	}

	// Guard: if the task was already skipped during verification (e.g. 3
	// rejected attempts), do not push or merge the rejected work.
	if p.taskID != "" {
		skipped, err := p.getSkippedTasksFn()
		if err != nil {
			p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Failed to load skipped tasks for %s: %v — conservatively not pushing", p.taskID, err)
			return completeTaskOut{action: signalSkipped}
		}
		for _, id := range skipped {
			if id == p.taskID {
				p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task %s was skipped during verification — not pushing", p.taskID)
				return completeTaskOut{action: signalSkipped}
			}
		}
	}

	// Preflight: check bead wasn't prematurely closed by the agent.
	if p.taskID != "" {
		phase, _ := p.getStateFn(p.taskID, "phase")
		if phase != "implementing" {
			p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Task %s phase is %q (expected implementing) — agent may have tampered with task state", p.taskID, phase)
		}
	}

	// If OnSignal was set, verification already passed in the runner.
	// If not (legacy/test path), run verification here as fallback.
	if !p.result.OnSignalUsed {
		if passed, reason := p.verifyFn(ctx, p.headBefore); !passed {
			p.logger.Emit(logging.Opts{Domain: logging.Test, Level: logging.Warn}, "Verification failed: %s", reason)
			p.recordAttemptFn(p.taskID, p.nextTask,
				"Signal received but verification failed: "+reason,
				p.diffStat,
				"verification_failed: fix must pass tests and produce commits before closing")
			return completeTaskOut{action: signalRetry}
		}
	}

	if p.taskID != "" {
		if err := p.setStateFn(p.taskID, "phase", "verified", "ralph: tests passed, commits present"); err != nil {
			p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "SetState phase=verified: %v", err)
		} else {
			p.logger.Emit(logging.Opts{Domain: logging.Beads}, "%s → verified", p.taskID)
		}
	}

	p.clearAttemptsFn(p.taskID, p.nextTask)
	p.recordCompletedFn(p.taskID, p.nextTask)
	p.touchPlanFlashFn()

	headAfterSignal := p.headRevFn()
	if p.headBefore != "" && headAfterSignal == p.headBefore {
		// No new commits but verification passed (agent + LLM + tests agree).
		p.logger.Emit(logging.Opts{Domain: logging.Git}, "No new commits — verified complete")

		// Check for an existing PR from a prior attempt that still needs merging.
		if p.taskID != "" {
			ref, _ := p.getExternalRefFn(p.taskID)
			if prNum := parsePRNumber(ref); prNum != 0 {
				prState, _ := p.getPRStateFn(prNum)
				if prState == git.PRStateOpen {
					p.logger.Emit(logging.Opts{Domain: logging.Git}, "Found open PR #%d from prior attempt — routing through merge", prNum)
					finalResult := p.finalizePRFn(ctx, p.taskID, p.nextTask, prNum, prState, "", p.workDir, p.rawLogPath)
					p.tagTaskEndFn(p.taskID)
					p.runPostTaskFn(ctx, p.taskID, prNum, finalResult.merged)
					if p.notify {
						notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary)
						if finalResult.merged {
							notify.TaskMerged(p.taskID, p.nextTask)
						}
					}
					if finalResult.merged && p.evolve {
						p.logger.Phase("Evolve: restarting with latest main")
						p.writeStateFn("status", "evolve_restart")
						return completeTaskOut{action: signalEvolve, merged: true}
					}
					return completeTaskOut{action: signalSkipped, merged: finalResult.merged}
				}
				if prState == git.PRStateMerged {
					p.logger.Emit(logging.Opts{Domain: logging.Git}, "PR #%d already merged", prNum)
				}
			}
		}

		// No existing PR to merge — close the bead directly.
		p.logger.Emit(logging.Opts{Domain: logging.Git}, "Closing bead (no PR to merge)")
		if p.taskID != "" {
			if ctx.Err() != nil {
				p.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
				return completeTaskOut{action: signalComplete}
			}
			closeReason := "verified complete (no new commits)"
			_ = p.setStateFn(p.taskID, "phase", "verified", closeReason)
			if err := p.closeTaskFn(p.taskID, closeReason); err != nil {
				skipReason := "close_failed"
				if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
					p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %s blocked by %v", p.taskID, blockers)
					skipReason = fmt.Sprintf("dependency_blocked_by:%s", strings.Join(blockers, ","))
				} else {
					p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %v", err)
				}
				p.skipTaskFn(p.taskID, skipReason)
			} else {
				p.logger.Emit(logging.Opts{Domain: logging.Beads}, "Closed task %s (%s)", p.taskID, closeReason)
				p.persistCompletedFn(p.taskID, false)
			}
		}
		p.tagTaskEndFn(p.taskID)
		p.runPostTaskFn(ctx, p.taskID, 0, false)
		if p.notify {
			notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary)
		}
		return completeTaskOut{action: signalSkipped}
	}

	if ctx.Err() != nil {
		p.logger.Emit(logging.Opts{Level: logging.Warn}, "Post-signal timeout — aborting before push")
		return completeTaskOut{action: signalComplete}
	}

	prNumber, shipURL := p.shipFn(ctx, p.taskID, p.nextTask, p.result.Summary)
	prState := git.PRStateOpen

	// Recovery: if ship didn't produce a PR, find any existing PR in any state.
	if prNumber == 0 && p.taskID != "" {
		if num, found := p.findExistingPRFn(p.taskID, p.worktreeBranchFn()); found {
			prNumber = num
			prState = "" // let finalizePR look up the actual state
		}
	}

	ct := p.buildCTFn(p.taskID, p.nextTask, p.result.Summary, prNumber)
	if shipURL != "" {
		ct.PRURL = shipURL
	}

	// buildCTFn may discover a PR that recovery missed. A PR found in the
	// post-push context was just created, so OPEN is a safe assumption.
	if prNumber == 0 && ct.PRNum != 0 {
		prNumber = ct.PRNum
		prState = git.PRStateOpen
	}

	if ctx.Err() != nil {
		p.logger.Emit(logging.Opts{Level: logging.Warn}, "Post-signal timeout — aborting before merge")
		return completeTaskOut{action: signalComplete, ct: &ct}
	}

	if prNumber == 0 {
		p.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "No PR created — closing bead for task %s", p.taskID)
		if p.taskID != "" {
			if ctx.Err() != nil {
				p.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
				return completeTaskOut{action: signalComplete, ct: &ct}
			}
			branch := p.worktreeBranchFn()
			closeReason := "Verified — no PR created"
			if branch != "" {
				closeReason = fmt.Sprintf("Verified — branch %s, no PR", branch)
			}
			if err := p.closeTaskFn(p.taskID, closeReason); err != nil {
				p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %v", err)
			}
		}
		return completeTaskOut{action: signalComplete, ct: &ct}
	}

	finalResult := p.finalizePRFn(ctx, p.taskID, p.nextTask, prNumber, prState, ct.PRURL, p.workDir, p.rawLogPath)

	p.runPostTaskFn(ctx, p.taskID, prNumber, finalResult.merged)

	if p.notify {
		notify.TaskCompleted(p.taskID, p.nextTask, p.result.Summary)
	}

	if finalResult.merged {
		notify.TaskMerged(p.taskID, p.nextTask)
		if p.evolve {
			p.tagTaskEndFn(p.taskID)
			p.logger.Phase("Evolve: restarting with latest main")
			p.writeStateFn("status", "evolve_restart")
			return completeTaskOut{action: signalEvolve, ct: &ct, merged: true}
		}
	}

	return completeTaskOut{action: signalComplete, ct: &ct, merged: finalResult.merged}
}

// buildCompletedTask assembles the CompletedTask record for a signal.
func buildCompletedTask(taskID, nextTask, summary string, prNumber int, g git.GitOps) CompletedTask {
	ct := CompletedTask{
		ID:      taskID,
		Title:   nextTask,
		Summary: summary,
		PRNum:   prNumber,
	}
	if num, t, u, err := g.FindPRForBranch(g.GetWorktreeBranch()); err == nil && num != 0 {
		ct.PRNum = num
		ct.PRTitle = t
		ct.PRURL = u
	} else if prNumber != 0 {
		ct.PRNum = prNumber
	}
	return ct
}

// finalizePRParams bundles the context and dependencies needed to finalize a
// PR: merge if applicable, then close the bead. Used by both the post-signal
// flow and the resume-via-PR flow so neither duplicates merge+close logic.
type finalizePRParams struct {
	ctx        context.Context
	taskID     string
	nextTask   string
	prNumber   int
	prState    git.PRState // looked up from GH if empty
	prURL      string
	workDir    string
	rawLogPath string
	// dependency fields
	autoMerge       bool
	activeReviewers []git.Reviewer
	git             git.GitOps
	logger               *logging.Logger
	backend              tasks.Backend
	state                *state.Store
	attempts             *attempts.Tracker
	verifier             *Verifier
	// mergeFunc overrides git.MergeWithRetry for tests; nil uses the real path.
	mergeFunc func(ctx context.Context) (bool, error)
}

type finalizePRResult struct {
	merged bool
	closed bool
}

// reviewAddressed returns true when the given reviewer's feedback was already
// addressed for this task in a previous finalizePR call.
func (p *finalizePRParams) reviewAddressed(botUsername string) bool {
	if p.state == nil || p.taskID == "" {
		return false
	}
	v, _ := p.state.Read("review_addressed:" + botUsername + ":" + p.taskID)
	return v == "true"
}

// markReviewAddressed records that the given reviewer's feedback was addressed
// so subsequent finalizePR calls for the same task skip re-polling.
func (p *finalizePRParams) markReviewAddressed(botUsername string) {
	if p.state == nil || p.taskID == "" {
		return
	}
	p.state.Write("review_addressed:"+botUsername+":"+p.taskID, "true")
}

// finalizePR handles an existing PR: merges if applicable, closes the bead.
// Returns the merge/close outcome so callers can act on it (e.g. evolve).
func finalizePR(p finalizePRParams) finalizePRResult {
	if p.prNumber == 0 {
		p.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "No PR — task %s stays open", p.taskID)
		return finalizePRResult{}
	}

	prState := p.prState
	if prState == "" {
		looked, err := p.git.GetPRState(p.prNumber)
		if err != nil || looked == "" {
			p.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: prLink(p.git, p.prNumber)}, "Failed to get state: %v", err)
			return finalizePRResult{}
		}
		prState = looked
	}

	merged := prState == git.PRStateMerged
	mergeFailed := false

	if prState == git.PRStateOpen && p.autoMerge {
		p.git.SetLocalTestsPassed(true)
		prBase := p.git.GetPRBase(p.prNumber)
		defaultBranch := p.git.DetectDefaultBranch()
		if prBase != "" && prBase != defaultBranch {
			p.logger.Emit(logging.Opts{Domain: "git", Link: prLink(p.git, p.prNumber)}, "targets %s — stacked, closing bead", prBase)
		} else {
			for _, reviewer := range p.activeReviewers {
				if p.reviewAddressed(reviewer.BotUsername) {
					continue
				}
				review, err := p.git.PollReview(reviewer.BotUsername, p.prNumber, reviewer.DefaultTimeout)
				if err != nil {
					p.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink(p.git, p.prNumber)}, "%s review poll: %v", reviewer.BotUsername, err)
					continue
				}
				if review != nil {
					p.logger.Emit(logging.Opts{Domain: logging.Git, Link: prLink(p.git, p.prNumber)}, "%s review received (%d comments)", reviewer.BotUsername, len(review.Comments))
					if p.verifier != nil {
						tryFixReviewComments(p.ctx, p.git, p.verifier, p.logger, reviewer.BotUsername, review, p.prNumber, p.nextTask, p.workDir, p.rawLogPath)
					}
					p.markReviewAddressed(reviewer.BotUsername)
				} else {
					p.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn, Link: prLink(p.git, p.prNumber)}, "No %s review arrived within timeout — proceeding to merge", reviewer.BotUsername)
				}
			}
			p.logger.Emit(logging.Opts{Domain: "git", Link: prLink(p.git, p.prNumber)}, "targets %s — merging", defaultBranch)
			p.git.SetKnownPRNumber(p.prNumber)
			defer p.git.SetKnownPRNumber(0)
			var mergeErr error
			merged, mergeErr = mergeWithRetry(p.ctx, mergeWithRetryParams{
				taskID:     p.taskID,
				nextTask:   p.nextTask,
				workDir:    p.workDir,
				rawLogPath: p.rawLogPath,
				mergeFunc:  p.mergeFunc,
				git:        p.git,
				verifier:   p.verifier,
				logger:     p.logger,
				backend:    p.backend,
			})
			if mergeErr != nil {
				p.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Auto-merge: %v", mergeErr)
				var ciExhausted *git.CIFixExhaustedError
				if errors.As(mergeErr, &ciExhausted) {
					p.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Error}, "CI fix agents gave up after %d attempts — tests still failing. Leaving task %s open for manual investigation.", ciExhausted.Attempts, p.taskID)
					return finalizePRResult{}
				}
				var ciFailure *git.CIFailureError
				if errors.As(mergeErr, &ciFailure) {
					p.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Error}, "CI failing on PR #%d — leaving task %s open.", ciFailure.PRNumber, p.taskID)
					return finalizePRResult{}
				}
			}
			if !merged {
				mergeFailed = true
				p.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: prLink(p.git, p.prNumber)}, "Merge pending — closing bead")
			}
		}
	}

	if merged {
		p.git.PostMergeUpdateMain()
	}

	if p.taskID == "" {
		return finalizePRResult{merged: merged, closed: true}
	}

	var closeReason string
	if mergeFailed {
		closeReason = fmt.Sprintf("Verified — PR #%d open, merge pending", p.prNumber)
		if p.prURL != "" {
			closeReason = fmt.Sprintf("Verified — %s open, merge pending", p.prURL)
		}
	} else {
		closeReason = fmt.Sprintf("Fixed in PR #%d", p.prNumber)
		if p.prURL != "" {
			closeReason = fmt.Sprintf("Fixed in %s", p.prURL)
		}
	}
	p.attempts.ClearMergeFailures(p.taskID)
	stateReason := "ralph: PR open or stacked"
	if merged {
		stateReason = "ralph: PR merged"
	}
	if p.ctx.Err() != nil {
		p.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", p.taskID)
		return finalizePRResult{merged: merged}
	}
	_ = p.backend.SetState(p.taskID, "phase", "verified", stateReason)
	if err := p.backend.CloseTask(p.taskID, closeReason); err != nil {
		skipReason := "close_failed"
		if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
			p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %s blocked by %v", p.taskID, blockers)
			skipReason = fmt.Sprintf("dependency_blocked_by:%s", strings.Join(blockers, ","))
		} else {
			p.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask failed: %v", err)
		}
		skipTask(p.backend, p.state, p.logger, p.taskID, skipReason)
	} else {
		p.logger.Emit(logging.Opts{Domain: logging.Beads}, "Closed task %s (%s)", p.taskID, closeReason)
		persistCompletedTask(p.state, p.logger, p.taskID, merged)
	}

	return finalizePRResult{merged: merged, closed: true}
}

// iterationPrompt holds the prepared prompt and context needed to invoke Claude.
type iterationPrompt struct {
	fullPrompt string
	headBefore string
	diffBefore bool // true if a diff already existed when this iteration started
	rawLogPath string
	logStart   int
	workDir    string
}

// prepareAndBuildPrompt sets the task phase, runs pre-iteration tests, reads
// feedback, assembles attempt context, and builds the full prompt. Returns
// false if Run() should break (internet or rate limit unavailable).
func (l *Loop) prepareAndBuildPrompt(ctx context.Context, taskID, nextTask string) (iterationPrompt, bool) {
	if taskID != "" {
		if err := l.cfg.TaskBackend.SetState(taskID, "phase", "implementing", "ralph: starting task"); err != nil {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "SetState phase=implementing: %v", err)
		}
	}

	promptsDir := l.cfg.Dirs.PromptsDir
	ralphDir := l.cfg.Dirs.RalphDir

	taskPrompt := buildTaskPrompt(nextTask, taskID, l.cfg.TaskBackend, promptsDir, ralphDir)
	buildStatus := runVerifyBuild(ctx, runVerifyBuildParams{
		verifyBuild: l.cfg.VerifyBuild,
		projectDir:  l.cfg.Dirs.ProjectDir,
		testTimeout: l.cfg.TestTimeout,
		logger:      l.logger,
	})
	testStatus := buildStatus + l.verifier.RunPreIterationTests(ctx)

	if !l.cfg.WaitForInternet(ctx, l.logger) {
		return iterationPrompt{}, false
	}
	if !l.waitForRate(ctx) {
		return iterationPrompt{}, false
	}

	headBefore := l.git.HeadRev()
	diffBefore := l.git.HasDiff()
	rawLogPath := filepath.Join(ralphDir, "raw.log")
	logStart := fileLineCount(rawLogPath)

	attemptContext := buildAttemptContext(taskID, nextTask, l.attempts, ralphDir)
	if attemptContext != "" {
		attemptCount := strings.Count(attemptContext, "### Attempt ")
		reflectionCount := strings.Count(attemptContext, "## Recent learnings")
		if attemptCount > 0 || reflectionCount > 0 {
			var parts []string
			if attemptCount > 0 {
				parts = append(parts, fmt.Sprintf("%d prior attempt(s)", attemptCount))
			}
			if reflectionCount > 0 {
				parts = append(parts, "learnings from other tasks")
			}
			l.logger.Emit(logging.Opts{}, "Including %s", strings.Join(parts, " + "))
		}
	}

	fullPrompt, err := buildPrompt(taskPrompt, attemptContext, testStatus, l.cfg.TaskBackend, promptsDir, l.cfg.Dirs.ProjectDir, l.git.GetWorkDir(), ralphDir, l.cfg.PlanFile, l.signals, l.logger)
	if err != nil {
		l.logger.Emit(logging.Opts{Level: logging.Error}, "Prompt build failed: %v", err)
		return iterationPrompt{}, false
	}

	return iterationPrompt{
		fullPrompt: fullPrompt,
		headBefore: headBefore,
		diffBefore: diffBefore,
		rawLogPath: rawLogPath,
		logStart:   logStart,
		workDir:    l.git.GetWorkDir(),
	}, true
}

// handleRunResultParams bundles the inputs and dependencies for handleRunResult.
type handleRunResultParams struct {
	result       claude.Result
	runErr       error
	taskID       string
	nextTask     string
	headBefore   string
	runIteration int
	model        string

	isOnlineFunc        func() bool
	waitForInternetFunc func(context.Context, *logging.Logger) bool
	logger              *logging.Logger
	git                 git.GitOps
	attempts            *attempts.Tracker
	limiter             interface {
		WaitUntil(ctx context.Context, target time.Time, onTick func(int)) error
	}
	backend  tasks.Backend
	state    *state.Store
	skipTask func(backend tasks.Backend, st *state.Store, logger *logging.Logger, id, reason string)
}

// handleRunResult processes errors and retryable conditions from a Claude
// run (offline, feedback kill, idle timeout, rate limit). Returns the
// loopAction Run() should take. When actionRetry is returned, the caller
// is responsible for not counting this iteration.
func handleRunResult(ctx context.Context, p handleRunResultParams) loopAction {
	if p.runErr != nil {
		if !p.isOnlineFunc() {
			p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Claude failed — internet appears down")
			if !p.waitForInternetFunc(ctx, p.logger) {
				return actionDone
			}
			return actionRetry
		}
		p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Claude failed on iteration %d, continuing...", p.runIteration)
	}
	if p.result.FeedbackKill {
		p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Restarting iteration %d — user feedback received", p.runIteration)
		diffStat := p.git.DiffStatRange(p.headBefore, p.git.HeadRev())
		p.attempts.Record(p.taskID, p.nextTask,
			"Killed: user feedback received (see bead notes for content)",
			diffStat,
			"user_feedback: check bead notes for details")
		return actionRetry
	}
	if p.result.IdleTimeout {
		p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Restarting iteration %d after idle timeout", p.runIteration)
		diffStat := p.git.DiffStatRange(p.headBefore, p.git.HeadRev())
		p.attempts.Record(p.taskID, p.nextTask,
			"Killed: idle timeout (no output for configured duration)",
			diffStat,
			"idle_timeout: consider a lighter approach or make incremental progress rather than deep-thinking without output")
		count, _ := p.attempts.RecordIdleTimeoutFailure(p.taskID)
		if count >= p.attempts.MaxIdleTimeoutFailures {
			p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Idle timeout %d times for %s — skipping task", count, p.taskID)
			p.skipTask(p.backend, p.state, p.logger, p.taskID, "idle_timeout_max_failures")
			return actionRetry
		}
		return actionRetry
	}
	if p.result.RateLimited {
		waitDur := claude.FormatWaitDuration(time.Until(p.result.ResetAt))
		p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Claude rate limit — waiting %s until %s", waitDur, p.result.ResetAt.Format("3:04pm"))
		err := p.limiter.WaitUntil(ctx, p.result.ResetAt, func(secs int) {
			p.logger.Emit(logging.Opts{Domain: logging.LLM, Model: p.model}, "Rate limit: %ds until reset", secs)
		})
		if err != nil {
			p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Warn, Model: p.model}, "Rate limit wait interrupted: %v", err)
			return actionDone
		}
		p.logger.Emit(logging.Opts{Domain: logging.LLM, Level: logging.Success, Model: p.model}, "Rate limit reset — resuming")
		return actionRetry
	}

	return actionProceed
}



