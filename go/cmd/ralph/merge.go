package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
	bypassRules := false
	for _, arg := range sub.Args {
		if arg == "--bypass-rules" {
			bypassRules = true
			continue
		}
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

	return runMerge(ctx, prs, projectDir, defaultBranch, gm, bypassRules, log)
}

// runMerge rebases the stack onto defaultBranch, then merges PRs bottom-up.
// For each PR after the first, it rebases the branch onto updated main and
// waits for fresh CI on the new HEAD before merging.
func runMerge(ctx context.Context, prs []stackPR, projectDir, defaultBranch string, gm *git.Manager, bypassRules bool, log *logging.Logger) int {
	topBranch := prs[len(prs)-1].head
	allBranches := make([]string, len(prs))
	for i, pr := range prs {
		allBranches[i] = pr.head
	}

	log.Phase("Rebasing stack onto %s", defaultBranch)
	if code := rebaseStackAndPush(projectDir, defaultBranch, topBranch, allBranches, gm, log); code != 0 {
		return code
	}

	gh := gm.GH()
	merged := 0
	repoURL := gm.RemoteURL()
	for _, pr := range prs {
		if merged > 0 {
			log.DashedSeparator(logging.Cyan)
		}
		log.Phase("Merging PR #%s (%d/%d)", pr.number, merged+1, len(prs))

		if merged > 0 {
			// Main moved after previous merge. Rebase this branch onto
			// the new main and force-push so CI runs against the correct base.
			log.Log("git", "Rebasing %s onto updated %s...", pr.head, defaultBranch)
			gitRunErr(projectDir, "fetch", "origin", defaultBranch)
			gitRunErr(projectDir, "fetch", "origin", pr.head)

			// Rebase in a detached state to avoid needing a worktree.
			gitRunErr(projectDir, "checkout", "origin/"+pr.head)
			if rebaseErr := gitRunErr(projectDir, "rebase", "origin/"+defaultBranch); rebaseErr != nil {
				log.Error("git", "Rebase failed for %s: %v", pr.head, rebaseErr)
				gitRunErr(projectDir, "rebase", "--abort")
				return 1
			}
			if pushErr := gitRunErr(projectDir, "push", "--force-with-lease", "origin", "HEAD:"+pr.head); pushErr != nil {
				log.Error("git", "Force-push failed for %s: %v", pr.head, pushErr)
				return 1
			}
			// Return to default branch.
			gitRunErr(projectDir, "checkout", defaultBranch)
		}

		// Get the HEAD SHA after push for fresh CI detection.
		expectedSHA, _ := gh.GetPRHeadSHA(projectDir, pr.number)

		// Wait for CI on the current HEAD.
		log.Log("ci", "Waiting for CI on PR #%s...", pr.number)
		_, ciStatus, ciErr := gm.AwaitCI(ctx, pr.number, repoURL, expectedSHA)
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
		opts := git.MergeOpts{DeleteBranch: true, Admin: bypassRules}
		result := gh.MergePR(pr.number, repoURL, opts)
		if !result.Merged {
			log.Error("git", "Merge failed for PR #%s: %s", pr.number, result.Message)
			return 1
		}
		merged++
		log.Success("git", "PR #%s merged (%d/%d)", pr.number, merged, len(prs))

		// Update local main to include the merge.
		gitRunErr(projectDir, "fetch", "origin", defaultBranch)
		gitRunErr(projectDir, "checkout", defaultBranch)
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
		byNumber[pr.Number] = info
		// Prefer OPEN PRs over CLOSED when multiple share the same head branch.
		existing, exists := byHead[pr.Head]
		if !exists || strings.ToUpper(pr.State) == "OPEN" && strings.ToUpper(existing.state) != "OPEN" {
			byHead[pr.Head] = info
		}
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

// rebaseStackAndPush creates a temp worktree on the top branch, rebases
// with --update-refs onto main, then force-pushes all branches.
// If a worktree already exists (from a previous conflict resolution),
// skips rebase and goes straight to push.
func rebaseStackAndPush(projectDir, defaultBranch, topBranch string, allBranches []string, gm *git.Manager, log *logging.Logger) int {
	slug := strings.ReplaceAll(topBranch, "/", "-")
	wtDir := filepath.Join(os.TempDir(), "ralph-merge-"+slug)
	tmpBranch := "ralph-merge/" + slug

	// Check if worktree exists from a previous run (conflict was resolved manually).
	if _, err := os.Stat(filepath.Join(wtDir, ".git")); err == nil {
		log.Log("git", "Resuming from existing worktree: %s", wtDir)
	} else {
		// Fresh worktree.
		os.RemoveAll(wtDir)
		exec.Command("git", "-C", projectDir, "worktree", "prune").Run()
		exec.Command("git", "-C", projectDir, "branch", "-D", tmpBranch).Run()

		// Only fetch top branch + main.
		log.Log("git", "Fetching %s and %s...", topBranch, defaultBranch)
		gitRunErr(projectDir, "fetch", "origin", defaultBranch)
		gitRunErr(projectDir, "fetch", "origin", topBranch)

		// Create worktree on the top branch.
		cmd := exec.Command("git", "-C", projectDir, "worktree", "add", "-b", tmpBranch, wtDir, "origin/"+topBranch)
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Error("git", "Worktree failed: %s", string(out))
			return 1
		}

		// Set up local tracking branches for --update-refs.
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
				log.Error("git", "Rebase has conflicts — resolve manually in:\n  %s", wtDir)
				log.Log("git", "Then run: cd %s && git-rebase-continue", wtDir)
				log.Log("git", "Then re-run: ralph merge %s", strings.Split(allBranches[len(allBranches)-1], "/")[len(strings.Split(allBranches[len(allBranches)-1], "/"))-1])
				log.Log("", "\n%s", string(autoOut))
				return 1
			}
		}
	}

	cleanup := func() {
		exec.Command("git", "-C", projectDir, "worktree", "remove", "--force", wtDir).Run()
		exec.Command("git", "-C", projectDir, "branch", "-D", tmpBranch).Run()
	}

	// Force-push all branches.
	log.Log("git", "Force-pushing %d branches...", len(allBranches))
	for _, b := range allBranches {
		log.Log("git", "  Pushing %s", b)
		if pushErr := gitRunErr(wtDir, "push", "--force", "origin", b); pushErr != nil {
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
	fmt.Println(`Usage: ralph merge <top-pr-number> [--bypass-rules]

Companion for ralph loop when --auto-merge is off. Give it any PR
in the stack — it finds the bottom, rebases the entire chain onto
main using --update-refs, force-pushes all branches, then merges
bottom-up waiting for CI between each merge.

Uses git-rebase-continue --auto for mechanical conflict resolution.

Flags:
  --bypass-rules  Bypass branch protection using gh pr merge --admin

Examples:
  ralph merge 321              Merge the stack from bottom to PR #321
  ralph merge 314              Merge just PR #314 if it's the only open one
  ralph merge 321 --bypass-rules  Merge bypassing branch protection`)
}
