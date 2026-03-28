package main

import (
	"context"
	"fmt"
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

	stack := false
	var prNumber string
	for _, arg := range sub.Args {
		if arg == "--stack" {
			stack = true
		} else if !strings.HasPrefix(arg, "-") && prNumber == "" {
			prNumber = arg
		}
	}
	if prNumber == "" {
		log.Error("", "Usage: ralph merge <pr-number> [--stack]")
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

	ctx := context.Background()
	merged := 0

	for {
		gh := gm.GH()
		if gh == nil || !gh.Available() {
			log.Error("", "gh CLI not available")
			return 1
		}

		if stack && merged > 0 {
			log.DashedSeparator(logging.Cyan)
		}
		log.Phase("PR #%s", prNumber)

		// Check PR state.
		prState, err := gh.GetPRState(projectDir, prNumber)
		if err != nil {
			log.Error("", "Failed to get PR #%s state: %v", prNumber, err)
			return 1
		}
		switch strings.ToUpper(prState) {
		case "MERGED":
			log.Success("git", "PR #%s already merged", prNumber)
			if !stack {
				return 0
			}
			next := findNextStackedPR(gh, gm, projectDir, prNumber, log)
			if next == "" {
				return logStackComplete(log, merged)
			}
			prNumber = next
			continue
		case "OPEN":
			// proceed
		default:
			log.Error("", "PR #%s is %s — cannot merge", prNumber, prState)
			return 1
		}

		// Get branch info.
		headBranch, _ := gh.GetPRHead(projectDir, prNumber)
		if headBranch == "" {
			log.Error("", "PR #%s has no head branch", prNumber)
			return 1
		}
		baseBranch, _ := gh.GetPRBase(projectDir, prNumber)
		if baseBranch == "" {
			baseBranch = gm.DetectDefaultBranch()
		}
		log.Log("git", "Branch: %s → %s", headBranch, baseBranch)

		// Fetch.
		log.Log("git", "Fetching branches...")
		gm.FetchBranch(headBranch)
		gm.FetchBranch(baseBranch)

		// Checkout head branch.
		if err := gitCheckoutBranch(projectDir, headBranch, log); err != nil {
			log.Error("git", "Checkout failed: %v", err)
			return 1
		}

		// Set up manager for Push.
		gm.WorkDir = projectDir
		gm.WorktreeBranch = headBranch
		gm.SetPrevBranch("")
		if baseBranch != gm.DetectDefaultBranch() {
			gm.SetPrevBranch(baseBranch)
		}

		// Squash + force-push.
		log.Log("git", "Squashing and pushing...")
		if err := gm.Push(ctx); err != nil {
			log.Error("git", "Push failed: %v", err)
			return 1
		}

		// Wait for CI.
		repoURL := gm.RemoteURL()
		log.Log("ci", "Waiting for CI on PR #%s...", prNumber)
		_, ciStatus, ciErr := gm.AwaitCI(ctx, prNumber, repoURL)
		if ciErr != nil {
			log.Warn("ci", "CI polling error: %v", ciErr)
		}
		if ciStatus == git.CIFailed {
			log.Error("ci", "CI failed on PR #%s — stopping", prNumber)
			return 1
		}
		log.Success("ci", "CI passed for PR #%s", prNumber)

		// Merge.
		log.Log("git", "Merging PR #%s...", prNumber)
		opts := git.MergeOpts{DeleteBranch: true}
		output, mergeErr := gh.MergePR(prNumber, repoURL, opts)
		if mergeErr != nil {
			log.Error("git", "Merge failed for PR #%s: %s", prNumber, output)
			return 1
		}
		merged++
		log.Success("git", "PR #%s merged (%d total)", prNumber, merged)

		// Update local main.
		defaultBranch := gm.DetectDefaultBranch()
		log.Log("git", "Updating local %s...", defaultBranch)
		if err := gitRunErr(projectDir, "checkout", defaultBranch); err != nil {
			log.Warn("git", "Checkout %s failed: %v", defaultBranch, err)
		}
		if err := gitRunErr(projectDir, "pull", "origin", defaultBranch); err != nil {
			log.Warn("git", "Pull %s failed: %v", defaultBranch, err)
		}

		if !stack {
			return 0
		}

		next := findNextStackedPR(gh, gm, projectDir, prNumber, log)
		if next == "" {
			return logStackComplete(log, merged)
		}
		prNumber = next
	}
}

// findNextStackedPR searches for an open PR that was targeting the merged
// branch. After GitHub retargets it to main, we find it by searching for
// open PRs on the repo.
func findNextStackedPR(gh git.GitHub, gm *git.Manager, workDir, mergedPR string, log *logging.Logger) string {
	// Search for the next sequential PR number that's open.
	// This is a heuristic — works when PRs were created in order.
	n := 0
	fmt.Sscanf(mergedPR, "%d", &n)
	for offset := 1; offset <= 3; offset++ {
		candidate := fmt.Sprintf("%d", n+offset)
		prState, err := gh.GetPRState(workDir, candidate)
		if err != nil {
			continue
		}
		switch strings.ToUpper(prState) {
		case "OPEN":
			log.Log("git", "Next in stack: PR #%s", candidate)
			return candidate
		case "MERGED":
			continue
		default:
			continue
		}
	}
	return ""
}

func logStackComplete(log *logging.Logger, merged int) int {
	log.Success("", "Stack complete — %d PRs merged", merged)
	return 0
}

func gitCheckoutBranch(dir, branch string, log *logging.Logger) error {
	cmd := exec.Command("git", "-C", dir, "checkout", branch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		cmd2 := exec.Command("git", "-C", dir, "checkout", "-b", branch, "origin/"+branch)
		out2, err2 := cmd2.CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("checkout %s: %s", branch, string(out2))
		}
		return nil
	}
	// Reset to remote to ensure we have the latest.
	cmd3 := exec.Command("git", "-C", dir, "reset", "--hard", "origin/"+branch)
	if out3, err3 := cmd3.CombinedOutput(); err3 != nil {
		log.Warn("git", "Reset to origin/%s: %s", branch, string(out3))
	}
	_ = out
	return nil
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
	fmt.Println(`Usage: ralph merge <pr-number> [--stack]

Companion for ralph loop when --auto-merge is off. Squashes the PR
branch to a single commit, force-pushes, waits for CI, and merges.

With --stack, cascades through the stacked PR chain: after each merge,
finds the next PR in the stack and repeats until the stack is empty
or CI fails. This handles the squash-merge ancestry issue that causes
merge conflicts when stacked PRs have multiple commits.

Options:
  --stack    Cascade through the entire stacked PR chain
  --help     Show this help

Examples:
  ralph merge 313              Merge a single PR
  ralph merge 313 --stack      Merge PR 313 and cascade through the stack`)
}
