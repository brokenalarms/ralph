package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/brokenalarms/ralph/internal/claude"
)

// Runner is the centralized agent module. All agent invocations in Ralph —
// loop iterations, fix agents, verification, review, task manager — go
// through this type. It wraps the underlying CLI with optional container
// isolation and provides the foundation for agent-agnosticism: swapping
// Claude for another agent becomes a configuration change, not a refactor.
type Runner struct {
	Logger         claude.Log
	OnTaskDetected claude.OnTaskDetected
	Sandbox        *Sandbox
	inner          *claude.Runner
}

// New creates a Runner with optional sandbox isolation. When sandbox is
// non-nil, all agent invocations run inside a macOS sandbox-exec container
// that restricts filesystem write access to the worktree and ralph state dir.
func New(logger claude.Log, sandbox *Sandbox) *Runner {
	r := &Runner{
		Logger:  logger,
		Sandbox: sandbox,
	}
	inner := &claude.Runner{
		Logger: logger,
	}
	if sandbox != nil {
		inner.CmdFactory = r.sandboxedCmdFactory
	}
	r.inner = inner
	return r
}

// Run spawns a non-interactive agent process. This is the single code path
// for iteration agents, fix agents, and any other long-running invocation
// that polls for signal files.
func (r *Runner) Run(cfg claude.RunConfig) (claude.Result, error) {
	r.inner.OnTaskDetected = r.OnTaskDetected
	return r.inner.Run(cfg)
}

// StopStreaming stops active streaming goroutines from a previous Run.
func (r *Runner) StopStreaming() {
	r.inner.StopStreaming()
}

// InjectMessage writes a user message to the running agent's stdin pipe.
func (r *Runner) InjectMessage(msg string) error {
	return r.inner.InjectMessage(msg)
}

// Query runs a quick non-interactive agent for verification (LLM review).
// Returns the raw response text. This is the single code path for all
// prompt-response style invocations (no signal polling, no streaming).
func (r *Runner) Query(ctx context.Context, workDir, prompt, model string) (string, error) {
	args := []string{"--print"}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "-p", prompt)

	var cmd *exec.Cmd
	if r.Sandbox != nil {
		cmd = r.Sandbox.Wrap(ctx, []string{workDir}, "claude", args...)
	} else {
		cmd = exec.CommandContext(ctx, "claude", args...)
	}
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Interactive runs an interactive agent session (review, task manager).
// Returns the exit code. This is the single code path for all interactive
// agent invocations that connect stdin/stdout to the terminal.
func (r *Runner) Interactive(workDir, systemPrompt string, extraArgs ...string) (int, error) {
	args := []string{"--system-prompt", systemPrompt}
	args = append(args, extraArgs...)

	var cmd *exec.Cmd
	if r.Sandbox != nil {
		cmd = r.Sandbox.Wrap(context.Background(), sandboxWriteDirs(workDir, filepath.Join(workDir, ".ralph")), "claude", args...)
	} else {
		cmd = exec.Command("claude", args...)
	}
	cmd.Dir = workDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

// sandboxedCmdFactory builds a sandbox-wrapped Claude command for use by
// the inner claude.Runner. Mirrors the default command construction in
// claude.go but wraps it in sandbox-exec.
func (r *Runner) sandboxedCmdFactory(cfg claude.RunConfig, rawLog *os.File) *exec.Cmd {
	args := []string{
		"--print", "--verbose",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--add-dir", cfg.WorkDir,
		"--add-dir", cfg.RalphDir,
		"--allowedTools", strings.Join(claude.IterationAllowedTools, ","),
		"--disallowedTools", strings.Join(claude.IterationDisallowedTools, ","),
		"-p", cfg.Prompt,
	}

	writeDirs := sandboxWriteDirs(cfg.WorkDir, cfg.RalphDir)
	cmd := r.Sandbox.Wrap(cfg.Ctx, writeDirs, "claude", args...)
	cmd.Dir = cfg.WorkDir
	cmd.Stdout = rawLog
	cmd.Stderr = rawLog
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// sandboxWriteDirs returns the directories that need write access inside
// the sandbox. Beyond the worktree and ralph dir, this includes the git
// common dir (worktrees point to the parent repo's .git/worktrees/) and
// tool caches (Go build cache, module cache).
func sandboxWriteDirs(workDir, ralphDir string) []string {
	dirs := []string{workDir, ralphDir}

	// Git worktrees have a .git file pointing to the parent repo's
	// .git/worktrees/<name>. The agent needs write access there for
	// git operations (commit, push, rebase).
	gitCommon := resolveGitCommonDir(workDir)
	if gitCommon != "" {
		dirs = append(dirs, gitCommon)
	}

	if home, err := os.UserHomeDir(); err == nil {
		// Claude CLI session state (session-env, history, etc.)
		dirs = append(dirs, filepath.Join(home, ".claude"))
		// Go build cache and module cache.
		dirs = append(dirs, filepath.Join(home, "Library", "Caches", "go-build"))
		dirs = append(dirs, filepath.Join(home, "go"))
	}

	return dirs
}

// resolveGitCommonDir finds the git common directory for worktrees.
// For a worktree, .git is a file containing "gitdir: /path/to/.git/worktrees/<name>".
// The common dir is the parent .git, which needs write access.
func resolveGitCommonDir(workDir string) string {
	gitPath := filepath.Join(workDir, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return gitPath
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "gitdir: ") {
		return ""
	}
	gitDir := strings.TrimPrefix(line, "gitdir: ")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(workDir, gitDir)
	}
	// gitDir is .git/worktrees/<name>, we need the parent .git
	commonDir := filepath.Dir(filepath.Dir(gitDir))
	return commonDir
}
