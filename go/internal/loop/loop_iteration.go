package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/notify"
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
	l.recordCompletedTask(p.taskID, p.nextTask)
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
	merged, mergeErr := l.mergeIfEnabled(p, prNumber)

	l.closeOrRetryTask(p.taskID, ct, merged, mergeErr)
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
	prBody := l.buildPRBody(p.taskID, p.result.Summary)
	prNumber, pushErr := l.pushAndCreatePR(p.ctx, p.taskID, p.nextTask, prBody)
	if pushErr != nil {
		if !isOnline() {
			l.logger.Warn("git", "Push failed — internet appears down")
			l.waitForInternet(p.ctx)
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

// mergeIfEnabled attempts to auto-merge after push, including checking
// existing PRs when no new PR was created. Returns the merge error so
// callers can distinguish stacked-PR-waiting from real failures.
func (l *Loop) mergeIfEnabled(p postSignalParams, prNumber string) (bool, error) {
	merged := false
	var lastErr error
	if l.cfg.AutoMerge && prNumber != "" {
		var mergeErr error
		merged, mergeErr = l.mergeWithRetry(p.ctx, p.taskID, p.nextTask, p.workDir, p.rawLogPath)
		if mergeErr != nil {
			l.logger.Warn("git", "Auto-merge: %v", mergeErr)
			lastErr = mergeErr
		}
	}

	if prNumber == "" && p.taskID != "" && l.cfg.AutoMerge {
		ref, _ := l.cfg.TaskBackend.GetExternalRef(p.taskID)
		if existingPR := parsePRNumber(ref); existingPR != "" {
			gh := l.git.GH()
			if gh != nil {
				prState, _ := gh.GetPRState(p.workDir, existingPR)
				switch strings.ToUpper(prState) {
				case "MERGED":
					l.logger.Log("git", "PR #%s already merged — work is on main", existingPR)
					merged = true
				case "OPEN":
					prBase := l.getPRBase(existingPR)
					defaultBranch := l.git.DetectDefaultBranch()
					if prBase != "" && prBase != defaultBranch {
						l.logger.Log("git", "Existing PR #%s targets %s (not %s) — stacked, skipping merge", existingPR, prBase, defaultBranch)
					} else {
						l.logger.Log("git", "Existing PR #%s still open — attempting merge", existingPR)
						var mergeErr error
						merged, mergeErr = l.mergeWithRetry(p.ctx, p.taskID, p.nextTask, p.workDir, p.rawLogPath)
						if mergeErr != nil {
							l.logger.Warn("git", "Auto-merge existing PR: %v", mergeErr)
						}
					}
				}
			}
		}
	}

	return merged, lastErr
}

// closeOrRetryTask closes the bead. The PR exists so the work is done.
// If merge failed after retries (CI fix agent already tried), skip for
// manual review — re-running the agent won't produce new information.
func (l *Loop) closeOrRetryTask(taskID string, ct CompletedTask, merged bool, mergeErr error) {
	if taskID == "" {
		return
	}

	closeReason := "completed by ralph"
	if ct.PRURL != "" {
		closeReason = fmt.Sprintf("Fixed in %s", ct.PRURL)
	} else if ct.PRNum != "" {
		closeReason = fmt.Sprintf("Fixed in PR #%s", ct.PRNum)
	}

	if !merged && l.cfg.AutoMerge && mergeErr != nil {
		l.logger.Warn("git", "Merge failed — skipping task %s (PR exists for manual review)", taskID)
		l.skipTask(taskID, "merge_failed")
		return
	}

	l.attempts.ClearMergeFailures(taskID)
	if err := l.cfg.TaskBackend.CloseTask(taskID, closeReason); err != nil {
		l.logger.Warn("beads", "CloseTask failed: %v", err)
	} else {
		l.logger.Log("beads", "Closed task %s (%s)", taskID, closeReason)
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
			if l.git.RemoteBranchRebasesCleanly(storedBranch) {
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

	if !l.waitForInternet(ctx) {
		return iterationPrompt{}, false
	}
	if !l.waitForRate(ctx) {
		return iterationPrompt{}, false
	}

	headBefore := l.git.HeadRev()
	rawLogPath := filepath.Join(l.cfg.Dirs.RalphDir, "raw.log")
	logStart := fileLineCount(rawLogPath)

	feedback := l.readFeedback()
	if feedback != "" {
		l.logger.Warn("", "[feedback] %s", feedback)
		l.clearFeedback()
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
			if !l.waitForInternet(ctx) {
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
