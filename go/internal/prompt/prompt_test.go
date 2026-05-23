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
		SignalToken:       "/tmp/project/.ralph/.signal_complete",
		CurrentTaskToken: "/tmp/project/.ralph/.signal_current_task",
		AllCompleteToken: "/tmp/project/.ralph/.signal_all_complete",
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
func TestBuildPrompt_FeedbackInstructions(t *testing.T) {
	v := testVars(t)
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if !strings.Contains(result, "User feedback") {
		t.Error("prompt missing user feedback section header")
	}
	if !strings.Contains(result, "attempt history") {
		t.Error("prompt should explain feedback appears in attempt history")
	}
}

// Proves: the feedback prompt explains the orchestrator handles feedback
// delivery — the agent doesn't need to poll for it.
func TestBuildPrompt_FeedbackExplainsOrchestration(t *testing.T) {
	v := testVars(t)
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if !strings.Contains(result, "orchestrator") {
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

// Proves: execution-bd.md does not expose PROJECT_DIR to the agent — the agent
// should only know about WORK_DIR to prevent file leaks into the main checkout.
func TestExecutionBD_NoProjectDir(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "execution-bd.md"))
	if err != nil {
		t.Fatalf("reading execution-bd.md: %v", err)
	}
	if strings.Contains(string(content), "{{PROJECT_DIR}}") {
		t.Error("execution-bd.md must not reference {{PROJECT_DIR}} — agent works only in WORK_DIR")
	}
}

// Proves: shared.md Commits section requires committing before running the
// full test suite so that work is not lost if the process is terminated.
func TestSharedPrompt_CommitBeforeTests(t *testing.T) {
	shared, err := os.ReadFile(filepath.Join(promptsDir(t), "shared.md"))
	if err != nil {
		t.Fatalf("reading shared.md: %v", err)
	}
	content := string(shared)
	if !strings.Contains(content, "MUST commit") {
		t.Error("shared prompt missing 'MUST commit' rule for commit-before-tests")
	}
	if !strings.Contains(content, "before running the full test suite") {
		t.Error("shared prompt missing 'before running the full test suite' guidance")
	}
	if !strings.Contains(content, "do not amend") {
		t.Error("shared prompt missing 'do not amend' instruction for failed-then-fixed commits")
	}
}

// Proves: shared.md Testing section states that regression is implicit in all
// acceptance criteria — existing tests and behavior MUST be preserved.
func TestSharedPrompt_RegressionImplicitInACs(t *testing.T) {
	shared, err := os.ReadFile(filepath.Join(promptsDir(t), "shared.md"))
	if err != nil {
		t.Fatalf("reading shared.md: %v", err)
	}
	content := string(shared)
	if !strings.Contains(content, "MUST continue to pass") {
		t.Error("shared prompt missing 'MUST continue to pass' regression rule")
	}
	if !strings.Contains(content, "MUST be preserved") {
		t.Error("shared prompt missing 'MUST be preserved' behavior preservation rule")
	}
}

// Proves: shared.md Testing section requires testing the actual code path,
// not just adding no-op stubs to satisfy compilation.
func TestSharedPrompt_TestCodePathNotStubs(t *testing.T) {
	shared, err := os.ReadFile(filepath.Join(promptsDir(t), "shared.md"))
	if err != nil {
		t.Fatalf("reading shared.md: %v", err)
	}
	content := string(shared)
	if !strings.Contains(content, "NEVER just add a no-op stub") {
		t.Error("shared prompt missing 'NEVER just add a no-op stub' rule")
	}
	if !strings.Contains(content, "not test coverage") {
		t.Error("shared prompt missing 'not test coverage' stub warning")
	}
}

// Proves: shared.md Testing section requires a single source of truth for
// test fixtures — one canonical file per module, no duplication.
func TestSharedPrompt_SingleSourceOfTruthFixtures(t *testing.T) {
	shared, err := os.ReadFile(filepath.Join(promptsDir(t), "shared.md"))
	if err != nil {
		t.Fatalf("reading shared.md: %v", err)
	}
	content := string(shared)
	if !strings.Contains(content, "MUST have exactly one file") {
		t.Error("shared prompt missing 'MUST have exactly one file' fixture rule")
	}
	if !strings.Contains(content, "NEVER duplicate stub types") {
		t.Error("shared prompt missing 'NEVER duplicate stub types' rule")
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

// Proves: execution-bd.md contains an explicit Working directory section that
// prohibits absolute-path cd and orchestrator-owned ralph:* npm scripts.
func TestExecutionBD_WorkingDirectorySection(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "execution-bd.md"))
	if err != nil {
		t.Fatalf("reading execution-bd.md: %v", err)
	}
	s := string(content)
	lower := strings.ToLower(s)
	if !strings.Contains(lower, "working directory") {
		t.Error("execution-bd.md must contain a 'Working directory' section")
	}
	if !strings.Contains(s, "npm run ralph:") {
		t.Error("execution-bd.md must explicitly prohibit npm run ralph:* scripts")
	}
	if !strings.Contains(s, "CWD") && !strings.Contains(s, "cd /absolute") {
		t.Error("execution-bd.md must explain the risk of absolute-path cd (mention CWD or cd /absolute)")
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
func TestBuildPrompt_BDCompletionOrderNoPush(t *testing.T) {
	v := testVars(t)
	v.TaskBackend = BackendBD
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	if strings.Contains(result, "Push your branch") {
		t.Error("completion section must not include push step — orchestrator owns push")
	}
	signalIdx := strings.Index(result, "Signal completion by writing to the signal file")
	if signalIdx < 0 {
		t.Fatal("bd completion section missing signal step")
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

// Proves: tasks context is injected into the prompt when provided,
// giving the agent immediate awareness of project state at startup.
func TestBuildPrompt_TasksContextIncluded(t *testing.T) {
	v := testVars(t)
	v.TaskBackend = BackendBD
	v.TasksContext = "○ task-1 [● P1] - Fix auth\n✓ task-0 ● P1 - Bootstrap"
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	if !strings.Contains(result, "task-1") {
		t.Error("prompt missing tasks context content")
	}
	if strings.Contains(result, "{{BEADS_CONTEXT}}") {
		t.Error("prompt still contains unsubstituted {{BEADS_CONTEXT}}")
	}
}

// Proves: no tasks context placeholder remains when context is empty.
func TestBuildPrompt_TasksContextEmpty(t *testing.T) {
	v := testVars(t)
	v.TaskBackend = BackendBD
	v.TasksContext = ""
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
	result, err := BuildTaskManagerPrompt(dir, "/home/user/proj", "/home/user/proj/.ralph", "")
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
	if strings.Contains(result, "{{STARTUP_CONTEXT}}") {
		t.Error("result should not contain raw {{STARTUP_CONTEXT}} placeholder")
	}
}

// Proves: BuildTaskManagerPrompt injects pre-loaded startup context into the
// prompt so the task manager can present the summary without running commands.
func TestBuildTaskManagerPrompt_InjectsStartupContext(t *testing.T) {
	dir := promptsDir(t)
	ctx := "$ bd list\nralph-abc  P1 bug  open  fix the widget"
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", ctx)
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}
	if !strings.Contains(result, "fix the widget") {
		t.Error("result should contain injected startup context")
	}
	if strings.Contains(result, "{{STARTUP_CONTEXT}}") {
		t.Error("result should not contain raw {{STARTUP_CONTEXT}} placeholder")
	}
}

// Proves: the task manager system prompt instructs Claude to present the
// startup summary as its first response — no injected user message needed.
func TestTaskManagerPrompt_ContainsAutoStartup(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "first response") {
		t.Fatal("task manager prompt must instruct Claude to auto-present startup summary")
	}
}

// Proves: BuildTaskManagerPrompt returns an error when the template is missing.
func TestBuildTaskManagerPrompt_MissingTemplate(t *testing.T) {
	_, err := BuildTaskManagerPrompt("/nonexistent/path", "/proj", "/proj/.ralph", "")
	if err == nil {
		t.Fatal("expected error for missing task-manager.md template")
	}
}

// Proves: task-manager.md prompt contains all required sections so the task
// manager pane has complete instructions for bead CRUD, triage, and constraints.
func TestBuildTaskManagerPrompt_RequiredSections(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
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
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
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

// Proves: after echoing back a created bead, the task manager asks the user
// to confirm or amend — so they don't have to run bd show separately.
func TestBuildTaskManagerPrompt_ConfirmAfterCreate(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	required := []struct {
		substr string
		reason string
	}{
		{"Looks good", "should ask the user to confirm the created bead"},
		{"confirm", "should mention confirming"},
		{"changes", "should offer the option to request changes"},
		{"bd update", "should use bd update to apply amendments"},
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
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
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
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
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

// Proves: task manager prompt instructs the LLM to query bd state for phase
// tracking, challenge closes that skipped the verified phase, and set
// phase=unverified when reopening falsely-closed tasks.
func TestBuildTaskManagerPrompt_PhaseLifecycleTracking(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	required := []struct {
		substr string
		reason string
	}{
		{"bd state", "prompt should instruct querying bd state for phase"},
		{"phase", "prompt should reference the phase dimension"},
		{"verified", "prompt should reference the verified phase"},
		{"set-state", "prompt should instruct using bd set-state to change phase"},
	}

	for _, tc := range required {
		if !strings.Contains(strings.ToLower(result), strings.ToLower(tc.substr)) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: task manager prompt includes detailed screenshot handling instructions:
// describe the visual issue, save with naming convention, and reference in bead.
func TestBuildTaskManagerPrompt_ScreenshotHandling(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	required := []struct {
		substr string
		reason string
	}{
		{"Describe", "should instruct describing the visual issue"},
		{"screenshots/", "should reference the screenshots directory path"},
		{"slug", "should explain the naming convention with slug"},
		{"bd update", "should instruct referencing screenshot path in the bead"},
		{"Read tool", "should mention the fixing agent reads via multimodal Read"},
	}

	for _, tc := range required {
		if !strings.Contains(result, tc.substr) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: task-manager.md requires --acceptance flag on bd create so every
// bead has specific, testable acceptance criteria the verifier can check.
func TestTaskManagerPrompt_RequiresAcceptanceCriteria(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	if !strings.Contains(result, "--acceptance") {
		t.Error("task-manager.md should require --acceptance flag on bd create")
	}
}

// Proves: task-manager.md instructs checking bead status before commenting
// or updating, and never modifying closed beads.
func TestTaskManagerPrompt_CheckStatusBeforeUpdate(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	lower := strings.ToLower(result)
	if !strings.Contains(lower, "check") || !strings.Contains(lower, "status") {
		t.Error("task-manager.md should instruct checking bead status before updates")
	}
	if !strings.Contains(lower, "closed") {
		t.Error("task-manager.md should warn against modifying closed beads")
	}
}

// Proves: bead-creation.md explicitly requires bd show before any bd update
// and prohibits both updating and reopening closed beads, so agents cannot
// silently mutate closed beads (bd update on closed beads succeeds without error).
func TestBeadCreation_ExplicitUpdateGuard(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "bead-creation.md"))
	if err != nil {
		t.Fatalf("reading bead-creation.md: %v", err)
	}
	lower := strings.ToLower(string(content))

	required := []struct {
		substr string
		reason string
	}{
		{"bd show", "must require bd show check before any bd update"},
		{"never update or reopen", "must explicitly prohibit both update and reopen on closed beads"},
		{"silently succeeds", "must explain why the check is necessary — bd update gives no error on closed beads"},
	}

	for _, tc := range required {
		if !strings.Contains(lower, tc.substr) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: bead-creation.md clearly distinguishes the two separate in-flight
// checks — phase=implementing (from bd state) vs status=in_progress (from bd
// show) — and requires explicit user confirmation before updating in either case,
// with a follow-up bead as the safe alternative.
func TestBeadCreation_DistinguishesPhaseFromStatus(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "bead-creation.md"))
	if err != nil {
		t.Fatalf("reading bead-creation.md: %v", err)
	}
	s := string(content)
	lower := strings.ToLower(s)

	required := []struct {
		substr string
		reason string
	}{
		{"bd state", "must reference bd state to check phase"},
		{"phase=implementing", "must explicitly label the phase=implementing check"},
		{"status=in_progress", "must explicitly label the status=in_progress check"},
		{"explicitly confirms", "both cases must require explicit user confirmation before updating"},
		{"follow-up bead", "both cases must offer follow-up bead as the safe alternative"},
	}
	for _, tc := range required {
		if !strings.Contains(lower, tc.substr) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}

	// Confirm contradictory phrasing is gone: "do not modify it. either confirm"
	if strings.Contains(lower, "do not modify it. either confirm") {
		t.Error("contradictory 'Do not modify it. Either confirm' phrasing must be removed")
	}
}

// Proves: task manager prompt requires verifying bead status with bd show
// before referencing any bead as a future fix, and instructs creating new
// beads instead of reopening closed ones when follow-on work is needed.
func TestBuildTaskManagerPrompt_NeverReferenceClosedBeadsAsFutureFixes(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	lower := strings.ToLower(result)

	required := []struct {
		substr string
		reason string
	}{
		{"bd show", "must require bd show before referencing a bead as a future fix"},
		{"verify", "must instruct verifying bead status before citing it"},
		{"create a new", "must instruct creating new beads instead of reopening closed ones"},
		{"never update or reopen", "must explicitly prohibit both updating and reopening closed beads"},
	}

	for _, tc := range required {
		if !strings.Contains(lower, tc.substr) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: task manager startup sequence includes bd ready after bd list,
// so the welcome summary distinguishes between open (possibly blocked)
// and ready (unblocked) beads.
func TestTaskManagerPrompt_StartupIncludesBdReady(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	if !strings.Contains(result, "bd ready") {
		t.Error("task-manager.md startup sequence should include bd ready")
	}

	listIdx := strings.Index(result, "bd list")
	readyIdx := strings.Index(result, "bd ready")
	if listIdx < 0 || readyIdx < 0 || readyIdx < listIdx {
		t.Error("bd ready should appear after bd list in the startup sequence")
	}

	lower := strings.ToLower(result)
	if !strings.Contains(lower, "ready") && !strings.Contains(lower, "unblocked") {
		t.Error("startup summary should distinguish ready/unblocked beads")
	}
	if !strings.Contains(lower, "blocked") {
		t.Error("startup summary should mention blocked beads")
	}
}

// Proves: execution-bd.md does not mention bd commands — the agent should
// have no awareness of the task management system.
func TestExecutionBD_NoBdReferences(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "execution-bd.md"))
	if err != nil {
		t.Fatalf("reading execution-bd.md: %v", err)
	}
	s := string(content)
	for _, banned := range []string{"bd create", "bd update", "bd close", "bd init", "bd prime", "`bd`"} {
		if strings.Contains(s, banned) {
			t.Errorf("execution-bd.md must not reference %q — agent should not know about task management", banned)
		}
	}
}

// Proves: the assembled loop-agent prompt includes bead update guard guidance
// from bead-creation.md so loop agents must check status before any bd update.
func TestBuildPrompt_IncludesBeadUpdateGuard(t *testing.T) {
	v := testVars(t)
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	lower := strings.ToLower(result)

	required := []struct {
		substr string
		reason string
	}{
		{"bd show", "loop agent must run bd show before any bd update"},
		{"never update or reopen", "loop agent must be told update and reopen are both prohibited on closed beads"},
		{"closed", "loop agent must be warned against modifying closed beads"},
	}

	for _, tc := range required {
		if !strings.Contains(lower, tc.substr) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: bead-creation.md is the single source of bead update guard guidance —
// execution-bd.md must not contain a duplicate "Updating beads" section.
func TestExecutionBD_NoDuplicateUpdateGuidance(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "execution-bd.md"))
	if err != nil {
		t.Fatalf("reading execution-bd.md: %v", err)
	}
	if strings.Contains(string(content), "## Updating beads") {
		t.Error("execution-bd.md must not contain an 'Updating beads' section — bead-creation.md is the single source")
	}
}

// Proves: task-manager.md enumerates valid bd types (bug, task, feature),
// requires --type on every bd create, and documents the issueTypeRank
// ordering so bugs are worked before tasks at the same priority level.
func TestTaskManagerPrompt_RequiresTypeOnCreate(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	required := []struct {
		needle string
		reason string
	}{
		{"--type", "must require --type flag on bd create"},
		{"bug", "must enumerate bug as a valid type"},
		{"task", "must enumerate task as a valid type"},
		{"feature", "must enumerate feature as a valid type"},
		{"bug", "must document bug-first ordering"},
	}
	for _, r := range required {
		if !strings.Contains(result, r.needle) {
			t.Errorf("task-manager.md %s (missing %q)", r.reason, r.needle)
		}
	}

	// Verify ordering documentation: bug before task before feature
	lower := strings.ToLower(result)
	bugIdx := strings.Index(lower, "bug")
	taskIdx := strings.LastIndex(lower[:strings.Index(lower, "feature")], "task")
	featureIdx := strings.Index(lower, "feature")
	if bugIdx < 0 || taskIdx < 0 || featureIdx < 0 {
		t.Fatal("missing one of bug/task/feature in type ordering documentation")
	}
}

// Proves: task-manager.md instructs using function/behavior references in bead
// descriptions instead of line numbers, which go stale between creation and execution.
func TestTaskManagerPrompt_ReferenceBehaviorsNotLines(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	if !strings.Contains(result, "line numbers") {
		t.Error("task-manager.md should warn against using line numbers in bead descriptions")
	}
	if !strings.Contains(result, "functions") || !strings.Contains(result, "behaviors") {
		t.Error("task-manager.md should instruct referencing functions and behaviors instead of line numbers")
	}
}

// Proves: bead-creation.md requires exact code identifiers when referencing
// project-specific names so agents can find the code without wasting time on
// paraphrases that don't exist in the codebase (e.g. 'historyBack' vs Command.goBack).
func TestBeadCreationPrompt_ExactIdentifiers(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "bead-creation.md"))
	if err != nil {
		t.Fatalf("reading bead-creation.md: %v", err)
	}
	s := string(content)

	if !strings.Contains(s, "exact name from the code") {
		t.Error("bead-creation.md should require using exact names from the code")
	}
	if !strings.Contains(s, "paraphrase") {
		t.Error("bead-creation.md should warn against using human paraphrases")
	}
}

// Proves: the reflection template includes an attempt-handoff status line
// so subsequent attempts can immediately see what was done, what remains,
// and whether tests pass — without re-verifying from scratch.
func TestReflection_AttemptHandoffStatus(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "reflection.md"))
	if err != nil {
		t.Fatalf("reading reflection.md: %v", err)
	}
	s := string(content)

	if !strings.Contains(s, "what was done") {
		t.Error("reflection.md should instruct noting what was done")
	}
	if !strings.Contains(s, "what remains") {
		t.Error("reflection.md should instruct noting what remains")
	}
	if !strings.Contains(s, "tests pass") {
		t.Error("reflection.md should instruct noting whether tests pass")
	}
}

// Proves: BuildReviewPrompt assembles the shared quality standards,
// refactor style guide, and reflections into a post-mortem review prompt.
func TestBuildReviewPrompt(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildReviewPrompt(dir, "/tmp/project", "/tmp/project/.ralph", "some reflections")
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
	_, err := BuildReviewPrompt("/nonexistent", "/tmp", "/tmp/.ralph", "")
	if err == nil {
		t.Error("expected error for missing prompts directory")
	}
}

// Proves: BuildReviewPrompt includes bead creation guidance so the review agent
// knows how to create well-formed beads with acceptance criteria and labels.
func TestBuildReviewPrompt_IncludesBeadCreationGuidance(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildReviewPrompt(dir, "/tmp/project", "/tmp/project/.ralph", "")
	if err != nil {
		t.Fatalf("BuildReviewPrompt error: %v", err)
	}

	required := []struct {
		substr string
		reason string
	}{
		{"How to work with issues", "review prompt must include shared bead creation section header"},
		{"--acceptance", "review prompt must include acceptance criteria requirement"},
		{"verbatim", "review prompt must include diagnostic content verbatim rule"},
		{"echo back", "review prompt must include echo-back rule"},
		{"never update or reopen", "review prompt must include rule against updating or reopening closed beads"},
	}

	for _, tc := range required {
		if !strings.Contains(strings.ToLower(result), strings.ToLower(tc.substr)) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: task-manager.md instructs the agent to never attempt manual gh-based
// stack drains, and instead prompts the user to run `ralph merge <top-pr-number>`,
// because ralph merge does the critical up-front --update-refs rebase that prevents
// downstream PRs from being auto-closed (the tabi 2026-04-16 cascade).
func TestTaskManagerPrompt_StackMergeSection(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	required := []struct {
		substr string
		reason string
	}{
		{"Stack merges", "prompt must have a Stack merges section"},
		{"ralph merge", "prompt must specify the exact ralph merge command"},
		{"gh pr merge", "prompt must prohibit gh pr merge on stacked PRs"},
		{"gh pr close", "prompt must prohibit gh pr close on stacked PRs"},
		{"--update-refs", "prompt must explain that ralph merge does the critical --update-refs rebase"},
		{"top", "prompt must include guidance for identifying the top PR of a stack"},
		{"never", "prompt must explicitly prohibit manual stack drain operations"},
	}

	for _, tc := range required {
		if !strings.Contains(strings.ToLower(result), strings.ToLower(tc.substr)) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: BuildReviewPrompt includes task creation quality guidelines so
// the review agent creates beads with sufficient detail for loop agents to
// execute without needing to infer architectural intent.
func TestBuildReviewPrompt_IncludesQualityGuidelines(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildReviewPrompt(dir, "/tmp/project", "/tmp/project/.ralph", "")
	if err != nil {
		t.Fatalf("BuildReviewPrompt error: %v", err)
	}

	required := []struct {
		substr string
		reason string
	}{
		{"Task creation quality guidelines", "review prompt must include quality guidelines section"},
		{"higher reasoning instructs lower reasoning", "must include detail principle"},
		{"Concrete task patterns", "must include concrete task patterns section"},
		{"regression guards", "must include acceptance criteria as regression guards"},
		{"Anti-pattern", "must name anti-patterns explicitly"},
	}

	for _, tc := range required {
		if !strings.Contains(result, tc.substr) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: BuildTaskManagerPrompt includes task creation quality guidelines
// from the shared bead-creation.md file so the task manager creates beads
// with detail sufficient for loop agents to execute mechanically.
func TestBuildTaskManagerPrompt_IncludesQualityGuidelines(t *testing.T) {
	dir := promptsDir(t)
	result, err := BuildTaskManagerPrompt(dir, "/proj", "/proj/.ralph", "")
	if err != nil {
		t.Fatalf("BuildTaskManagerPrompt error: %v", err)
	}

	required := []struct {
		substr string
		reason string
	}{
		{"Task creation quality guidelines", "task manager must include quality guidelines section"},
		{"higher reasoning instructs lower reasoning", "must include detail principle"},
		{"Concrete task patterns", "must include concrete task patterns section"},
		{"regression guards", "must include acceptance criteria as regression guards"},
		{"Anti-pattern", "must name anti-patterns explicitly"},
	}

	for _, tc := range required {
		if !strings.Contains(result, tc.substr) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: internal.md contains the rebase baseline instruction, so an agent
// that receives commits from main via evolve/rebase does not treat them as
// regressions to fix — the rebased state is the new baseline.
func TestInternalPrompt_RebaseBaselineInstruction(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "internal.md"))
	if err != nil {
		t.Fatalf("reading internal.md: %v", err)
	}
	s := string(content)

	required := []struct {
		substr string
		reason string
	}{
		{"rebase", "prompt must address rebase scenario"},
		{"Never revert", "prompt must explicitly prohibit reverting rebased changes"},
		{"new baseline", "prompt must state rebased state is the new baseline"},
		{"verification passes", "prompt must make verification the gate, not diff shape"},
		{"Boy Scout Rule", "prompt must clarify the Boy Scout Rule still applies"},
	}

	for _, tc := range required {
		if !strings.Contains(s, tc.substr) {
			t.Errorf("internal.md missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: bead-creation.md requires diagnosing the root cause before creating
// a bead — agents must identify the specific file/function/cause and title the
// bead around the cause, not the observed symptom.
func TestBeadCreation_RequiresDiagnosisBeforeCreating(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "bead-creation.md"))
	if err != nil {
		t.Fatalf("reading bead-creation.md: %v", err)
	}
	s := string(content)
	lower := strings.ToLower(s)

	required := []struct {
		substr string
		reason string
	}{
		{"diagnos", "must require diagnosis before creating a bead"},
		{"root cause", "must require identifying the root cause"},
		{"symptom", "must distinguish symptom from cause in bead titles"},
		{"cause", "must instruct titling beads around the cause"},
	}

	for _, tc := range required {
		if !strings.Contains(lower, tc.substr) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}
}

// Proves: the assembled loop-agent prompt includes the diagnosis-before-creating
// requirement from bead-creation.md so loop agents investigate before filing bugs.
func TestBuildPrompt_IncludesDiagnosisRequirement(t *testing.T) {
	v := testVars(t)
	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	lower := strings.ToLower(result)

	if !strings.Contains(lower, "root cause") {
		t.Error("loop prompt must include root cause diagnosis requirement from bead-creation.md")
	}
	if !strings.Contains(lower, "symptom") {
		t.Error("loop prompt must include symptom vs cause distinction from bead-creation.md")
	}
}

// Proves: the assembled prompt includes the rebase baseline instruction even
// when the agent has stale attempt history referencing pre-rebase state —
// simulating evolve pulling user commits while reflections say "regressed".
func TestBuildPrompt_RebaseBaselineInstructionPresentWithStaleAttemptHistory(t *testing.T) {
	v := testVars(t)
	// Simulate stale reflection: agent previously noted a "regression" in a
	// file that was actually modified by a user commit pulled via rebase.
	v.AttemptHistory = "## Previous attempts on this task\n" +
		"### Attempt 1\n" +
		"Summary: attempted to fix config.go but it regressed — status check broken\n" +
		"Changes: config.go 3 insertions\n" +
		"Analysis: warn:stuck\n"

	result, err := BuildPrompt(v)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}

	// Stale reflection must be present (test setup check)
	if !strings.Contains(result, "regressed") {
		t.Error("test setup: stale reflection content should be present in prompt")
	}

	// Rebase baseline instruction must still be present to override stale reflection
	if !strings.Contains(result, "new baseline") {
		t.Error("prompt must contain 'new baseline' instruction so agent doesn't revert rebased changes")
	}
	if !strings.Contains(result, "Never revert") {
		t.Error("prompt must explicitly prohibit reverting rebased changes")
	}
}

// Proves: bead-creation.md gates substantive bead creation on a pre-creation
// architecture echo — the task manager must emit the proposed approach and wait
// for explicit user confirmation before calling bd create.
func TestBeadCreation_PreCreationArchitectureEcho(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "bead-creation.md"))
	if err != nil {
		t.Fatalf("reading bead-creation.md: %v", err)
	}
	s := string(content)
	lower := strings.ToLower(s)

	required := []struct {
		substr string
		reason string
	}{
		{"Pre-creation architecture echo", "must have a named Pre-creation architecture echo section"},
		{"feature", "trigger conditions must include feature type"},
		{"refactor", "trigger conditions must include refactor/extraction scope tasks"},
		{"clearly identified", "must distinguish clearly-identified cause (exempt) from unclear cause (trigger)"},
		{"multiple", "trigger conditions must include bugs with multiple plausible fix paths"},
		{"files and functions", "echo contents must require listing files and functions to be modified"},
		{"shape", "echo contents must require describing the shape of the change"},
		{"call-site", "echo contents must require describing call-site impact"},
		{"ac sketch", "echo contents must require an AC sketch"},
		{"explicit", "must require waiting for explicit user confirmation before bd create"},
	}

	for _, tc := range required {
		if !strings.Contains(lower, strings.ToLower(tc.substr)) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}

	// Section placement: echo section must appear before 'Creating beads'
	echoIdx := strings.Index(s, "Pre-creation architecture echo")
	creatingIdx := strings.Index(s, "Creating beads")
	if echoIdx < 0 {
		t.Fatal("Pre-creation architecture echo section not found")
	}
	if creatingIdx < 0 {
		t.Fatal("Creating beads section not found")
	}
	if echoIdx > creatingIdx {
		t.Error("Pre-creation architecture echo section must appear before 'Creating beads'")
	}
}

// Proves: bead-creation.md requires a minimal DOM markup repro in the description
// for all UI/DOM/visual bug beads, independent of whether the architecture echo fires.
func TestBeadCreation_DOMToyCase(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(promptsDir(t), "bead-creation.md"))
	if err != nil {
		t.Fatalf("reading bead-creation.md: %v", err)
	}
	s := string(content)
	lower := strings.ToLower(s)

	required := []struct {
		substr string
		reason string
	}{
		{"ui/dom", "must name UI/DOM/visual bug beads as the target scope"},
		{"constructed repro", "must specify the constructed-repro path when user provides no markup"},
		{"⚠️", "constructed repro must use the ⚠️ marker as specified"},
		{"verbatim", "must require verbatim inclusion of user-pasted markup"},
		{"infer", "rationale must explain agents infer/invent their own version of the problem without a toy case"},
	}

	for _, tc := range required {
		if !strings.Contains(lower, tc.substr) {
			t.Errorf("missing %q: %s", tc.substr, tc.reason)
		}
	}

	// DOM toy case must appear inside the 'Creating beads' section (after its header)
	creatingIdx := strings.Index(s, "### Creating beads")
	domIdx := strings.Index(s[creatingIdx:], "DOM")
	if creatingIdx < 0 || domIdx < 0 {
		t.Error("DOM toy case requirement must appear within the 'Creating beads' section")
	}
}
