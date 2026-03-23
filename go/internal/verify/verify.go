package verify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/git"
)

// Result describes the outcome of a post-signal verification.
type Result struct {
	Passed  bool
	Reason  string
	Details string
}

// TestCommand holds the detected test runner for a project.
type TestCommand struct {
	Cmd  string
	Args []string
}

// DetectTestCommand inspects the project directory for common test runners
// and returns the command to execute. Returns nil if no test runner is found.
func DetectTestCommand(dir string) *TestCommand {
	if hasMakeTarget(dir, "test") {
		return &TestCommand{Cmd: "make", Args: []string{"test"}}
	}

	if fileExists(filepath.Join(dir, "package.json")) {
		if hasNPMScript(dir, "test") {
			return &TestCommand{Cmd: "npm", Args: []string{"test"}}
		}
	}

	if fileExists(filepath.Join(dir, "Cargo.toml")) {
		return &TestCommand{Cmd: "cargo", Args: []string{"test"}}
	}

	if hasGoMod(dir) {
		return &TestCommand{Cmd: "go", Args: []string{"test", "-count=1", "./..."}}
	}

	if fileExists(filepath.Join(dir, "pyproject.toml")) || fileExists(filepath.Join(dir, "setup.py")) {
		return &TestCommand{Cmd: "python", Args: []string{"-m", "pytest"}}
	}

	return nil
}

// RunTests executes the detected test command and returns the result.
// If no test command is detected, verification passes (we can't block
// on projects that don't have tests).
func RunTests(ctx context.Context, dir string) Result {
	tc := DetectTestCommand(dir)
	if tc == nil {
		return Result{Passed: true, Reason: "no test runner detected"}
	}

	cmd := exec.CommandContext(ctx, tc.Cmd, tc.Args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := string(out)
		tail := lastNLines(output, 30)
		return Result{
			Passed:  false,
			Reason:  fmt.Sprintf("test suite failed: %v", err),
			Details: tail,
		}
	}

	return Result{Passed: true, Reason: "tests passed"}
}

// CheckCommits returns a Result indicating whether HEAD moved since the
// given baseline revision. A signal with no new commits is suspicious.
func CheckCommits(dir, headBefore string) Result {
	if headBefore == "" {
		return Result{Passed: true, Reason: "no baseline to compare"}
	}

	headAfter := git.HeadRev(dir)
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
func PreflightChecks(workDir, headBefore string, beadStatus string) PreflightResult {
	return PreflightResult{
		FilesChanged: git.DiffStatRange(workDir, headBefore, "HEAD") != "",
		HasCommits:   git.LogOneline(workDir, headBefore, "HEAD") != "",
		BeadOpen:     beadStatus == "in_progress",
	}
}


// LLMVerifyPR verifies that a task's acceptance criteria are satisfied.
// Prefers the PR diff (which covers work from prior iterations) over the
// current iteration's diff. Falls back to iteration diff when no PR exists.
// Uses prompts/verify-review.md as the review template when available.
func LLMVerifyPR(ctx context.Context, workDir, promptsDir, taskID, headBefore, beadTitle, beadDescription string, gh git.GitHub, model ...string) Result {
	diff := getPRDiff(ctx, workDir, taskID, gh)
	source := "PR"
	if diff == "" {
		diff = git.DiffFull(workDir, headBefore, "HEAD")
		if diff == "" {
			return Result{Passed: true, Reason: "no PR found and no new commits — agent confirms task complete"}
		}
		source = "iteration"
	}

	if len(diff) > 100000 {
		diff = diff[:100000] + "\n\n[diff truncated at 100000 chars]"
	}

	prompt := loadReviewPrompt(promptsDir, beadTitle, beadDescription, source, diff)
	return callLLM(ctx, workDir, prompt, model...)
}

func loadReviewPrompt(promptsDir, beadTitle, beadDescription, source, diff string) string {
	tmplPath := filepath.Join(promptsDir, "verify-review.md")
	data, err := os.ReadFile(tmplPath)
	if err == nil {
		prompt := string(data)
		prompt = strings.ReplaceAll(prompt, "{{TASK_TITLE}}", beadTitle)
		prompt = strings.ReplaceAll(prompt, "{{TASK_DESCRIPTION}}", beadDescription)
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

// getPRDiff finds a PR matching the task ID and returns its diff.
func getPRDiff(_ context.Context, workDir, taskID string, gh git.GitHub) string {
	if gh == nil {
		return ""
	}
	prNumber, err := gh.SearchPR(workDir, taskID)
	if err != nil || prNumber == "" {
		return ""
	}
	diff, err := gh.PRDiff(workDir, prNumber)
	if err != nil {
		return ""
	}
	return diff
}

// callLLM sends a prompt to a Claude model and interprets YES/NO response.
func callLLM(ctx context.Context, workDir, prompt string, model ...string) Result {
	m := "claude-haiku-4-5-20251001"
	if len(model) > 0 && model[0] != "" {
		m = model[0]
	}
	cmd := exec.CommandContext(ctx, "claude", "--print", "--model", m, "-p", prompt)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Result{Passed: true, Reason: "LLM verification skipped: " + err.Error()}
	}

	response := strings.TrimSpace(string(out))
	if strings.HasPrefix(strings.ToUpper(response), "NO") {
		return Result{
			Passed:  false,
			Reason:  "LLM verification rejected: " + response,
			Details: response,
		}
	}

	return Result{Passed: true, Reason: "LLM verified: " + response}
}


func lastNLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
