package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/config"
)

// Runner is the centralized agent module. All agent invocations in Ralph —
// loop iterations, fix agents, verification, review, task manager — go
// through this type. It wraps the underlying CLI and provides the foundation
// for agent-agnosticism: swapping Claude for another agent becomes a
// configuration change, not a refactor.
// Re-export model constants from config for convenience.
const (
	ModelSonnet = config.ModelSonnet
	ModelOpus   = config.ModelOpus
	ModelFable  = config.ModelFable
)

type Runner struct {
	Logger         claude.Log
	OnTaskDetected claude.OnTaskDetected
	Model          string
	// ProjectDir is the repository root that agents must NEVER chdir into.
	// All agent spawn paths (Run, Query, Interactive) reject workDir values
	// that are empty or equal to ProjectDir. This is the chokepoint that
	// enforces the worktree invariant — see the worktree-invariant docs in
	// internal/git for the architectural rationale.
	ProjectDir string
	Stdout     io.Writer
	Stderr     io.Writer
	inner      *claude.Runner
}

// New creates a Runner that delegates to the underlying CLI.
//
// projectDir is the repository root. It is stored so every agent spawn can
// verify cmd.Dir != projectDir before exec — preventing the "worktree leaked
// into main" failure mode where a misconfigured workDir causes agents to
// write into the main checkout.
func New(logger claude.Log, projectDir string) *Runner {
	r := &Runner{
		Logger:     logger,
		ProjectDir: projectDir,
	}
	r.inner = &claude.Runner{
		Logger:     logger,
		ProjectDir: projectDir,
	}
	return r
}

// checkWorkDir enforces the worktree invariant: agents must run in a
// worktree, never in the project root and never with an empty cwd.
// Called from Run, Query, and Interactive — every code path that spawns
// an agent process.
func (r *Runner) checkWorkDir(workDir string) error {
	if workDir == "" {
		return fmt.Errorf("agent spawn refused: workDir is empty (worktree setup must have failed)")
	}
	if r.ProjectDir != "" && workDir == r.ProjectDir {
		return fmt.Errorf("agent spawn refused: workDir == projectDir (%s) — worktree setup must have failed", workDir)
	}
	return nil
}

// Run spawns a non-interactive agent process. This is the single code path
// for iteration agents, fix agents, and any other long-running invocation
// that polls for signal files.
func (r *Runner) Run(cfg claude.RunConfig) (claude.Result, error) {
	if err := r.checkWorkDir(cfg.WorkDir); err != nil {
		return claude.Result{}, err
	}
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
func (r *Runner) Query(ctx context.Context, workDir, prompt, model string, allowedTools []string) (string, error) {
	if err := r.checkWorkDir(workDir); err != nil {
		return "", err
	}
	// Hermetic context: the LLM verifier is a code agent — load only
	// repo-committed project settings/CLAUDE.md, never user-global config.
	args := []string{"--print", "--setting-sources", "project"}
	if model != "" {
		args = append(args, "--model", model)
	}
	if len(allowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowedTools, ","))
	}
	args = append(args, "-p", prompt)

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = workDir
	// Disable Claude's machine-local auto-memory for reproducible verification,
	// and auto-compaction so the LLM verifier uses the full 200K context window
	// instead of being silently summarized.
	cmd.Env = append(os.Environ(), "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "DISABLE_AUTO_COMPACT=1")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Interactive runs an interactive agent session (review, task manager).
// Returns the exit code. This is the single code path for all interactive
// agent invocations that connect stdin/stdout to the terminal.
//
// After the subprocess exits, a trailing newline is written to stdout and
// stderr. This prevents the caller's subsequent log output from being
// interleaved with the CLI's exit message (e.g. "Resume this session…").
func (r *Runner) Interactive(workDir, systemPrompt string, extraArgs ...string) (int, error) {
	if err := r.checkWorkDir(workDir); err != nil {
		return 1, err
	}
	args := []string{"--permission-mode", "bypassPermissions", "--system-prompt", systemPrompt}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	args = append(args, extraArgs...)

	stdout := r.stdout()
	stderr := r.stderr()

	cmd := exec.Command("claude", args...)
	cmd.Dir = workDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return 1, err
		}
	}

	fmt.Fprintln(stdout)
	fmt.Fprintln(stderr)

	return exitCode, nil
}

func (r *Runner) stdout() io.Writer {
	if r.Stdout != nil {
		return r.Stdout
	}
	return os.Stdout
}

func (r *Runner) stderr() io.Writer {
	if r.Stderr != nil {
		return r.Stderr
	}
	return os.Stderr
}
