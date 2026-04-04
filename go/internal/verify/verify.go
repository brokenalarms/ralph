package verify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/config"
)

// TestTimeout is the maximum duration RunTests will wait for the test
// command to complete. Exported so tests can override it.
var TestTimeout = 5 * time.Minute

// GitQuerier abstracts the git operations that verify needs, allowing the
// package to work without calling git package-level functions directly.
type GitQuerier interface {
	HeadRev() string
	DiffStatRange(from, to string) string
	DiffFull(from, to string) string
	LogOneline(from, to string) string
}

// Model IDs used as defaults for verification escalation.
const (
	ModelHaiku  = config.ModelHaiku
	ModelSonnet = config.ModelSonnet
	ModelOpus   = config.ModelOpus
)

// ModelShortName extracts a friendly name from a model ID string.
// "claude-sonnet-4-5-20241022" → "sonnet", "claude-haiku-4-5-20251001" → "haiku".
// Returns the full ID if no known family is found.
func ModelShortName(model string) string {
	for _, family := range []string{"opus", "sonnet", "haiku"} {
		if strings.Contains(model, family) {
			return family
		}
	}
	return model
}

// modelTier returns a numeric tier for a model ID: haiku=0, sonnet=1, opus=2.
// Unknown models default to sonnet tier.
func modelTier(model string) int {
	switch {
	case strings.Contains(model, "haiku"):
		return 0
	case strings.Contains(model, "sonnet"):
		return 1
	case strings.Contains(model, "opus"):
		return 2
	default:
		return 1
	}
}

// CapModel returns the lesser of cap and model by tier. If cap is empty,
// model is returned unchanged. Used to enforce the --model ceiling across
// all LLM interactions.
func CapModel(cap, model string) string {
	if cap == "" {
		return model
	}
	if modelTier(cap) < modelTier(model) {
		return cap
	}
	return model
}

// QueryFunc runs a prompt through an agent and returns the response text.
// This is injected by the orchestrator so LLM verification goes through
// the centralized agent module rather than directly spawning processes.
type QueryFunc func(ctx context.Context, workDir, prompt, model string) (string, error)

// Result describes the outcome of a post-signal verification.
type Result struct {
	Passed  bool
	NoDiff  bool // true when verification passed because no diff was found
	Reason  string
	Details string
}

// TestCommand holds the detected test runner for a project.
type TestCommand struct {
	Cmd  string
	Args []string
}

// DetectTestCommand looks for an explicit test:verify script in the project.
// Projects must declare what "verified" means — the loop does not guess.
// Returns nil if no test:verify script is found; callers should refuse to
// start the loop without a verify command.
func DetectTestCommand(dir string) *TestCommand {
	if fileExists(filepath.Join(dir, "package.json")) {
		if hasNPMScript(dir, "test:verify") {
			return &TestCommand{Cmd: "npm", Args: []string{"run", "test:verify"}}
		}
	}

	if hasMakeTarget(dir, "test-verify") {
		return &TestCommand{Cmd: "make", Args: []string{"test-verify"}}
	}

	return nil
}

// RunTests executes the detected test command and returns the result.
// Returns a failure if no test:verify command is detected — the loop
// should have caught this at startup, but we fail safe here too.
func RunTests(ctx context.Context, dir string) Result {
	tc := DetectTestCommand(dir)
	if tc == nil {
		return Result{Passed: false, Reason: "no test:verify script found — add a \"test:verify\" script to package.json"}
	}

	ctx, cancel := context.WithTimeout(ctx, TestTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, tc.Cmd, tc.Args...)
	cmd.Dir = dir
	cmd.WaitDelay = 3 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := string(out)
		tail := lastNLines(output, 30)
		reason := fmt.Sprintf("test suite failed: %v", err)
		if ctx.Err() == context.DeadlineExceeded {
			reason = fmt.Sprintf(
				"test suite timed out after %s — a test may be hanging. Do not run the full suite; run individual test files to isolate",
				TestTimeout.Truncate(time.Second),
			)
		}
		return Result{
			Passed:  false,
			Reason:  reason,
			Details: tail,
		}
	}

	return Result{Passed: true, Reason: "tests passed"}
}

// CheckCommits returns a Result indicating whether HEAD moved since the
// given baseline revision. A signal with no new commits is suspicious.
func CheckCommits(gq GitQuerier, headBefore string) Result {
	if headBefore == "" {
		return Result{Passed: true, Reason: "no baseline to compare"}
	}

	headAfter := gq.HeadRev()
	if headAfter == "" {
		return Result{Passed: true, Reason: "could not read HEAD"}
	}

	if headBefore == headAfter {
		return Result{
			Passed: false,
			Reason: "no new commits — task signaled completion without code changes",
		}
	}

	return Result{Passed: true, Reason: "new commits detected"}
}

func hasMakeTarget(dir, target string) bool {
	if !fileExists(filepath.Join(dir, "Makefile")) {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, "Makefile"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, target+":") {
			return true
		}
	}
	return false
}

func hasNPMScript(dir, script string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), fmt.Sprintf(`"%s"`, script))
}

func hasGoMod(dir string) bool {
	for d := dir; ; d = filepath.Dir(d) {
		if fileExists(filepath.Join(d, "go.mod")) {
			return true
		}
		if d == filepath.Dir(d) {
			break
		}
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// PreflightResult describes the outcome of pre-verification checks.
type PreflightResult struct {
	FilesChanged bool
	HasCommits   bool
	BeadOpen     bool // true if bead is still in_progress (not prematurely closed)
}

// PreflightChecks runs lightweight shell checks before the full test suite.
// These are cheap and catch obvious failures: no files changed, no commits,
// or bead prematurely closed by the agent.
func PreflightChecks(gq GitQuerier, headBefore string, beadStatus string) PreflightResult {
	return PreflightResult{
		FilesChanged: gq.DiffStatRange(headBefore, "HEAD") != "",
		HasCommits:   gq.LogOneline(headBefore, "HEAD") != "",
		BeadOpen:     beadStatus == "in_progress",
	}
}

// VerifyOpts holds the parameters for LLMVerifyPR.
type VerifyOpts struct {
	Ctx             context.Context
	Git             GitQuerier
	WorkDir         string
	PromptsDir      string
	TaskID          string
	HeadBefore      string
	BeadTitle       string
	BeadDescription string
	BeadAcceptance  string
	PRDiff          string // pre-fetched PR diff; empty falls back to iteration diff
	QueryFn         QueryFunc
	Model           string
}

// LLMVerifyPR verifies that a task's acceptance criteria are satisfied.
// Prefers the PR diff (which covers work from prior iterations) over the
// current iteration's diff. Falls back to iteration diff when no PR exists.
// Uses prompts/verify-review.md as the review template when available.
// When QueryFn is non-nil, LLM calls go through the centralized agent module.
func LLMVerifyPR(opts VerifyOpts) Result {
	diff := opts.PRDiff
	source := "PR"
	if diff == "" {
		diff = opts.Git.DiffFull(opts.HeadBefore, "HEAD")
		if diff == "" {
			return Result{Passed: true, NoDiff: true, Reason: "no PR found and no new commits — agent confirms task complete"}
		}
		source = "iteration"
	}

	if len(diff) > 100000 {
		diff = diff[:100000] + "\n\n[diff truncated at 100000 chars]"
	}

	prompt := loadReviewPrompt(opts.PromptsDir, opts.BeadTitle, opts.BeadDescription, opts.BeadAcceptance, source, diff)
	var model []string
	if opts.Model != "" {
		model = []string{opts.Model}
	}
	return callLLM(opts.Ctx, opts.WorkDir, prompt, opts.QueryFn, model...)
}

func loadReviewPrompt(promptsDir, beadTitle, beadDescription, beadAcceptance, source, diff string) string {
	tmplPath := filepath.Join(promptsDir, "verify-review.md")
	data, err := os.ReadFile(tmplPath)
	if err == nil {
		prompt := string(data)
		prompt = strings.ReplaceAll(prompt, "{{TASK_TITLE}}", beadTitle)
		prompt = strings.ReplaceAll(prompt, "{{TASK_DESCRIPTION}}", beadDescription)
		prompt = strings.ReplaceAll(prompt, "{{ACCEPTANCE_CRITERIA}}", beadAcceptance)
		prompt = strings.ReplaceAll(prompt, "{{DIFF_SOURCE}}", source)
		prompt = strings.ReplaceAll(prompt, "{{DIFF}}", diff)
		return prompt
	}

	// Fallback if template not found.
	return fmt.Sprintf(`You are a code reviewer verifying that a diff matches its task description.

TASK: %s
DESCRIPTION: %s

%s DIFF:
%s

Answer these questions:
1. Does this diff implement what the task asks for?
2. For code changes: do the test changes (if any) actually prove the functionality, or are they superficial?
3. For code changes: are error paths and edge cases handled, or does the code fail silently?

Some tasks are implemented through prompt or configuration changes (markdown files, .md templates) rather than traditional code. For these changes, only question 1 applies — do not reject for missing tests or error handling.

Reply with exactly one line: YES or NO followed by a one-sentence reason.`, beadTitle, beadDescription, source, diff)
}


// callLLM sends a prompt to a Claude model and interprets YES/NO response.
// When queryFn is non-nil, the call goes through the centralized agent module.
// Falls back to direct exec.Command when queryFn is nil (tests, standalone use).
func callLLM(ctx context.Context, workDir, prompt string, queryFn QueryFunc, model ...string) Result {
	m := ModelSonnet
	if len(model) > 0 && model[0] != "" {
		m = model[0]
	}

	var response string
	var err error
	if queryFn != nil {
		response, err = queryFn(ctx, workDir, prompt, m)
	} else {
		cmd := exec.CommandContext(ctx, "claude", "--print", "--model", m, "-p", prompt)
		cmd.Dir = workDir
		out, cmdErr := cmd.CombinedOutput()
		response = string(out)
		err = cmdErr
	}
	if err != nil {
		return Result{Passed: true, Reason: "LLM verification skipped: " + err.Error()}
	}

	response = strings.TrimSpace(response)
	if strings.HasPrefix(strings.ToUpper(response), "NO") {
		return Result{
			Passed:  false,
			Reason:  "LLM verification rejected: " + response,
			Details: response,
		}
	}

	return Result{Passed: true, Reason: "LLM verified: " + response}
}

// CompileCheckTimeout is the maximum duration CompileCheck will wait for the
// compilation to complete. Exported so tests can override it.
var CompileCheckTimeout = 60 * time.Second

// CompileCheck verifies that all packages (including test files) compile
// without running any tests. Checks Go projects via go test -run=^$ and
// TypeScript projects via tsc --noEmit (or npm run typecheck when available).
// Both checks run when a project contains both go.mod and tsconfig.json.
func CompileCheck(ctx context.Context, dir string) Result {
	ctx, cancel := context.WithTimeout(ctx, CompileCheckTimeout)
	defer cancel()

	foundAny := false

	goDir := findGoModDir(dir)
	if goDir != "" {
		foundAny = true
		cmd := exec.CommandContext(ctx, "go", "test", "-run=^$", "-count=1", "./...")
		cmd.Dir = goDir
		cmd.WaitDelay = 3 * time.Second
		out, err := cmd.CombinedOutput()
		if err != nil {
			return Result{
				Passed:  false,
				Reason:  "pre-push compile check failed: not all packages compile",
				Details: lastNLines(filterFailures(string(out)), 30),
			}
		}
	}

	if fileExists(filepath.Join(dir, "tsconfig.json")) {
		foundAny = true
		var cmd *exec.Cmd
		if hasNPMScript(dir, "typecheck") {
			cmd = exec.CommandContext(ctx, "npm", "run", "typecheck")
		} else {
			cmd = exec.CommandContext(ctx, "tsc", "--noEmit")
		}
		cmd.Dir = dir
		cmd.WaitDelay = 3 * time.Second
		out, err := cmd.CombinedOutput()
		if err != nil {
			return Result{
				Passed:  false,
				Reason:  "pre-push TypeScript compile check failed",
				Details: lastNLines(string(out), 30),
			}
		}
	}

	if !foundAny {
		return Result{Passed: true, Reason: "no Go module or TypeScript project found — skipping compile check"}
	}
	return Result{Passed: true, Reason: "all packages compile"}
}

// findGoModDir locates the directory containing go.mod by checking the given
// directory, common subdirectories (e.g. go/), then walking up the tree.
func findGoModDir(dir string) string {
	if fileExists(filepath.Join(dir, "go.mod")) {
		return dir
	}
	goSubdir := filepath.Join(dir, "go")
	if fileExists(filepath.Join(goSubdir, "go.mod")) {
		return goSubdir
	}
	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		if fileExists(filepath.Join(d, "go.mod")) {
			return d
		}
		if d == filepath.Dir(d) {
			break
		}
	}
	return ""
}

func filterFailures(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "ok \t") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func lastNLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
