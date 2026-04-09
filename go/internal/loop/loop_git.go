package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/brokenalarms/ralph/internal/attempts"
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

// prLink builds a logging.Link for a PR number using the remote URL.
func prLink(g git.GitOps, prNumber int) *logging.Link {
	url := prURL(g.RemoteURL(), prNumber)
	if url == "" {
		return nil
	}
	return &logging.Link{Text: fmt.Sprintf("PR #%d", prNumber), URL: url}
}

// prLinkURL builds a logging.Link for a PR from a remote URL string.
func prLinkURL(remoteURL string, prNumber int) *logging.Link {
	url := prURL(remoteURL, prNumber)
	if url == "" {
		return nil
	}
	return &logging.Link{Text: fmt.Sprintf("PR #%d", prNumber), URL: url}
}

// mergeWithRetryParams bundles the dependencies for mergeWithRetry.
type mergeWithRetryParams struct {
	taskID     string
	nextTask   string
	workDir    string
	rawLogPath string
	// mergeFunc overrides git.MergeWithRetry for tests; nil uses the real path.
	mergeFunc func(ctx context.Context) (bool, error)
	git       git.GitOps
	verifier  *Verifier
	logger    *logging.Logger
	backend   tasks.Backend
}

// mergeWithRetry delegates to git.Manager.MergeWithRetry, passing CI fix and
// conflict callbacks. When mergeFunc is set (test override), it short-circuits
// the git module call.
func mergeWithRetry(ctx context.Context, p mergeWithRetryParams) (bool, error) {
	if p.mergeFunc != nil {
		return p.mergeFunc(ctx)
	}
	return p.git.MergeWithRetry(ctx, git.MergeRetryOpts{
		OnCIFailure: func(ciErr *git.CIFailureError) git.CIFixResult {
			return tryFixCI(ctx, p.git, p.verifier, p.logger, ciErr, p.nextTask, p.workDir, p.rawLogPath)
		},
		OnConflict: func(conflictErr *git.UnresolvedConflictError) bool {
			return tryFixConflict(ctx, p.git, p.verifier, p.logger, p.backend, p.taskID, p.nextTask, p.workDir, p.rawLogPath)
		},
	})
}

// resumeViaPRParams bundles the dependencies for resumeViaPR.
type resumeViaPRParams struct {
	taskID    string
	nextTask  string
	backend   tasks.Backend
	git       git.GitOps
	logger    *logging.Logger
	attempts  *attempts.Tracker
	state     *state.Store
	autoMerge bool
	notify    bool
	ralphDir  string
	verifier  *Verifier
	// mergeFunc overrides git.MergeWithRetry for tests; nil uses the real path.
	mergeFunc func(ctx context.Context) (bool, error)
}

// findExistingPRForTask returns any PR number (open, closed, or merged) for the
// given task. It checks the external-ref first, then finds any PR for the branch.
// Returns (prNumber, true) if found; (0, false) otherwise.
func findExistingPRForTask(taskID, branch string, backend tasks.Backend, g git.GitOps) (int, bool) {
	ref, _ := backend.GetExternalRef(taskID)
	if num := parsePRNumber(ref); num != 0 {
		return num, true
	}
	if branch != "" {
		if num, _, _, err := g.FindPRForBranch(branch); err == nil && num != 0 {
			return num, true
		}
	}
	return 0, false
}

// resumeViaPR checks the bead's metadata and external-ref for existing work
// and resolves accordingly. Returns true if the task was fully handled (merged
// or skipped) and the loop should continue to the next task; false if the
// agent should run.
func resumeViaPR(ctx context.Context, p resumeViaPRParams) bool {
	if p.taskID == "" {
		return false
	}

	// Check metadata for the branch name stored when work started.
	// An invalid branch (not containing taskID) is treated as missing, but
	// the external-ref check via findExistingPRForTask still applies.
	branch, _ := p.backend.GetMetadata(p.taskID, "branch")
	if branch != "" && !strings.Contains(branch, p.taskID) {
		branch = ""
	}

	// Check external-ref and any PR for the branch (in any state: open, closed, merged).
	prNumber, found := findExistingPRForTask(p.taskID, branch, p.backend, p.git)
	if found {
		// Only log and set external-ref when discovered via branch (external-ref already set otherwise).
		existingRef, _ := p.backend.GetExternalRef(p.taskID)
		if parsePRNumber(existingRef) == 0 && branch != "" {
			p.logger.Emit(logging.Opts{Domain: "git", Link: prLink(p.git, prNumber)}, "Found for %s (task %s) — resolving", branch, p.taskID)
			_ = p.backend.SetExternalRef(p.taskID, prURL(p.git.RemoteURL(), prNumber))
		}
		return resolveByPRState(ctx, resolveByPRStateParams{
			taskID:    p.taskID,
			nextTask:  p.nextTask,
			prNumber:  prNumber,
			backend:   p.backend,
			git:       p.git,
			logger:    p.logger,
			attempts:  p.attempts,
			state:     p.state,
			autoMerge: p.autoMerge,
			notify:    p.notify,
			ralphDir:  p.ralphDir,
			verifier:  p.verifier,
			mergeFunc: p.mergeFunc,
		})
	}

	// No PR found — check if the remote branch exists with clean work on top of main.
	if branch == "" {
		return false
	}
	_ = p.git.FetchBranch(branch)
	if p.git.RemoteBranchHasCommits(branch) {
		if !p.git.RemoteBranchIsOnMain(branch) {
			p.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Remote branch %s diverged from main — abandoning stale work", branch)
			_ = p.git.DeleteRemoteBranchByName(branch)
			return false
		}
		if !p.git.BranchIsAheadOfMain(branch) {
			p.logger.Emit(logging.Opts{Domain: logging.Git}, "Remote branch %s has no commits ahead of main — already resolved, cleaning up", branch)
			_ = p.git.DeleteRemoteBranchByName(branch)
			return false
		}
		p.logger.Emit(logging.Opts{Domain: logging.Git}, "Remote branch %s has clean work but no PR — creating PR", branch)
		p.git.CheckoutRemoteBranch(branch)
		var err error
		var shipResult git.ShipResult
		shipResult, err = p.git.Ship(ctx, git.ShipOpts{TaskID: p.taskID, TaskTitle: p.nextTask})
		prNum := shipResult.PRNumber
		if err == nil && prNum != 0 {
			p.logger.Emit(logging.Opts{Domain: "git", Link: prLink(p.git, prNum)}, "Created for %s (task %s)", branch, p.taskID)
			_ = p.backend.SetExternalRef(p.taskID, prURL(p.git.RemoteURL(), prNum))
			return resolveByPRState(ctx, resolveByPRStateParams{
				taskID:    p.taskID,
				nextTask:  p.nextTask,
				prNumber:  prNum,
				backend:   p.backend,
				git:       p.git,
				logger:    p.logger,
				attempts:  p.attempts,
				state:     p.state,
				autoMerge: p.autoMerge,
				notify:    p.notify,
				ralphDir:  p.ralphDir,
				verifier:  p.verifier,
				mergeFunc: p.mergeFunc,
			})
		}
	}

	return false
}

// resolveByPRStateParams bundles the dependencies for resolveByPRState.
type resolveByPRStateParams struct {
	taskID    string
	nextTask  string
	prNumber  int
	backend   tasks.Backend
	git       git.GitOps
	logger    *logging.Logger
	attempts  *attempts.Tracker
	state     *state.Store
	autoMerge bool
	notify    bool
	ralphDir  string
	verifier  *Verifier
	mergeFunc func(ctx context.Context) (bool, error)
}

// resolveByPRState inspects the PR's state and takes the appropriate action.
// Delegates the merge pipeline to Ship and bead-closing to closeBeadAfterShip.
func resolveByPRState(ctx context.Context, p resolveByPRStateParams) bool {
	rawLogPath := filepath.Join(p.ralphDir, "raw.log")

	shipResult, shipErr := p.git.Ship(ctx, git.ShipOpts{
		PRNumber:  p.prNumber,
		AutoMerge: p.autoMerge,
		Logger:    p.logger,
		MergeFn: func(ctx context.Context) (bool, error) {
			return mergeWithRetry(ctx, mergeWithRetryParams{
				taskID:     p.taskID,
				nextTask:   p.nextTask,
				workDir:    p.git.GetWorkDir(),
				rawLogPath: rawLogPath,
				mergeFunc:  p.mergeFunc,
				git:        p.git,
				verifier:   p.verifier,
				logger:     p.logger,
				backend:    p.backend,
			})
		},
		OnReviewFix: func(ctx context.Context, botUsername string, review *git.AutoReview, prNumber int) bool {
			return tryFixReviewComments(ctx, p.git, p.verifier, p.logger, botUsername, review, prNumber, p.nextTask, p.git.GetWorkDir(), rawLogPath)
		},
		ReviewAddressedFn: func(botUsername string) bool {
			if p.state == nil || p.taskID == "" {
				return false
			}
			v, _ := p.state.Read("review_addressed:" + botUsername + ":" + p.taskID)
			return v == "true"
		},
		MarkReviewAddressedFn: func(botUsername string) {
			if p.state == nil || p.taskID == "" {
				return
			}
			p.state.Write("review_addressed:"+botUsername+":"+p.taskID, "true")
		},
	})

	if isCIError(shipErr) {
		return false
	}

	switch shipResult.PRState {
	case git.PRStateMerged:
		p.logger.Emit(logging.Opts{Domain: "git", Level: logging.Success, Link: prLink(p.git, p.prNumber)}, "already merged — closing bead and moving on")
		p.attempts.Clear(p.taskID, p.nextTask)
		p.state.RecordCompletedTask(p.taskID, p.nextTask)
		closeBeadAfterShip(closeBeadParams{
			ctx:       ctx,
			taskID:    p.taskID,
			prNumber:  p.prNumber,
			remoteURL: p.git.RemoteURL(),
			merged:    shipResult.Merged,
			logger:    p.logger,
			backend:   p.backend,
			state:     p.state,
			attempts:  p.attempts,
		})
		if p.notify {
			notify.TaskCompleted(p.taskID, p.nextTask, "")
		}
		notify.TaskMerged(p.taskID, p.nextTask)
		return true

	case git.PRStateOpen:
		if ok, reason := p.git.PRChainIsHealthy(p.prNumber); !ok {
			p.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: prLink(p.git, p.prNumber)}, "chain unhealthy: %s — re-running agent", reason)
			return false
		}
		closeBeadAfterShip(closeBeadParams{
			ctx:         ctx,
			taskID:      p.taskID,
			prNumber:    p.prNumber,
			remoteURL:   p.git.RemoteURL(),
			merged:      shipResult.Merged,
			mergeFailed: shipResult.MergeFailed,
			logger:      p.logger,
			backend:     p.backend,
			state:       p.state,
			attempts:    p.attempts,
		})
		if p.notify {
			notify.TaskCompleted(p.taskID, p.nextTask, "")
		}
		return true

	default:
		p.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: prLink(p.git, p.prNumber)}, "is %s (not merged) — re-running agent", shipResult.PRState)
		if p.taskID != "" {
			_ = p.backend.SetExternalRef(p.taskID, "")
			_ = p.backend.SetMetadata(p.taskID, "branch", "")
		}
		p.git.PrepareForNextTask(p.taskID)
		checkoutExistingBranch(p.git, p.backend, p.logger, p.taskID, p.nextTask)
		return false
	}
}

// flushUnpushedWorkParams bundles the dependencies for flushUnpushedWork.
type flushUnpushedWorkParams struct {
	autoMerge      bool
	lastTaskMerged bool
	state          *state.Store
	git            git.GitOps
	logger         *logging.Logger
}

// flushUnpushedWork pushes any unpushed commits and optionally merges before
// the loop exits or enters wait mode. lastTaskMerged prevents a double-merge
// when the signal handler already merged the task.
func flushUnpushedWork(ctx context.Context, p flushUnpushedWorkParams) {
	taskID, _ := p.state.Read("last_task_id")
	taskDesc, _ := p.state.Read("last_task")
	merged, err := p.git.FlushUnpushedWork(ctx, taskID, taskDesc, p.autoMerge && !p.lastTaskMerged)
	if err != nil {
		p.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Flush: %v", err)
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
