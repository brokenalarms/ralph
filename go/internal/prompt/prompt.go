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

	// TaskManagerBootstrapPrompt is the initial user message sent to the
	// task manager Claude session so it executes the startup sequence
	// (bd prime, bd list, status summary) without waiting for user input.
	TaskManagerBootstrapPrompt = "Run your startup sequence."

	// ReviewBootstrapPrompt is the initial user message sent to the review
	// session so it begins analysis immediately.
	ReviewBootstrapPrompt = "Begin your review. Read AGENTS.md/CLAUDE.md first, then work through each responsibility."
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

// BuildReviewPrompt assembles the system prompt for the interactive review
// session, combining the shared quality standards with the refactor style guide
// and post-mortem reflection analysis.
func BuildReviewPrompt(promptsDir, projectDir, ralphDir, reflections string) (string, error) {
	shared, err := readTemplate(promptsDir, "shared.md")
	if err != nil {
		return "", err
	}
	style, err := readTemplate(promptsDir, "refactor-style.md")
	if err != nil {
		return "", err
	}

	tmpl := shared + "\n" + reviewInstructions(projectDir, ralphDir, style, reflections)
	return tmpl, nil
}

func reviewInstructions(projectDir, ralphDir, style, reflections string) string {
	reflectionSection := "No reflections found — skip reflection analysis and focus on test audit and refactor opportunities."
	if reflections != "" {
		reflectionSection = reflections
	}

	return fmt.Sprintf(`## Review Mode — Post-Mortem

You are running an interactive post-mortem review session.

START by presenting your reflection analysis (Responsibility 1). Show the
user what the agents learned before anything else — this is the primary
value of the review session. Then proceed to the other responsibilities
as the user directs.

### Context
- Project: %s
- Ralph state: %s

### Style Guide
%s

### Responsibility 1: Reflection Analysis

Read the reflections below. Extract:
- **Recurring patterns**: issues that appear across multiple reflections
- **Permanent learnings**: insights that should be codified in AGENTS.md, CLAUDE.md, or prompt templates
- **One-off surprises**: things that were non-obvious but don't need permanent rules

<reflections>
%s
</reflections>

### Responsibility 2: Test Audit

Examine the test suite for:
- **Stale tests**: tests for removed or renamed functionality
- **Weak assertions**: tests that assert true, check only exit codes, or pin prompt prose instead of behavior
- **Missing coverage**: behavioral code (branching, state, algorithms) without corresponding tests

### Responsibility 3: Refactor Opportunities

Using the style guide above, identify:
- Files over 500 lines with distinct responsibilities worth splitting
- Dead code: unused functions, unreachable branches, commented-out blocks
- Naming issues in recently changed code
- Duplicated logic that has grown past three occurrences

### How to present

Present each responsibility's findings as you complete it — don't batch.
For each finding:
1. State what you found and why it matters
2. Propose the action (add to AGENTS.md, create a bead, refactor now, delete dead code)
3. Wait for user approval before acting

For approved actions that are too large to do in this session, create a bead:
` + "```" + `
bd create --title="..." --description="..." --type=task --priority=3
` + "```" + `

### Rules
- This is an interactive session — present, discuss, then act. Do not silently make changes.
- Refactoring preserves external behavior. Do NOT add new features.
- One refactor = one commit. Atomic. Use a refactor: prefix.
- If nothing meaningful stands out, say so. No-op is a valid outcome.
`, projectDir, ralphDir, style, reflectionSection)
}

func readTemplate(dir, name string) (string, error) {
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("prompt template not found: %s", path)
	}
	return string(data), nil
}
