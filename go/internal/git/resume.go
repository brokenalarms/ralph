package git

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/brokenalarms/ralph/internal/logging"
)

// ResumeTaskMeta is task data the loop extracts from the backend before calling ResumeTask.
type ResumeTaskMeta struct {
	TaskID      string
	TaskTitle   string
	Branch      string // from backend.GetMetadata(taskID, "branch")
	ExternalRef string // from backend.GetExternalRef(taskID)
}

// ResumeTaskOpts controls ResumeTask behavior.
type ResumeTaskOpts struct {
	AutoMerge       bool
	Reviewers       []Reviewer
	ReviewAddressed map[string]bool
}

// ResumeTaskResult is the outcome of ResumeTask.
type ResumeTaskResult struct {
	// Handled is true when the task was resolved and the loop should continue
	// to the next task. False means the agent should run.
	Handled bool

	// AlreadyMerged is true when the PR was already merged before ResumeTask ran.
	AlreadyMerged bool

	// Merged is true when the PR was merged during this call.
	Merged bool

	// PRNumber is the PR found or created; 0 if no prior work was found.
	PRNumber int

	// PRURLToStore is non-empty when ResumeTask discovered a PR via branch lookup
	// (not via existing external-ref). The loop should persist this as external-ref.
	PRURLToStore string

	// ClearMetadata is true when the PR was closed (not merged). The loop should
	// clear the external-ref and update the branch metadata to NewBranch.
	ClearMetadata bool

	// NewBranch is the branch name after rename (only set when ClearMetadata=true).
	// The loop should store this in branch metadata after clearing the external-ref.
	NewBranch string
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

// buildPRURL constructs the canonical PR URL from the manager's remote URL.
func (r *Repo) buildPRURL(prNumber int) string {
	nwo := NWOFromRemote(r.RemoteURL())
	if nwo == "" || prNumber == 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", nwo, prNumber)
}

// buildPRLink constructs a logging.Link for a PR number.
func (r *Repo) buildPRLink(prNumber int) *logging.Link {
	url := r.buildPRURL(prNumber)
	if url == "" {
		return nil
	}
	return &logging.Link{Text: fmt.Sprintf("PR #%d", prNumber), URL: url}
}

// findExistingPR returns any PR number (open, closed, or merged) for the given task.
// Checks external-ref first, then finds any PR for the branch.
func (r *Repo) findExistingPR(meta ResumeTaskMeta) (int, bool) {
	if num := parsePRNumber(meta.ExternalRef); num != 0 {
		return num, true
	}
	if meta.Branch != "" {
		if num, _, _, err := r.FindPRForBranch(meta.Branch); err == nil && num != 0 {
			return num, true
		}
	}
	return 0, false
}

// ResumeTask checks for existing work for the task and resolves it.
// Returns a ResumeTaskResult describing what happened — whether the task was
// fully handled (loop continues to next task) or the agent should run.
func (r *Repo) ResumeTask(ctx context.Context, meta ResumeTaskMeta, opts ResumeTaskOpts) (ResumeTaskResult, error) {
	if meta.TaskID == "" {
		return ResumeTaskResult{}, nil
	}

	prNumber, found := r.findExistingPR(meta)
	var prURLToStore string
	if found {
		// When found via branch (no existing external-ref), capture the URL for the loop to persist.
		if parsePRNumber(meta.ExternalRef) == 0 && meta.Branch != "" {
			r.logger.Emit(logging.Opts{Domain: "git", Link: r.buildPRLink(prNumber)}, "Found for %s (task %s) — resolving", meta.Branch, meta.TaskID)
			prURLToStore = r.buildPRURL(prNumber)
		}
		result, err := r.resolveByState(ctx, prNumber, meta, opts)
		result.PRURLToStore = prURLToStore
		return result, err
	}

	// No PR found — check if the remote branch has clean work on top of main.
	if meta.Branch == "" {
		return ResumeTaskResult{}, nil
	}
	_ = r.FetchBranch(meta.Branch)
	if !r.RemoteBranchHasCommits(meta.Branch) {
		return ResumeTaskResult{}, nil
	}
	if !r.RemoteBranchIsOnMain(meta.Branch) {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Remote branch %s diverged from main — abandoning stale work", meta.Branch)
		_ = r.DeleteRemoteBranchByName(meta.Branch)
		return ResumeTaskResult{}, nil
	}
	if !r.BranchIsAheadOfMain(meta.Branch) {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Remote branch %s has no commits ahead of main — already resolved, cleaning up", meta.Branch)
		_ = r.DeleteRemoteBranchByName(meta.Branch)
		return ResumeTaskResult{}, nil
	}

	// Branch has clean work but no PR — create one via Ship.
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Remote branch %s has clean work but no PR — creating PR", meta.Branch)
	r.CheckoutRemoteBranch(meta.Branch)
	shipResult, err := r.Ship(ctx, ShipOpts{TaskID: meta.TaskID, TaskTitle: meta.TaskTitle})
	prNum := shipResult.PRNumber
	if err != nil || prNum == 0 {
		return ResumeTaskResult{}, nil
	}
	prURLToStore = r.buildPRURL(prNum)
	r.logger.Emit(logging.Opts{Domain: "git", Link: r.buildPRLink(prNum)}, "Created for %s (task %s)", meta.Branch, meta.TaskID)
	result, err := r.resolveByState(ctx, prNum, meta, opts)
	result.PRURLToStore = prURLToStore
	return result, err
}

// resolveByState inspects PR state and returns what the loop should do next.
func (r *Repo) resolveByState(ctx context.Context, prNumber int, meta ResumeTaskMeta, opts ResumeTaskOpts) (ResumeTaskResult, error) {
	shipResult, err := r.Ship(ctx, ShipOpts{
		PRNumber:        prNumber,
		AutoMerge:       opts.AutoMerge,
		Reviewers:       opts.Reviewers,
		ReviewAddressed: opts.ReviewAddressed,
	})
	if err != nil {
		r.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: r.buildPRLink(prNumber)}, "Ship (resume): %v", err)
		return ResumeTaskResult{PRNumber: prNumber}, nil
	}

	switch {
	case shipResult.AlreadyMerged:
		r.logger.Emit(logging.Opts{Domain: "git", Level: logging.Success, Link: r.buildPRLink(prNumber)}, "already merged — closing bead and moving on")
		return ResumeTaskResult{Handled: true, AlreadyMerged: true, Merged: true, PRNumber: prNumber}, nil

	case shipResult.Closed:
		r.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: r.buildPRLink(prNumber)}, "is closed (not merged) — re-running agent")
		r.PrepareForNextTask(meta.TaskID)
		_ = r.RenameBranchForTask(meta.TaskTitle, meta.TaskID)
		newBranch := ""
		if meta.TaskID != "" && strings.Contains(r.WorktreeBranch, meta.TaskID) {
			newBranch = r.WorktreeBranch
		}
		return ResumeTaskResult{PRNumber: prNumber, ClearMetadata: true, NewBranch: newBranch}, nil

	default:
		// PR is open — Ship already attempted merge if AutoMerge was set.
		if ok, reason := r.PRChainIsHealthy(prNumber); !ok {
			r.logger.Emit(logging.Opts{Domain: "git", Level: logging.Warn, Link: r.buildPRLink(prNumber)}, "chain unhealthy: %s — re-running agent", reason)
			return ResumeTaskResult{PRNumber: prNumber}, nil
		}
		return ResumeTaskResult{Handled: true, Merged: shipResult.Merged, PRNumber: prNumber}, nil
	}
}
