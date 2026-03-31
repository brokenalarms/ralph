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
	GetPRHead(workDir, prNumber string) (head string, err error)
	GetPRHeadSHA(workDir, prNumber string) (sha string, err error)
	ListOpenPRBranches(repoURL string) ([]string, error)
	ReopenPR(prNumber, repoURL string) error
	CreatePRViaAPI(nwo string, opts CreatePROpts) (prNumber string, err error)
	GetJobStepCount(nwo, prNumber string) (int, error)
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

func (g *ghCLI) ListOpenPRBranches(repoURL string) ([]string, error) {
	cmd := exec.Command("gh", "pr", "list", "--state", "open",
		"--json", "headRefName", "--jq", ".[].headRefName", "-R", repoURL)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list failed: %w", err)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, nil
	}
	return strings.Split(raw, "\n"), nil
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
	return checks, nil
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
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
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

func (g *ghCLI) ReopenPR(prNumber, repoURL string) error {
	args := []string{"pr", "reopen", prNumber}
	if repoURL != "" {
		args = append(args, "-R", repoURL)
	}
	cmd := exec.Command("gh", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr reopen failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (g *ghCLI) CreatePRViaAPI(nwo string, opts CreatePROpts) (string, error) {
	body := fmt.Sprintf(`{"title":%q,"body":%q,"head":%q,"base":%q}`,
		opts.Title, opts.Body, opts.Head, opts.Base)
	endpoint := fmt.Sprintf("repos/%s/pulls", nwo)
	cmd := exec.Command("gh", "api", endpoint, "--method", "POST", "--input", "-")
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("API PR creation failed: %s", strings.TrimSpace(string(out)))
	}
	var resp struct {
		Number int `json:"number"`
	}
	if jsonErr := json.Unmarshal(out, &resp); jsonErr != nil {
		return "", fmt.Errorf("parsing PR API response: %w", jsonErr)
	}
	if resp.Number == 0 {
		return "", fmt.Errorf("API PR creation returned no number")
	}
	return fmt.Sprintf("%d", resp.Number), nil
}

func (g *ghCLI) GetJobStepCount(nwo, prNumber string) (int, error) {
	cmd := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/actions/runs?event=pull_request&per_page=1", nwo),
		"--jq", ".workflow_runs[0].id")
	out, err := cmd.Output()
	if err != nil {
		return -1, fmt.Errorf("failed to get runs: %w", err)
	}
	runID := strings.TrimSpace(string(out))
	if runID == "" {
		return -1, fmt.Errorf("no runs found")
	}
	jobsCmd := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/actions/runs/%s/jobs", nwo, runID),
		"--jq", "[.jobs[].steps | length] | add // 0")
	jobsOut, err := jobsCmd.Output()
	if err != nil {
		return -1, fmt.Errorf("failed to get jobs: %w", err)
	}
	count := 0
	fmt.Sscanf(strings.TrimSpace(string(jobsOut)), "%d", &count)
	return count, nil
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

func (g *ghCLI) GetPRHead(workDir, prNumber string) (string, error) {
	cmd := exec.Command("gh", "pr", "view", prNumber,
		"--json", "headRefName", "--jq", ".headRefName")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *ghCLI) GetPRHeadSHA(workDir, prNumber string) (string, error) {
	cmd := exec.Command("gh", "pr", "view", prNumber,
		"--json", "headRefOid", "--jq", ".headRefOid")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
