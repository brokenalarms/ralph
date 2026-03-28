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

	for {
		log.Phase("Merging PR #%s", prNumber)

		gh := gm.GH()
		if gh == nil || !gh.Available() {
			log.Error("", "gh CLI not available")
			return 1
		}

		prState, err := gh.GetPRState(projectDir, prNumber)
		if err != nil {
			log.Error("", "Failed to get PR #%s state: %v", prNumber, err)
			return 1
		}
		if strings.ToUpper(prState) == "MERGED" {
			log.Success("", "PR #%s already merged", prNumber)
			if !stack {
				return 0
			}
			next := findNextStackedPR(gh, projectDir, prNumber)
			if next == "" {
				log.Success("", "Stack complete — no more PRs")
				return 0
			}
			prNumber = next
			continue
		}
		if strings.ToUpper(prState) != "OPEN" {
			log.Error("", "PR #%s is %s — cannot merge", prNumber, prState)
			return 1
		}

		headBranch, _ := gh.GetPRHead(projectDir, prNumber)
		if headBranch == "" {
			log.Error("", "PR #%s has no head branch", prNumber)
			return 1
		}

		baseBranch, _ := gh.GetPRBase(projectDir, prNumber)
		if baseBranch == "" {
			baseBranch = gm.DetectDefaultBranch()
		}

		// Fetch branches.
		log.Log("git", "Fetching %s and %s...", headBranch, baseBranch)
		gm.FetchBranch(headBranch)
		gm.FetchBranch(baseBranch)

		// Checkout head branch.
		gitCheckout(projectDir, headBranch)

		// Set up manager for Push.
		gm.WorkDir = projectDir
		gm.WorktreeBranch = headBranch
		gm.SetPrevBranch("")
		if baseBranch != gm.DetectDefaultBranch() {
			gm.SetPrevBranch(baseBranch)
		}

		// Push (squashes + force-pushes).
		if err := gm.Push(ctx); err != nil {
			log.Error("git", "Push failed: %v", err)
			return 1
		}

		// Wait for CI.
		repoURL := gm.RemoteURL()
		log.Log("ci", "Waiting for CI on PR #%s...", prNumber)
		_, ciStatus, ciErr := gm.AwaitCI(ctx, prNumber, repoURL)
		if ciErr != nil {
			log.Warn("ci", "CI polling: %v", ciErr)
		}
		if ciStatus == git.CIFailed {
			log.Error("ci", "CI failed on PR #%s — stopping", prNumber)
			return 1
		}
		log.Success("ci", "CI passed for PR #%s", prNumber)

		// Merge.
		opts := git.MergeOpts{DeleteBranch: true}
		output, mergeErr := gh.MergePR(prNumber, repoURL, opts)
		if mergeErr != nil {
			log.Error("git", "Merge failed: %s", output)
			return 1
		}
		log.Success("git", "PR #%s merged", prNumber)

		// Update local main.
		defaultBranch := gm.DetectDefaultBranch()
		gitRun(projectDir, "checkout", defaultBranch)
		gitRun(projectDir, "pull", "origin", defaultBranch)

		if !stack {
			return 0
		}

		next := findNextStackedPR(gh, projectDir, prNumber)
		if next == "" {
			log.Success("", "Stack complete — no more PRs")
			return 0
		}
		prNumber = next
	}
}

func findNextStackedPR(gh git.GitHub, workDir, mergedPR string) string {
	n := 0
	fmt.Sscanf(mergedPR, "%d", &n)
	if n > 0 {
		candidate := fmt.Sprintf("%d", n+1)
		prState, err := gh.GetPRState(workDir, candidate)
		if err == nil && strings.ToUpper(prState) == "OPEN" {
			return candidate
		}
	}
	return ""
}

func gitCheckout(dir, branch string) {
	cmd := exec.Command("git", "-C", dir, "checkout", branch)
	if err := cmd.Run(); err != nil {
		cmd2 := exec.Command("git", "-C", dir, "checkout", "-b", branch, "origin/"+branch)
		cmd2.Run()
	} else {
		exec.Command("git", "-C", dir, "reset", "--hard", "origin/"+branch).Run()
	}
}

func gitRun(dir string, args ...string) {
	fullArgs := append([]string{"-C", dir}, args...)
	exec.Command("git", fullArgs...).Run()
}

func printMergeUsage() {
	fmt.Println(`Usage: ralph merge <pr-number> [--stack]

Squash-merges a PR, ensuring the branch has exactly one commit.
With --stack, continues to the next PR in the chain until the
stack is empty or CI fails.

Options:
  --stack    Cascade through the stacked PR chain
  --help     Show this help`)
}
