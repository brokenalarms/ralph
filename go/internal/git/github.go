package git

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// MergeOpts configures a PR merge operation.
type MergeOpts struct {
	DeleteBranch bool
	Admin        bool
}

// GitHub abstracts GitHub CLI operations for testability. Production code
// uses ghCLI; tests inject stubs to avoid shelling out to gh.
type GitHub interface {
	Available() bool
	FindOpenPR(branch, repoURL string) (prNumber string, err error)
	CreatePR(dir, branch, baseBranch, title, body, repoURL string) error
	MergePR(prNumber, repoURL string, opts MergeOpts) (output string, err error)
	UpdateBranch(dir, nwo, prNumber string) (updated bool, err error)
	FetchChecks(prNumber, repoURL string) ([]CICheckResult, error)
}

// ghCLI implements GitHub using the gh CLI tool.
type ghCLI struct{}

func (g *ghCLI) Available() bool {
	p, err := exec.LookPath("gh")
	return err == nil && p != ""
}

func (g *ghCLI) FindOpenPR(branch, repoURL string) (string, error) {
	cmd := exec.Command("gh", "pr", "list", "--head", branch, "--state", "open",
		"--json", "number", "--jq", ".[0].number", "-R", repoURL)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr list failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *ghCLI) CreatePR(dir, branch, baseBranch, title, body, repoURL string) error {
	args := []string{"pr", "create", "--head", branch}
	if baseBranch != "" {
		args = append(args, "--base", baseBranch)
	}
	args = append(args, "--title", title, "--body", body, "-R", repoURL)
	cmd := exec.Command("gh", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("PR creation failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (g *ghCLI) MergePR(prNumber, repoURL string, opts MergeOpts) (string, error) {
	args := []string{"pr", "merge", prNumber, "--squash", "-R", repoURL}
	if opts.DeleteBranch {
		args = append(args, "--delete-branch")
	}
	if opts.Admin {
		args = append(args, "--admin")
	}
	cmd := exec.Command("gh", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (g *ghCLI) UpdateBranch(dir, nwo, prNumber string) (bool, error) {
	endpoint := fmt.Sprintf("repos/%s/pulls/%s/update-branch", nwo, prNumber)
	cmd := exec.Command("gh", "api", endpoint, "--method", "PUT")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := strings.TrimSpace(string(out))
		if strings.Contains(output, "already up to date") ||
			strings.Contains(output, "expected_head_sha") {
			return false, nil
		}
		return false, fmt.Errorf("update-branch API: %s", output)
	}
	return true, nil
}

func (g *ghCLI) FetchChecks(prNumber, repoURL string) ([]CICheckResult, error) {
	args := []string{"pr", "checks", prNumber, "--json", "name,state,bucket"}
	if repoURL != "" {
		args = append(args, "-R", repoURL)
	}
	cmd := exec.Command("gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr checks failed: %w", err)
	}
	var checks []CICheckResult
	if err := json.Unmarshal(out, &checks); err != nil {
		return nil, fmt.Errorf("parsing check results: %w", err)
	}
	return checks, nil
}
