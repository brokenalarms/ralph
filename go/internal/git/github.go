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
	Subject      string
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
	EditPR(prNumber, repoURL, title, body string) error
	GetRunLog(prNumber, workDir string) string
	CheckEnforceAdmins(nwo, branch string) (enabled bool, err error)
	PostEnforceAdmins(nwo, branch string) (output string, err error)
	FindPR(branch, workDir string) (number, title, url string, err error)
	SearchPR(workDir, query string) (prNumber string, err error)
	PRDiff(workDir, prNumber string) (string, error)
	GetPRState(workDir, prNumber string) (state string, err error)
	GetPRBase(workDir, prNumber string) (base string, err error)
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

func (g *ghCLI) EditPR(prNumber, repoURL, title, body string) error {
	args := []string{"pr", "edit", prNumber, "--title", title, "-R", repoURL}
	if body != "" {
		args = append(args, "--body", body)
	}
	cmd := exec.Command("gh", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("PR edit failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (g *ghCLI) MergePR(prNumber, repoURL string, opts MergeOpts) (string, error) {
	args := []string{"pr", "merge", prNumber, "--squash", "-R", repoURL}
	if opts.Subject != "" {
		args = append(args, "--subject", opts.Subject)
	}
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

func isHarmlessUpdateBranchError(output string) bool {
	return strings.Contains(output, "already up to date") ||
		strings.Contains(output, "expected_head_sha") ||
		strings.Contains(output, "no new commits")
}

func (g *ghCLI) UpdateBranch(dir, nwo, prNumber string) (bool, error) {
	endpoint := fmt.Sprintf("repos/%s/pulls/%s/update-branch", nwo, prNumber)
	cmd := exec.Command("gh", "api", endpoint, "--method", "PUT")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := strings.TrimSpace(string(out))
		if isHarmlessUpdateBranchError(output) {
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

	// Mark required checks by querying branch protection rules.
	requiredNames := g.getRequiredCheckNames(repoURL)
	for i := range checks {
		for _, req := range requiredNames {
			if checks[i].Name == req {
				checks[i].Required = true
				break
			}
		}
	}
	return checks, nil
}

// getRequiredCheckNames fetches the required status check names from branch protection.
func (g *ghCLI) getRequiredCheckNames(repoURL string) []string {
	nwo := nwoFromRemote(repoURL)
	if nwo == "" {
		return nil
	}
	// Try main, then develop
	for _, branch := range []string{"main", "develop"} {
		endpoint := fmt.Sprintf("/repos/%s/branches/%s/protection/required_status_checks", nwo, branch)
		cmd := exec.Command("gh", "api", endpoint)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		var result struct {
			Contexts []string `json:"contexts"`
		}
		if json.Unmarshal(out, &result) == nil && len(result.Contexts) > 0 {
			return result.Contexts
		}
	}
	return nil
}

func (g *ghCLI) CheckEnforceAdmins(nwo, branch string) (bool, error) {
	endpoint := fmt.Sprintf("/repos/%s/branches/%s/protection/enforce_admins", nwo, branch)
	cmd := exec.Command("gh", "api", endpoint)
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("gh api failed: %w", err)
	}

	var resp struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return false, fmt.Errorf("parsing response: %w", err)
	}
	return resp.Enabled, nil
}

func (g *ghCLI) PostEnforceAdmins(nwo, branch string) (string, error) {
	endpoint := fmt.Sprintf("/repos/%s/branches/%s/protection/enforce_admins", nwo, branch)
	cmd := exec.Command("gh", "api", "-X", "POST", endpoint)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
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

func (g *ghCLI) FindPR(branch, workDir string) (string, string, string, error) {
	cmd := exec.Command("gh", "pr", "list",
		"--head", branch,
		"--state", "all", "--json", "number,title,url", "--jq", `.[0] | "\(.number)\t\(.title)\t\(.url)"`)
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", "", "", fmt.Errorf("gh pr list failed: %w", err)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return "", "", "", nil
	}
	parts := strings.SplitN(raw, "\t", 3)
	if len(parts) < 2 {
		return parts[0], "", "", nil
	}
	if len(parts) < 3 {
		return parts[0], parts[1], "", nil
	}
	return parts[0], parts[1], parts[2], nil
}

func (g *ghCLI) SearchPR(workDir, query string) (string, error) {
	cmd := exec.Command("gh", "pr", "list", "--search", query,
		"--state", "all", "--json", "number", "--jq", ".[0].number")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr list search failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *ghCLI) PRDiff(workDir, prNumber string) (string, error) {
	cmd := exec.Command("gh", "pr", "diff", prNumber)
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr diff failed: %w", err)
	}
	return string(out), nil
}

func (g *ghCLI) GetPRState(workDir, prNumber string) (string, error) {
	cmd := exec.Command("gh", "pr", "view", prNumber,
		"--json", "state", "--jq", ".state")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *ghCLI) GetPRBase(workDir, prNumber string) (string, error) {
	cmd := exec.Command("gh", "pr", "view", prNumber,
		"--json", "baseRefName", "--jq", ".baseRefName")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
