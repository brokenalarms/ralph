package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TaskBackend selects which execution instructions template to use.
type TaskBackend string

const (
	BackendBD TaskBackend = "bd"

)

// Vars holds all substitution values for prompt template assembly.
// Field names are neutral — the prompt module doesn't care whether the
// task list comes from beads, files, or any other backend.
type Vars struct {
	PromptsDir       string
	ProjectDir       string
	WorkDir          string
	RalphDir         string
	PlanFile         string
	SignalToken        string
	CurrentTaskToken   string
	AllCompleteToken   string
	NoCodeNeededToken  string
	TaskPrompt       string
	AttemptHistory   string
	TestStatus       string
	TasksContext     string
	TaskBackend      TaskBackend
}

// BuildPrompt assembles the execution prompt from template files and
// substitutes all {{VARIABLE}} placeholders. Matches ralph.sh build_prompt.
func BuildPrompt(v Vars) (string, error) {
	shared, err := readTemplate(v.PromptsDir, "shared.md")
	if err != nil {
		return "", err
	}
	internal, err := readTemplate(v.PromptsDir, "internal.md")
	if err != nil {
		return "", err
	}
	reflection, err := readTemplate(v.PromptsDir, "reflection.md")
	if err != nil {
		return "", err
	}
	signal, err := readTemplate(v.PromptsDir, "signal.md")
	if err != nil {
		return "", err
	}
	feedback, err := readTemplate(v.PromptsDir, "feedback.md")
	if err != nil {
		return "", err
	}
	beadCreation, err := readTemplate(v.PromptsDir, "bead-creation.md")
	if err != nil {
		return "", err
	}

	result := shared + "\n" + internal + "\n" + reflection + "\n" + signal + "\n" + feedback + "\n" + beadCreation

	taskInstructions, err := executionInstructions(v)
	if err != nil {
		return "", err
	}
	result = strings.ReplaceAll(result, "{{TASK_INSTRUCTIONS}}", taskInstructions)

	attemptSection := ""
	if v.AttemptHistory != "" {
		attemptSection = "\n" + v.AttemptHistory
	}

	r := strings.NewReplacer(
		"{{PROJECT_DIR}}", v.ProjectDir,
		"{{WORK_DIR}}", v.WorkDir,
		"{{RALPH_DIR}}", v.RalphDir,
		"{{PLAN_FILE}}", v.PlanFile,
		"{{SIGNAL_TOKEN}}", v.SignalToken,
		"{{CURRENT_TASK_TOKEN}}", v.CurrentTaskToken,
		"{{ALL_COMPLETE_TOKEN}}", v.AllCompleteToken,
		"{{NO_CODE_NEEDED_TOKEN}}", v.NoCodeNeededToken,
		"{{TASK_PROMPT}}", v.TaskPrompt,
		"{{ATTEMPT_HISTORY}}", attemptSection,
		"{{TEST_STATUS}}", v.TestStatus,
		// {{BEADS_CONTEXT}} is the template placeholder name; the Go field
		// is neutralized to TasksContext but the template token stays for
		// backwards compatibility with existing prompt template files.
		"{{BEADS_CONTEXT}}", v.TasksContext,
	)
	return r.Replace(result), nil
}

// executionInstructions returns the bd execution template content.
func executionInstructions(v Vars) (string, error) {
	return readTemplate(v.PromptsDir, "execution-bd.md")
}

// BuildTaskManagerPrompt assembles the system prompt for the interactive
// task manager pane, substituting project, worktree, and ralph directory
// paths. workDir is the concrete session worktree the task session is
// anchored to (e.g. .ralph/worktrees/ralph-task-*) — substituted into
// {{WORKTREE_DIR}} so the prompt names the actual assigned directory instead
// of only the generic ".ralph/worktrees/" wording. startupContext is the
// pre-loaded output of bd list/ready — injected so the task manager can
// present the summary without making tool calls.
func BuildTaskManagerPrompt(promptsDir, projectDir, workDir, ralphDir, startupContext string) (string, error) {
	tmpl, err := readTemplate(promptsDir, "task-manager.md")
	if err != nil {
		return "", err
	}
	beadCreation, err := readTemplate(promptsDir, "bead-creation.md")
	if err != nil {
		return "", err
	}
	r := strings.NewReplacer(
		"{{PROJECT_DIR}}", projectDir,
		"{{WORKTREE_DIR}}", workDir,
		"{{RALPH_DIR}}", ralphDir,
		"{{STARTUP_CONTEXT}}", startupContext,
	)
	return r.Replace(tmpl + "\n" + beadCreation), nil
}

// BuildReviewPrompt assembles the system prompt for the interactive review
// session, combining the shared quality standards with the code style guide,
// post-mortem reflection analysis, and shared bead creation guidance.
func BuildReviewPrompt(promptsDir, projectDir, ralphDir, reflections string) (string, error) {
	shared, err := readTemplate(promptsDir, "shared.md")
	if err != nil {
		return "", err
	}
	style, err := readTemplate(promptsDir, "style-guide.md")
	if err != nil {
		return "", err
	}
	beadCreation, err := readTemplate(promptsDir, "bead-creation.md")
	if err != nil {
		return "", err
	}

	review, err := reviewInstructions(promptsDir, projectDir, ralphDir, style, reflections)
	if err != nil {
		return "", err
	}

	tmpl := shared + "\n" + review + "\n" + beadCreation
	return tmpl, nil
}

// reviewInstructions loads prompts/review.md and substitutes the project
// context, style guide, and reflection content into it. Falls back to a
// status note when no reflections are available.
func reviewInstructions(promptsDir, projectDir, ralphDir, style, reflections string) (string, error) {
	reflectionSection := "No reflections found — skip reflection analysis and focus on test audit and refactor opportunities."
	if reflections != "" {
		reflectionSection = reflections
	}

	tmpl, err := readTemplate(promptsDir, "review.md")
	if err != nil {
		return "", err
	}

	r := strings.NewReplacer(
		"{{PROJECT_DIR}}", projectDir,
		"{{RALPH_DIR}}", ralphDir,
		"{{STYLE_GUIDE}}", style,
		"{{REFLECTIONS}}", reflectionSection,
	)
	return r.Replace(tmpl), nil
}

func readTemplate(dir, name string) (string, error) {
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("prompt template not found: %s", path)
	}
	return string(data), nil
}
