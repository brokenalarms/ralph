package loop

import (
	"context"
	"fmt"
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

// handlePostSignal runs after the agent signals completion: verifies the
// work, pushes a PR, merges if configured, and closes the bead. Returns
// the action Run() should take. Callers pass runIteration and iteration
// pointers so the no-commits path can increment them.
func (l *Loop) handlePostSignal(p postSignalParams, runIteration, iteration *int) postSignalAction {
	// Preflight: check bead wasn't prematurely closed by the agent.
	if p.taskID != "" {
		phase, _ := l.cfg.TaskBackend.GetState(p.taskID, "phase")
		if phase != "implementing" {
			l.logger.Warn("beads", "Task %s phase is %q (expected implementing) — agent may have tampered with task state", p.taskID, phase)
		}
	}

	// If OnSignal was set, verification already passed in the runner.
	// If not (legacy/test path), run verification here as fallback.
	if !p.result.OnSignalUsed {
		if passed, reason := l.verifyCompletion(p.ctx, p.headBefore); !passed {
			l.logger.Warn("test", "Verification failed: %s", reason)
			l.attempts.Record(p.taskID, p.nextTask,
				"Signal received but verification failed: "+reason,
				p.diffStat,
				"verification_failed: fix must pass tests and produce commits before closing")
			return signalRetry
		}
	}

	if p.taskID != "" {
		if err := l.cfg.TaskBackend.SetState(p.taskID, "phase", "verified", "ralph: tests passed, commits present"); err != nil {
			l.logger.Warn("beads", "SetState phase=verified: %v", err)
		} else {
			l.logger.Log("beads", "%s → verified", p.taskID)
		}
	}

	l.attempts.Clear(p.taskID, p.nextTask)
	recordCompletedTask(l.cfg.Dirs.RalphDir, p.taskID, p.nextTask)
	touchFile(filepath.Join(l.cfg.Dirs.RalphDir, ".plan-flash"))

	headAfterSignal := l.git.HeadRev()
	if p.headBefore != "" && headAfterSignal == p.headBefore {
		// No new commits — check if there's a merged PR proving the work is on main.
		l.logger.Log("git", "No new commits — checking for merged PR")
		canClose := false
		closeReason := "work already on main"
		if p.taskID != "" {
			ref, _ := l.cfg.TaskBackend.GetExternalRef(p.taskID)
			if prNum := parsePRNumber(ref); prNum != "" {
				gh := l.git.GH()
				if gh != nil {
					if prState, _ := gh.GetPRState(l.git.WorkDir, prNum); strings.ToUpper(prState) == "MERGED" {
						canClose = true
						closeReason = fmt.Sprintf("PR #%s already merged", prNum)
						l.logger.Log("git", "PR #%s confirmed merged — closing bead", prNum)
					} else {
						l.logger.Warn("git", "No new commits but PR #%s is %s — task stays open", prNum, prState)
					}
				}
			} else {
				// No PR reference — only close if worktree tree matches main,
				// proving the work actually landed. Without this check, beads
				// get closed while their branch work is orphaned.
				if l.git.WorktreeMatchesMain() {
					canClose = true
					closeReason = "tree matches main (no PR reference)"
					l.logger.Log("git", "No PR reference but worktree matches main — closing bead")
				} else {
					l.logger.Warn("git", "No PR reference and worktree differs from main — task stays open")
				}
			}
		}
		if canClose && p.taskID != "" {
			_ = l.cfg.TaskBackend.SetState(p.taskID, "phase", "verified", closeReason)
			if err := l.cfg.TaskBackend.CloseTask(p.taskID, closeReason); err != nil {
				l.logger.Warn("beads", "CloseTask: %v", err)
			} else {
				l.logger.Log("beads", "Closed task %s (%s)", p.taskID, closeReason)
				persistCompletedTask(l.state, l.logger, p.taskID, p.nextTask, "", closeReason)
			}
		} else if p.taskID != "" {
			l.logger.Warn("beads", "Task %s stays open — no merged PR to confirm work on main", p.taskID)
		}
		l.git.TagTaskEnd(p.taskID)
		*runIteration++
		*iteration++
		return signalSkipped
	}

	prNumber := l.pushSignalPR(p)
	ct := l.buildCompletedTask(p.taskID, p.nextTask, p.result.Summary, prNumber, p.workDir)
	merged, mergeErr := mergeIfEnabled(l.mergeDeps(), p, prNumber)

	closeOrRetryTask(l.closeTaskDeps(), p.taskID, ct, merged, mergeErr)
	l.sessionTasks = append(l.sessionTasks, ct)

	if merged {
		l.lastTaskMerged = true
		notify.TaskMerged(p.taskID, p.nextTask)
		l.git.PostMergeUpdateMain()
		if l.cfg.Evolve {
			l.git.TagTaskEnd(p.taskID)
			l.logger.Phase("Evolve: restarting with latest main")
			l.state.Write("status", "evolve_restart")
			return signalEvolve
		}
	}

	return signalComplete
}

// pushSignalPR pushes the branch and creates a PR after a successful signal.
func (l *Loop) pushSignalPR(p postSignalParams) string {
	prBody := buildPRBody(l.cfg.TaskBackend, p.taskID, p.result.Summary)
	prNumber, pushErr := l.pushAndCreatePR(p.ctx, p.taskID, p.nextTask, prBody)
	if pushErr != nil {
		if !isOnline() {
			l.logger.Warn("git", "Push failed — internet appears down")
			waitForInternet(p.ctx, l.logger)
			prNumber, pushErr = l.pushAndCreatePR(p.ctx, p.taskID, p.nextTask, prBody)
		}
		if pushErr != nil {
			l.logger.Warn("git", "Push/PR: %v", pushErr)
		}
	}
	if prNumber != "" && p.taskID != "" {
		_, _, prURL := l.findPRInfo(p.workDir)
		ref := prURL
		if ref == "" {
			ref = "gh-" + prNumber
		}
		l.logger.Log("git", "Linking task %s to %s (branch: %s)", p.taskID, ref, l.git.WorktreeBranch)
		if refErr := l.cfg.TaskBackend.SetExternalRef(p.taskID, ref); refErr != nil {
			l.logger.Warn("beads", "SetExternalRef: %v", refErr)
		}
	}
	return prNumber
}

// buildCompletedTask assembles the CompletedTask record for a signal.
func (l *Loop) buildCompletedTask(taskID, nextTask, summary, prNumber, workDir string) CompletedTask {
	ct := CompletedTask{
		ID:      taskID,
		Title:   nextTask,
		Summary: summary,
		PRNum:   prNumber,
	}
	if prNum, prTitle, prURL := l.findPRInfo(workDir); prNum != "" {
		ct.PRNum = prNum
		ct.PRTitle = prTitle
		ct.PRURL = prURL
	} else if prNumber != "" {
		ct.PRNum = prNumber
	}
	return ct
}

// mergeDeps holds the narrow dependencies needed by mergeIfEnabled.
type mergeDeps struct {
	AutoMerge     bool
	Backend       tasks.Backend
	GitHub        git.GitHub
	DefaultBranch string
	WorkDir       string
	Logger        *logging.Logger
	MergeFn       func(ctx context.Context, taskID, nextTask, workDir, rawLogPath string) (bool, error)
}

func mergeIfEnabled(deps mergeDeps, p postSignalParams, prNumber string) (bool, error) {
	merged := false
	var lastErr error
	if deps.AutoMerge && prNumber != "" {
		var mergeErr error
		merged, mergeErr = deps.MergeFn(p.ctx, p.taskID, p.nextTask, p.workDir, p.rawLogPath)
		if mergeErr != nil {
			deps.Logger.Warn("git", "Auto-merge: %v", mergeErr)
			lastErr = mergeErr
		}
	}

	if prNumber == "" && p.taskID != "" && deps.AutoMerge {
		ref, _ := deps.Backend.GetExternalRef(p.taskID)
		if existingPR := parsePRNumber(ref); existingPR != "" {
			if deps.GitHub != nil {
				prState, _ := deps.GitHub.GetPRState(p.workDir, existingPR)
				switch strings.ToUpper(prState) {
				case "MERGED":
					deps.Logger.Log("git", "PR #%s already merged — work is on main", existingPR)
					merged = true
				case "OPEN":
					prBase := getPRBase(deps.GitHub, deps.WorkDir, existingPR)
					if prBase != "" && prBase != deps.DefaultBranch {
						deps.Logger.Log("git", "Existing PR #%s targets %s (not %s) — stacked, skipping merge", existingPR, prBase, deps.DefaultBranch)
					} else {
						deps.Logger.Log("git", "Existing PR #%s still open — attempting merge", existingPR)
						var mergeErr error
						merged, mergeErr = deps.MergeFn(p.ctx, p.taskID, p.nextTask, p.workDir, p.rawLogPath)
						if mergeErr != nil {
							deps.Logger.Warn("git", "Auto-merge existing PR: %v", mergeErr)
						}
					}
				}
			}
		}
	}

	return merged, lastErr
}

// closeTaskDeps holds the narrow dependencies needed by closeOrRetryTask.
type closeTaskDeps struct {
	AutoMerge bool
	Backend   tasks.Backend
	Attempts  *attempts.Tracker
	State     *state.Store
	Logger    *logging.Logger
	SkipFn    func(id, reason string)
}

func closeOrRetryTask(deps closeTaskDeps, taskID string, ct CompletedTask, merged bool, mergeErr error) {
	if taskID == "" {
		return
	}

	// No PR = not done. Work must be in a PR to close the bead.
	if ct.PRNum == "" {
		deps.Logger.Warn("git", "No PR created — task %s stays open", taskID)
		return
	}

	closeReason := fmt.Sprintf("Fixed in PR #%s", ct.PRNum)
	if ct.PRURL != "" {
		closeReason = fmt.Sprintf("Fixed in %s", ct.PRURL)
	}

	if !merged && deps.AutoMerge && mergeErr != nil {
		deps.Logger.Warn("git", "Merge failed — skipping task %s (PR exists for manual review)", taskID)
		deps.SkipFn(taskID, "merge_failed")
		return
	}

	deps.Attempts.ClearMergeFailures(taskID)
	if err := deps.Backend.CloseTask(taskID, closeReason); err != nil {
		deps.Logger.Warn("beads", "CloseTask failed: %v", err)
	} else {
		deps.Logger.Log("beads", "Closed task %s (%s)", taskID, closeReason)
		persistCompletedTask(deps.State, deps.Logger, taskID, ct.Title, ct.PRNum, closeReason)
	}
}

// checkoutExistingBranch checks metadata for a branch from a previous
// iteration. If the remote has that branch with work, it checks it out.
// Otherwise, it renames the current branch for the task and stores the
// new name in metadata. Returns true if an existing remote branch was
// checked out.
func (l *Loop) checkoutExistingBranch(taskID, nextTask string) bool {
	storedBranch := ""
	if taskID != "" {
		storedBranch, _ = l.cfg.TaskBackend.GetMetadata(taskID, "branch")
	}
	if storedBranch != "" {
		_ = l.git.FetchBranch(storedBranch)
		if l.git.RemoteBranchHasCommits(storedBranch) {
			if l.git.RemoteBranchIsOnMain(storedBranch) {
				l.git.CheckoutRemoteBranch(storedBranch)
				return true
			}
			// Stale branch diverged from main. If no open PR, clean it up.
			l.logger.Warn("git", "Remote branch %s diverged from main — cleaning up", storedBranch)
			ref, _ := l.cfg.TaskBackend.GetExternalRef(taskID)
			if parsePRNumber(ref) == "" {
				if err := l.git.DeleteRemoteBranchByName(storedBranch); err != nil {
					l.logger.Warn("git", "Failed to delete stale remote branch: %v", err)
				}
			}
		}
		// Reuse the branch name locally, starting from main.
		l.git.RenameBranchTo(storedBranch)
		return false
	}
	l.git.RenameBranchForTask(nextTask, taskID)
	if taskID != "" && l.git.WorktreeBranch != "" && strings.Contains(l.git.WorktreeBranch, taskID) {
		_ = l.cfg.TaskBackend.SetMetadata(taskID, "branch", l.git.WorktreeBranch)
	}
	return false
}

// iterationPrompt holds the prepared prompt and context needed to invoke Claude.
type iterationPrompt struct {
	fullPrompt string
	headBefore string
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
			l.logger.Warn("beads", "SetState phase=implementing: %v", err)
		}
	}

	taskPrompt := l.buildTaskPrompt(nextTask, taskID)
	testStatus := l.runPreIterationTests(ctx)

	if !waitForInternet(ctx, l.logger) {
		return iterationPrompt{}, false
	}
	if !l.waitForRate(ctx) {
		return iterationPrompt{}, false
	}

	headBefore := l.git.HeadRev()
	rawLogPath := filepath.Join(l.cfg.Dirs.RalphDir, "raw.log")
	logStart := fileLineCount(rawLogPath)

	feedback := readFeedback(l.cfg.Dirs.RalphDir)
	if feedback != "" {
		l.logger.Warn("", "[feedback] %s", feedback)
		clearFeedback(l.cfg.Dirs.RalphDir)
		l.attempts.Record(taskID, nextTask,
			"User feedback (pre-iteration): "+feedback,
			"",
			"user_feedback: "+feedback)
	}

	attemptContext := l.buildAttemptContext(taskID, nextTask)
	if attemptContext != "" {
		attemptCount := strings.Count(attemptContext, "### Attempt ")
		reflectionCount := strings.Count(attemptContext, "## Recent learnings")
		if attemptCount > 0 || reflectionCount > 0 {
			l.logger.Log("", "Including prior context: %d attempt(s), cross-task learnings: %v", attemptCount, reflectionCount > 0)
		}
	}

	fullPrompt, err := l.buildPrompt(taskPrompt, attemptContext, testStatus)
	if err != nil {
		l.logger.Error("", "Prompt build failed: %v", err)
		return iterationPrompt{}, false
	}

	return iterationPrompt{
		fullPrompt: fullPrompt,
		headBefore: headBefore,
		rawLogPath: rawLogPath,
		logStart:   logStart,
		workDir:    l.git.WorkDir,
	}, true
}

// runResultAction describes what Run() should do after handling the Claude result.
type runResultAction int

const (
	resultProceed   runResultAction = iota // normal: continue to signal check
	resultRetry                            // decrement counters and continue
	resultBreak                            // break out of loop
)

// handleRunResult processes errors and retryable conditions from a Claude
// run (offline, feedback kill, idle timeout, rate limit). Returns the
// action Run() should take.
func (l *Loop) handleRunResult(ctx context.Context, result claude.Result, runErr error,
	taskID, nextTask, headBefore string, runIteration, iteration *int) runResultAction {

	if runErr != nil {
		if !isOnline() {
			l.logger.Warn("llm", "Claude failed — internet appears down")
			if !waitForInternet(ctx, l.logger) {
				return resultBreak
			}
			*runIteration--
			*iteration--
			return resultRetry
		}
		l.logger.Warn("llm", "Claude failed on iteration %d, continuing...", *runIteration)
	}
	if result.FeedbackKill {
		l.logger.Warn("llm", "Restarting iteration %d — feedback injection failed, agent killed", *runIteration)
		diffStat := l.git.DiffStatRange(headBefore, l.git.HeadRev())
		l.attempts.Record(taskID, nextTask,
			"Killed: feedback injection failed. Feedback: "+result.FeedbackContent,
			diffStat,
			"user_feedback: "+result.FeedbackContent)
		*runIteration--
		*iteration--
		return resultRetry
	}
	if result.IdleTimeout {
		l.logger.Warn("llm", "Restarting iteration %d after idle timeout", *runIteration)
		diffStat := l.git.DiffStatRange(headBefore, l.git.HeadRev())
		l.attempts.Record(taskID, nextTask,
			"Killed: idle timeout (no output for configured duration)",
			diffStat,
			"idle_timeout: consider a lighter approach or make incremental progress rather than deep-thinking without output")
		*runIteration--
		*iteration--
		return resultRetry
	}
	if result.RateLimited {
		waitDur := claude.FormatWaitDuration(time.Until(result.ResetAt))
		l.logger.Warn("llm", "Claude rate limit — waiting %s until %s", waitDur, result.ResetAt.Format("3:04pm"))
		err := l.limiter.WaitUntil(ctx, result.ResetAt, func(secs int) {
			l.logger.Log("llm", "Rate limit: %ds until reset", secs)
		})
		if err != nil {
			l.logger.Warn("llm", "Rate limit wait interrupted: %v", err)
			return resultBreak
		}
		l.logger.Success("llm", "Rate limit reset — resuming")
		*runIteration--
		*iteration--
		return resultRetry
	}

	return resultProceed
}

func (l *Loop) mergeDeps() mergeDeps {
	return mergeDeps{
		AutoMerge:     l.cfg.AutoMerge,
		Backend:       l.cfg.TaskBackend,
		GitHub:        l.git.GH(),
		DefaultBranch: l.git.DetectDefaultBranch(),
		WorkDir:       l.git.WorkDir,
		Logger:        l.logger,
		MergeFn:       l.mergeWithRetry,
	}
}

func (l *Loop) closeTaskDeps() closeTaskDeps {
	return closeTaskDeps{
		AutoMerge: l.cfg.AutoMerge,
		Backend:   l.cfg.TaskBackend,
		Attempts:  l.attempts,
		State:     l.state,
		Logger:    l.logger,
		SkipFn: func(id, reason string) {
			skipTask(l.state, l.cfg.TaskBackend, l.logger, id, reason)
		},
	}
}
