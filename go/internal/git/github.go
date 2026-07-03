package git

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// ciSkipMarkerRe matches GitHub's documented skip-CI bracket markers,
// case-insensitively.
var ciSkipMarkerRe = regexp.MustCompile(`(?i)\[(skip ci|ci skip|no ci|skip actions|actions skip)\]`)

// neutralizeCISkipMarkers replaces each GitHub skip-CI bracket marker in s
// with a hyphenated form (e.g. "[skip ci]" → "[skip-ci]") that GitHub does
// not recognise, so bead text that describes CI skip behaviour cannot
// inadvertently suppress workflows on the resulting squash-merge commit.
func neutralizeCISkipMarkers(s string) string {
	return ciSkipMarkerRe.ReplaceAllStringFunc(s, func(match string) string {
		inner := match[1 : len(match)-1]
		return "[" + strings.ReplaceAll(inner, " ", "-") + "]"
	})
}

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
	ID   int // GitHub REST comment database ID, used for replies and thread resolution
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
	repo string
	Dir  string
}

// formatPRBody assembles a PR description from task context (description,
// acceptance criteria, agent summary). The caller pre-fetches the data
// from whatever task backend it uses and passes it as plain strings —
// git/github owns the GitHub-specific markdown layout, the caller owns
// the data.
//
// Returns an empty string when no context is available. Never produces
// generic boilerplate ("Automated PR for X" or similar) — if the caller
// has nothing to say, the body is empty.
func formatPRBody(description, acceptance, summary string) string {
	var sections []string
	if description != "" {
		sections = append(sections, "## Description\n"+description)
	}
	if acceptance != "" {
		sections = append(sections, "## Acceptance Criteria\n"+acceptance)
	}
	if summary != "" {
		sections = append(sections, "## Summary\n"+summary)
	}
	if len(sections) == 0 {
		return ""
	}
	return neutralizeCISkipMarkers(strings.Join(sections, "\n\n"))
}

// MergeOpts configures a PR merge operation.
type MergeOpts struct {
	DeleteBranch bool
	Subject      string
	// Admin bypasses branch protection rules. Requires administrator access.
	// Only set this when isInfrastructureFailure has confirmed the CI never ran.
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
	// MergedSHA is the commit SHA produced by the squash-merge, populated
	// when Merged=true. Empty when the response body cannot be parsed.
	MergedSHA string
}

// gitHub abstracts GitHub CLI operations. Unexported — the production
// implementation (ghCLI) is always constructed by New(). Git-package
// tests inject stubGitHub via newStubGitHub (same-package, unexported).
type gitHub interface {
	Available() bool
	FindOpenPR(ctx context.Context, branch, repoURL string) (prNumber int, err error)
	CreatePR(ctx context.Context, opts CreatePROpts) (prNumber int, err error)
	MergePR(ctx context.Context, prNumber int, repoURL string, opts MergeOpts) MergeResult
	ListChecks(ctx context.Context, prNumber int, repoURL string) ([]CICheckResult, error)
	EditPR(ctx context.Context, prNumber int, repoURL, title, body string) error
	// EditPRBase retargets a PR to the given base branch via PATCH /repos/{nwo}/pulls/{number}.
	EditPRBase(ctx context.Context, prNumber int, repoURL, base string) error
	GetRunLog(ctx context.Context, prNumber int, workDir string) string
	FindPR(ctx context.Context, branch, repoURL string) (number int, title, url string, err error)
	PRDiff(ctx context.Context, repoURL string, prNumber int) (string, error)
	GetPR(ctx context.Context, nwo string, prNumber int) (*PRDetail, error)
	ListOpenPRBranches(ctx context.Context, repoURL string) ([]string, error)
	ReopenPR(ctx context.Context, prNumber int, repoURL string) error
	CreatePRViaAPI(ctx context.Context, nwo string, opts CreatePROpts) (prNumber int, err error)
	GetJobStepCount(ctx context.Context, nwo string, prNumber int) (int, error)
	// ListAllPRs returns all PRs (open and closed) for chain-walking during stack merge.
	ListAllPRs(ctx context.Context, workDir string) ([]PRInfo, error)
	// DetectActiveReviewers queries the repo's installed GitHub Apps and cross-
	// references against the Known reviewer registry. For Copilot it also checks
	// rulesets to set the correct timeout. Returns the active reviewer list.
	DetectActiveReviewers(ctx context.Context, nwo string) ([]Reviewer, error)
	// PollReview polls for a review from the given bot username on the given PR,
	// returning it with inline comments when found. Returns nil without error if
	// the timeout expires before a review arrives.
	PollReview(ctx context.Context, nwo string, botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error)
	// GetRequiredChecks returns the required status check context names for the
	// given branch from branch protection rulesets. Returns an empty slice when
	// no required checks are configured, which means all checks are evaluated.
	GetRequiredChecks(ctx context.Context, nwo, branch string) ([]string, error)
	// ReplyToReviewComment posts a reply to an inline review comment thread.
	ReplyToReviewComment(ctx context.Context, nwo string, prNumber, commentID int, body string) error
	// FetchReviewThreadIDs returns a map from REST comment database ID to GraphQL
	// thread node ID for all review threads on the given PR. Used to resolve threads
	// after addressing review feedback.
	FetchReviewThreadIDs(ctx context.Context, nwo string, prNumber int, commentIDs []int) (map[int]string, error)
	// ResolveReviewThread resolves a review thread by its GraphQL node ID.
	ResolveReviewThread(ctx context.Context, threadID string) error
	// Ping verifies that GitHub is reachable. Returns nil when reachable, an
	// error otherwise (including timeout).
	Ping(ctx context.Context) error
}

// ghCLI implements GitHub using the gh CLI tool.
type ghCLI struct {
	logger                      Log
	CopilotGatedTimeout         time.Duration
	CopilotOpportunisticTimeout time.Duration
	CodeRabbitTimeout           time.Duration
}

// firstErr returns the context error if present, otherwise the command error.
// Context cancellation ("context deadline exceeded") is more informative than
// the raw OS signal that gh reports when killed.
func firstErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// shellSafeArgs joins gh args into a single string suitable for the log entry.
// Args containing shell metacharacters are quoted with %q.
func shellSafeArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t\n\"'\\") {
			quoted[i] = fmt.Sprintf("%q", a)
		} else {
			quoted[i] = a
		}
	}
	return strings.Join(quoted, " ")
}

// logGHFailure emits exactly one Warn entry for a failed gh invocation.
// stderrOut and stdoutOut are the raw buffer bytes; if empty, "(empty)" is
// substituted. stdoutOut is capped at 500 bytes to keep the log scannable.
func (g *ghCLI) logGHFailure(args []string, stderrOut, stdoutOut []byte, elapsed time.Duration, err error) {
	if g.logger == nil {
		return
	}
	stderrStr := "(empty)"
	if len(stderrOut) > 0 {
		stderrStr = string(stderrOut)
	}
	stdoutStr := "(empty)"
	if len(stdoutOut) > 0 {
		b := stdoutOut
		if len(b) > 500 {
			b = b[:500]
		}
		stdoutStr = string(b)
	}
	g.logger.Emit(logging.Opts{Domain: logging.Git, Level: logging.Warn},
		`[gh] command "%s" failed after %s — stderr: %s — stdout: %s — err: %v`,
		shellSafeArgs(args), elapsed.Round(time.Millisecond), stderrStr, stdoutStr, err)
}

// ghCallTimeout bounds a single gh invocation. The loop no longer applies an
// iteration-wide deadline (see completeTask), so each gh call carries its own
// per-operation timeout — generous enough for any individual call (PR view,
// merge, api, single CI/review poll fetch) but short enough that a genuinely
// hung gh process (network partition, stuck auth) cannot block the loop
// indefinitely. Polling loops (PollReview, waitForCI) issue many short calls
// and bound their own total duration, so this per-call cap does not truncate
// them.
const ghCallTimeout = 3 * time.Minute

// runGHCmd runs gh with the given args, capturing stderr to a buffer.
// On err != nil or ctx.Err() != nil, emits one Warn log entry.
// Returns stdout output and the error.
func (g *ghCLI) runGHCmd(ctx context.Context, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ghCallTimeout)
	defer cancel()
	start := time.Now()
	var stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stderr = &stderrBuf
	out, err := cmd.Output()
	if err != nil || ctx.Err() != nil {
		g.logGHFailure(args, stderrBuf.Bytes(), out, time.Since(start), firstErr(ctx, err))
	}
	return out, err
}

// runGHCombined runs gh with the given args, capturing stdout and stderr into
// separate buffers. On err != nil or ctx.Err() != nil, emits one Warn log entry.
// Returns stdout, stderr, and the error.
func (g *ghCLI) runGHCombined(ctx context.Context, args []string) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, ghCallTimeout)
	defer cancel()
	start := time.Now()
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err = cmd.Run()
	stdout = stdoutBuf.Bytes()
	stderr = stderrBuf.Bytes()
	if err != nil || ctx.Err() != nil {
		g.logGHFailure(args, stderr, stdout, time.Since(start), firstErr(ctx, err))
	}
	return
}

func (g *ghCLI) Available() bool {
	p, err := exec.LookPath("gh")
	return err == nil && p != ""
}

// ghPingTimeout bounds the GitHub connectivity check. Kept short (10s)
// relative to ghCallTimeout because Ping runs at startup to fail fast on a
// blocked connection, rather than as part of a normal operation.
const ghPingTimeout = 10 * time.Second

// Ping verifies that GitHub is reachable via gh api /rate_limit.
func (g *ghCLI) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ghPingTimeout)
	defer cancel()
	_, err := g.runGHCmd(ctx, []string{"api", "/rate_limit"})
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("GitHub connectivity check timed out after %s", ghPingTimeout)
		}
		return fmt.Errorf("gh api /rate_limit: %w", err)
	}
	return nil
}

func (g *ghCLI) FindOpenPR(ctx context.Context, branch, repoURL string) (int, error) {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return 0, fmt.Errorf("cannot determine owner/repo from %q", repoURL)
	}
	args := []string{"pr", "list",
		"--head", branch,
		"--repo", nwo,
		"--state", "open",
		"--json", "number",
		"--jq", ".[0].number // empty"}
	out, err := g.runGHCmd(ctx, args)
	if err != nil {
		return 0, fmt.Errorf("gh pr list failed: %w", err)
	}
	result := strings.TrimSpace(string(out))
	if result == "" {
		return 0, nil
	}
	return ParsePRNumber(result)
}

func (g *ghCLI) ListOpenPRBranches(ctx context.Context, repoURL string) ([]string, error) {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return nil, fmt.Errorf("cannot determine owner/repo from %q", repoURL)
	}
	endpoint := fmt.Sprintf("repos/%s/pulls?state=open", nwo)
	out, err := g.runGHCmd(ctx, []string{"api", "--paginate", endpoint, "--jq", ".[].head.ref"})
	if err != nil {
		return nil, fmt.Errorf("gh api pulls failed: %w", err)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, nil
	}
	return strings.Split(raw, "\n"), nil
}

func (g *ghCLI) CreatePR(ctx context.Context, opts CreatePROpts) (int, error) {
	nwo := NWOFromRemote(opts.repo)
	if nwo == "" {
		return 0, fmt.Errorf("cannot determine owner/repo from %q", opts.repo)
	}
	return g.CreatePRViaAPI(ctx, nwo, opts)
}

func (g *ghCLI) EditPR(ctx context.Context, prNumber int, repoURL, title, body string) error {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return fmt.Errorf("cannot determine owner/repo from %q", repoURL)
	}
	endpoint := fmt.Sprintf("repos/%s/pulls/%d", nwo, prNumber)
	args := []string{"api", "-X", "PATCH", endpoint, "-f", "title=" + title}
	if body != "" {
		args = append(args, "-f", "body="+body)
	}
	stdout, stderr, err := g.runGHCombined(ctx, args)
	if err != nil {
		msg := strings.TrimSpace(string(append(stderr, stdout...)))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("PR edit failed: %s", msg)
	}
	return nil
}

func (g *ghCLI) EditPRBase(ctx context.Context, prNumber int, repoURL, base string) error {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return fmt.Errorf("cannot determine owner/repo from %q", repoURL)
	}
	endpoint := fmt.Sprintf("repos/%s/pulls/%d", nwo, prNumber)
	args := []string{"api", "-X", "PATCH", endpoint, "-f", "base=" + base}
	stdout, stderr, err := g.runGHCombined(ctx, args)
	if err != nil {
		msg := strings.TrimSpace(string(append(stderr, stdout...)))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("PR base retarget failed: %s", msg)
	}
	return nil
}

func (g *ghCLI) MergePR(ctx context.Context, prNumber int, repoURL string, opts MergeOpts) MergeResult {
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
	if opts.Admin {
		args = append(args, "-F", "bypass_restrictions=true")
	}
	stdout, _, err := g.runGHCombined(ctx, args)

	result := classifyMergeStatus(string(stdout), err)
	if result.Merged {
		if opts.DeleteBranch {
			g.deleteBranch(ctx, nwo, pr)
		}
		return result
	}
	return result
}

// classifyMergeStatus maps gh api --include output to a structured MergeResult.
// HTTP 200 = merged, 405 = blocked by branch protection, 409 = merge conflict.
func classifyMergeStatus(output string, err error) MergeResult {
	statusCode := parseHTTPStatus(output)
	if err == nil || statusCode == 200 {
		return MergeResult{Merged: true, Message: "merged", MergedSHA: parseMergedSHA(output)}
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

// parseMergedSHA extracts the "sha" field from a GitHub squash-merge API
// response body (the part after the HTTP header block in --include output).
func parseMergedSHA(output string) string {
	body := output
	if idx := strings.Index(output, "\r\n\r\n"); idx > 0 {
		body = output[idx+4:]
	} else if idx := strings.Index(output, "\n\n"); idx > 0 {
		body = output[idx+2:]
	}
	var resp struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &resp); err == nil {
		return resp.SHA
	}
	return ""
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
func (g *ghCLI) deleteBranch(ctx context.Context, nwo, prNumber string) {
	// Look up the head branch name from the PR.
	out, err := g.runGHCmd(ctx, []string{"api", fmt.Sprintf("repos/%s/pulls/%s", nwo, prNumber), "--jq", ".head.ref"})
	if err != nil {
		return
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return
	}
	// best-effort delete — log on failure but don't propagate
	g.runGHCmd(ctx, []string{"api", fmt.Sprintf("repos/%s/git/refs/heads/%s", nwo, branch), "--method", "DELETE"}) //nolint:errcheck
}

func (g *ghCLI) ListChecks(ctx context.Context, prNumber int, repoURL string) ([]CICheckResult, error) {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return nil, fmt.Errorf("cannot determine owner/repo from %q", repoURL)
	}
	detail, err := g.GetPR(ctx, nwo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("getting PR head SHA: %w", err)
	}
	endpoint := fmt.Sprintf("repos/%s/commits/%s/check-runs", nwo, detail.HeadSHA)
	out, err := g.runGHCmd(ctx, []string{"api", endpoint, "--jq", ".check_runs"})
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

func (g *ghCLI) GetRequiredChecks(ctx context.Context, nwo, branch string) ([]string, error) {
	endpoint := fmt.Sprintf("repos/%s/rules/branches/%s", nwo, branch)
	out, err := g.runGHCmd(ctx, []string{"api", endpoint})
	if err != nil {
		return nil, fmt.Errorf("gh api rules/branches failed: %w", err)
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(out, &rules); err != nil {
		return nil, fmt.Errorf("parsing branch rules: %w", err)
	}
	var checks []string
	for _, rule := range rules {
		if rule.Type == "required_status_checks" {
			for _, c := range rule.Parameters.RequiredStatusChecks {
				checks = append(checks, c.Context)
			}
		}
	}
	return checks, nil
}

func (g *ghCLI) ReplyToReviewComment(ctx context.Context, nwo string, prNumber, commentID int, body string) error {
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/comments/%d/replies", nwo, prNumber, commentID)
	input, err := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: body})
	if err != nil {
		return fmt.Errorf("marshaling reply body: %w", err)
	}
	start := time.Now()
	args := []string{"api", "--method", "POST", endpoint, "--input", "-"}
	var stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = &stderrBuf
	out, err := cmd.Output()
	if err != nil || ctx.Err() != nil {
		g.logGHFailure(args, stderrBuf.Bytes(), out, time.Since(start), firstErr(ctx, err))
		return fmt.Errorf("gh api reply to review comment: %w", firstErr(ctx, err))
	}
	return nil
}

func (g *ghCLI) FetchReviewThreadIDs(ctx context.Context, nwo string, prNumber int, commentIDs []int) (map[int]string, error) {
	parts := strings.SplitN(nwo, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid nwo: %q", nwo)
	}
	const q = `query($owner: String!, $repo: String!, $prNumber: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $prNumber) {
      reviewThreads(first: 100) {
        nodes {
          id
          comments(first: 20) {
            nodes {
              databaseId
            }
          }
        }
      }
    }
  }
}`
	args := []string{"api", "graphql",
		"-f", "query=" + q,
		"-f", "owner=" + parts[0],
		"-f", "repo=" + parts[1],
		"-F", fmt.Sprintf("prNumber=%d", prNumber),
	}
	out, err := g.runGHCmd(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("gh api graphql review threads: %w", err)
	}
	var resp struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							ID       string `json:"id"`
							Comments struct {
								Nodes []struct {
									DatabaseID int `json:"databaseId"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parsing review threads: %w", err)
	}
	idSet := make(map[int]bool, len(commentIDs))
	for _, id := range commentIDs {
		idSet[id] = true
	}
	result := make(map[int]string)
	for _, thread := range resp.Data.Repository.PullRequest.ReviewThreads.Nodes {
		for _, comment := range thread.Comments.Nodes {
			if idSet[comment.DatabaseID] {
				result[comment.DatabaseID] = thread.ID
			}
		}
	}
	return result, nil
}

func (g *ghCLI) ResolveReviewThread(ctx context.Context, threadID string) error {
	const q = `mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id }
  }
}`
	args := []string{"api", "graphql",
		"-f", "query=" + q,
		"-f", "threadId=" + threadID,
	}
	if _, err := g.runGHCmd(ctx, args); err != nil {
		return fmt.Errorf("gh api graphql resolve thread: %w", err)
	}
	return nil
}

func (g *ghCLI) GetRunLog(ctx context.Context, prNumber int, workDir string) string {
	pr := strconv.Itoa(prNumber)
	start1 := time.Now()
	args1 := []string{"pr", "checks", pr, "--json", "name,state,link", "--jq",
		`.[] | select(.state == "FAILURE") | .link`}
	var stderrBuf1 bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", args1...)
	cmd.Dir = workDir
	cmd.Stderr = &stderrBuf1
	out, err := cmd.Output()
	if err != nil || ctx.Err() != nil {
		g.logGHFailure(args1, stderrBuf1.Bytes(), out, time.Since(start1), firstErr(ctx, err))
		return ""
	}
	if len(out) == 0 {
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

	start2 := time.Now()
	args2 := []string{"run", "view", runID, "--log-failed"}
	var stderrBuf2 bytes.Buffer
	logCmd := exec.CommandContext(ctx, "gh", args2...)
	logCmd.Dir = workDir
	logCmd.Stderr = &stderrBuf2
	logOut, err := logCmd.Output()
	if err != nil {
		g.logGHFailure(args2, stderrBuf2.Bytes(), logOut, time.Since(start2), firstErr(ctx, err))
		return ""
	}

	lines := strings.Split(string(logOut), "\n")
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	return strings.Join(lines, "\n")
}

func (g *ghCLI) FindPR(ctx context.Context, branch, repoURL string) (int, string, string, error) {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return 0, "", "", fmt.Errorf("cannot determine owner/repo from %q", repoURL)
	}
	args := []string{"pr", "list",
		"--head", branch,
		"--repo", nwo,
		"--state", "all",
		"--json", "number,title,url",
		"--jq", `.[0] // empty | "\(.number)\t\(.title)\t\(.url)"`}
	out, err := g.runGHCmd(ctx, args)
	if err != nil {
		return 0, "", "", fmt.Errorf("gh pr list failed: %w", err)
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

func (g *ghCLI) PRDiff(ctx context.Context, repoURL string, prNumber int) (string, error) {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return "", fmt.Errorf("could not determine repo NWO from %q", repoURL)
	}
	args := []string{"api",
		fmt.Sprintf("repos/%s/pulls/%d", nwo, prNumber),
		"-H", "Accept: application/vnd.github.diff"}
	out, err := g.runGHCmd(ctx, args)
	if err != nil {
		return "", fmt.Errorf("gh api pull diff failed: %w", err)
	}
	return string(out), nil
}

func (g *ghCLI) ReopenPR(ctx context.Context, prNumber int, repoURL string) error {
	nwo := NWOFromRemote(repoURL)
	if nwo == "" {
		return fmt.Errorf("cannot determine owner/repo from %q", repoURL)
	}
	endpoint := fmt.Sprintf("repos/%s/pulls/%d", nwo, prNumber)
	args := []string{"api", "-X", "PATCH", endpoint, "-f", "state=open"}
	stdout, stderr, err := g.runGHCombined(ctx, args)
	if err != nil {
		msg := strings.TrimSpace(string(append(stderr, stdout...)))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("gh api PATCH %s failed: %s", endpoint, msg)
	}
	return nil
}

func (g *ghCLI) CreatePRViaAPI(ctx context.Context, nwo string, opts CreatePROpts) (int, error) {
	type prRequest struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	bodyBytes, err := json.Marshal(prRequest{Title: opts.Title, Body: opts.Body, Head: opts.Head, Base: opts.Base})
	if err != nil {
		return 0, fmt.Errorf("marshaling PR request: %w", err)
	}
	endpoint := fmt.Sprintf("repos/%s/pulls", nwo)
	start := time.Now()
	args := []string{"api", endpoint, "--method", "POST", "--input", "-"}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdin = bytes.NewReader(bodyBytes)
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err = cmd.Run()
	out := stdoutBuf.Bytes()
	if err != nil || ctx.Err() != nil {
		g.logGHFailure(args, stderrBuf.Bytes(), out, time.Since(start), firstErr(ctx, err))
	}
	if err != nil {
		if strings.Contains(string(out), "already exists") {
			if existing, findErr := g.FindOpenPR(ctx, opts.Head, opts.repo); findErr == nil && existing != 0 {
				return existing, nil
			}
		}
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

func (g *ghCLI) GetJobStepCount(ctx context.Context, nwo string, prNumber int) (int, error) {
	out, err := g.runGHCmd(ctx, []string{"api",
		fmt.Sprintf("repos/%s/actions/runs?event=pull_request&per_page=1", nwo),
		"--jq", ".workflow_runs[0].id"})
	if err != nil {
		return -1, fmt.Errorf("failed to get runs: %w", err)
	}
	runID := strings.TrimSpace(string(out))
	if runID == "" {
		return -1, fmt.Errorf("no runs found")
	}
	jobsOut, err := g.runGHCmd(ctx, []string{"api",
		fmt.Sprintf("repos/%s/actions/runs/%s/jobs", nwo, runID),
		"--jq", "[.jobs[].steps | length] | add // 0"})
	if err != nil {
		return -1, fmt.Errorf("failed to get jobs: %w", err)
	}
	count := 0
	fmt.Sscanf(strings.TrimSpace(string(jobsOut)), "%d", &count)
	return count, nil
}

func (g *ghCLI) GetPR(ctx context.Context, nwo string, prNumber int) (*PRDetail, error) {
	endpoint := fmt.Sprintf("repos/%s/pulls/%d", nwo, prNumber)
	args := []string{"api", endpoint,
		"--jq", `(if .merged_at != null then "MERGED" elif .state == "open" then "OPEN" else "CLOSED" end)+"\t"+.base.ref+"\t"+.head.ref+"\t"+.head.sha`}
	out, err := g.runGHCmd(ctx, args)
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

// DetectActiveReviewers probes the repo for known automated reviewers using
// endpoints that work with standard OAuth tokens. For Copilot, it uses the
// rulesets endpoint (checkCopilotRulesets). Reviewers with no probe are skipped.
func (g *ghCLI) DetectActiveReviewers(ctx context.Context, nwo string) ([]Reviewer, error) {
	var active []Reviewer
	for _, r := range Known {
		switch r.AppSlug {
		case "copilot-code-review":
			enabled, reviewOnPush, err := g.checkCopilotRulesets(ctx, nwo)
			if err != nil || !enabled {
				continue
			}
			reviewer := r
			reviewer.ReviewOnPush = reviewOnPush
			if reviewOnPush {
				reviewer.DefaultTimeout = g.CopilotGatedTimeout
			} else {
				reviewer.DefaultTimeout = g.CopilotOpportunisticTimeout
			}
			active = append(active, reviewer)
		}
	}
	return active, nil
}

// checkCopilotRulesets fetches each ruleset detail to find a copilot_code_review
// rule. Returns (enabled, reviewOnPush, error).
func (g *ghCLI) checkCopilotRulesets(ctx context.Context, nwo string) (bool, bool, error) {
	listOut, err := g.runGHCmd(ctx, []string{"api", fmt.Sprintf("repos/%s/rulesets", nwo)})
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
		detailOut, err := g.runGHCmd(ctx, []string{"api", fmt.Sprintf("repos/%s/rulesets/%d", nwo, entry.ID)})
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

func (g *ghCLI) PollReview(ctx context.Context, nwo string, botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error) {
	// Check if a completed review already exists before doing anything else.
	existing, err := g.fetchReview(ctx, nwo, botUsername, prNumber)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	// If the bot is not a requested reviewer and has no review, none is coming.
	requested, err := g.isRequestedReviewer(ctx, nwo, botUsername, prNumber)
	if err != nil {
		return nil, err
	}
	if !requested {
		return nil, nil
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		review, err := g.fetchReview(ctx, nwo, botUsername, prNumber)
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
	fmt.Printf("no review within timeout\n")
	return nil, nil
}

// isRequestedReviewer reports whether botUsername is listed as a requested reviewer on the PR.
func (g *ghCLI) isRequestedReviewer(ctx context.Context, nwo, botUsername string, prNumber int) (bool, error) {
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/requested_reviewers", nwo, prNumber)
	out, err := g.runGHCmd(ctx, []string{"api", endpoint})
	if err != nil {
		return false, fmt.Errorf("gh api requested_reviewers: %w", err)
	}
	var resp struct {
		Users []struct {
			Login string `json:"login"`
		} `json:"users"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return false, fmt.Errorf("parsing requested_reviewers: %w", err)
	}
	for _, u := range resp.Users {
		if u.Login == botUsername {
			return true, nil
		}
	}
	return false, nil
}

// terminalReviewStates are GitHub review states that mean the review is complete.
// PENDING means Copilot is still composing the review — keep polling.
var terminalReviewStates = map[string]bool{
	"APPROVED":          true,
	"COMMENTED":         true,
	"CHANGES_REQUESTED": true,
	"DISMISSED":         true,
}

func (g *ghCLI) fetchReview(ctx context.Context, nwo, botUsername string, prNumber int) (*AutoReview, error) {
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/reviews", nwo, prNumber)
	out, err := g.runGHCmd(ctx, []string{"api", endpoint})
	if err != nil {
		return nil, fmt.Errorf("gh api reviews: %w", err)
	}
	var reviews []struct {
		ID   int `json:"id"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body  string `json:"body"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &reviews); err != nil {
		return nil, fmt.Errorf("parsing reviews: %w", err)
	}
	for _, r := range reviews {
		if r.User.Login == botUsername && terminalReviewStates[r.State] {
			comments, err := g.fetchReviewComments(ctx, nwo, prNumber, r.ID)
			if err != nil {
				return nil, err
			}
			return &AutoReview{Body: r.Body, Comments: comments}, nil
		}
	}
	return nil, nil
}

func (g *ghCLI) fetchReviewComments(ctx context.Context, nwo string, prNumber, reviewID int) ([]ReviewComment, error) {
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/comments", nwo, prNumber)
	jqFilter := fmt.Sprintf("[.[] | select(.pull_request_review_id == %d) | {id: .id, path: .path, line: (.line // .original_line // 0), body: .body}]", reviewID)
	out, err := g.runGHCmd(ctx, []string{"api", endpoint, "--jq", jqFilter})
	if err != nil {
		return nil, fmt.Errorf("gh api pr comments: %w", err)
	}
	var raw []struct {
		ID   int    `json:"id"`
		Path string `json:"path"`
		Line int    `json:"line"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing review comments: %w", err)
	}
	comments := make([]ReviewComment, len(raw))
	for i, c := range raw {
		comments[i] = ReviewComment{ID: c.ID, Path: c.Path, Line: c.Line, Body: c.Body}
	}
	return comments, nil
}

func (g *ghCLI) ListAllPRs(ctx context.Context, workDir string) ([]PRInfo, error) {
	remoteOut, err := exec.Command("git", "-C", workDir, "remote", "get-url", "origin").Output()
	if err != nil {
		return nil, fmt.Errorf("get remote URL: %w", err)
	}
	nwo := NWOFromRemote(strings.TrimSpace(string(remoteOut)))
	if nwo == "" {
		return nil, fmt.Errorf("cannot determine owner/repo from remote URL")
	}
	endpoint := fmt.Sprintf("repos/%s/pulls?state=all", nwo)
	out, err := g.runGHCmd(ctx, []string{"api", "--paginate", "--jq", ".[]", endpoint})
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
