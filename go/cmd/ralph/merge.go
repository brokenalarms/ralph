package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		log.Error("", "Usage: ralph merge <top-pr-number>")
		return 1
	}

	projectDir, _ := filepath.Abs(sub.Dir)
	if !git.IsGitRepo(projectDir) {
		log.Error("", "Not a git repository: %s", projectDir)
		return 1
	}

	ralphDir := filepath.Join(projectDir, ".ralph")
	st := state.NewStore(filepath.Join(ralphDir, "state.json"))
	gm := &git.Manager{
		ProjectDir: projectDir,
		WorkDir:    projectDir,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     log,
	}
	gh := gm.GH()
	if gh == nil || !gh.Available() {
		log.Error("", "gh CLI not available")
		return 1
	}
	ctx := context.Background()
	defaultBranch := gm.DetectDefaultBranch()

	// Collect all PRs in the stack, walking down from the given PR to
	// find the bottom, then collecting upward.
	prs := collectStack(gh, projectDir, prNumber, log)
	if len(prs) == 0 {
		log.Error("", "No open PRs found starting from #%s", prNumber)
		return 1
	}

	log.Phase("Stack: %d PRs to merge", len(prs))
	for _, pr := range prs {
		log.Log("git", "  PR #%s: %s", pr.number, pr.head)
	}

	// Step 1: Rebase entire stack with --update-refs in a temp worktree.
	topBranch := prs[len(prs)-1].head
	allBranches := make([]string, len(prs))
	for i, pr := range prs {
		allBranches[i] = pr.head
	}

	log.Phase("Rebasing stack onto %s", defaultBranch)
	if code := rebaseStackAndPush(projectDir, defaultBranch, topBranch, allBranches, gm, log); code != 0 {
		return code
	}

	// Step 2: Merge bottom-up.
	merged := 0
	repoURL := gm.RemoteURL()
	for _, pr := range prs {
		if merged > 0 {
			log.DashedSeparator(logging.Cyan)
		}
		log.Phase("Merging PR #%s", pr.number)

		// Wait for GitHub to retarget if needed.
		if merged > 0 {
			log.Log("git", "Waiting for PR #%s to retarget to %s...", pr.number, defaultBranch)
			for i := 0; i < 30; i++ {
				base, _ := gh.GetPRBase(projectDir, pr.number)
				if base == defaultBranch {
					break
				}
				time.Sleep(2 * time.Second)
			}
		}

		// Record pre-push SHA for fresh CI detection.
		preSHA, _ := gh.GetPRHeadSHA(projectDir, pr.number)

		// Wait for fresh CI.
		_, ciStatus, ciErr := gm.AwaitFreshCI(ctx, pr.number, repoURL, preSHA)
		if ciErr != nil {
			log.Warn("ci", "CI polling error: %v", ciErr)
		}
		if ciStatus == git.CIFailed {
			log.Error("ci", "CI failed on PR #%s — stopping", pr.number)
			return 1
		}
		log.Success("ci", "CI passed for PR #%s", pr.number)

		// Merge.
		log.Log("git", "Merging PR #%s...", pr.number)
		opts := git.MergeOpts{DeleteBranch: true}
		output, mergeErr := gh.MergePR(pr.number, repoURL, opts)
		if mergeErr != nil {
			log.Error("git", "Merge failed for PR #%s: %s", pr.number, output)
			return 1
		}
		merged++
		log.Success("git", "PR #%s merged (%d/%d)", pr.number, merged, len(prs))

		// Update local main.
		gitRunErr(projectDir, "fetch", "origin", defaultBranch)
		gitRunErr(projectDir, "reset", "--hard", "origin/"+defaultBranch)
	}

	log.Success("", "Stack complete — %d PRs merged", merged)
	return 0
}

type stackPR struct {
	number string
	head   string
}

// collectStack walks the base chain from the given PR down to main,
// collecting the full stack in bottom-up order. Uses gh pr list --json
// to get all open PRs in one call, then walks the base references.
func collectStack(gh git.GitHub, workDir, topPR string, log *logging.Logger) []stackPR {
	// Get all PRs (open + closed) to walk the full chain.
	// Closed PRs may be links in the base chain even if they won't be merged.
	cmd := exec.Command("gh", "pr", "list", "--state", "all",
		"--json", "number,headRefName,baseRefName,state", "--limit", "200")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		log.Warn("git", "Failed to list PRs: %v", err)
		return nil
	}

	var allPRs []struct {
		Number int    `json:"number"`
		Head   string `json:"headRefName"`
		Base   string `json:"baseRefName"`
		State  string `json:"state"`
	}
	if jsonErr := json.Unmarshal(out, &allPRs); jsonErr != nil {
		log.Warn("git", "Failed to parse PR list: %v", jsonErr)
		return nil
	}

	// Index by head branch name for chain walking (all PRs, any state).
	type prInfo struct {
		number int
		head   string
		base   string
		state  string
	}
	byHead := make(map[string]prInfo)
	byNumber := make(map[int]prInfo)
	for _, pr := range allPRs {
		info := prInfo{pr.Number, pr.Head, pr.Base, pr.State}
		byHead[pr.Head] = info
		byNumber[pr.Number] = info
	}

	// Find the starting PR.
	topNum := 0
	fmt.Sscanf(topPR, "%d", &topNum)
	start, ok := byNumber[topNum]
	if !ok {
		return nil
	}

	// Walk the base chain from top down to main.
	// Include all PRs for chain walking, but only OPEN ones for merging.
	var chain []stackPR
	if strings.ToUpper(start.state) == "OPEN" {
		chain = append(chain, stackPR{number: topPR, head: start.head})
	}
	currentBase := start.base

	for i := 0; i < 20; i++ {
		pr, found := byHead[currentBase]
		if !found {
			break // base is main or a branch with no PR
		}
		if strings.ToUpper(pr.state) == "OPEN" {
			chain = append(chain, stackPR{number: fmt.Sprintf("%d", pr.number), head: pr.head})
		}
		currentBase = pr.base
	}

	// Reverse to get bottom-up order.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// rebaseStackAndPush creates a temp worktree on the top branch, sets up
// local tracking for all stack branches, rebases with --update-refs onto
// main, then force-pushes all branches.
func rebaseStackAndPush(projectDir, defaultBranch, topBranch string, allBranches []string, gm *git.Manager, log *logging.Logger) int {
	slug := strings.ReplaceAll(topBranch, "/", "-")
	wtDir := filepath.Join(os.TempDir(), "ralph-merge-"+slug)
	os.RemoveAll(wtDir)

	exec.Command("git", "-C", projectDir, "worktree", "prune").Run()
	tmpBranch := "ralph-merge/" + slug
	exec.Command("git", "-C", projectDir, "branch", "-D", tmpBranch).Run()

	// Fetch all branches.
	log.Log("git", "Fetching %d branches...", len(allBranches)+1)
	gitRunErr(projectDir, "fetch", "origin", defaultBranch)
	for _, b := range allBranches {
		gitRunErr(projectDir, "fetch", "origin", b)
	}

	// Create worktree on the top branch.
	cmd := exec.Command("git", "-C", projectDir, "worktree", "add", "-b", tmpBranch, wtDir, "origin/"+topBranch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Error("git", "Worktree failed: %s", string(out))
		return 1
	}
	cleanup := func() {
		exec.Command("git", "-C", projectDir, "worktree", "remove", "--force", wtDir).Run()
		exec.Command("git", "-C", projectDir, "branch", "-D", tmpBranch).Run()
		for _, b := range allBranches {
			exec.Command("git", "-C", projectDir, "branch", "-D", b).Run()
		}
	}

	// Set up local tracking branches for all stack branches so --update-refs finds them.
	for _, b := range allBranches {
		exec.Command("git", "-C", wtDir, "branch", "-f", b, "origin/"+b).Run()
	}

	// Rebase with --update-refs onto main.
	log.Log("git", "Rebasing with --update-refs onto origin/%s...", defaultBranch)
	if rebaseErr := gitRunErr(wtDir, "rebase", "--update-refs", "origin/"+defaultBranch); rebaseErr != nil {
		log.Log("git", "Rebase conflict — attempting auto-resolve...")
		autoCmd := exec.Command("git-rebase-continue", "--auto")
		autoCmd.Dir = wtDir
		autoOut, autoErr := autoCmd.CombinedOutput()
		if autoErr != nil {
			gitRunErr(wtDir, "rebase", "--abort")
			cleanup()
			log.Error("git", "Rebase failed — resolve conflicts manually\n%s", string(autoOut))
			return 1
		}
	}

	// Force-push all branches.
	log.Log("git", "Force-pushing %d branches...", len(allBranches))
	for _, b := range allBranches {
		log.Log("git", "  Pushing %s", b)
		if pushErr := gitRunErr(wtDir, "push", "--force", "origin", b+":refs/heads/"+b); pushErr != nil {
			cleanup()
			log.Error("git", "Push failed for %s: %v", b, pushErr)
			return 1
		}
	}

	cleanup()
	gm.WorkDir = projectDir
	log.Success("git", "All branches rebased and pushed")
	return 0
}

func cmdOutputDir(dir, name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, _ := cmd.Output()
	return string(out)
}

func gitRunErr(dir string, args ...string) error {
	fullArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", strings.Join(args, " "), string(out))
	}
	return nil
}

func printMergeUsage() {
	fmt.Println(`Usage: ralph merge <top-pr-number>

Companion for ralph loop when --auto-merge is off. Give it any PR
in the stack — it finds the bottom, rebases the entire chain onto
main using --update-refs, force-pushes all branches, then merges
bottom-up waiting for CI between each merge.

Uses git-rebase-continue --auto for mechanical conflict resolution.

Examples:
  ralph merge 321              Merge the stack from bottom to PR #321
  ralph merge 314              Merge just PR #314 if it's the only open one`)
}
