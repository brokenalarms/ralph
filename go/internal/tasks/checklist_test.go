package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupChecklist(t *testing.T, content string) *Checklist {
	t.Helper()
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan.md")
	if content != "" {
		if err := os.WriteFile(planFile, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return &Checklist{
		PlanFile:   planFile,
		PromptsDir: dir,
	}
}

// Proves: the iteration loop continues when unchecked items exist.
func TestHasRemaining_WithUnchecked(t *testing.T) {
	c := setupChecklist(t, "- [ ] Fix auth bug\n- [x] Add tests\n")
	got, err := c.HasRemaining()
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected HasRemaining=true when unchecked items exist")
	}
}

// Proves: the iteration loop stops when all items are checked.
func TestHasRemaining_AllDone(t *testing.T) {
	c := setupChecklist(t, "- [x] Fix auth bug\n- [x] Add tests\n")
	got, err := c.HasRemaining()
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected HasRemaining=false when all items checked")
	}
}

// Proves: completed/remaining/total counts are accurate for a mixed plan.
func TestCounts_MixedPlan(t *testing.T) {
	plan := `- [x] Done task 1
- [ ] Todo task 2
- [x] Done task 3
- [ ] Todo task 4
- [ ] Todo task 5
`
	c := setupChecklist(t, plan)

	completed, _ := c.CountCompleted()
	remaining, _ := c.CountRemaining()
	total, _ := c.CountTotal()

	if completed != 2 {
		t.Errorf("CountCompleted = %d, want 2", completed)
	}
	if remaining != 3 {
		t.Errorf("CountRemaining = %d, want 3", remaining)
	}
	if total != 5 {
		t.Errorf("CountTotal = %d, want 5", total)
	}
}

// Proves: ralph selects the first unchecked task in plan order.
func TestGetNextTask_ReturnsFirstUnchecked(t *testing.T) {
	plan := `- [x] Done task
- [ ] Second task is next
- [ ] Third task
`
	c := setupChecklist(t, plan)
	got, err := c.GetNextTask()
	if err != nil {
		t.Fatal(err)
	}
	if got != "Second task is next" {
		t.Errorf("GetNextTask = %q, want %q", got, "Second task is next")
	}
}

// Proves: HasTasks returns true when the plan contains checkbox items.
func TestHasTasks_WithTasks(t *testing.T) {
	c := setupChecklist(t, "- [ ] A task\n")
	got, err := c.HasTasks()
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected HasTasks=true")
	}
}

// Proves: HasTasks returns false when no plan file exists.
func TestHasTasks_NoPlanFile(t *testing.T) {
	c := &Checklist{PlanFile: "/nonexistent/plan.md"}
	got, err := c.HasTasks()
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("expected HasTasks=false without plan file")
	}
}

// Proves: counts return zero when the plan file doesn't exist.
func TestCounts_NoPlanFile(t *testing.T) {
	c := &Checklist{PlanFile: "/nonexistent/plan.md"}
	completed, _ := c.CountCompleted()
	remaining, _ := c.CountRemaining()
	total, _ := c.CountTotal()

	if completed != 0 || remaining != 0 || total != 0 {
		t.Errorf("expected all zeros, got completed=%d remaining=%d total=%d",
			completed, remaining, total)
	}
}

// Proves: GetNextTask returns empty when no unchecked items remain.
func TestGetNextTask_NoneRemaining(t *testing.T) {
	c := setupChecklist(t, "- [x] All done\n")
	got, err := c.GetNextTask()
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("GetNextTask = %q, want empty string", got)
	}
}

// Proves: GetNextTaskID always returns empty for checklist backend.
func TestGetNextTaskID_AlwaysEmpty(t *testing.T) {
	c := setupChecklist(t, "- [ ] A task\n")
	got, err := c.GetNextTaskID()
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("GetNextTaskID = %q, want empty", got)
	}
}

// Proves: indented checkboxes are recognized (nested task support).
func TestCounts_IndentedCheckboxes(t *testing.T) {
	plan := `- [ ] Top level
  - [ ] Indented task
  - [x] Indented done
`
	c := setupChecklist(t, plan)
	remaining, _ := c.CountRemaining()
	completed, _ := c.CountCompleted()
	total, _ := c.CountTotal()

	if remaining != 2 {
		t.Errorf("CountRemaining = %d, want 2", remaining)
	}
	if completed != 1 {
		t.Errorf("CountCompleted = %d, want 1", completed)
	}
	if total != 3 {
		t.Errorf("CountTotal = %d, want 3", total)
	}
}

// Proves: SkipTask marks the first unchecked task with [s] and appends the reason.
func TestChecklist_SkipTask(t *testing.T) {
	plan := "- [ ] Fix auth bug\n- [ ] Add tests\n"
	c := setupChecklist(t, plan)
	if err := c.SkipTask("", "stagnation"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.PlanFile)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "[s] Fix auth bug (stagnation)") {
		t.Errorf("expected skipped marker, got:\n%s", content)
	}
	if !strings.Contains(content, "[ ] Add tests") {
		t.Errorf("expected second task unchanged, got:\n%s", content)
	}
}

// Proves: ExecutionInstructions reads from the prompts directory and contains
// plan file and checkbox references.
func TestChecklist_ExecutionInstructions(t *testing.T) {
	c := setupChecklist(t, "")
	content := "Read the plan at {{PLAN_FILE}} and mark done with [x].\n"
	os.WriteFile(filepath.Join(c.PromptsDir, "execution-checklist.md"), []byte(content), 0644)
	got, err := c.ExecutionInstructions()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "{{PLAN_FILE}}") {
		t.Error("expected execution instructions to reference {{PLAN_FILE}}")
	}
	if !strings.Contains(got, "[x]") {
		t.Error("expected execution instructions to reference [x]")
	}
}

// Proves: Checklist satisfies the Backend interface at compile time.
var _ Backend = (*Checklist)(nil)
