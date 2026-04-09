package loop

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/notify"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// branchParams bundles the dependencies needed by prepareBranch and its helpers.
type branchParams struct {
	git     git.GitOps
	backend tasks.Backend
	state   *state.Store
	logger  *logging.Logger
}

// prepareBranch is the package-level implementation of branch setup for a task.
// Called by the Loop method wrapper which passes its fields as a branchParams.
func prepareBranch(ctx context.Context, p branchParams, taskID, title string) error {
	p.git.PrepareForNextTask(taskID)

	if p.git.GetWorktreeBranch() != "" && p.git.GetWorkDir() != p.git.GetProjectDir() {
		setStackHead(p.git, p.backend, p.state, p.logger)
		if p.git.GetPrevBranch() == "" {
			p.git.ResetToDefaultBranch()
		}
		if err := p.git.EnsureUpToDate(ctx); err != nil {
			return err
		}
	} else {
		setStackHead(p.git, p.backend, p.state, p.logger)
	}

	if _, err := checkoutExistingBranch(p.git, p.backend, p.logger, taskID, title); err != nil {
		return err
	}
	p.state.WriteRunBranch(p.git.GetWorktreeBranch())
	return nil
}

// setStackHead finds the most recent branch that's cleanly ahead of main
// and sets it as the stack base for the next task. When gh is available,
// fetches only branches with open PRs (one API call). Otherwise falls
// back to checking all completed task branches.
func setStackHead(g git.GitOps, backend tasks.Backend, st *state.Store, logger *logging.Logger) {
	g.SetPrevBranch("")

	completedTasks, err := st.GetCompletedTasks()
	if err != nil || len(completedTasks) == 0 {
		return
	}

	openBranches, err := g.ListOpenPRBranches()
	if err != nil || len(openBranches) == 0 {
		return
	}
	openSet := make(map[string]bool, len(openBranches))
	for _, b := range openBranches {
		openSet[b] = true
	}

	for i := len(completedTasks) - 1; i >= 0; i-- {
		id := completedTasks[i].ID
		if id == "" {
			continue
		}
		branch, _ := backend.GetMetadata(id, "branch")
		if branch == "" || !openSet[branch] {
			continue
		}
		if err := g.FetchBranch(branch); err != nil {
			continue
		}
		if !g.RemoteBranchHasCommits(branch) {
			continue
		}
		if !g.BranchIsAheadOfMain(branch) {
			logger.Emit(logging.Opts{Domain: logging.Git}, "Branch %s not ahead of main — skipping", branch)
			continue
		}
		g.SetPrevBranch(branch)
		logger.Emit(logging.Opts{Domain: logging.Git}, "Stack head: %s (from %s)", branch, id)
		return
	}
	logger.Emit(logging.Opts{Domain: logging.Git}, "No stacked parents — starting from %s", g.DetectDefaultBranch())
}

// checkoutExistingBranch checks metadata for a branch from a previous
// iteration. If the remote has that branch with work, it checks it out.
// Otherwise, it renames the current branch for the task and stores the
// new name in metadata. Returns true if an existing remote branch was
// checked out. Returns an error if the branch rename fails.
func checkoutExistingBranch(g git.GitOps, backend tasks.Backend, logger *logging.Logger, taskID, nextTask string) (bool, error) {
	storedBranch := ""
	if taskID != "" {
		storedBranch, _ = backend.GetMetadata(taskID, "branch")
	}
	if storedBranch != "" {
		_ = g.FetchBranch(storedBranch)
		if g.RemoteBranchHasCommits(storedBranch) {
			if g.RemoteBranchIsOnMain(storedBranch) {
				g.CheckoutRemoteBranch(storedBranch)
				return true, nil
			}
			logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Remote branch %s diverged from main — cleaning up", storedBranch)
			ref, _ := backend.GetExternalRef(taskID)
			if parsePRNumber(ref) == 0 {
				if err := g.DeleteRemoteBranchByName(storedBranch); err != nil {
					logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to delete stale remote branch: %v", err)
				}
			}
		}
		g.RenameBranchTo(storedBranch)
		return false, nil
	}
	if err := g.RenameBranchForTask(nextTask, taskID); err != nil {
		return false, fmt.Errorf("branch rename failed: %w", err)
	}
	if taskID != "" && g.GetWorktreeBranch() != "" && strings.Contains(g.GetWorktreeBranch(), taskID) {
		_ = backend.SetMetadata(taskID, "branch", g.GetWorktreeBranch())
	}
	return false, nil
}

// prLink builds a logging.Link for a PR number using the loop's git remote URL.
func (l *Loop) prLink(prNumber int) *logging.Link {
	url := prURL(l.git.RemoteURL(), prNumber)
	if url == "" {
		return nil
	}
	return &logging.Link{Text: fmt.Sprintf("PR #%d", prNumber), URL: url}
}

// closeResumedTask closes the bead after the resume path resolves the PR.
func (l *Loop) closeResumedTask(ctx context.Context, taskID, taskTitle string, result git.ResumeTaskResult) {
	if taskID == "" {
		return
	}
	if ctx.Err() != nil {
		l.logger.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — leaving bead %s open", taskID)
		return
	}
	prRef := prURL(l.git.RemoteURL(), result.PRNumber)
	if prRef == "" && result.PRNumber != 0 {
		prRef = fmt.Sprintf("PR #%d", result.PRNumber)
	}
	merged := result.AlreadyMerged || result.Merged
	var closeReason string
	if prRef == "" {
		closeReason = "Verified — merge pending"
	} else if merged {
		closeReason = fmt.Sprintf("Fixed in %s", prRef)
	} else {
		closeReason = fmt.Sprintf("Verified — %s open, merge pending", prRef)
	}
	l.attempts.ClearMergeFailures(taskID)
	if result.AlreadyMerged {
		l.attempts.Clear(taskID, taskTitle)
		l.state.RecordCompletedTask(taskID, taskTitle)
	}
	stateReason := "ralph: PR open or stacked"
	if merged {
		stateReason = "ralph: PR merged"
	}
	_ = l.cfg.TaskBackend.SetState(taskID, "phase", "verified", stateReason)
	if err := l.cfg.TaskBackend.CloseTask(taskID, closeReason); err != nil {
		skipReason := "close_failed"
		if blockers := tasks.ParseDependencyBlock(err); len(blockers) > 0 {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask: %s blocked by %v", taskID, blockers)
			skipReason = fmt.Sprintf("dependency_blocked_by:%s", strings.Join(blockers, ","))
		} else {
			l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "CloseTask failed: %v", err)
		}
		l.skipTask(taskID, skipReason)
	} else {
		l.logger.Emit(logging.Opts{Domain: logging.Beads}, "Closed task %s (%s)", taskID, closeReason)
		if err := l.state.AddCompletedTask(taskID, merged); err != nil {
			l.logger.Emit(logging.Opts{Domain: "state", Level: logging.Warn}, "AddCompletedTask: %v", err)
		}
	}
}

// onResumeDone handles the loop-level aftermath after ResumeTask reports the task was handled:
// closes the bead and sends notifications.
func (l *Loop) onResumeDone(ctx context.Context, taskID, taskTitle string, result git.ResumeTaskResult) {
	l.closeResumedTask(ctx, taskID, taskTitle, result)
	if l.cfg.Notify {
		notify.TaskCompleted(taskID, taskTitle, "")
	}
	if result.AlreadyMerged {
		notify.TaskMerged(taskID, taskTitle)
	}
}

// readReviewAddressedForTask reads from state which reviewers had their
// feedback addressed for the given task, returning a map of botUsername → true.
func readReviewAddressedForTask(st *state.Store, taskID string, reviewers []git.Reviewer) map[string]bool {
	if st == nil || taskID == "" || len(reviewers) == 0 {
		return nil
	}
	result := make(map[string]bool, len(reviewers))
	for _, r := range reviewers {
		v, _ := st.Read("review_addressed:" + r.BotUsername + ":" + taskID)
		if v == "true" {
			result[r.BotUsername] = true
		}
	}
	return result
}

// writeReviewAddressed records that a reviewer's feedback was addressed for
// the given task so subsequent Ship calls skip re-polling.
func writeReviewAddressed(st *state.Store, taskID, botUsername string) {
	if st == nil || taskID == "" || botUsername == "" {
		return
	}
	st.Write("review_addressed:"+botUsername+":"+taskID, "true")
}

// flushUnpushedWork pushes any unpushed commits and optionally merges before
// the loop exits or enters wait mode. lastTaskMerged prevents a double-merge
// when the signal handler already merged the task.
func (l *Loop) flushUnpushedWork(ctx context.Context, lastTaskMerged bool) {
	taskID, _ := l.state.Read("last_task_id")
	taskDesc, _ := l.state.Read("last_task")
	merged, err := l.git.FlushUnpushedWork(ctx, taskID, taskDesc, l.cfg.AutoMerge && !lastTaskMerged)
	if err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Flush: %v", err)
	}
	if merged {
		notify.TaskMerged(taskID, taskDesc)
	}
}

// buildPRBody assembles a PR description from bead context and agent summary.
// Uses whatever context is available — bead description, acceptance criteria,
// agent summary — and composes them into a coherent body.
func buildPRBody(backend tasks.Backend, taskID, summary string) string {
	var sections []string

	if taskID != "" {
		if desc, err := backend.GetDescription(taskID); err == nil && desc != "" {
			sections = append(sections, "## Description\n"+desc)
		}
		if ac, err := backend.GetAcceptance(taskID); err == nil && ac != "" {
			sections = append(sections, "## Acceptance Criteria\n"+ac)
		}
	}

	if summary != "" {
		sections = append(sections, "## Summary\n"+summary)
	}

	if len(sections) == 0 {
		return ""
	}
	return strings.Join(sections, "\n\n")
}

// prURL builds the canonical PR URL from the remote URL and PR number.
// Always returns a full URL; never returns a "gh-" prefixed string.
func prURL(remoteURL string, prNumber int) string {
	nwo := git.NWOFromRemote(remoteURL)
	if nwo == "" || prNumber == 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", nwo, prNumber)
}

// parsePRNumber extracts a PR number from a URL
// (https://github.com/owner/repo/pull/123) or a legacy gh-123 ref.
func parsePRNumber(ref string) int {
	var s string
	if strings.HasPrefix(ref, "gh-") {
		s = strings.TrimPrefix(ref, "gh-")
	} else if strings.Contains(ref, "/pull/") {
		parts := strings.Split(ref, "/pull/")
		if len(parts) == 2 && parts[1] != "" {
			s = parts[1]
		}
	}
	n, _ := strconv.Atoi(s)
	return n
}
