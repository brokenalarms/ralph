package git

import (
	"context"
	"fmt"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// MergeStackOpts configures a MergeStack call.
type MergeStackOpts struct {
	TopPR      string // PR number (as string, e.g. "321")
	SkipCIWait bool   // when true, skip AwaitCI and merge immediately (use when CI is known to be down)
	// AdminMergeOnCIInfraFailure authorizes admin-merge bypass of branch protection
	// when isInfrastructureFailure returns true (zero job steps). Has no effect
	// when CI failure has non-zero job steps (real test failures).
	AdminMergeOnCIInfraFailure bool
}

// MergeStackResult reports what MergeStack accomplished.
type MergeStackResult struct {
	MergedCount int
	TotalPRs    int
}

type stackPR struct {
	number int
	head   string
}

type stackResult struct {
	prs        []stackPR
	baseBranch string
}

// MergeStack merges a stack of PRs bottom-up. It collects the stack from
// the given top PR, rebases the stack onto the base branch, then iterates
// bottom-up: waiting for CI, merging, and rebasing the next PR onto the
// updated base branch.
func (r *repo) MergeStack(ctx context.Context, opts MergeStackOpts) (MergeStackResult, error) {
	if r.hasUncommittedChangesIn(r.projectDir) {
		return MergeStackResult{}, fmt.Errorf("uncommitted changes in %s — commit or stash before merging", r.projectDir)
	}

	stack := r.collectStack(opts.TopPR)
	if len(stack.prs) == 0 {
		return MergeStackResult{}, fmt.Errorf("no open PRs found starting from #%s", opts.TopPR)
	}

	defaultBranch := stack.baseBranch
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Stack: %d PRs to merge", len(stack.prs))
	for _, pr := range stack.prs {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "  PR #%d: %s", pr.number, pr.head)
	}

	merged, err := r.runMergeStack(ctx, stack.prs, defaultBranch, opts)
	return MergeStackResult{MergedCount: merged, TotalPRs: len(stack.prs)}, err
}

func (r *repo) runMergeStack(ctx context.Context, prs []stackPR, defaultBranch string, opts MergeStackOpts) (int, error) {
	topBranch := prs[len(prs)-1].head
	allBranches := make([]string, len(prs))
	for i, pr := range prs {
		allBranches[i] = pr.head
	}

	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Rebasing stack onto %s", defaultBranch)
	if err := r.RebaseStack(ctx, RebaseStackOpts{
		TopBranch:   topBranch,
		BaseBranch:  defaultBranch,
		TopPR:       prs[len(prs)-1].number,
		AllBranches: allBranches,
	}); err != nil {
		return 0, err
	}

	merged := 0
	repoURL := r.RemoteURL()
	for i, pr := range prs {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Merging PR #%d (%d/%d)", pr.number, merged+1, len(prs))

		var pushedAt time.Time
		if merged > 0 {
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Rebasing %s onto updated %s...", pr.head, defaultBranch)
			pushedAt = time.Now()
			if err := r.RebaseBranchOntoRemote(ctx, pr.head, defaultBranch); err != nil {
				return merged, fmt.Errorf("rebase conflicts on PR #%d (%s): %w\nResolve manually in: %s\nThen run: cd %s && git-rebase-continue",
					pr.number, pr.head, err, r.projectDir, r.projectDir)
			}
		}

		isInfra := false
		if opts.SkipCIWait {
			r.logger.Emit(logging.Opts{Domain: logging.CI}, "CI wait skipped for PR #%d (--no-ci-wait) — classifying via job-step count", pr.number)
			if r.isInfrastructureFailure(ctx, pr.number) {
				isInfra = true
				r.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn}, "PR #%d CI is infrastructure-only (zero job steps) — proceeding with merge", pr.number)
			} else {
				r.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn}, "PR #%d has executed CI job steps — proceeding anyway per --no-ci-wait", pr.number)
			}
		} else {
			r.logger.Emit(logging.Opts{Domain: logging.CI}, "Waiting for CI on PR #%d...", pr.number)
			_, ciStatus, ciErr := r.AwaitCI(ctx, pr.number, repoURL, pushedAt)
			if ciErr != nil {
				r.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn}, "CI polling error: %v", ciErr)
			}
			if ciStatus == CIFailed {
				if r.isInfrastructureFailure(ctx, pr.number) {
					isInfra = true
					r.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn}, "CI failure on PR #%d is infrastructure-only (zero job steps) — proceeding with merge", pr.number)
				} else {
					return merged, fmt.Errorf("CI failed on PR #%d", pr.number)
				}
			} else {
				r.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Success}, "CI passed for PR #%d", pr.number)
			}
		}

		// Retarget the next PR's base to defaultBranch before merging the current
		// one. When the current PR merges and its head branch is deleted, the next
		// PR already points to main — GitHub won't auto-close it on repos where
		// delete_branch_on_merge=false.
		if i < len(prs)-1 {
			nextPR := prs[i+1]
			r.logger.Emit(logging.Opts{Domain: logging.Git}, "Retargeting PR #%d base to %s...", nextPR.number, defaultBranch)
			if err := r.github.EditPRBase(nextPR.number, repoURL, defaultBranch); err != nil {
				return merged, fmt.Errorf("failed to retarget PR #%d base to %s: %w", nextPR.number, defaultBranch, err)
			}
		}

		mergeOpts := MergeOpts{DeleteBranch: true}
		if isInfra && opts.AdminMergeOnCIInfraFailure {
			r.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn},
				"⚠ --admin-merge-on-ci-infra-failure: PR #%d CI has zero job steps (infra outage). Merging with admin override. Required checks will not have run.", pr.number)
			mergeOpts.Admin = true
		}

		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Merging PR #%d...", pr.number)
		result := r.MergeStackPR(pr.number, mergeOpts)
		if !result.Merged {
			// Retarget was set before the merge attempt; roll it back since the
			// parent branch still exists (only deleted on successful merge).
			if i < len(prs)-1 {
				if rbErr := r.github.EditPRBase(prs[i+1].number, repoURL, pr.head); rbErr != nil {
					r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
						"Warning: failed to roll back PR #%d base to %s after merge failure: %v",
						prs[i+1].number, pr.head, rbErr)
				}
			}
			if result.Conflict {
				return merged, fmt.Errorf("PR #%d has merge conflicts — cannot merge", pr.number)
			}
			if result.Blocked {
				if isInfra {
					return merged, fmt.Errorf("PR #%d blocked by branch protection despite infra-only CI failure — re-run with --admin-merge-on-ci-infra-failure to bypass", pr.number)
				}
				return merged, fmt.Errorf("PR #%d blocked by branch protection: %s", pr.number, result.Message)
			}
			return merged, fmt.Errorf("merge failed for PR #%d: %s", pr.number, result.Message)
		}
		merged++
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Success}, "PR #%d merged (%d/%d)", pr.number, merged, len(prs))

		r.ResetBranchToRemote(ctx, defaultBranch)
	}

	r.logger.Emit(logging.Opts{Level: logging.Success}, "Stack complete — %d PRs merged", merged)
	return merged, nil
}

func (r *repo) collectStack(topPR string) stackResult {
	allPRs, err := r.github.ListAllPRs(r.projectDir)
	if err != nil {
		r.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to list PRs: %v", err)
		return stackResult{}
	}
	return collectStackFromPRs(allPRs, topPR)
}

// collectStackFromPRs is the pure logic of stack collection: given a
// slice of PRInfo and a top PR number, walks the base chain and returns
// the stack in bottom-up order.
func collectStackFromPRs(allPRs []PRInfo, topPR string) stackResult {
	type prInfo struct {
		number int
		head   string
		base   string
		state  PRState
	}
	byHead := make(map[string]prInfo)
	byNumber := make(map[int]prInfo)
	for _, pr := range allPRs {
		info := prInfo{pr.Number, pr.Head, pr.Base, pr.State}
		byNumber[pr.Number] = info
		existing, exists := byHead[pr.Head]
		if !exists || pr.State == PRStateOpen && existing.state != PRStateOpen {
			byHead[pr.Head] = info
		}
	}

	topNum := 0
	if n, _ := fmt.Sscanf(topPR, "%d", &topNum); n == 0 || topNum <= 0 {
		return stackResult{}
	}
	start, ok := byNumber[topNum]
	if !ok {
		return stackResult{}
	}

	var chain []stackPR
	if start.state == PRStateOpen {
		chain = append(chain, stackPR{number: topNum, head: start.head})
	}
	currentBase := start.base

	for i := 0; i < 20; i++ {
		pr, found := byHead[currentBase]
		if !found {
			break
		}
		if pr.state == PRStateOpen {
			chain = append(chain, stackPR{number: pr.number, head: pr.head})
		}
		currentBase = pr.base
	}

	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return stackResult{prs: chain, baseBranch: currentBase}
}
