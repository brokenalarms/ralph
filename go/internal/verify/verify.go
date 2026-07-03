package verify

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/logging"
)

// killProcessGroup configures cmd to run in its own process group and sets
// Cancel to kill the entire group (not just the direct child). This ensures
// grandchild processes spawned by npm/make are cleaned up on timeout.
func killProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
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
	return config.ModelShortName(model)
}

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
// Script detection runs first; configVerify is a fallback used only when no
// ralph:verify script is found in package.json or Makefile.
// Accepts multiple directories checked in order; returns the first match.
// Returns nil if neither a ralph:verify script nor configVerify is present;
// callers should refuse to start the loop without a verify command.
func DetectTestCommand(configVerify string, dirs ...string) *TestCommand {
	if tc := detectScript("ralph:verify", "ralph-verify", dirs...); tc != nil {
		return tc
	}
	if configVerify != "" {
		parts := strings.Fields(configVerify)
		cmd := parts[0]
		args := parts[1:]
		dir := "."
		for _, d := range dirs {
			if d != "" {
				dir = d
				break
			}
		}
		return &TestCommand{Cmd: cmd, Args: args, Dir: dir}
	}
	return nil
}

// DetectPostTaskCommand returns the post-task command to run. Uses configPostTask
// (from config.toml or CLI) first; falls back to detecting a ralph:post-task
// script in the given directories. Returns empty string when nothing is configured.
func DetectPostTaskCommand(configPostTask string, dirs ...string) string {
	if configPostTask != "" {
		return configPostTask
	}
	tc := detectScript("ralph:post-task", "ralph-post-task", dirs...)
	if tc != nil {
		return tc.Cmd + " " + strings.Join(tc.Args, " ")
	}
	return ""
}

// DetectPostTask returns the TestCommand for the detected ralph:post-task
// script, including which directory it was found in. Returns nil if not found
// in any of the given directories. Does not consider the CLI fallback.
func DetectPostTask(dirs ...string) *TestCommand {
	return detectScript("ralph:post-task", "ralph-post-task", dirs...)
}

// DetectVerifyBuild returns the TestCommand for the detected ralph:verify-build
// script, including which directory it was found in. Returns nil if not found
// in any of the given directories. Does not consider the config.toml value.
func DetectVerifyBuild(dirs ...string) *TestCommand {
	return detectScript("ralph:verify-build", "ralph-verify-build", dirs...)
}

// RunTests executes the detected test command and returns the result.
// When configVerify is non-empty it is used as the command instead of detecting
// ralph:verify scripts. Accepts multiple directories checked in order — the command
// runs in the first directory where ralph:verify is found. Returns a failure if
// no ralph:verify command is detected.
func RunTests(ctx context.Context, timeout time.Duration, configVerify string, dirs ...string) Result {
	tc := DetectTestCommand(configVerify, dirs...)
	if tc == nil {
		return Result{Passed: false, ScriptMissing: true, Reason: "no ralph:verify script found — add a \"ralph:verify\" script to package.json"}
	}

	command := strings.TrimRight(tc.Cmd+" "+strings.Join(tc.Args, " "), " ")

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, tc.Cmd, tc.Args...)
	cmd.Dir = tc.Dir
	cmd.WaitDelay = 3 * time.Second
	killProcessGroup(cmd)

	tracker := newTestTracker()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout // merge stderr into stdout pipe

	if startErr := cmd.Start(); startErr != nil {
		return Result{
			Passed: false,
			Reason: fmt.Sprintf("failed to start test command: %v", startErr),
			Command: command,
			Dir:     tc.Dir,
		}
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		tracker.observe(scanner.Text())
	}

	err := cmd.Wait()
	if err != nil {
		reason := fmt.Sprintf("test suite failed: %v", err)
		details := lastNLines(tracker.allOutput(), 30)
		if ctx.Err() == context.DeadlineExceeded {
			reason = fmt.Sprintf(
				"test suite timed out after %s — a test may be hanging. Do not run the full suite; run individual test files to isolate",
				timeout.Truncate(time.Second),
			)
			details = tracker.timeoutSummary()
		}
		return Result{
			Passed:  false,
			Reason:  reason,
			Details: details,
			Command: command,
			Dir:     tc.Dir,
		}
	}

	return Result{Passed: true, Reason: "tests passed", Command: command, Dir: tc.Dir}
}

// RunVerifyBuildParams configures RunVerifyBuild.
type RunVerifyBuildParams struct {
	VerifyBuild string
	WorktreeDir string
	ProjectDir  string
	PromptsDir  string
	TestTimeout time.Duration
	Logger      *logging.Logger
}

// RunVerifyBuild executes the verify-build script if configured. Uses the
// VerifyBuild config/CLI value first; falls back to detecting a
// ralph:verify-build npm script or ralph-verify-build Makefile target
// (worktree then project root). Runs in the project directory with a timeout
// matching the test suite timeout. Returns empty string if the script passes
// or is not configured. Returns a build failure message (stdout+stderr) if
// the script exits non-zero.
func RunVerifyBuild(ctx context.Context, p RunVerifyBuildParams) string {
	if ctx.Err() != nil {
		return ""
	}

	var script string
	if p.VerifyBuild != "" {
		script = p.VerifyBuild
		p.Logger.Emit(logging.Opts{Domain: logging.Build}, "Using verify_build config: %s", script)
	} else {
		tc := DetectVerifyBuild(p.WorktreeDir, p.ProjectDir)
		if tc != nil {
			script = tc.Cmd + " " + strings.Join(tc.Args, " ")
			p.Logger.Emit(logging.Opts{Domain: logging.Build}, "Detected ralph:verify-build script: %s (in %s)", script, tc.Dir)
		}
	}
	if script == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, p.TestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = p.ProjectDir
	p.Logger.Emit(logging.Opts{Domain: logging.Build}, "Running verify-build: %s", script)
	out, err := cmd.CombinedOutput()
	if err == nil {
		p.Logger.Emit(logging.Opts{Domain: logging.Build, Level: logging.Success}, "Build health check passed")
		return ""
	}
	output := strings.TrimSpace(string(out))
	p.Logger.Emit(logging.Opts{Domain: logging.Build, Level: logging.Warn}, "Build health check failed: %v", err)
	msg := "\n" + readPromptFragment(p.PromptsDir, "status-build-broken.md")
	if output != "" {
		msg += "\n  Build failure output:\n  " + strings.ReplaceAll(output, "\n", "\n  ")
	}
	return msg
}

// readPromptFragment reads a short agent-facing status fragment from
// promptsDir, trimming the trailing newline. Returns empty string when the
// fragment is missing — a misconfigured promptsDir degrades the message
// rather than failing the build check itself.
func readPromptFragment(promptsDir, name string) string {
	data, err := os.ReadFile(filepath.Join(promptsDir, name))
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(data), "\n")
}

// RunPostTaskParams configures RunPostTask.
type RunPostTaskParams struct {
	PostTask    string
	WorktreeDir string
	ProjectDir  string
	Logger      *logging.Logger
}

// RunPostTask executes the post-task script if configured. Uses the PostTask
// config/CLI value first; falls back to detecting a ralph:post-task npm script
// or ralph-post-task Makefile target (worktree then project root). Runs in the
// worktree directory with RALPH_TASK_ID, RALPH_PR_NUMBER, and RALPH_MERGED env vars.
// Non-zero exit warns and continues.
func RunPostTask(ctx context.Context, p RunPostTaskParams, taskID string, prNumber int, merged bool) {
	var script string
	if p.PostTask != "" {
		script = p.PostTask
		p.Logger.Emit(logging.Opts{Domain: logging.PostTask}, "Using post_task config: %s (in %s)", script, p.WorktreeDir)
	} else {
		tc := DetectPostTask(p.WorktreeDir, p.ProjectDir)
		if tc != nil {
			script = tc.Cmd + " " + strings.Join(tc.Args, " ")
			p.Logger.Emit(logging.Opts{Domain: logging.PostTask}, "Detected ralph:post-task script: %s (in %s)", script, tc.Dir)
		}
	}
	if script == "" {
		p.Logger.Emit(logging.Opts{Domain: logging.PostTask}, "No post_task config and no ralph:post-task npm script or ralph-post-task Makefile target found — skipping post-task")
		return
	}
	prStr := strconv.Itoa(prNumber)
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = p.WorktreeDir
	cmd.Env = append(os.Environ(),
		"RALPH_TASK_ID="+taskID,
		"RALPH_PR_NUMBER="+prStr,
		"RALPH_MERGED="+fmt.Sprintf("%t", merged),
		"RALPH_PROJECT_DIR="+p.ProjectDir,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	p.Logger.Emit(logging.Opts{Domain: logging.PostTask}, "Running %s (task=%s pr=%d merged=%t)", script, taskID, prNumber, merged)
	if err := cmd.Run(); err != nil {
		p.Logger.Emit(logging.Opts{Domain: logging.PostTask, Level: logging.Warn}, "Script exited with error: %v", err)
	}
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

// ReviewPromptInput is the pure-data input to BuildReviewPrompt. No
// callbacks, no functions, no module references — just the strings needed
// to fill in the verify-review.md template.
type ReviewPromptInput struct {
	PromptsDir  string
	Title       string
	Description string
	Acceptance  string
	Diff        string // pre-fetched diff
	DiffSource  string // "PR" or "iteration"
}

// BuildReviewPrompt loads prompts/verify-review.md from the configured
// promptsDir and substitutes task variables into the template. Pure
// function: data in, string out, no I/O beyond the template read, no LLM
// calls. Returns an error when the template file cannot be read — callers
// treat that as an infrastructure fault, not a review verdict.
//
// Diffs longer than 100000 characters are truncated with a marker.
func BuildReviewPrompt(in ReviewPromptInput) (string, error) {
	diff := in.Diff
	if len(diff) > 100000 {
		diff = diff[:100000] + "\n\n[diff truncated at 100000 chars]"
	}
	source := in.DiffSource
	if source == "" {
		source = "PR"
	}

	tmplPath := filepath.Join(in.PromptsDir, "verify-review.md")
	data, err := os.ReadFile(tmplPath)
	if err != nil {
		return "", fmt.Errorf("verify-review template not found: %s", tmplPath)
	}

	prompt := string(data)
	prompt = strings.ReplaceAll(prompt, "{{TASK_TITLE}}", in.Title)
	prompt = strings.ReplaceAll(prompt, "{{TASK_DESCRIPTION}}", in.Description)
	prompt = strings.ReplaceAll(prompt, "{{ACCEPTANCE_CRITERIA}}", in.Acceptance)
	prompt = strings.ReplaceAll(prompt, "{{DIFF_SOURCE}}", source)
	prompt = strings.ReplaceAll(prompt, "{{DIFF}}", diff)
	return prompt, nil
}

// ParseReviewResponse interprets a YES/NO LLM response and returns a Result.
// Pure function: string in, Result out. The leading "YES" or "NO" token
// (case-insensitive) determines pass/fail; the remainder of the response
// becomes the Reason / Details. Whitespace is trimmed.
func ParseReviewResponse(response string) Result {
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
		killProcessGroup(cmd)
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
		killProcessGroup(cmd)
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

// testTracker observes test output line-by-line, tracking which test was last
// started (=== RUN), last completed (--- PASS/FAIL), and which packages
// passed. When the suite times out, timeoutSummary returns a focused report
// instead of a useless goroutine dump.
type testTracker struct {
	lastStarted   string // last "=== RUN   TestXxx" line
	lastCompleted string // last "--- PASS: TestXxx" or "--- FAIL: TestXxx" line
	passedPkgs    []string
	failedPkgs    []string
	lines         []string
}

func newTestTracker() *testTracker {
	return &testTracker{}
}

func (t *testTracker) observe(line string) {
	t.lines = append(t.lines, line)
	trimmed := strings.TrimSpace(line)

	if strings.HasPrefix(trimmed, "=== RUN") {
		t.lastStarted = trimmed
	} else if strings.HasPrefix(trimmed, "--- PASS:") || strings.HasPrefix(trimmed, "--- FAIL:") {
		t.lastCompleted = trimmed
	} else if strings.HasPrefix(trimmed, "ok ") {
		t.passedPkgs = append(t.passedPkgs, trimmed)
	} else if strings.HasPrefix(trimmed, "FAIL\t") {
		t.failedPkgs = append(t.failedPkgs, trimmed)
	}
}

func (t *testTracker) allOutput() string {
	return strings.Join(t.lines, "\n")
}

func (t *testTracker) timeoutSummary() string {
	var b strings.Builder

	if t.lastCompleted != "" {
		fmt.Fprintf(&b, "Last completed test before timeout:\n  %s\n\n", t.lastCompleted)
	}
	if t.lastStarted != "" {
		fmt.Fprintf(&b, "Last started test (likely hanging):\n  %s\n\n", t.lastStarted)
	}
	if len(t.failedPkgs) > 0 {
		b.WriteString("Failed packages:\n")
		for _, f := range t.failedPkgs {
			fmt.Fprintf(&b, "  %s\n", f)
		}
		b.WriteString("\n")
	}
	if len(t.passedPkgs) > 0 {
		fmt.Fprintf(&b, "%d packages passed before timeout (skip these)\n", len(t.passedPkgs))
	}

	result := b.String()
	if result == "" {
		return lastNLines(t.allOutput(), 30)
	}
	return result
}

func lastNLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

