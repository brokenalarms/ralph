// Package planning implements ralph's planning phase: interactive and
// autonomous modes that produce a task list before execution begins.
package planning

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// Deps bundles the dependencies the planning phase needs from the outer loop.
type Deps struct {
	Ctx        context.Context
	Backend    tasks.Backend
	StateStore *state.Store
	Logger     *logging.Logger
	PromptsDir string
	WorkDir    string // worktree or project directory
	RalphDir   string // .ralph state directory
	PlanFile   string // path to plan.md (checklist backend)
	Prompt     string // user-supplied --prompt override (may be empty)

	// SkipPlanning mirrors --skip-planning: suppresses the interactive session.
	SkipPlanning bool

	// ForcePlan mirrors --plan: forces re-entry into interactive planning even
	// when resuming a session that already has tasks.
	ForcePlan bool

	// RunClaude launches claude in non-interactive (autonomous) mode with the
	// given system prompt. The caller is responsible for signal polling.
	// If nil, defaultRunClaude is used.
	RunClaude func(ctx context.Context, prompt string) error
}

// Run executes the planning phase. It mirrors ralph.sh's run_planning():
//
//  1. If a pre-made plan file was supplied (--plan-file), copy it and return.
//  2. If resuming and status is past "initialized", skip planning.
//  3. Launch interactive planning (user chats with Claude).
//  4. If interactive didn't produce tasks, fall back to autonomous planning.
//  5. Validate that planning succeeded, update state, rename worktree.
func Run(d Deps) error {
	d.Logger.Phase("=== PHASE 1: PLANNING ===")

	// Pre-made plan: copy file and count tasks.
	if d.PlanFile != "" {
		if copied, err := tryCopyPlanFile(d); err != nil {
			return err
		} else if copied {
			return nil
		}
	}

	// Resume: skip planning if already past initialized (unless --plan forces it).
	s, err := d.StateStore.Load()
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	if s.Status != "" && s.Status != "initialized" && !d.ForcePlan {
		d.Logger.Task("Resuming execution (status: %s)", s.Status)
		return nil
	}

	// Interactive planning session — always run on fresh start (status empty/initialized).
	// NeedsPlanning checks if tasks exist, but even with existing beads the user
	// should get a chance to review/modify the plan on a fresh run.
	if !d.SkipPlanning {
		if err := runInteractive(d); err != nil {
			d.Logger.Warn("Interactive planning session ended: %v", err)
		}
	}

	// Check if interactive session created tasks.
	ok, err := d.Backend.PlanningSucceeded()
	if err != nil {
		return fmt.Errorf("checking planning_succeeded: %w", err)
	}
	if ok {
		return finalize(d)
	}

	// Fallback: autonomous planning.
	if err := runAutonomous(d); err != nil {
		return fmt.Errorf("autonomous planning: %w", err)
	}

	ok, err = d.Backend.PlanningSucceeded()
	if err != nil {
		return fmt.Errorf("checking planning_succeeded: %w", err)
	}
	if !ok {
		d.Logger.TaskError("Planning failed — no tasks created")
		return fmt.Errorf("planning failed: no tasks created")
	}

	return finalize(d)
}

// tryCopyPlanFile copies a user-supplied --plan-file into the worktree
// and returns true if the copy happened (file existed on disk).
func tryCopyPlanFile(d Deps) (bool, error) {
	src := d.PlanFile
	dst := planFilePath(d)

	if _, err := os.Stat(src); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("stat plan file: %w", err)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return false, fmt.Errorf("reading plan file: %w", err)
	}

	if err := os.WriteFile(dst, data, 0644); err != nil {
		return false, fmt.Errorf("writing plan file: %w", err)
	}

	total, err := d.Backend.CountTotal()
	if err != nil {
		total = 0
	}

	if err := setStatus(d, "planned"); err != nil {
		return true, err
	}

	d.Logger.Task("Copied plan from %s (%d tasks)", src, total)
	return true, nil
}

// runInteractive launches an interactive Claude session for the user to
// define specs and a plan. This is a foreground process — the user chats
// directly with Claude.
func runInteractive(d Deps) error {
	d.Logger.Log("Starting interactive planning session...")
	d.Logger.Log("Task backend: %s", d.Backend.Label())
	d.Logger.Log("Chat with Claude to define your spec and plan. Exit when done.")

	prompt, err := buildInteractivePrompt(d)
	if err != nil {
		return err
	}

	args := []string{
		"--add-dir", d.WorkDir,
		"--add-dir", d.RalphDir,
		"--permission-mode", "plan",
		"--allowedTools", "Bash",
		"--system-prompt", prompt,
	}

	cmd := exec.CommandContext(d.Ctx, "claude", args...)
	cmd.Dir = d.WorkDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err = cmd.Run()
	d.Logger.Log("Interactive planning session ended.")
	return err
}

// runAutonomous launches Claude in non-interactive mode with the planning
// prompt. Claude writes a plan and signals completion.
func runAutonomous(d Deps) error {
	prompt, err := buildAutonomousPrompt(d)
	if err != nil {
		return err
	}

	runner := d.RunClaude
	if runner == nil {
		runner = defaultRunClaude(d)
	}

	return runner(d.Ctx, prompt)
}

// buildInteractivePrompt assembles the interactive planning system prompt
// from the template, substituting backend-specific instructions.
func buildInteractivePrompt(d Deps) (string, error) {
	tmplPath := filepath.Join(d.PromptsDir, "interactive-planning.md")
	data, err := os.ReadFile(tmplPath)
	if err != nil {
		return "", fmt.Errorf("reading interactive planning template: %w", err)
	}

	prompt := string(data)
	prompt = strings.ReplaceAll(prompt, "{{WORK_DIR}}", d.WorkDir)
	prompt = strings.ReplaceAll(prompt, "{{RALPH_DIR}}", d.RalphDir)

	stateFile := d.StateStore.Path()
	prompt = strings.ReplaceAll(prompt, "{{STATE_FILE}}", stateFile)

	prompt = strings.ReplaceAll(prompt, "{{TASK_INSTRUCTIONS}}", d.Backend.PlanningInstructions())

	// BD backend doesn't use a plan file; checklist does.
	if d.Backend.Label() == "beads" {
		prompt = strings.ReplaceAll(prompt, "{{PLAN_FILE_LINE}}", "")
	} else {
		prompt = strings.ReplaceAll(prompt, "{{PLAN_FILE_LINE}}", "- Plan file: "+planFilePath(d))
	}

	// When re-planning (--plan), tell Claude that tasks already exist so it
	// can modify the plan rather than starting from scratch.
	prompt = strings.ReplaceAll(prompt, "{{EXISTING_TASKS_CONTEXT}}", existingTasksContext(d))

	return prompt, nil
}

// existingTasksContext returns a prompt section describing existing tasks when
// re-planning, or empty string on a fresh start.
func existingTasksContext(d Deps) string {
	if !d.ForcePlan {
		return ""
	}
	hasTasks, _ := d.Backend.HasTasks()
	if !hasTasks {
		return ""
	}
	total, _ := d.Backend.CountTotal()
	completed, _ := d.Backend.CountCompleted()
	remaining, _ := d.Backend.CountRemaining()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\n## Existing tasks (%d total, %d completed, %d remaining)\n", total, completed, remaining))
	sb.WriteString("Tasks already exist from a previous planning session. Review and modify the existing plan rather than starting from scratch.\n")
	if d.Backend.Label() == "beads" {
		sb.WriteString("Run `bd list` to see all current tasks and their status.\n")
	} else {
		sb.WriteString(fmt.Sprintf("Read the existing plan at `%s` to see current tasks.\n", planFilePath(d)))
	}
	return sb.String()
}

// buildAutonomousPrompt assembles the autonomous planning prompt from the
// template with context and backend instructions.
func buildAutonomousPrompt(d Deps) (string, error) {
	tmplPath := filepath.Join(d.PromptsDir, "planning.md")
	data, err := os.ReadFile(tmplPath)
	if err != nil {
		return "", fmt.Errorf("reading planning template: %w", err)
	}

	prompt := string(data)
	prompt = strings.ReplaceAll(prompt, "{{PLANNING_CONTEXT}}", d.Prompt)
	prompt = strings.ReplaceAll(prompt, "{{PLAN_FILE}}", planFilePath(d))
	prompt = strings.ReplaceAll(prompt, "{{RALPH_DIR}}", d.RalphDir)

	stateFile := d.StateStore.Path()
	prompt = strings.ReplaceAll(prompt, "{{STATE_FILE}}", stateFile)

	prompt = strings.ReplaceAll(prompt, "{{TASK_INSTRUCTIONS}}", d.Backend.PlanningInstructions())

	return prompt, nil
}

// finalize writes "planned" status and logs success.
func finalize(d Deps) error {
	if err := setStatus(d, "planned"); err != nil {
		return err
	}

	total, err := d.Backend.CountTotal()
	if err != nil {
		total = 0
	}
	d.Logger.TaskSuccess("Plan created with %d tasks", total)

	return nil
}

// setStatus writes "planned" (or other status) to state.json.
func setStatus(d Deps, status string) error {
	return d.StateStore.Write("status", status)
}

// planFilePath returns the plan file destination inside the worktree.
// If PlanFile is set, it's used as-is; otherwise defaults to
// <ralph_dir>/plan.md.
func planFilePath(d Deps) string {
	if d.PlanFile != "" {
		return d.PlanFile
	}
	return filepath.Join(d.RalphDir, "plan.md")
}

// defaultRunClaude returns a RunClaude function that spawns claude in
// non-interactive (--print) mode with the given system prompt.
func defaultRunClaude(d Deps) func(context.Context, string) error {
	return func(ctx context.Context, prompt string) error {
		args := []string{
			"--print", "--verbose",
			"--output-format", "stream-json",
			"--add-dir", d.WorkDir,
			"--add-dir", d.RalphDir,
			"--allowedTools", strings.Join(claude.IterationAllowedTools, ","),
			"-p", prompt,
		}

		cmd := exec.CommandContext(ctx, "claude", args...)
		cmd.Dir = d.WorkDir
		cmd.Stdin = nil
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		return cmd.Run()
	}
}
