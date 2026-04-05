package git

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// PRState is the lifecycle state of a GitHub pull request.
type PRState string

const (
	PRStateOpen   PRState = "OPEN"
	PRStateClosed PRState = "CLOSED"
	PRStateMerged PRState = "MERGED"
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

// AutoReview is an automated code review on a pull request from a GitHub App reviewer.
type AutoReview struct {
	Body     string
	Comments []ReviewComment
}

// ReviewComment is an inline comment from a pull request review.
type ReviewComment struct {
	Path string
	Line int
	Body string
}

// PRInfo holds basic metadata about a GitHub pull request.
type PRInfo struct {
	Number int
	Head   string
	Base   string
	State  PRState
}

// PRDetail holds the full detail of a single GitHub pull request fetched via
// the REST API. Consolidates the four fields previously fetched individually.
type PRDetail struct {
	State   PRState
	BaseRef string
	HeadRef string
	HeadSHA string
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
	// Admin bypasses branch protection rules via admin token privileges.
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
	PRDiff(repoURL string, prNumber int) (string, error)
	GetPR(nwo string, prNumber int) (*PRDetail, error)
	ListOpenPRBranches(repoURL string) ([]string, error)
	ReopenPR(prNumber int, repoURL string) error
	CreatePRViaAPI(nwo string, opts CreatePROpts) (prNumber int, err error)
	GetJobStepCount(nwo string, prNumber int) (int, error)
	// ListAllPRs returns all PRs (open and closed) for chain-walking during stack merge.
	ListAllPRs(workDir string) ([]PRInfo, error)
	// DetectActiveReviewers queries the repo's installed GitHub Apps and cross-
	// references against the Known reviewer registry. For Copilot it also checks
	// rulesets to set the correct timeout. Returns the active reviewer list.
	DetectActiveReviewers(nwo string) ([]Reviewer, error)
	// PollReview polls for a review from the given bot username on the given PR,
	// returning it with inline comments when found. Returns nil without error if
	// the timeout expires before a review arrives.
	PollReview(nwo string, botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error)
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
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return nil, fmt.Errorf("cannot determine owner/repo from %q", repoURL)
	}
	endpoint := fmt.Sprintf("repos/%s/pulls?state=open", nwo)
	cmd := exec.Command("gh", "api", "--paginate", endpoint, "--jq", ".[].head.ref")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api pulls failed: %w", err)
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
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return fmt.Errorf("cannot determine owner/repo from %q", repoURL)
	}
	endpoint := fmt.Sprintf("repos/%s/pulls/%d", nwo, prNumber)
	args := []string{"api", "-X", "PATCH", endpoint, "-f", "title=" + title}
	if body != "" {
		args = append(args, "-f", "body="+body)
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
	// If caller opted in to admin bypass, retry via mergeAdmin with token privileges.
	if result.Blocked && opts.Admin {
		return g.mergeAdmin(pr, repoURL, nwo, opts)
	}
	return result
}

// mergeAdmin uses gh api PUT to merge a PR with admin token privileges,
// bypassing branch protection rules. Admin access is implicit from the token.
func (g *ghCLI) mergeAdmin(prNumber, repoURL, nwo string, opts MergeOpts) MergeResult {
	endpoint := fmt.Sprintf("repos/%s/pulls/%s/merge", nwo, prNumber)
	args := []string{"api", "-X", "PUT", endpoint, "--include", "-f", "merge_method=squash"}
	cmd := exec.Command("gh", args...)
	out, err := cmd.CombinedOutput()
	result := classifyMergeStatus(string(out), err)
	if result.Merged && opts.DeleteBranch {
		g.deleteBranch(nwo, prNumber)
	}
	return result
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
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return nil, fmt.Errorf("cannot determine owner/repo from %q", repoURL)
	}
	detail, err := g.GetPR(nwo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("getting PR head SHA: %w", err)
	}
	endpoint := fmt.Sprintf("repos/%s/commits/%s/check-runs", nwo, detail.HeadSHA)
	cmd := exec.Command("gh", "api", endpoint, "--jq", ".check_runs")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api check-runs failed: %w", err)
	}
	type apiCheckRun struct {
		Name       string     `json:"name"`
		Status     string     `json:"status"`
		Conclusion *string    `json:"conclusion"`
		StartedAt  *time.Time `json:"started_at"`
	}
	var runs []apiCheckRun
	if err := json.Unmarshal(out, &runs); err != nil {
		return nil, fmt.Errorf("parsing check-runs: %w", err)
	}
	checks := make([]CICheckResult, len(runs))
	for i, run := range runs {
		checks[i] = mapCheckRun(run.Name, run.Status, run.Conclusion, run.StartedAt)
	}
	return checks, nil
}

// mapCheckRun converts a GitHub REST API check-run to a CICheckResult.
// GitHub API: status (queued/in_progress/completed) + conclusion (success/failure/neutral/etc).
// neutral and skipped conclusions are treated as passing; cancelled and related conclusions as failing.
func mapCheckRun(name, status string, conclusion *string, startedAt *time.Time) CICheckResult {
	var state, bucket string
	if status == "completed" {
		c := ""
		if conclusion != nil {
			c = *conclusion
		}
		switch c {
		case "success", "neutral", "skipped":
			state, bucket = "SUCCESS", "pass"
		case "failure", "timed_out", "action_required", "cancelled", "startup_failure", "stale":
			state, bucket = "FAILURE", "fail"
		default:
			state, bucket = "PENDING", "pending"
		}
	} else {
		state, bucket = "PENDING", "pending"
	}
	var t time.Time
	if startedAt != nil {
		t = *startedAt
	}
	return CICheckResult{Name: name, State: state, Bucket: bucket, StartedAt: t}
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
	remoteOut, err := exec.Command("git", "-C", workDir, "remote", "get-url", "origin").Output()
	if err != nil {
		return 0, fmt.Errorf("get remote URL: %w", err)
	}
	nwo := NWOFromRemote(strings.TrimSpace(string(remoteOut)))
	if nwo == "" {
		return 0, fmt.Errorf("cannot determine owner/repo from remote URL")
	}
	q := fmt.Sprintf("%s+repo:%s+type:pr", query, nwo)
	out, err := exec.Command("gh", "api", "search/issues", "-f", "q="+q, "--jq", ".items[0].number // empty").Output()
	if err != nil {
		return 0, fmt.Errorf("gh api search failed: %w", err)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return 0, nil
	}
	return ParsePRNumber(raw)
}

func (g *ghCLI) PRDiff(repoURL string, prNumber int) (string, error) {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return "", fmt.Errorf("could not determine repo NWO from %q", repoURL)
	}
	cmd := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/pulls/%d", nwo, prNumber),
		"-H", "Accept: application/vnd.github.diff")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh api pull diff failed: %w", err)
	}
	return string(out), nil
}

func (g *ghCLI) ReopenPR(prNumber int, repoURL string) error {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return fmt.Errorf("cannot determine owner/repo from %q", repoURL)
	}
	endpoint := fmt.Sprintf("repos/%s/pulls/%d", nwo, prNumber)
	cmd := exec.Command("gh", "api", "-X", "PATCH", endpoint, "-f", "state=open")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh api PATCH %s failed: %s", endpoint, strings.TrimSpace(string(out)))
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

func (g *ghCLI) GetPR(nwo string, prNumber int) (*PRDetail, error) {
	endpoint := fmt.Sprintf("repos/%s/pulls/%d", nwo, prNumber)
	cmd := exec.Command("gh", "api", endpoint,
		"--jq", `(if .merged_at != null then "MERGED" elif .state == "open" then "OPEN" else "CLOSED" end)+"\t"+.base.ref+"\t"+.head.ref+"\t"+.head.sha`)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api PR failed: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 4)
	if len(parts) != 4 {
		return nil, fmt.Errorf("unexpected PR response: %q", strings.TrimSpace(string(out)))
	}
	return &PRDetail{
		State:   PRState(parts[0]),
		BaseRef: parts[1],
		HeadRef: parts[2],
		HeadSHA: parts[3],
	}, nil
}

// DetectActiveReviewers queries the repo's installed GitHub Apps, cross-
// references against Known, and for Copilot additionally checks rulesets to
// set the correct polling timeout.
func (g *ghCLI) DetectActiveReviewers(nwo string) ([]Reviewer, error) {
	cmd := exec.Command("gh", "api", fmt.Sprintf("repos/%s/installations", nwo))
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api installations: %w", err)
	}
	var resp struct {
		Installations []struct {
			AppSlug string `json:"app_slug"`
		} `json:"installations"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parsing installations: %w", err)
	}
	slugSet := make(map[string]bool, len(resp.Installations))
	for _, inst := range resp.Installations {
		slugSet[inst.AppSlug] = true
	}

	var active []Reviewer
	for _, r := range Known {
		if !slugSet[r.AppSlug] {
			continue
		}
		reviewer := r
		if r.AppSlug == "copilot-code-review" {
			_, reviewOnPush, err := g.checkCopilotRulesets(nwo)
			if err == nil {
				reviewer.ReviewOnPush = reviewOnPush
				if !reviewOnPush {
					reviewer.DefaultTimeout = 30 * time.Second
				}
			}
		}
		active = append(active, reviewer)
	}
	return active, nil
}

// checkCopilotRulesets fetches each ruleset detail to find a copilot_code_review
// rule. Returns (enabled, reviewOnPush, error).
func (g *ghCLI) checkCopilotRulesets(nwo string) (bool, bool, error) {
	listCmd := exec.Command("gh", "api", fmt.Sprintf("repos/%s/rulesets", nwo))
	listOut, err := listCmd.Output()
	if err != nil {
		return false, false, fmt.Errorf("gh api rulesets failed: %w", err)
	}
	var listing []struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(listOut, &listing); err != nil {
		return false, false, fmt.Errorf("parsing rulesets list: %w", err)
	}
	for _, entry := range listing {
		detailCmd := exec.Command("gh", "api", fmt.Sprintf("repos/%s/rulesets/%d", nwo, entry.ID))
		detailOut, err := detailCmd.Output()
		if err != nil {
			return false, false, fmt.Errorf("gh api rulesets/%d failed: %w", entry.ID, err)
		}
		var detail struct {
			Rules []struct {
				Type       string `json:"type"`
				Parameters struct {
					ReviewOnPush bool `json:"review_on_push"`
				} `json:"parameters"`
			} `json:"rules"`
		}
		if err := json.Unmarshal(detailOut, &detail); err != nil {
			return false, false, fmt.Errorf("parsing ruleset %d: %w", entry.ID, err)
		}
		for _, rule := range detail.Rules {
			if rule.Type == "copilot_code_review" {
				return true, rule.Parameters.ReviewOnPush, nil
			}
		}
	}
	return false, false, nil
}

func (g *ghCLI) PollReview(nwo string, botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		review, err := g.fetchReview(nwo, botUsername, prNumber)
		if err != nil {
			return nil, err
		}
		if review != nil {
			return review, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		sleep := 10 * time.Second
		if sleep > remaining {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
	return nil, nil
}

func (g *ghCLI) fetchReview(nwo, botUsername string, prNumber int) (*AutoReview, error) {
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/reviews", nwo, prNumber)
	cmd := exec.Command("gh", "api", endpoint)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api reviews: %w", err)
	}
	var reviews []struct {
		ID   int `json:"id"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(out, &reviews); err != nil {
		return nil, fmt.Errorf("parsing reviews: %w", err)
	}
	for _, r := range reviews {
		if r.User.Login == botUsername {
			comments, err := g.fetchReviewComments(nwo, prNumber, r.ID)
			if err != nil {
				return nil, err
			}
			return &AutoReview{Body: r.Body, Comments: comments}, nil
		}
	}
	return nil, nil
}

func (g *ghCLI) fetchReviewComments(nwo string, prNumber, reviewID int) ([]ReviewComment, error) {
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/comments", nwo, prNumber)
	jqFilter := fmt.Sprintf("[.[] | select(.pull_request_review_id == %d) | {path: .path, line: (.line // .original_line // 0), body: .body}]", reviewID)
	cmd := exec.Command("gh", "api", endpoint, "--jq", jqFilter)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api pr comments: %w", err)
	}
	var raw []struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing review comments: %w", err)
	}
	comments := make([]ReviewComment, len(raw))
	for i, c := range raw {
		comments[i] = ReviewComment{Path: c.Path, Line: c.Line, Body: c.Body}
	}
	return comments, nil
}

func (g *ghCLI) ListAllPRs(workDir string) ([]PRInfo, error) {
	remoteOut, err := exec.Command("git", "-C", workDir, "remote", "get-url", "origin").Output()
	if err != nil {
		return nil, fmt.Errorf("get remote URL: %w", err)
	}
	nwo := NWOFromRemote(strings.TrimSpace(string(remoteOut)))
	if nwo == "" {
		return nil, fmt.Errorf("cannot determine owner/repo from remote URL")
	}
	endpoint := fmt.Sprintf("repos/%s/pulls?state=all", nwo)
	out, err := exec.Command("gh", "api", "--paginate", "--jq", ".[]", endpoint).Output()
	if err != nil {
		return nil, fmt.Errorf("gh api pulls: %w", err)
	}
	type rawPR struct {
		Number   int                  `json:"number"`
		Head     struct{ Ref string `json:"ref"` } `json:"head"`
		Base     struct{ Ref string `json:"ref"` } `json:"base"`
		State    string               `json:"state"`
		MergedAt *string              `json:"merged_at"`
	}
	var prs []PRInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var r rawPR
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("parse PR list: %w", err)
		}
		var state PRState
		switch strings.ToLower(r.State) {
		case "open":
			state = PRStateOpen
		case "closed":
			if r.MergedAt != nil && *r.MergedAt != "" {
				state = PRStateMerged
			} else {
				state = PRStateClosed
			}
		default:
			return nil, fmt.Errorf("unrecognised PR state %q", r.State)
		}
		prs = append(prs, PRInfo{Number: r.Number, Head: r.Head.Ref, Base: r.Base.Ref, State: state})
	}
	return prs, nil
}
