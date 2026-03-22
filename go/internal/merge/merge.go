package merge

import (
	"fmt"
	"os/exec"
	"strings"
)

// AutoMerge attempts to squash-merge the current worktree branch's PR
// into main. It skips gracefully when conditions aren't met (no branch,
// no worktree, no gh CLI, no PR).
func AutoMerge(worktreeBranch, workDir, projectDir string, deleteBranch bool) (string, error) {
	if worktreeBranch == "" {
		return "", nil
	}
	if workDir == projectDir {
		return "", nil
	}

	if _, err := exec.LookPath("gh"); err != nil {
		return "", fmt.Errorf("gh CLI not found")
	}

	prNum, err := findPR(worktreeBranch, projectDir)
	if err != nil {
		return "", err
	}
	if prNum == "" {
		return "No open PR found", nil
	}

	mergeArgs := []string{"pr", "merge", prNum, "--squash"}
	if deleteBranch {
		mergeArgs = append(mergeArgs, "--delete-branch")
	}
	cmd := exec.Command("gh", mergeArgs...)
	cmd.Dir = projectDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("merge failed: %s", strings.TrimSpace(string(out)))
	}

	// Fetch merged main so the new tag lands on the merge commit.
	fetch := exec.Command("git", "fetch", "origin", "main")
	fetch.Dir = projectDir
	fetch.CombinedOutput()

	tag, tagErr := BumpPatchTag(projectDir, "origin/main")
	if tagErr != nil {
		return fmt.Sprintf("Merged PR #%s (tag bump failed: %v)", prNum, tagErr), nil
	}

	return fmt.Sprintf("Merged PR #%s → %s", prNum, tag), nil
}

func findPR(branch, projectDir string) (string, error) {
	cmd := exec.Command("gh", "pr", "list", "--head", branch, "--json", "number", "--jq", ".[0].number")
	cmd.Dir = projectDir
	out, err := cmd.Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}
