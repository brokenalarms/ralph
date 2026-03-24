package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// promptsDir returns the absolute path to the embedded prompts directory.
// Tests read the real template files so they stay in sync with the actual prompts.
func promptsDir(t *testing.T) string {
	t.Helper()
	// go/internal/prompt/ → go/cmd/ralph/prompts/
	dir, err := filepath.Abs(filepath.Join("..", "..", "cmd", "ralph", "prompts"))
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
		TaskBackend:      BackendBD,
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

	for _, want := range []string{v.WorkDir, v.RalphDir} {
		if !strings.Contains(result, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	for _, raw := range []string{"{{WORK_DIR}}", "{{RALPH_DIR}}"} {
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

// Proves: live feedback instructions are always included in the prompt,
// telling the agent to check the feedback file between tool calls.
func TestBuildPrompt_LiveFeedbackInstructions(t *testing.T) {
	v := testVars(t)
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if !strings.Contains(result, "Live feedback") {
		t.Error("prompt missing live feedback section header")
	}
	if !strings.Contains(result, "feedback") {
		t.Error("prompt missing feedback file check instructions")
	}
}

// Proves: the feedback instructions reference the ralph dir path so the
// agent knows where to find the feedback file.
func TestBuildPrompt_FeedbackReferencesRalphDir(t *testing.T) {
	v := testVars(t)
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if !strings.Contains(result, v.RalphDir+"/feedback") {
		t.Error("feedback instructions should reference {{RALPH_DIR}}/feedback with substituted path")
	}
}

// Proves: the prompt uses execution-bd.md for task instructions.
func TestBuildPrompt_BDBackend(t *testing.T) {
	v := testVars(t)
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if !strings.Contains(result, "bd") {
		t.Error("prompt should reference bd")
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
	if !strings.Contains(result, ".signal_complete") {
		t.Error("refactor prompt missing signal protocol")
	}
	if strings.Contains(result, "{{RECENT_FILES}}") {
		t.Error("refactor prompt still contains unsubstituted {{RECENT_FILES}}")
	}
}

// Proves: refactor prompt includes shared standards (Boy Scout Rule)
// and refactor-specific instructions.
func TestBuildRefactorPrompt_IncludesSharedAndRefactorInstructions(t *testing.T) {
	v := testVars(t)
	result, err := BuildRefactorPrompt(v, "ralph.sh server.js")
	if err != nil {
		t.Fatalf("BuildRefactorPrompt: %v", err)
	}
	if !strings.Contains(result, "Boy Scout Rule") {
		t.Error("refactor prompt missing Boy Scout Rule from shared")
	}
	if !strings.Contains(result, "refactor-only iteration") {
		t.Error("refactor prompt missing 'refactor-only iteration'")
	}
	if !strings.Contains(result, "ralph.sh server.js") {
		t.Error("refactor prompt missing recent files content")
	}
}

// Proves: refactor prompt resolves all template variables and no
// raw placeholders remain.
func TestBuildRefactorPrompt_ResolvesTemplateVariables(t *testing.T) {
	v := testVars(t)
	result, err := BuildRefactorPrompt(v, "lib/tasks.sh")
	if err != nil {
		t.Fatalf("BuildRefactorPrompt: %v", err)
	}
	if !strings.Contains(result, v.WorkDir) {
		t.Error("refactor prompt missing WORK_DIR value")
	}
	if !strings.Contains(result, ".signal_complete") {
		t.Error("refactor prompt missing signal token")
	}
	for _, raw := range []string{"{{WORK_DIR}}", "{{RALPH_DIR}}", "{{RECENT_FILES}}"} {
		if strings.Contains(result, raw) {
			t.Errorf("refactor prompt still contains unsubstituted %s", raw)
		}
	}
}

// Proves: refactor prompt enforces behavior preservation.
func TestBuildRefactorPrompt_EnforcesNoBehaviorChange(t *testing.T) {
	v := testVars(t)
	result, err := BuildRefactorPrompt(v, "file.sh")
	if err != nil {
		t.Fatalf("BuildRefactorPrompt: %v", err)
	}
	if !strings.Contains(result, "Do NOT add new features or change behavior") {
		t.Error("refactor prompt missing behavior preservation rule")
	}
}

// Proves: refactor prompt requires refactor: commit prefix.
func TestBuildRefactorPrompt_RequiresRefactorCommitPrefix(t *testing.T) {
	v := testVars(t)
	result, err := BuildRefactorPrompt(v, "file.sh")
	if err != nil {
		t.Fatalf("BuildRefactorPrompt: %v", err)
	}
	if !strings.Contains(result, "refactor:") {
		t.Error("refactor prompt missing 'refactor:' commit prefix requirement")
	}
}

// Proves: refactor prompt allows skipping when no meaningful debt exists.
func TestBuildRefactorPrompt_AllowsNoOp(t *testing.T) {
	v := testVars(t)
	result, err := BuildRefactorPrompt(v, "file.sh")
	if err != nil {
		t.Fatalf("BuildRefactorPrompt: %v", err)
	}
	if !strings.Contains(result, "signal completion without making changes") {
		t.Error("refactor prompt missing no-op allowance")
	}
}

// Proves: refactor prompt warns against premature abstractions.
func TestBuildRefactorPrompt_DiscouragesPrematureAbstractions(t *testing.T) {
	v := testVars(t)
	result, err := BuildRefactorPrompt(v, "file.sh")
	if err != nil {
		t.Fatalf("BuildRefactorPrompt: %v", err)
	}
	hasUtility := strings.Contains(result, "utility functions")
	hasOneTime := strings.Contains(result, "one-time operations")
	if !hasUtility && !hasOneTime {
		t.Error("refactor prompt missing premature abstraction warning")
	}
}

// Proves: refactor prompt uses 500 lines as the split signal.
func TestBuildRefactorPrompt_References500LineThreshold(t *testing.T) {
	v := testVars(t)
	result, err := BuildRefactorPrompt(v, "file.sh")
	if err != nil {
		t.Fatalf("BuildRefactorPrompt: %v", err)
	}
	if !strings.Contains(result, "500") {
		t.Error("refactor prompt missing 500 line threshold")
	}
}

// Proves: refactor prompt includes quality findings section header.
func TestBuildRefactorPrompt_IncludesQualityFindings(t *testing.T) {
	v := testVars(t)
	result, err := BuildRefactorPrompt(v, "src/auth.ts")
	if err != nil {
		t.Fatalf("BuildRefactorPrompt: %v", err)
	}
	if !strings.Contains(result, "Quality signals detected") {
		t.Error("refactor prompt missing 'Quality signals detected' section")
	}
}

// Proves: shared prompt includes Boy Scout Rule as a reminder.
func TestSharedPrompt_IncludesBoyScoutRule(t *testing.T) {
	shared, err := os.ReadFile(filepath.Join(promptsDir(t), "shared.md"))
	if err != nil {
		t.Fatalf("reading shared.md: %v", err)
	}
	content := string(shared)
	if !strings.Contains(content, "Boy Scout Rule") {
		t.Error("shared prompt missing 'Boy Scout Rule'")
	}
	if !strings.Contains(content, "Dead code") {
		t.Error("shared prompt missing 'Dead code'")
	}
	if !strings.Contains(content, "leave it alone") {
		t.Error("shared prompt missing 'leave it alone'")
	}
}

// Proves: execution-bd.md reinforces that the Boy Scout Rule applies despite single-task focus.
func TestExecutionBD_ReinforcesBoyScoutRule(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "execution-bd.md"))
	if err != nil {
		t.Fatalf("reading execution-bd.md: %v", err)
	}
	if !strings.Contains(string(content), "Boy Scout Rule") {
		t.Error("execution-bd.md should reinforce the Boy Scout Rule in its single-task focus rule")
	}
}

// Proves: execution-bd.md instructs the agent to echo bead details neatly
// after bd create — ID, priority, type, labels, title, description — instead
// of dumping the raw command with all flags.
func TestExecutionBD_BeadEchoBack(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "execution-bd.md"))
	if err != nil {
		t.Fatalf("reading execution-bd.md: %v", err)
	}
	s := string(content)

	required := []struct {
		substr string
		reason string
	}{
		{"echo back", "should instruct echoing bead details after creation"},
		{"priority", "echo should include priority"},
		{"labels", "echo should include labels"},
		{"truncat", "echo should mention truncation for long descriptions"},
	}

	for _, tc := range required {
		if !strings.Contains(s, tc.substr) {
			t.Errorf("execution-bd.md missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: execution-bd.md requires TDD — write a failing test first, then implement.
func TestExecutionBD_RequiresTDD(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "execution-bd.md"))
	if err != nil {
		t.Fatalf("reading execution-bd.md: %v", err)
	}
	s := string(content)
	if !strings.Contains(s, "must FAIL") || !strings.Contains(s, "must PASS") {
		t.Error("execution-bd.md should require TDD: write a test that must FAIL, implement, then it must PASS")
	}
}

// Proves: attempt history is injected into the prompt when previous
// attempts exist on the current task.
func TestBuildPrompt_IncludesAttemptHistory(t *testing.T) {
	v := testVars(t)
	v.AttemptHistory = "## Previous attempts on this task\n### Attempt 1\nSummary: broke it\nChanges: none\nAnalysis: warn:stuck\n"
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if !strings.Contains(result, "Previous attempts on this task") {
		t.Error("prompt missing attempt history section header")
	}
	if !strings.Contains(result, "broke it") {
		t.Error("prompt missing attempt history content")
	}
}

// Proves: no attempt history section in prompt for fresh tasks.
func TestBuildPrompt_OmitsAttemptHistoryForFreshTask(t *testing.T) {
	v := testVars(t)
	v.AttemptHistory = ""
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if strings.Contains(result, "Previous attempts") {
		t.Error("prompt should not contain attempt history section for fresh task")
	}
	if strings.Contains(result, "{{ATTEMPT_HISTORY}}") {
		t.Error("prompt still contains unsubstituted {{ATTEMPT_HISTORY}}")
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

// Proves: the signal protocol warns the agent that writing the signal file
// triggers immediate process termination, preventing premature signaling.
func TestBuildPrompt_SignalWarnsAboutTermination(t *testing.T) {
	v := testVars(t)
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if !strings.Contains(result, "kill") || !strings.Contains(result, "immediately") {
		t.Error("signal protocol should warn that the process will be killed immediately after signal")
	}
}

// Proves: the bd execution template specifies that pushing and PR creation
// must happen before signaling completion.
func TestBuildPrompt_BDCompletionOrderIncludesPush(t *testing.T) {
	v := testVars(t)
	v.TaskBackend = BackendBD
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	pushIdx := strings.Index(result, "Push your branch")
	signalIdx := strings.Index(result, "Signal completion by writing to the signal file")
	if pushIdx < 0 {
		t.Fatal("bd completion section missing push step")
	}
	if signalIdx < 0 {
		t.Fatal("bd completion section missing signal step")
	}
	if pushIdx >= signalIdx {
		t.Error("push step must come before signal step in completion order")
	}
}

// Proves: the iteration prompt instructs the agent to be concise —
// no narration or reasoning aloud, just state/fix/test/signal.
func TestBuildPrompt_DemandsConciseOutput(t *testing.T) {
	v := testVars(t)
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if !strings.Contains(result, "concise") {
		t.Error("prompt should instruct the agent to be concise")
	}
	if !strings.Contains(result, "narrat") {
		t.Error("prompt should explicitly prohibit narration")
	}
}

// Proves: beads context is injected into the prompt when provided,
// giving the agent immediate awareness of project state at startup.
func TestBuildPrompt_BeadsContextIncluded(t *testing.T) {
	v := testVars(t)
	v.TaskBackend = BackendBD
	v.BeadsContext = "○ task-1 [● P1] - Fix auth\n✓ task-0 ● P1 - Bootstrap"
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if !strings.Contains(result, "task-1") {
		t.Error("prompt missing beads context content")
	}
	if strings.Contains(result, "{{BEADS_CONTEXT}}") {
		t.Error("prompt still contains unsubstituted {{BEADS_CONTEXT}}")
	}
}

// Proves: no beads context placeholder remains when context is empty.
func TestBuildPrompt_BeadsContextEmpty(t *testing.T) {
	v := testVars(t)
	v.TaskBackend = BackendBD
	v.BeadsContext = ""
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if strings.Contains(result, "{{BEADS_CONTEXT}}") {
		t.Error("prompt still contains unsubstituted {{BEADS_CONTEXT}}")
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

// Proves: BuildTaskManagerPrompt reads the task-manager.md template and
// substitutes PROJECT_DIR and RALPH_DIR placeholders, so the task manager
// pane gets project-specific instructions.
func TestBuildTaskManagerPrompt(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/home/user/proj", "/home/user/proj/.ralph")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}
	if !strings.Contains(result, "/home/user/proj") {
		t.Error("result should contain substituted PROJECT_DIR")
	}
	if !strings.Contains(result, "/home/user/proj/.ralph") {
		t.Error("result should contain substituted RALPH_DIR")
	}
	if strings.Contains(result, "{{PROJECT_DIR}}") {
		t.Error("result should not contain raw {{PROJECT_DIR}} placeholder")
	}
	if strings.Contains(result, "{{RALPH_DIR}}") {
		t.Error("result should not contain raw {{RALPH_DIR}} placeholder")
	}
}

// Proves: TaskManagerBootstrapPrompt is non-empty, so the task manager
// Claude session receives an initial user message that triggers the startup
// sequence (bd prime, bd list, status summary) without waiting for user input.
func TestTaskManagerBootstrapPrompt_NonEmpty(t *testing.T) {
	if TaskManagerBootstrapPrompt == "" {
		t.Fatal("TaskManagerBootstrapPrompt must be non-empty to trigger startup")
	}
}

// Proves: BuildTaskManagerPrompt returns an error when the template is missing.
func TestBuildTaskManagerPrompt_MissingTemplate(t *testing.T) {
	_, err := BuildTaskManagerPrompt("/nonexistent/path", "/proj", "/proj/.ralph")
	if err == nil {
		t.Fatal("expected error for missing task-manager.md template")
	}
}

// Proves: task-manager.md prompt contains all required sections so the task
// manager pane has complete instructions for bead CRUD, triage, and constraints.
func TestBuildTaskManagerPrompt_RequiredSections(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	required := []struct {
		substr string
		reason string
	}{
		{"Welcome", "prompt should have a welcome/preamble section"},
		{"light triage", "prompt should describe light triage mode"},
		{"hands-on fix", "prompt should describe hands-on fix mode"},
		{"echo back", "prompt should instruct echoing back bead details after creation"},
		{"label", "prompt should require labels on every bead"},
		{"screenshot", "prompt should describe screenshot handling"},
		{"P0", "prompt should reference priority levels"},
		{"in_progress", "prompt should warn about modifying in-progress beads"},
		{"verbatim", "prompt should instruct including diagnostic content verbatim"},
		{"split", "prompt should describe when to split beads"},
	}

	for _, tc := range required {
		if !strings.Contains(result, tc.substr) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: bead echo-back instructions require showing ID, priority, type,
// title, and description — with truncation guidance for long descriptions —
// so the user can review and amend before moving on.
func TestBuildTaskManagerPrompt_EchoBackIncludesDescription(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	required := []struct {
		substr string
		reason string
	}{
		{"review and amend", "echo-back should tell user they can review and amend"},
		{"truncat", "echo-back should mention truncation for long descriptions"},
		{"description", "echo-back should explicitly mention showing the description"},
	}

	for _, tc := range required {
		if !strings.Contains(result, tc.substr) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: task manager prompt tells the task manager that user bug reports
// reference loop log output, so it doesn't ask for clarification about where
// things were seen.
func TestBuildTaskManagerPrompt_LoopLogContext(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	if !strings.Contains(result, "loop log") {
		t.Error("task manager prompt should reference loop log as default context for bug reports")
	}
}

// Proves: task manager prompt includes unwieldy bead detection instructions
// so it proactively audits beads for excessive scope and suggests splitting
// them into focused subtasks with acceptance criteria and dependencies.
func TestBuildTaskManagerPrompt_UnwieldyBeadDetection(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	required := []struct {
		substr string
		reason string
	}{
		{"unwieldy", "prompt should name the concept of unwieldy beads"},
		{"acceptance criteria", "split suggestions should include acceptance criteria"},
		{"subtask", "prompt should instruct creating subtasks"},
		{"bd show", "detection should use bd show to inspect bead details"},
	}

	for _, tc := range required {
		if !strings.Contains(strings.ToLower(result), strings.ToLower(tc.substr)) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: BuildReviewPrompt assembles the shared quality standards and
// refactor style guide into an interactive review session prompt.
func TestBuildReviewPrompt(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildReviewPrompt(dir, "/tmp/project", "/tmp/project/.ralph")
	if err != nil {
		t.Fatalf("BuildReviewPrompt error: %v", err)
	}

	if !strings.Contains(result, "Review Mode") {
		t.Error("review prompt missing 'Review Mode' header")
	}
	if !strings.Contains(result, "/tmp/project") {
		t.Error("review prompt missing project directory")
	}
	if !strings.Contains(result, "Commandments") {
		t.Error("review prompt missing refactor style guide content")
	}
	if !strings.Contains(result, "refactor:") {
		t.Error("review prompt should require refactor: commit prefix")
	}
}

// Proves: BuildReviewPrompt returns an error when prompts directory is missing.
func TestBuildReviewPrompt_MissingTemplate(t *testing.T) {
	_, err := BuildReviewPrompt("/nonexistent", "/tmp", "/tmp/.ralph")
	if err == nil {
		t.Error("expected error for missing prompts directory")
	}
}
