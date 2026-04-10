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
	Passed        bool
	NoDiff        bool // true when verification passed because no diff was found
	ScriptMissing bool // true when no ralph:verify script was found — not a test failure
	Reason        string
	Details       string
	Command       string // the command that was run (e.g. "npm run ralph:verify")
	Dir           string // the directory the command ran in
}

// TestCommand holds the detected test runner for a project.
type TestCommand struct {
	Cmd  string
	Args []string
	Dir  string // the directory where the command should be run
}

// detectScript checks each directory in order and returns a TestCommand for
// the first directory that contains npmScript in package.json or makeTarget
// in a Makefile. Empty directories are skipped. Returns nil if nothing found.
func detectScript(npmScript, makeTarget string, dirs ...string) *TestCommand {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if fileExists(filepath.Join(dir, "package.json")) && hasNPMScript(dir, npmScript) {
			return &TestCommand{Cmd: "npm", Args: []string{"run", npmScript}, Dir: dir}
		}
		if hasMakeTarget(dir, makeTarget) {
			return &TestCommand{Cmd: "make", Args: []string{makeTarget}, Dir: dir}
		}
	}
	return nil
}

// DetectTestCommand looks for an explicit ralph:verify script in the project.
// Projects must declare what "verified" means — the loop does not guess.
// Accepts multiple directories checked in order; returns the first match.
// Returns nil if no ralph:verify script is found; callers should refuse to
// start the loop without a verify command.
func DetectTestCommand(dirs ...string) *TestCommand {
	return detectScript("ralph:verify", "ralph-verify", dirs...)
}

// DetectPostTaskCommand looks for a ralph:post-task script in the project,
// falling back to the CLI --post-task value. Accepts multiple directories
// checked in order (typically worktree first, project root second).
// Returns empty string when neither is configured (post-task is optional).
func DetectPostTaskCommand(cliPostTask string, dirs ...string) string {
	tc := detectScript("ralph:post-task", "ralph-post-task", dirs...)
	if tc != nil {
		return tc.Cmd + " " + strings.Join(tc.Args, " ")
	}
	return cliPostTask
}

// DetectPostTask returns the TestCommand for the detected ralph:post-task
// script, including which directory it was found in. Returns nil if not found
// in any of the given directories. Does not consider the CLI fallback.
func DetectPostTask(dirs ...string) *TestCommand {
	return detectScript("ralph:post-task", "ralph-post-task", dirs...)
}

// RunTests executes the detected test command and returns the result.
// Accepts multiple directories checked in order — the command runs in the
// first directory where ralph:verify is found. Returns a failure if no
// ralph:verify command is detected.
func RunTests(ctx context.Context, timeout time.Duration, dirs ...string) Result {
	tc := DetectTestCommand(dirs...)
	if tc == nil {
		return Result{Passed: false, ScriptMissing: true, Reason: "no ralph:verify script found — add a \"ralph:verify\" script to package.json"}
	}

	command := tc.Cmd + " " + strings.Join(tc.Args, " ")

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, tc.Cmd, tc.Args...)
	cmd.Dir = tc.Dir
	cmd.WaitDelay = 3 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := string(out)
		tail := lastNLines(output, 30)
		reason := fmt.Sprintf("test suite failed: %v", err)
		if ctx.Err() == context.DeadlineExceeded {
			reason = fmt.Sprintf(
				"test suite timed out after %s — a test may be hanging. Do not run the full suite; run individual test files to isolate",
				timeout.Truncate(time.Second),
			)
		}
		return Result{
			Passed:  false,
			Reason:  reason,
			Details: tail,
			Command: command,
			Dir:     tc.Dir,
		}
	}

	return Result{Passed: true, Reason: "tests passed", Command: command, Dir: tc.Dir}
}

// CheckCommits returns a Result indicating whether HEAD moved since the
// given baseline revision. A signal with no new commits is suspicious.
// The caller pre-fetches headAfter so this function operates on data only.
func CheckCommits(headBefore, headAfter string) Result {
	if headBefore == "" {
		return Result{Passed: true, Reason: "no baseline to compare"}
	}
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

// VerifyOpts holds the parameters for LLMVerifyPR. The caller pre-fetches
// the diff (PR diff or iteration diff) and passes it as data — verify never
// reaches into git itself.
type VerifyOpts struct {
	Ctx             context.Context
	WorkDir         string
	PromptsDir      string
	TaskID          string
	BeadTitle       string
	BeadDescription string
	BeadAcceptance  string
	Diff            string // pre-fetched diff (PR diff preferred, fall back to iteration)
	DiffSource      string // human label for the diff origin: "PR" or "iteration"
	QueryFn         QueryFunc
	Model           string
}

// LLMVerifyPR verifies that a task's acceptance criteria are satisfied.
// The caller is responsible for choosing between PR diff and iteration diff
// and passing it via opts.Diff. An empty Diff returns NoDiff=true.
// Uses prompts/verify-review.md as the review template when available.
// When QueryFn is non-nil, LLM calls go through the centralized agent module.
func LLMVerifyPR(opts VerifyOpts) Result {
	diff := opts.Diff
	if diff == "" {
		return Result{Passed: true, NoDiff: true, Reason: "no PR found and no new commits — agent confirms task complete"}
	}

	if len(diff) > 100000 {
		diff = diff[:100000] + "\n\n[diff truncated at 100000 chars]"
	}

	source := opts.DiffSource
	if source == "" {
		source = "PR"
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

// CompileCheck verifies that all packages (including test files) compile
// without running any tests. Checks Go projects via go test -run=^$ and
// TypeScript projects via tsc --noEmit (or npm run typecheck when available).
// Both checks run when a project contains both go.mod and tsconfig.json.
func CompileCheck(ctx context.Context, timeout time.Duration, dir string) Result {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	foundAny := false
	var commands []string

	goDir := findGoModDir(dir)
	if goDir != "" {
		foundAny = true
		commands = append(commands, "go test -run=^$ ./...")
		cmd := exec.CommandContext(ctx, "go", "test", "-run=^$", "-count=1", "./...")
		cmd.Dir = goDir
		cmd.WaitDelay = 3 * time.Second
		out, err := cmd.CombinedOutput()
		if err != nil {
			return Result{
				Passed:  false,
				Reason:  "pre-push compile check failed: not all packages compile",
				Details: lastNLines(filterFailures(string(out)), 30),
				Command: strings.Join(commands, " + "),
				Dir:     dir,
			}
		}
	}

	if fileExists(filepath.Join(dir, "tsconfig.json")) {
		foundAny = true
		var tsCmd string
		var cmd *exec.Cmd
		if hasNPMScript(dir, "typecheck") {
			tsCmd = "npm run typecheck"
			cmd = exec.CommandContext(ctx, "npm", "run", "typecheck")
		} else {
			tsCmd = "tsc --noEmit"
			cmd = exec.CommandContext(ctx, "tsc", "--noEmit")
		}
		commands = append(commands, tsCmd)
		cmd.Dir = dir
		cmd.WaitDelay = 3 * time.Second
		out, err := cmd.CombinedOutput()
		if err != nil {
			return Result{
				Passed:  false,
				Reason:  "pre-push TypeScript compile check failed",
				Details: lastNLines(string(out), 30),
				Command: strings.Join(commands, " + "),
				Dir:     dir,
			}
		}
	}

	if !foundAny {
		return Result{Passed: true, Reason: "no Go module or TypeScript project found — skipping compile check"}
	}
	return Result{Passed: true, Reason: "all packages compile", Command: strings.Join(commands, " + "), Dir: dir}
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
