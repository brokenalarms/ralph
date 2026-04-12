package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
)

func handleMerge(sub config.Subcommand, log *logging.Logger) int {
	if hasHelpFlag(sub.Args) || len(sub.Args) == 0 {
		printMergeUsage()
		return 0
	}

	var prNumber string
	for _, arg := range sub.Args {
		if !strings.HasPrefix(arg, "-") && prNumber == "" {
			prNumber = arg
		}
	}
	if prNumber == "" {
		log.Emit(logging.Opts{Level: logging.Error}, "Usage: ralph merge <top-pr-number>")
		return 1
	}

	projectDir, _ := filepath.Abs(sub.Dir)
	if !git.IsGitRepo(projectDir) {
		log.Emit(logging.Opts{Level: logging.Error}, "Not a git repository: %s", projectDir)
		return 1
	}

	ralphDir := filepath.Join(projectDir, ".ralph")
	st := state.NewStore(filepath.Join(ralphDir, "state.json"))
	gm := git.New(git.Config{
		WorkDir:  projectDir,
		RalphDir: ralphDir,
		State:    st,
		Logger:   log,
	})
	if !gm.GitHubAvailable() {
		log.Emit(logging.Opts{Level: logging.Error}, "gh CLI not available")
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Collect all PRs in the stack, walking down from the given PR to
	// find the bottom, then collecting upward.
	stack := collectStack(gm, projectDir, prNumber, log)
	if len(stack.prs) == 0 {
		log.Emit(logging.Opts{Level: logging.Error}, "No open PRs found starting from #%s", prNumber)
		return 1
	}

	defaultBranch := stack.baseBranch
	log.Phase("Stack: %d PRs to merge", len(stack.prs))
	for _, pr := range stack.prs {
		log.Emit(logging.Opts{Domain: logging.Git}, "  PR #%d: %s", pr.number, pr.head)
	}

	return runMerge(ctx, stack.prs, projectDir, defaultBranch, gm, log)
}

// runMerge rebases the stack onto defaultBranch, then merges PRs bottom-up.
// For each PR after the first, it rebases the branch onto updated main and
// waits for fresh CI on the new HEAD before merging.
func runMerge(ctx context.Context, prs []stackPR, projectDir, defaultBranch string, gm *git.Repo, log *logging.Logger) int {
	topBranch := prs[len(prs)-1].head
	allBranches := make([]string, len(prs))
	for i, pr := range prs {
		allBranches[i] = pr.head
	}

	log.Phase("Rebasing stack onto %s", defaultBranch)
	if err := gm.RebaseStack(ctx, git.RebaseStackOpts{
		TopBranch:   topBranch,
		BaseBranch:  defaultBranch,
		TopPR:       prs[len(prs)-1].number,
		AllBranches: allBranches,
	}); err != nil {
		return 1
	}

	merged := 0
	repoURL := gm.RemoteURL()
	for _, pr := range prs {
		if merged > 0 {
			log.DashedSeparator(logging.Cyan)
		}
		log.Phase("Merging PR #%d (%d/%d)", pr.number, merged+1, len(prs))

		var pushedAt time.Time
		if merged > 0 {
			// Main moved after previous merge. Rebase this branch onto
			// the new main and force-push so CI runs against the correct base.
			log.Emit(logging.Opts{Domain: logging.Git}, "Rebasing %s onto updated %s...", pr.head, defaultBranch)
			pushedAt = time.Now()
			if err := gm.RebaseBranchOntoRemote(ctx, pr.head, defaultBranch); err != nil {
				log.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error},
					"Rebase conflicts on PR #%d (%s): %v", pr.number, pr.head, err)
				log.Emit(logging.Opts{Domain: logging.Git},
					"Resolve manually in: %s", projectDir)
				log.Emit(logging.Opts{Domain: logging.Git},
					"Then run: cd %s && git-rebase-continue", projectDir)
				return 1
			}
		}

		// Wait for CI on the current HEAD.
		log.Emit(logging.Opts{Domain: logging.CI}, "Waiting for CI on PR #%d...", pr.number)
		_, ciStatus, ciErr := gm.AwaitCI(ctx, pr.number, repoURL, pushedAt)
		if ciErr != nil {
			log.Emit(logging.Opts{Domain: logging.CI, Level: logging.Warn}, "CI polling error: %v", ciErr)
		}
		if ciStatus == git.CIFailed {
			log.Emit(logging.Opts{Domain: logging.CI, Level: logging.Error}, "CI failed on PR #%d — stopping", pr.number)
			return 1
		}
		log.Emit(logging.Opts{Domain: logging.CI, Level: logging.Success}, "CI passed for PR #%d", pr.number)

		// Merge.
		log.Emit(logging.Opts{Domain: logging.Git}, "Merging PR #%d...", pr.number)
		result := gm.MergeStackPR(pr.number, git.MergeOpts{DeleteBranch: true})
		if !result.Merged {
			if result.Conflict {
				log.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error}, "PR #%d has merge conflicts — cannot merge", pr.number)
				return 1
			}
			if result.Blocked {
				log.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "PR #%d blocked by branch protection: %s", pr.number, result.Message)
				return 1
			}
			log.Emit(logging.Opts{Domain: logging.Git, Level: logging.Error}, "Merge failed for PR #%d: %s", pr.number, result.Message)
			return 1
		}
		merged++
		log.Emit(logging.Opts{Domain: logging.Git, Level: logging.Success}, "PR #%d merged (%d/%d)", pr.number, merged, len(prs))

		// Update local main to include the merge.
		gm.ResetBranchToRemote(ctx, defaultBranch)
	}

	log.Emit(logging.Opts{Level: logging.Success}, "Stack complete — %d PRs merged", merged)
	return 0
}

type stackPR struct {
	number int
	head   string
}

type stackResult struct {
	prs        []stackPR
	baseBranch string // what the bottom PR targets (e.g. "main")
}

// prLister abstracts the PR listing operation collectStack needs. *git.Repo
// satisfies this interface structurally.
type prLister interface {
	ListAllPRs(workDir string) ([]git.PRInfo, error)
}

// collectStack walks the base chain from the given PR down to main,
// collecting the full stack in bottom-up order. Uses g.ListAllPRs
// to get all open PRs in one call, then walks the base references.
func collectStack(g prLister, workDir, topPR string, log *logging.Logger) stackResult {
	allPRs, err := g.ListAllPRs(workDir)
	if err != nil {
		log.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn}, "Failed to list PRs: %v", err)
		return stackResult{}
	}

	// Index by head branch name for chain walking (all PRs, any state).
	type prInfo struct {
		number int
		head   string
		base   string
		state  git.PRState
	}
	byHead := make(map[string]prInfo)
	byNumber := make(map[int]prInfo)
	for _, pr := range allPRs {
		info := prInfo{pr.Number, pr.Head, pr.Base, pr.State}
		byNumber[pr.Number] = info
		// Prefer OPEN PRs over CLOSED when multiple share the same head branch.
		existing, exists := byHead[pr.Head]
		if !exists || pr.State == git.PRStateOpen && existing.state != git.PRStateOpen {
			byHead[pr.Head] = info
		}
	}

	// Find the starting PR.
	topNum := 0
	fmt.Sscanf(topPR, "%d", &topNum)
	start, ok := byNumber[topNum]
	if !ok {
		return stackResult{}
	}

	// Walk the base chain from top down to main.
	// Include all PRs for chain walking, but only OPEN ones for merging.
	var chain []stackPR
	if start.state == git.PRStateOpen {
		chain = append(chain, stackPR{number: topNum, head: start.head})
	}
	currentBase := start.base

	for i := 0; i < 20; i++ {
		pr, found := byHead[currentBase]
		if !found {
			break // base is main or a branch with no PR
		}
		if pr.state == git.PRStateOpen {
			chain = append(chain, stackPR{number: pr.number, head: pr.head})
		}
		currentBase = pr.base
	}

	// Reverse to get bottom-up order.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return stackResult{prs: chain, baseBranch: currentBase}
}

func printMergeUsage() {
	fmt.Println(`Usage: ralph merge <top-pr-number>

Companion for ralph loop when --auto-merge is off. Give it any PR
in the stack — it finds the bottom, rebases the entire chain onto
main using --update-refs, force-pushes all branches, then merges
bottom-up waiting for CI between each merge.

Uses rebasecontinue.Run --auto for mechanical conflict resolution.

Examples:
  ralph merge 321  Merge the stack from bottom to PR #321
  ralph merge 314  Merge just PR #314 if it's the only open one`)
}
