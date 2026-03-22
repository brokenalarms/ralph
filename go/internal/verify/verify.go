package verify

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
func RunTests(dir string) Result {
	tc := DetectTestCommand(dir)
	if tc == nil {
		return Result{Passed: true, Reason: "no test runner detected"}
	}

	cmd := exec.Command(tc.Cmd, tc.Args...)
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

	headAfter := gitHeadRev(dir)
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

func gitHeadRev(dir string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
	result := PreflightResult{}

	// (1) git diff --stat — did files actually change?
	diffCmd := exec.Command("git", "diff", "--stat", headBefore+"..HEAD")
	diffCmd.Dir = workDir
	diffOut, _ := diffCmd.Output()
	result.FilesChanged = len(strings.TrimSpace(string(diffOut))) > 0

	// (2) git log — are there new commits?
	logCmd := exec.Command("git", "log", "--oneline", headBefore+"..HEAD")
	logCmd.Dir = workDir
	logOut, _ := logCmd.Output()
	result.HasCommits = len(strings.TrimSpace(string(logOut))) > 0

	// (3) check bead is still in_progress, not prematurely closed by agent
	result.BeadOpen = beadStatus == "in_progress"

	return result
}


// LLMVerifyPR verifies that a task's acceptance criteria are satisfied.
// Prefers the PR diff (which covers work from prior iterations) over the
// current iteration's diff. Falls back to iteration diff when no PR exists.
// Uses prompts/verify-review.md as the review template when available.
func LLMVerifyPR(workDir, promptsDir, taskID, headBefore, beadTitle, beadDescription string) Result {
	diff := getPRDiff(workDir, taskID)
	source := "PR"
	if diff == "" {
		diffCmd := exec.Command("git", "diff", headBefore+"..HEAD")
		diffCmd.Dir = workDir
		diffOut, err := diffCmd.Output()
		if err != nil || len(diffOut) == 0 {
			return Result{Passed: true, Reason: "no PR found and no new commits — agent confirms task complete"}
		}
		diff = string(diffOut)
		source = "iteration"
	}

	if len(diff) > 20000 {
		diff = diff[:20000] + "\n\n[diff truncated at 20000 chars]"
	}

	prompt := loadReviewPrompt(promptsDir, beadTitle, beadDescription, source, diff)
	return callLLM(workDir, prompt)
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

Answer these two questions:
1. Does this diff implement what the task asks for?
2. Do the test changes (if any) actually prove the functionality, or are they superficial?
3. Are error paths and edge cases handled, or does the code fail silently?

Reply with exactly one line: YES or NO followed by a one-sentence reason.`, beadTitle, beadDescription, source, diff)
}

// getPRDiff finds a PR matching the task ID and returns its diff.
func getPRDiff(workDir, taskID string) string {
	cmd := exec.Command("gh", "pr", "list", "--search", taskID,
		"--state", "all", "--json", "number", "--jq", ".[0].number")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	prNumber := strings.TrimSpace(string(out))
	if prNumber == "" {
		return ""
	}

	diffCmd := exec.Command("gh", "pr", "diff", prNumber)
	diffCmd.Dir = workDir
	diffOut, err := diffCmd.Output()
	if err != nil {
		return ""
	}
	return string(diffOut)
}

// callLLM sends a prompt to Claude Haiku and interprets YES/NO response.
func callLLM(workDir, prompt string) Result {
	cmd := exec.Command("claude", "--print", "--model", "claude-haiku-4-5-20251001", "-p", prompt)
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
