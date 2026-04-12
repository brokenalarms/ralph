package git

import (
	"context"
	"fmt"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// MergeStackOpts configures a MergeStack call.
type MergeStackOpts struct {
	TopPR string // PR number (as string, e.g. "321")
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
func (r *Repo) MergeStack(ctx context.Context, opts MergeStackOpts) (MergeStackResult, error) {
	stack := r.collectStack(opts.TopPR)
	if len(stack.prs) == 0 {
		return MergeStackResult{}, fmt.Errorf("no open PRs found starting from #%s", opts.TopPR)
	}

	defaultBranch := stack.baseBranch
	r.logger.Emit(logging.Opts{Domain: logging.Git}, "Stack: %d PRs to merge", len(stack.prs))
	for _, pr := range stack.prs {
		r.logger.Emit(logging.Opts{Domain: logging.Git}, "  PR #%d: %s", pr.number, pr.head)
	}

	merged, err := r.runMergeStack(ctx, stack.prs, defaultBranch)
	return MergeStackResult{MergedCount: merged, TotalPRs: len(stack.prs)}, err
}

func (r *Repo) runMergeStack(ctx context.Context, prs []stackPR, defaultBranch string) (int, error) {
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
	for _, pr := range prs {
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

		r.logger.Emit(logging.Opts{Domain: logging.CI}, "Waiting for CI on PR #%d...", pr.number)
		_, ciStatus, ciErr := r.AwaitCI(ctx, pr.number, repoURL, pushedAt)
		if ciErr != nil {
			r.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn}, "CI polling error: %v", ciErr)
		}
		if ciStatus == CIFailed {
			return merged, fmt.Errorf("CI failed on PR #%d", pr.number)
		}
		r.logger.Emit(logging.Opts{Domain: logging.CI, Level: logging.Success}, "CI passed for PR #%d", pr.number)

		r.logger.Emit(logging.Opts{Domain: logging.Git}, "Merging PR #%d...", pr.number)
		result := r.MergeStackPR(pr.number, MergeOpts{DeleteBranch: true})
		if !result.Merged {
			if result.Conflict {
				return merged, fmt.Errorf("PR #%d has merge conflicts — cannot merge", pr.number)
			}
			if result.Blocked {
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

func (r *Repo) collectStack(topPR string) stackResult {
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
	fmt.Sscanf(topPR, "%d", &topNum)
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
