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
type Vars struct {
	PromptsDir       string
	ProjectDir       string
	WorkDir          string
	RalphDir         string
	PlanFile         string
	SignalToken       string
	CurrentTaskToken string
	AllCompleteToken string
	TaskPrompt       string
	AttemptHistory   string
	TestStatus       string
	BeadsContext     string
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

	result := shared + "\n" + internal + "\n" + reflection + "\n" + signal + "\n" + feedback

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
		"{{TASK_PROMPT}}", v.TaskPrompt,
		"{{ATTEMPT_HISTORY}}", attemptSection,
		"{{TEST_STATUS}}", v.TestStatus,
		"{{BEADS_CONTEXT}}", v.BeadsContext,
	)
	return r.Replace(result), nil
}

// BuildRefactorPrompt assembles the refactor iteration prompt.
// Matches ralph.sh build_refactor_prompt.
func BuildRefactorPrompt(v Vars, recentFiles string) (string, error) {
	shared, err := readTemplate(v.PromptsDir, "shared.md")
	if err != nil {
		return "", err
	}
	refactor, err := readTemplate(v.PromptsDir, "refactor.md")
	if err != nil {
		return "", err
	}
	signal, err := readTemplate(v.PromptsDir, "signal.md")
	if err != nil {
		return "", err
	}

	result := shared + "\n" + refactor + "\n" + signal

	r := strings.NewReplacer(
		"{{WORK_DIR}}", v.WorkDir,
		"{{RALPH_DIR}}", v.RalphDir,
		"{{RECENT_FILES}}", recentFiles,
		"{{SIGNAL_TOKEN}}", v.SignalToken,
		"{{CURRENT_TASK_TOKEN}}", v.CurrentTaskToken,
		"{{ALL_COMPLETE_TOKEN}}", v.AllCompleteToken,
	)
	return r.Replace(result), nil
}

// executionInstructions returns the bd execution template content.
func executionInstructions(v Vars) (string, error) {
	return readTemplate(v.PromptsDir, "execution-bd.md")
}

// BuildTaskManagerPrompt assembles the system prompt for the interactive
// task manager pane, substituting project and ralph directory paths.
func BuildTaskManagerPrompt(promptsDir, projectDir, ralphDir string) (string, error) {
	tmpl, err := readTemplate(promptsDir, "task-manager.md")
	if err != nil {
		return "", err
	}
	r := strings.NewReplacer(
		"{{PROJECT_DIR}}", projectDir,
		"{{RALPH_DIR}}", ralphDir,
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
