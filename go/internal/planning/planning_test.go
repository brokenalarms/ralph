package planning

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
)

// mockBackend implements tasks.Backend for testing without real task systems.
type mockBackend struct {
	label              string
	needsPlanning      bool
	planningSucceeded  bool
	totalTasks         int
	planningInstr      string
	planningSucceedSeq []bool // if set, PlanningSucceeded returns these in order
	callIdx            int
}

func (m *mockBackend) Init() error                            { return nil }
func (m *mockBackend) HasRemaining() (bool, error)            { return false, nil }
func (m *mockBackend) CountCompleted() (int, error)           { return 0, nil }
func (m *mockBackend) CountRemaining() (int, error)           { return 0, nil }
func (m *mockBackend) CountTotal() (int, error)               { return m.totalTasks, nil }
func (m *mockBackend) GetNextTask() (string, error)           { return "", nil }
func (m *mockBackend) GetNextTaskID() (string, error)         { return "", nil }
func (m *mockBackend) HasTasks() (bool, error)                { return m.totalTasks > 0, nil }
func (m *mockBackend) CloseTask(string, string) error         { return nil }
func (m *mockBackend) ExecutionInstructions() (string, error) { return "", nil }
func (m *mockBackend) Label() string                          { return m.label }
func (m *mockBackend) PlanningInstructions() string           { return m.planningInstr }

func (m *mockBackend) NeedsPlanning() (bool, error) {
	return m.needsPlanning, nil
}

func (m *mockBackend) PlanningSucceeded() (bool, error) {
	if m.planningSucceedSeq != nil && m.callIdx < len(m.planningSucceedSeq) {
		v := m.planningSucceedSeq[m.callIdx]
		m.callIdx++
		return v, nil
	}
	return m.planningSucceeded, nil
}

// testDeps creates a Deps with temp directories and sensible defaults for testing.
func testDeps(t *testing.T) (Deps, string) {
	t.Helper()
	tmp := t.TempDir()

	ralphDir := filepath.Join(tmp, ".ralph")
	promptsDir := filepath.Join(tmp, "prompts")
	workDir := filepath.Join(tmp, "project")

	for _, d := range []string{ralphDir, promptsDir, workDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Write minimal prompt templates so buildPrompt doesn't fail.
	writeFile(t, filepath.Join(promptsDir, "interactive-planning.md"),
		"Interactive: {{WORK_DIR}} {{RALPH_DIR}} {{STATE_FILE}} {{TASK_INSTRUCTIONS}} {{PLAN_FILE_LINE}}")
	writeFile(t, filepath.Join(promptsDir, "planning.md"),
		"Auto: {{PLANNING_CONTEXT}} {{PLAN_FILE}} {{RALPH_DIR}} {{STATE_FILE}} {{TASK_INSTRUCTIONS}}")

	store := state.NewStore(ralphDir)
	if err := store.Save(state.State{Status: "initialized"}); err != nil {
		t.Fatal(err)
	}

	log := logging.New(nil)

	return Deps{
		Backend:    &mockBackend{label: "checklist", needsPlanning: true, totalTasks: 3, planningSucceeded: true, planningInstr: "write tasks to plan.md"},
		StateStore: store,
		Logger:     log,
		PromptsDir: promptsDir,
		WorkDir:    workDir,
		RalphDir:   ralphDir,
		RunClaude:  func(string) error { return nil },
	}, tmp
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// Pre-made plan file is copied and status set to "planned".
func TestCopyPreMadePlanFile(t *testing.T) {
	d, tmp := testDeps(t)

	srcPlan := filepath.Join(tmp, "external-plan.md")
	writeFile(t, srcPlan, "- [ ] Task A\n- [ ] Task B\n- [ ] Task C\n")

	d.PlanFile = srcPlan
	d.SkipPlanning = true

	if err := Run(d); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	s, _ := d.StateStore.Load()
	if s.Status != "planned" {
		t.Errorf("status = %q, want %q", s.Status, "planned")
	}
}

// When resuming with status past "initialized", planning is skipped entirely.
func TestResumeSkipsPlanning(t *testing.T) {
	d, _ := testDeps(t)
	d.StateStore.Set("status", "planned")

	claudeCalled := false
	d.RunClaude = func(string) error {
		claudeCalled = true
		return nil
	}

	if err := Run(d); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if claudeCalled {
		t.Error("RunClaude should not be called when resuming past initialized")
	}
}

// When SkipPlanning is set, interactive session is skipped but autonomous runs.
func TestSkipPlanningGoesDirectToAutonomous(t *testing.T) {
	d, _ := testDeps(t)
	d.SkipPlanning = true

	// Backend says it needs planning, but first PlanningSucceeded check
	// returns false (no interactive) then second returns true (autonomous).
	d.Backend = &mockBackend{
		label:              "checklist",
		needsPlanning:      true,
		totalTasks:         5,
		planningInstr:      "write tasks",
		planningSucceedSeq: []bool{false, true},
	}

	autonomousCalled := false
	d.RunClaude = func(prompt string) error {
		autonomousCalled = true
		return nil
	}

	if err := Run(d); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if !autonomousCalled {
		t.Error("autonomous planning should have been called when SkipPlanning is set")
	}
}

// Autonomous planning failure (no tasks created) returns an error.
func TestAutonomousPlanningFailure(t *testing.T) {
	d, _ := testDeps(t)
	d.SkipPlanning = true
	d.Backend = &mockBackend{
		label:         "checklist",
		needsPlanning: true,
		planningInstr: "write tasks",
		// PlanningSucceeded always returns false.
		planningSucceeded: false,
	}

	d.RunClaude = func(string) error { return nil }

	err := Run(d)
	if err == nil {
		t.Fatal("expected error when planning produces no tasks")
	}
	if !strings.Contains(err.Error(), "no tasks created") {
		t.Errorf("error = %q, want it to mention 'no tasks created'", err)
	}
}

// Interactive planning prompt includes backend-specific instructions and
// substitutes all template variables.
func TestInteractivePromptSubstitution(t *testing.T) {
	d, _ := testDeps(t)

	prompt, err := buildInteractivePrompt(d)
	if err != nil {
		t.Fatalf("buildInteractivePrompt() error: %v", err)
	}

	for _, want := range []string{d.WorkDir, d.RalphDir, d.StateStore.Path(), "write tasks to plan.md"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	// No unresolved placeholders.
	if strings.Contains(prompt, "{{") {
		t.Errorf("prompt has unresolved placeholders: %s", prompt)
	}
}

// Autonomous planning prompt substitutes context and backend instructions.
func TestAutonomousPromptSubstitution(t *testing.T) {
	d, _ := testDeps(t)
	d.Prompt = "build auth system"

	prompt, err := buildAutonomousPrompt(d)
	if err != nil {
		t.Fatalf("buildAutonomousPrompt() error: %v", err)
	}

	for _, want := range []string{"build auth system", d.RalphDir, d.StateStore.Path(), "write tasks to plan.md"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	if strings.Contains(prompt, "{{") {
		t.Errorf("prompt has unresolved placeholders: %s", prompt)
	}
}

// BD backend omits plan file line from interactive prompt.
func TestInteractivePromptBDBackendNoPlanFileLine(t *testing.T) {
	d, _ := testDeps(t)
	d.Backend = &mockBackend{label: "beads", planningInstr: "use bd create"}

	prompt, err := buildInteractivePrompt(d)
	if err != nil {
		t.Fatalf("buildInteractivePrompt() error: %v", err)
	}

	if strings.Contains(prompt, "Plan file:") {
		t.Error("BD backend prompt should not contain plan file line")
	}
}

// Checklist backend includes plan file line in interactive prompt.
func TestInteractivePromptChecklistIncludesPlanFile(t *testing.T) {
	d, _ := testDeps(t)

	prompt, err := buildInteractivePrompt(d)
	if err != nil {
		t.Fatalf("buildInteractivePrompt() error: %v", err)
	}

	if !strings.Contains(prompt, "Plan file:") {
		t.Error("Checklist backend prompt should contain plan file line")
	}
}

// After successful planning, state status is "planned" and worktree rename is called.
func TestFinalizeUpdatesStateAndRenames(t *testing.T) {
	d, _ := testDeps(t)
	d.SkipPlanning = true

	// Write theme to state so rename picks it up.
	d.StateStore.Set("theme", "auth rewrite")

	var renamedTheme string
	d.RenameWorktree = func(theme string) error {
		renamedTheme = theme
		return nil
	}

	if err := Run(d); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	s, _ := d.StateStore.Load()
	if s.Status != "planned" {
		t.Errorf("status = %q, want %q", s.Status, "planned")
	}
	if renamedTheme != "auth rewrite" {
		t.Errorf("renamed theme = %q, want %q", renamedTheme, "auth rewrite")
	}
}

// Theme fallback reads first line of plan file when state has no theme.
func TestRenameWorktreeThemeFallbackFromPlanFile(t *testing.T) {
	d, _ := testDeps(t)
	d.SkipPlanning = true

	// No theme in state, but plan file has a heading.
	planPath := planFilePath(d)
	writeFile(t, planPath, "# Database migration\n- [ ] Task 1\n")

	var renamedTheme string
	d.RenameWorktree = func(theme string) error {
		renamedTheme = theme
		return nil
	}

	if err := Run(d); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if renamedTheme != "Database migration" {
		t.Errorf("renamed theme = %q, want %q", renamedTheme, "Database migration")
	}
}

// When no theme and no plan file heading exist, worktree rename is not called.
func TestNoThemeNoRename(t *testing.T) {
	d, _ := testDeps(t)
	d.SkipPlanning = true

	renameCalled := false
	d.RenameWorktree = func(string) error {
		renameCalled = true
		return nil
	}

	if err := Run(d); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if renameCalled {
		t.Error("worktree rename should not be called when no theme exists")
	}
}

// planFilePath defaults to <ralph_dir>/plan.md when PlanFile is empty.
func TestPlanFilePathDefault(t *testing.T) {
	d, _ := testDeps(t)
	d.PlanFile = ""

	got := planFilePath(d)
	want := filepath.Join(d.RalphDir, "plan.md")
	if got != want {
		t.Errorf("planFilePath() = %q, want %q", got, want)
	}
}

// planFilePath uses PlanFile when set.
func TestPlanFilePathExplicit(t *testing.T) {
	d, _ := testDeps(t)
	d.PlanFile = "/custom/plan.md"

	got := planFilePath(d)
	if got != "/custom/plan.md" {
		t.Errorf("planFilePath() = %q, want %q", got, "/custom/plan.md")
	}
}

// Full run with autonomous planning: verifies Claude receives the right prompt
// and state ends up as "planned".
func TestFullAutonomousRun(t *testing.T) {
	d, _ := testDeps(t)
	d.SkipPlanning = true
	d.Prompt = "implement caching layer"
	d.Backend = &mockBackend{
		label:              "checklist",
		needsPlanning:      true,
		totalTasks:         4,
		planningInstr:      "write markdown checkboxes",
		planningSucceedSeq: []bool{false, true},
	}

	var capturedPrompt string
	d.RunClaude = func(prompt string) error {
		capturedPrompt = prompt
		return nil
	}

	if err := Run(d); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if !strings.Contains(capturedPrompt, "implement caching layer") {
		t.Error("autonomous prompt should contain user-supplied context")
	}
	if !strings.Contains(capturedPrompt, "write markdown checkboxes") {
		t.Error("autonomous prompt should contain backend planning instructions")
	}

	s, _ := d.StateStore.Load()
	if s.Status != "planned" {
		t.Errorf("status = %q, want %q", s.Status, "planned")
	}
}

// Pre-made plan file that doesn't exist on disk is a no-op (falls through
// to normal planning).
func TestCopyPlanFileNonExistent(t *testing.T) {
	d, tmp := testDeps(t)
	d.PlanFile = filepath.Join(tmp, "does-not-exist.md")
	d.SkipPlanning = true

	// Backend says planning succeeds after autonomous run.
	d.Backend = &mockBackend{
		label:              "checklist",
		needsPlanning:      true,
		totalTasks:         2,
		planningInstr:      "write tasks",
		planningSucceedSeq: []bool{false, true},
	}

	autonomousCalled := false
	d.RunClaude = func(string) error {
		autonomousCalled = true
		return nil
	}

	if err := Run(d); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if !autonomousCalled {
		t.Error("should fall through to autonomous when plan file doesn't exist")
	}
}

// State overflow fields survive the planning phase (we don't clobber them).
func TestStateOverflowPreserved(t *testing.T) {
	d, _ := testDeps(t)
	d.SkipPlanning = true

	// Write a custom overflow field.
	s, _ := d.StateStore.Load()
	if s.Overflow == nil {
		s.Overflow = make(map[string]json.RawMessage)
	}
	s.Overflow["custom_field"] = json.RawMessage(`"preserved"`)
	d.StateStore.Save(s)

	if err := Run(d); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	after, _ := d.StateStore.Load()
	if after.Overflow == nil {
		t.Fatal("overflow is nil after planning")
	}
	raw, ok := after.Overflow["custom_field"]
	if !ok {
		t.Fatal("custom_field lost from overflow")
	}
	if string(raw) != `"preserved"` {
		t.Errorf("custom_field = %s, want %q", raw, `"preserved"`)
	}
}

// When RunClaude returns an error, autonomous planning propagates it.
func TestAutonomousRunClaudeError(t *testing.T) {
	d, _ := testDeps(t)
	d.SkipPlanning = true
	d.Backend = &mockBackend{
		label:              "checklist",
		needsPlanning:      true,
		planningInstr:      "write tasks",
		planningSucceedSeq: []bool{false},
	}

	d.RunClaude = func(string) error {
		return fmt.Errorf("claude crashed")
	}

	err := Run(d)
	if err == nil {
		t.Fatal("expected error when RunClaude fails")
	}
	if !strings.Contains(err.Error(), "claude crashed") {
		t.Errorf("error = %q, want it to contain 'claude crashed'", err)
	}
}
