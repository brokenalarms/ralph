package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"github.com/brokenalarms/ralph/internal/claude"
)

// Runner is the centralized agent module. All agent invocations in Ralph —
// loop iterations, fix agents, verification, review, task manager — go
// through this type. It wraps the underlying CLI and provides the foundation
// for agent-agnosticism: swapping Claude for another agent becomes a
// configuration change, not a refactor.
type Runner struct {
	Logger         claude.Log
	OnTaskDetected claude.OnTaskDetected
	inner          *claude.Runner
}

// New creates a Runner that delegates to the underlying CLI.
func New(logger claude.Log) *Runner {
	r := &Runner{
		Logger: logger,
	}
	r.inner = &claude.Runner{
		Logger: logger,
	}
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

	cmd := exec.CommandContext(ctx, "claude", args...)
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

	cmd := exec.Command("claude", args...)
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
