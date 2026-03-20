package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// promptsDir returns the absolute path to the project's prompts/ directory.
// Tests read the real template files so they stay in sync with the actual prompts.
func promptsDir(t *testing.T) string {
	t.Helper()
	// go/internal/prompt/ → project root
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "prompts"))
	if err != nil {
		t.Fatalf("resolve prompts dir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("prompts directory not found at %s", dir)
	}
	return dir
}

func testVars(t *testing.T) Vars {
	t.Helper()
	return Vars{
		PromptsDir:       promptsDir(t),
		ProjectDir:       "/tmp/project",
		WorkDir:          "/tmp/project/worktree",
		RalphDir:         "/tmp/project/.ralph",
		PlanFile:         "/tmp/project/.ralph/plan.md",
		SignalToken:       "###RALPH_TASK_COMPLETE###",
		CurrentTaskToken: "###RALPH_CURRENT_TASK###",
		AllCompleteToken: "###RALPH_ALL_COMPLETE###",
		TaskPrompt:       "Fix auth",
		TaskBackend:      BackendChecklist,
	}
}

// Proves: all template variables are substituted with actual values and
// no raw {{VARIABLE}} placeholders remain in the output.
func TestBuildPrompt_VariablesSubstituted(t *testing.T) {
	v := testVars(t)
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

<<<<<<< HEAD
<<<<<<< HEAD
	for _, want := range []string{v.WorkDir, v.RalphDir, v.PlanFile} {
=======
	for _, want := range []string{v.WorkDir, v.RalphDir, v.PlanFile, v.SignalToken} {
>>>>>>> e0c9eef (Add Go prompt assembly with template substitution)
=======
	for _, want := range []string{v.WorkDir, v.RalphDir, v.PlanFile} {
>>>>>>> 6a2ca6f (fix: update prompt tests for signal file-based protocol)
		if !strings.Contains(result, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
<<<<<<< HEAD
<<<<<<< HEAD
	for _, raw := range []string{"{{WORK_DIR}}", "{{RALPH_DIR}}", "{{PLAN_FILE}}"} {
=======
	for _, raw := range []string{"{{WORK_DIR}}", "{{RALPH_DIR}}", "{{PLAN_FILE}}", "{{SIGNAL_TOKEN}}"} {
>>>>>>> e0c9eef (Add Go prompt assembly with template substitution)
=======
	for _, raw := range []string{"{{WORK_DIR}}", "{{RALPH_DIR}}", "{{PLAN_FILE}}"} {
>>>>>>> 6a2ca6f (fix: update prompt tests for signal file-based protocol)
		if strings.Contains(result, raw) {
			t.Errorf("prompt still contains unsubstituted %s", raw)
		}
	}
}

// Proves: the assembled prompt includes task selection instructions
// and the task content itself.
func TestBuildPrompt_IncludesTaskContent(t *testing.T) {
	v := testVars(t)
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if !strings.Contains(result, "Task selection") {
		t.Error("prompt missing task selection instructions")
	}
	if !strings.Contains(result, "Fix auth") {
		t.Error("prompt missing task content")
	}
}

// Proves: the shared quality standards preamble is included in the prompt.
func TestBuildPrompt_IncludesSharedPrompt(t *testing.T) {
	v := testVars(t)
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	shared, _ := os.ReadFile(filepath.Join(v.PromptsDir, "shared.md"))
	firstLine := strings.SplitN(string(shared), "\n", 2)[0]
	if !strings.Contains(result, firstLine) {
		t.Errorf("prompt missing first line of shared.md: %q", firstLine)
	}
}

// Proves: user feedback is injected into the prompt when provided.
func TestBuildPrompt_FeedbackIncluded(t *testing.T) {
	v := testVars(t)
	v.Feedback = "make it generic, use plugins"
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if !strings.Contains(result, "User feedback") {
		t.Error("prompt missing feedback section header")
	}
	if !strings.Contains(result, "make it generic") {
		t.Error("prompt missing feedback content")
	}
}

// Proves: no feedback section is present when feedback is empty.
func TestBuildPrompt_NoFeedbackWhenEmpty(t *testing.T) {
	v := testVars(t)
	v.Feedback = ""
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if strings.Contains(result, "User feedback") {
		t.Error("prompt should not contain feedback section when feedback is empty")
	}
}

// Proves: the bd backend uses execution-bd.md for task instructions.
func TestBuildPrompt_BDBackend(t *testing.T) {
	v := testVars(t)
	v.TaskBackend = BackendBD
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	// bd template references bd prime; checklist template references plan file
	if !strings.Contains(result, "bd") {
		t.Error("bd backend prompt should reference bd")
	}
}

// Proves: BuildRefactorPrompt includes refactor instructions and
// substitutes recent files and signal tokens.
func TestBuildRefactorPrompt(t *testing.T) {
	v := testVars(t)
	files := "main.go\nconfig.go"
	result, err := BuildRefactorPrompt(v, files)
	if err != nil {
		t.Fatalf("BuildRefactorPrompt: %v", err)
	}

	if !strings.Contains(result, "refactor") {
		t.Error("refactor prompt missing refactor instructions")
	}
	if !strings.Contains(result, "main.go") {
		t.Error("refactor prompt missing recent files")
	}
<<<<<<< HEAD
<<<<<<< HEAD
	if !strings.Contains(result, ".signal_complete") {
		t.Error("refactor prompt missing signal protocol")
=======
	if !strings.Contains(result, v.SignalToken) {
		t.Error("refactor prompt missing signal token")
>>>>>>> e0c9eef (Add Go prompt assembly with template substitution)
=======
	if !strings.Contains(result, ".signal_complete") {
		t.Error("refactor prompt missing signal protocol")
>>>>>>> 6a2ca6f (fix: update prompt tests for signal file-based protocol)
	}
	if strings.Contains(result, "{{RECENT_FILES}}") {
		t.Error("refactor prompt still contains unsubstituted {{RECENT_FILES}}")
	}
}

// Proves: the assembled prompt includes post-task reflection instructions
// so Claude writes a reflection file before signaling completion.
func TestBuildPrompt_IncludesReflection(t *testing.T) {
	v := testVars(t)
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if !strings.Contains(result, "Post-task reflection") {
		t.Error("prompt missing reflection instructions")
	}
	if !strings.Contains(result, "reflections/") {
		t.Error("prompt missing reflections directory path")
	}
}

// Proves: an unknown backend returns an error instead of producing
// a malformed prompt.
func TestBuildPrompt_UnknownBackendErrors(t *testing.T) {
	v := testVars(t)
	v.TaskBackend = "nonexistent"
	_, err := BuildPrompt(v)
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

// Proves: a missing template file returns a descriptive error.
func TestBuildPrompt_MissingTemplateErrors(t *testing.T) {
	v := testVars(t)
	v.PromptsDir = "/nonexistent/path"
	_, err := BuildPrompt(v)
	if err == nil {
		t.Fatal("expected error for missing template")
	}
	if !strings.Contains(err.Error(), "prompt template not found") {
		t.Errorf("error should mention template not found, got: %v", err)
	}
}
