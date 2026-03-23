package git

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// CreatePROpts configures a PR creation operation.
type CreatePROpts struct {
	Head string
	Base string
	Title string
	Body string
	Repo string
	Dir  string
}

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
	CreatePR(opts CreatePROpts) error
	MergePR(prNumber, repoURL string, opts MergeOpts) (output string, err error)
	UpdateBranch(dir, nwo, prNumber string) (updated bool, err error)
	ListChecks(prNumber, repoURL string) ([]CICheckResult, error)
	GetRunLog(prNumber, workDir string) string
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

func (g *ghCLI) CreatePR(opts CreatePROpts) error {
	args := []string{"pr", "create", "--head", opts.Head}
	if opts.Base != "" {
		args = append(args, "--base", opts.Base)
	}
	args = append(args, "--title", opts.Title, "--body", opts.Body, "-R", opts.Repo)
	cmd := exec.Command("gh", args...)
	cmd.Dir = opts.Dir
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

func (g *ghCLI) ListChecks(prNumber, repoURL string) ([]CICheckResult, error) {
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

func (g *ghCLI) GetRunLog(prNumber, workDir string) string {
	cmd := exec.Command("gh", "pr", "checks", prNumber, "--json", "name,state,link", "--jq",
		`.[] | select(.state == "FAILURE") | .link`)
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return ""
	}

	link := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	parts := strings.Split(link, "/")

	var runID string
	for i, p := range parts {
		if p == "runs" && i+1 < len(parts) {
			runID = parts[i+1]
			break
		}
	}
	if runID == "" {
		return ""
	}

	logCmd := exec.Command("gh", "run", "view", runID, "--log-failed")
	logCmd.Dir = workDir
	logOut, err := logCmd.Output()
	if err != nil {
		return ""
	}

	lines := strings.Split(string(logOut), "\n")
	if len(lines) > 50 {
		lines = lines[len(lines)-50:]
	}
	return strings.Join(lines, "\n")
}
