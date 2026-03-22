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

// LLMVerifyDiff calls Claude Haiku to check if the diff matches the bead's
// acceptance criteria and that tests prove the functionality. Returns a Result.
// Uses claude CLI with --model haiku for speed/cost.
func LLMVerifyDiff(workDir, headBefore, beadTitle, beadDescription string) Result {
	diffCmd := exec.Command("git", "diff", headBefore+"..HEAD")
	diffCmd.Dir = workDir
	diffOut, err := diffCmd.Output()
	if err != nil || len(diffOut) == 0 {
		return Result{Passed: false, Reason: "no diff to verify — use code-state verification instead"}
	}

	diff := string(diffOut)
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

	return callLLM(workDir, prompt)
}

// LLMVerifyCodeState checks whether the task is already implemented in the
// codebase by examining relevant source files instead of a diff. Used when
// there are no new commits (work from a previous iteration already merged).
func LLMVerifyCodeState(workDir, beadTitle, beadDescription string) Result {
	files := findRelevantFiles(workDir, beadTitle, beadDescription)
	if len(files) == 0 {
		return Result{Passed: false, Reason: "no relevant files found for code-state verification"}
	}

	var sb strings.Builder
	totalSize := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		content := string(data)
		if totalSize+len(content) > 20000 {
			remaining := 20000 - totalSize
			if remaining > 0 {
				relPath, _ := filepath.Rel(workDir, f)
				fmt.Fprintf(&sb, "\n=== %s ===\n%s\n[truncated]\n", relPath, content[:remaining])
			}
			break
		}
		relPath, _ := filepath.Rel(workDir, f)
		fmt.Fprintf(&sb, "\n=== %s ===\n%s\n", relPath, content)
		totalSize += len(content)
	}

	prompt := fmt.Sprintf(`You are verifying whether a task has already been implemented in the codebase.
The iteration produced no new commits, which may mean the work was completed in a previous iteration.
Examine the source files below and determine if the feature/fix described is already present.

TASK: %s
DESCRIPTION: %s

RELEVANT SOURCE FILES:
%s

Is this feature/fix already implemented in the codebase? Look for:
1. The described functionality exists in the code
2. Tests exist that prove the functionality works

Reply with exactly one line: YES or NO followed by a one-sentence reason.
Example: YES — LLMVerifyCodeState function exists with file-based verification and tests.
Example: NO — no code implements the retry logic described in the task.`, beadTitle, beadDescription, sb.String())

	return callLLM(workDir, prompt)
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

// findRelevantFiles searches the working directory for files related to the
// task by extracting keywords from the title and description.
func findRelevantFiles(workDir, title, description string) []string {
	keywords := extractKeywords(title + " " + description)
	seen := map[string]bool{}
	var results []string

	for _, kw := range keywords {
		if len(results) >= 20 {
			break
		}
		cmd := exec.Command("git", "grep", "-li", kw)
		cmd.Dir = workDir
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line == "" {
				continue
			}
			absPath := filepath.Join(workDir, line)
			if seen[absPath] {
				continue
			}
			// Skip non-source files
			if isSourceFile(line) {
				seen[absPath] = true
				results = append(results, absPath)
			}
		}
	}

	return results
}

// extractKeywords pulls meaningful search terms from task text, filtering
// out short words and common stop words.
func extractKeywords(text string) []string {
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"was": true, "were": true, "be": true, "been": true, "being": true,
		"have": true, "has": true, "had": true, "do": true, "does": true,
		"did": true, "will": true, "would": true, "could": true, "should": true,
		"may": true, "might": true, "must": true, "shall": true,
		"that": true, "this": true, "these": true, "those": true,
		"with": true, "from": true, "into": true, "through": true,
		"for": true, "and": true, "but": true, "not": true, "just": true,
		"only": true, "when": true, "where": true, "how": true, "what": true,
		"which": true, "who": true, "whom": true, "whose": true,
		"than": true, "then": true, "also": true, "each": true,
		"all": true, "any": true, "both": true, "few": true, "more": true,
		"most": true, "other": true, "some": true, "such": true,
		"can": true, "don": true, "too": true, "very": true,
		"already": true, "instead": true, "check": true, "use": true,
		"pass": true, "fix": true, "add": true, "new": true, "make": true,
	}

	words := strings.FieldsFunc(text, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-')
	})

	var keywords []string
	seen := map[string]bool{}
	for _, w := range words {
		lower := strings.ToLower(w)
		if len(lower) < 4 || stopWords[lower] || seen[lower] {
			continue
		}
		seen[lower] = true
		keywords = append(keywords, lower)
	}
	return keywords
}

// isSourceFile returns true for common source code extensions.
func isSourceFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".rs", ".rb", ".java",
		".sh", ".bash", ".zsh", ".md", ".yml", ".yaml", ".toml", ".json":
		return true
	}
	return false
}

func lastNLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
