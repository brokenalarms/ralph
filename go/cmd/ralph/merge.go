package main

import (
	"context"
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
			next := findNextStackedPR(gh, projectDir, prNumber, log)
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

		// Fetch to get current state.
		gm.FetchBranch(headBranch)
		gm.FetchBranch(baseBranch)

		// Record SHA before push so we can detect when it changes.
		preSHA, _ := gh.GetPRHeadSHA(projectDir, prNumber)

		// Rebase onto latest base and squash to 1 commit if needed.
		if code := rebaseSquashAndPush(ctx, projectDir, headBranch, baseBranch, gm, gh, prNumber, log); code != 0 {
			return code
		}

		// Wait for SHA to change from pre-push value, then wait for CI.
		repoURL := gm.RemoteURL()
		_, ciStatus, ciErr := gm.AwaitFreshCI(ctx, prNumber, repoURL, preSHA)
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
		gitRunErr(projectDir, "fetch", "origin", defaultBranch)
		gitRunErr(projectDir, "reset", "--hard", "origin/"+defaultBranch)

		if !stack {
			return 0
		}

		// Brief pause for GitHub to retarget the next PR.
		time.Sleep(2 * time.Second)

		next := findNextStackedPR(gh, projectDir, prNumber, log)
		if next == "" {
			return logStackComplete(log, merged)
		}
		prNumber = next
	}
}

// rebaseSquashAndPush creates a temp worktree, rebases onto the latest
// base branch, squashes to 1 commit if needed, and force-pushes.
func rebaseSquashAndPush(ctx context.Context, projectDir, headBranch, baseBranch string, gm *git.Manager, gh git.GitHub, prNumber string, log *logging.Logger) int {
	slug := strings.ReplaceAll(headBranch, "/", "-")
	wtDir := filepath.Join(os.TempDir(), "ralph-merge-"+slug)
	os.RemoveAll(wtDir)

	exec.Command("git", "-C", projectDir, "worktree", "prune").Run()
	tmpBranch := "ralph-merge/" + slug
	exec.Command("git", "-C", projectDir, "branch", "-D", tmpBranch).Run()

	cmd := exec.Command("git", "-C", projectDir, "worktree", "add", "-b", tmpBranch, wtDir, "origin/"+headBranch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Error("git", "Worktree failed: %s", string(out))
		return 1
	}
	cleanup := func() {
		exec.Command("git", "-C", projectDir, "worktree", "remove", "--force", wtDir).Run()
		exec.Command("git", "-C", projectDir, "branch", "-D", tmpBranch).Run()
	}

	// Rebase onto latest base.
	gitRunErr(wtDir, "fetch", "origin", baseBranch)
	baseRef := "origin/" + baseBranch
	if rebaseErr := gitRunErr(wtDir, "rebase", baseRef); rebaseErr != nil {
		gitRunErr(wtDir, "rebase", "--abort")
		cleanup()
		log.Error("git", "Rebase onto %s failed — resolve conflicts manually", baseBranch)
		return 1
	}

	// Squash.
	baseSHA := strings.TrimSpace(cmdOutputDir(wtDir, "git", "rev-parse", baseRef))
	gm.WorkDir = wtDir
	commitMsg := strings.TrimSpace(cmdOutputDir(wtDir, "git", "log", "-1", "--format=%s"))
	if err := gm.SquashToOneCommit(baseSHA, commitMsg); err != nil {
		log.Warn("git", "Squash: %v", err)
	}

	// Force-push.
	gitRunErr(wtDir, "fetch", "origin", headBranch)
	log.Log("git", "Force-pushing %s...", headBranch)
	if pushErr := gitRunErr(wtDir, "push", "--force-with-lease", "origin", "HEAD:refs/heads/"+headBranch); pushErr != nil {
		cleanup()
		log.Error("git", "Push failed: %v", pushErr)
		return 1
	}

	cleanup()
	gm.WorkDir = projectDir
	return 0
}

func findNextStackedPR(gh git.GitHub, workDir, mergedPR string, log *logging.Logger) string {
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
		}
	}
	return ""
}

func logStackComplete(log *logging.Logger, merged int) int {
	log.Success("", "Stack complete — %d PRs merged", merged)
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func printMergeUsage() {
	fmt.Println(`Usage: ralph merge <pr-number> [--stack]

Companion for ralph loop when --auto-merge is off. Merges a PR,
squashing to a single commit if needed.

With --stack, cascades through the stacked PR chain: after each merge,
finds the next PR and repeats until the stack is empty or CI fails.
Stacked PRs that already have 1 commit skip the squash step.

Options:
  --stack    Cascade through the entire stacked PR chain
  --help     Show this help

Examples:
  ralph merge 313              Merge a single PR
  ralph merge 313 --stack      Merge PR 313 and cascade through the stack`)
}
