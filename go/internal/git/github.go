package git

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ParsePRNumber validates and converts a raw PR number string (typically
// from jq or gh CLI output) into a typed int. It rejects empty strings,
// the literal "null" (jq's output for missing values), non-numeric values,
// and zero/negative numbers.
func ParsePRNumber(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("empty PR number")
	}
	if s == "null" {
		return 0, fmt.Errorf("PR number is null (no PR found)")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid PR number %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid PR number %d: must be positive", n)
	}
	return n, nil
}

// PRInfo holds basic metadata about a GitHub pull request.
type PRInfo struct {
	Number int
	Head   string
	Base   string
	State  string
}

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
	// Admin bypasses branch protection rules using gh pr merge --admin.
	// Only set when the caller has explicitly opted in (infrastructure failure
	// with local tests passing, or --bypass-rules flag).
	Admin bool
}

// MergeResult is the structured outcome of a merge attempt.
type MergeResult struct {
	Merged  bool
	Message string
	// Blocked is true when branch protection prevents the merge
	// (required checks pending, review required, etc.)
	Blocked bool
	// Conflict is true when there are merge conflicts.
	Conflict bool
}

// GitHub abstracts GitHub CLI operations for testability. Production code
// uses ghCLI; tests inject stubs to avoid shelling out to gh.
type GitHub interface {
	Available() bool
	FindOpenPR(branch, repoURL string) (prNumber int, err error)
	CreatePR(opts CreatePROpts) (prNumber int, err error)
	MergePR(prNumber int, repoURL string, opts MergeOpts) MergeResult
	ListChecks(prNumber int, repoURL string) ([]CICheckResult, error)
	EditPR(prNumber int, repoURL, title, body string) error
	GetRunLog(prNumber int, workDir string) string
	CheckEnforceAdmins(nwo, branch string) (enabled bool, err error)
	PostEnforceAdmins(nwo, branch string) (output string, err error)
	FindPR(branch, repoURL string) (number int, title, url string, err error)
	SearchPR(workDir, query string) (prNumber int, err error)
	PRDiff(workDir string, prNumber int) (string, error)
	GetPRState(workDir string, prNumber int) (state string, err error)
	GetPRBase(workDir string, prNumber int) (base string, err error)
	GetPRHead(workDir string, prNumber int) (head string, err error)
	GetPRHeadSHA(workDir string, prNumber int) (sha string, err error)
	ListOpenPRBranches(repoURL string) ([]string, error)
	ReopenPR(prNumber int, repoURL string) error
	CreatePRViaAPI(nwo string, opts CreatePROpts) (prNumber int, err error)
	GetJobStepCount(nwo string, prNumber int) (int, error)
	// ListAllPRs returns all PRs (open and closed) for chain-walking during stack merge.
	ListAllPRs(workDir string) ([]PRInfo, error)
}

// ghCLI implements GitHub using the gh CLI tool.
type ghCLI struct{}

func (g *ghCLI) Available() bool {
	p, err := exec.LookPath("gh")
	return err == nil && p != ""
}

func (g *ghCLI) FindOpenPR(branch, repoURL string) (int, error) {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return 0, fmt.Errorf("cannot determine owner/repo from %q", repoURL)
	}
	owner := strings.SplitN(nwo, "/", 2)[0]
	endpoint := fmt.Sprintf("repos/%s/pulls", nwo)
	cmd := exec.Command("gh", "api", endpoint,
		"-f", "state=open",
		"-f", fmt.Sprintf("head=%s:%s", owner, branch),
		"--jq", ".[0].number // empty")
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("gh api pulls failed: %w", err)
	}
	result := strings.TrimSpace(string(out))
	if result == "" {
		return 0, nil
	}
	return ParsePRNumber(result)
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

func (g *ghCLI) CreatePR(opts CreatePROpts) (int, error) {
	nwo := NWOFromRemote(opts.Repo)
	if nwo == "" {
		return 0, fmt.Errorf("cannot determine owner/repo from %q", opts.Repo)
	}
	return g.CreatePRViaAPI(nwo, opts)
}

func (g *ghCLI) EditPR(prNumber int, repoURL, title, body string) error {
	pr := strconv.Itoa(prNumber)
	args := []string{"pr", "edit", pr, "--title", title, "-R", repoURL}
	if body != "" {
		args = append(args, "--body", body)
	}
	cmd := exec.Command("gh", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("PR edit failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (g *ghCLI) MergePR(prNumber int, repoURL string, opts MergeOpts) MergeResult {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return MergeResult{Message: "cannot determine owner/repo from remote URL"}
	}
	pr := strconv.Itoa(prNumber)

	endpoint := fmt.Sprintf("repos/%s/pulls/%s/merge", nwo, pr)
	args := []string{"api", "-X", "PUT", endpoint, "--include", "-f", "merge_method=squash"}
	if opts.Subject != "" {
		args = append(args, "-f", "commit_title="+opts.Subject)
	}
	cmd := exec.Command("gh", args...)
	out, err := cmd.CombinedOutput()

	result := classifyMergeStatus(string(out), err)
	if result.Merged {
		if opts.DeleteBranch {
			g.deleteBranch(nwo, pr)
		}
		return result
	}

	// Method Not Allowed: branch protection blocks merge via REST API.
	// If caller opted in to admin bypass, fall back to gh pr merge --admin.
	if result.Blocked && opts.Admin {
		return g.mergeAdmin(pr, repoURL, nwo, opts)
	}
	return result
}

// mergeAdmin uses gh pr merge --admin to bypass branch protection rules.
// Used when the caller explicitly opts in via MergeOpts.Admin.
func (g *ghCLI) mergeAdmin(prNumber, repoURL, nwo string, opts MergeOpts) MergeResult {
	args := []string{"pr", "merge", prNumber, "--admin", "--squash", "-R", repoURL}
	if opts.DeleteBranch {
		args = append(args, "--delete-branch")
	}
	cmd := exec.Command("gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return MergeResult{Message: strings.TrimSpace(string(out))}
	}
	return MergeResult{Merged: true, Message: "merged (admin)"}
}

// classifyMergeStatus maps gh api --include output to a structured MergeResult.
// HTTP 200 = merged, 405 = blocked by branch protection, 409 = merge conflict.
func classifyMergeStatus(output string, err error) MergeResult {
	statusCode := parseHTTPStatus(output)
	if err == nil || statusCode == 200 {
		return MergeResult{Merged: true, Message: "merged"}
	}
	msg := parseAPIMessage(output)
	switch statusCode {
	case 405:
		return MergeResult{Blocked: true, Message: msg}
	case 409:
		return MergeResult{Conflict: true, Message: msg}
	default:
		return MergeResult{Message: msg}
	}
}

// parseHTTPStatus extracts the status code from the first line of
// gh api --include output (e.g. "HTTP/2.0 200 OK\n...").
func parseHTTPStatus(output string) int {
	firstLine := output
	if idx := strings.IndexByte(output, '\n'); idx > 0 {
		firstLine = output[:idx]
	}
	parts := strings.Fields(firstLine)
	if len(parts) >= 2 {
		var code int
		if _, err := fmt.Sscanf(parts[1], "%d", &code); err == nil {
			return code
		}
	}
	return 0
}

// parseAPIMessage extracts the "message" field from a GitHub API JSON
// response. The response body follows HTTP headers (separated by blank line)
// in gh api --include output.
func parseAPIMessage(output string) string {
	// Find the blank line separating headers from body.
	body := output
	if idx := strings.Index(output, "\r\n\r\n"); idx > 0 {
		body = output[idx+4:]
	} else if idx := strings.Index(output, "\n\n"); idx > 0 {
		body = output[idx+2:]
	}

	var resp struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &resp); err == nil && resp.Message != "" {
		return resp.Message
	}
	return strings.TrimSpace(body)
}

// deleteBranch deletes the PR's head branch after a successful merge.
func (g *ghCLI) deleteBranch(nwo, prNumber string) {
	// Look up the head branch name from the PR.
	cmd := exec.Command("gh", "api", fmt.Sprintf("repos/%s/pulls/%s", nwo, prNumber), "--jq", ".head.ref")
	out, err := cmd.Output()
	if err != nil {
		return
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return
	}
	delCmd := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/git/refs/heads/%s", nwo, branch),
		"--method", "DELETE")
	delCmd.CombinedOutput() // best-effort
}

func (g *ghCLI) ListChecks(prNumber int, repoURL string) ([]CICheckResult, error) {
	pr := strconv.Itoa(prNumber)
	args := []string{"pr", "checks", pr, "--json", "name,state,bucket"}
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

func (g *ghCLI) GetRunLog(prNumber int, workDir string) string {
	pr := strconv.Itoa(prNumber)
	cmd := exec.Command("gh", "pr", "checks", pr, "--json", "name,state,link", "--jq",
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

func (g *ghCLI) FindPR(branch, repoURL string) (int, string, string, error) {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return 0, "", "", fmt.Errorf("cannot determine owner/repo from %q", repoURL)
	}
	owner := strings.SplitN(nwo, "/", 2)[0]
	endpoint := fmt.Sprintf("repos/%s/pulls", nwo)
	cmd := exec.Command("gh", "api", endpoint,
		"-f", "state=all",
		"-f", fmt.Sprintf("head=%s:%s", owner, branch),
		"--jq", `.[0] // empty | "\(.number)\t\(.title)\t\(.html_url)"`)
	out, err := cmd.Output()
	if err != nil {
		return 0, "", "", fmt.Errorf("gh api pulls failed: %w", err)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return 0, "", "", nil
	}
	parts := strings.SplitN(raw, "\t", 3)
	num, parseErr := ParsePRNumber(parts[0])
	if parseErr != nil {
		return 0, "", "", parseErr
	}
	title := ""
	url := ""
	if len(parts) >= 2 {
		title = parts[1]
	}
	if len(parts) >= 3 {
		url = parts[2]
	}
	return num, title, url, nil
}

func (g *ghCLI) SearchPR(workDir, query string) (int, error) {
	cmd := exec.Command("gh", "pr", "list", "--search", query,
		"--state", "all", "--json", "number", "--jq", ".[0].number")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("gh pr list search failed: %w", err)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return 0, nil
	}
	return ParsePRNumber(raw)
}

func (g *ghCLI) PRDiff(workDir string, prNumber int) (string, error) {
	cmd := exec.Command("gh", "pr", "diff", strconv.Itoa(prNumber))
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr diff failed: %w", err)
	}
	return string(out), nil
}

func (g *ghCLI) ReopenPR(prNumber int, repoURL string) error {
	args := []string{"pr", "reopen", strconv.Itoa(prNumber)}
	if repoURL != "" {
		args = append(args, "-R", repoURL)
	}
	cmd := exec.Command("gh", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr reopen failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (g *ghCLI) CreatePRViaAPI(nwo string, opts CreatePROpts) (int, error) {
	body := fmt.Sprintf(`{"title":%q,"body":%q,"head":%q,"base":%q}`,
		opts.Title, opts.Body, opts.Head, opts.Base)
	endpoint := fmt.Sprintf("repos/%s/pulls", nwo)
	cmd := exec.Command("gh", "api", endpoint, "--method", "POST", "--input", "-")
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("API PR creation failed: %s", strings.TrimSpace(string(out)))
	}
	var resp struct {
		Number int `json:"number"`
	}
	if jsonErr := json.Unmarshal(out, &resp); jsonErr != nil {
		return 0, fmt.Errorf("parsing PR API response: %w", jsonErr)
	}
	if resp.Number == 0 {
		return 0, fmt.Errorf("API PR creation returned no number")
	}
	return resp.Number, nil
}

func (g *ghCLI) GetJobStepCount(nwo string, prNumber int) (int, error) {
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

func (g *ghCLI) GetPRState(workDir string, prNumber int) (string, error) {
	cmd := exec.Command("gh", "pr", "view", strconv.Itoa(prNumber),
		"--json", "state", "--jq", ".state")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *ghCLI) GetPRBase(workDir string, prNumber int) (string, error) {
	cmd := exec.Command("gh", "pr", "view", strconv.Itoa(prNumber),
		"--json", "baseRefName", "--jq", ".baseRefName")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *ghCLI) GetPRHead(workDir string, prNumber int) (string, error) {
	cmd := exec.Command("gh", "pr", "view", strconv.Itoa(prNumber),
		"--json", "headRefName", "--jq", ".headRefName")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *ghCLI) GetPRHeadSHA(workDir string, prNumber int) (string, error) {
	cmd := exec.Command("gh", "pr", "view", strconv.Itoa(prNumber),
		"--json", "headRefOid", "--jq", ".headRefOid")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *ghCLI) ListAllPRs(workDir string) ([]PRInfo, error) {
	cmd := exec.Command("gh", "pr", "list", "--state", "all",
		"--json", "number,headRefName,baseRefName,state", "--limit", "200")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	var raw []struct {
		Number int    `json:"number"`
		Head   string `json:"headRefName"`
		Base   string `json:"baseRefName"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse PR list: %w", err)
	}
	prs := make([]PRInfo, len(raw))
	for i, r := range raw {
		prs[i] = PRInfo{Number: r.Number, Head: r.Head, Base: r.Base, State: r.State}
	}
	return prs, nil
}
