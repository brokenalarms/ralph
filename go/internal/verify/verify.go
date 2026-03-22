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

// CheckChanges returns a Result indicating whether the working tree has
// any modifications, staged changes, or untracked files. Used when the
// orchestrator owns commits — the agent leaves changes uncommitted.
func CheckChanges(dir string) Result {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return Result{Passed: true, Reason: "could not check working tree"}
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return Result{
			Passed: false,
			Reason: "no changes — task signaled completion without code changes",
		}
	}
	return Result{Passed: true, Reason: "working tree has changes"}
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

// LLMVerifyDiff calls Claude Haiku to check if the diff matches the bead's
// acceptance criteria and that tests prove the functionality. Returns a Result.
// Uses claude CLI with --model haiku for speed/cost.
func LLMVerifyDiff(workDir, headBefore, beadTitle, beadDescription string) Result {
	// Get the diff — try committed changes first, fall back to working tree.
	var diff string
	headAfter := gitHeadRev(workDir)
	if headBefore != "" && headAfter != "" && headBefore != headAfter {
		diffCmd := exec.Command("git", "diff", headBefore+"..HEAD")
		diffCmd.Dir = workDir
		diffOut, _ := diffCmd.Output()
		diff = string(diffOut)
	}
	if diff == "" {
		// No committed changes — agent left work uncommitted.
		// Stage temporarily to capture full diff including new files.
		stageCmd := exec.Command("git", "add", "-A")
		stageCmd.Dir = workDir
		_ = stageCmd.Run()
		baseline := headBefore
		if baseline == "" {
			baseline = "HEAD"
		}
		diffCmd := exec.Command("git", "diff", "--cached", baseline)
		diffCmd.Dir = workDir
		diffOut, _ := diffCmd.Output()
		diff = string(diffOut)
		// Unstage so the working tree stays clean for subsequent operations.
		unstageCmd := exec.Command("git", "reset", "HEAD")
		unstageCmd.Dir = workDir
		_ = unstageCmd.Run()
	}
	if len(diff) == 0 {
		return Result{Passed: true, Reason: "no diff to verify"}
	}
	// Truncate large diffs to avoid token limits
	if len(diff) > 20000 {
		diff = diff[:20000] + "\n\n[diff truncated at 20000 chars]"
	}

	prompt := fmt.Sprintf(`You are a code reviewer verifying that a diff matches its task description.

TASK: %s
DESCRIPTION: %s

DIFF:
%s

Answer these two questions:
1. Does this diff implement what the task asks for?
2. Do the test changes (if any) actually prove the functionality, or are they superficial (e.g. assert true, always-pass stubs)?

Reply with exactly one line: YES or NO followed by a one-sentence reason.
Example: YES — adds retry loop with test that verifies 3 retries on failure.
Example: NO — tests use a stub that always passes, proving nothing.`, beadTitle, beadDescription, diff)

	cmd := exec.Command("claude", "--print", "--model", "claude-haiku-4-5-20251001", "-p", prompt)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// If claude CLI fails, don't block — pass with warning
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
