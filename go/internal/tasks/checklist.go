package tasks

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	uncheckedRe = regexp.MustCompile(`(?m)^\s*- \[ \]`)
	checkedRe   = regexp.MustCompile(`(?m)^\s*- \[x\]`)
	anyCheckRe  = regexp.MustCompile(`(?m)^\s*- \[[ x]\]`)
	// Captures the task text after the checkbox marker.
	taskTextRe = regexp.MustCompile(`^\s*- \[ \]\s*(.*)`)
)

// Checklist implements Backend using a markdown plan file with
// `- [ ]` / `- [x]` checkboxes.
type Checklist struct {
	PlanFile  string // path to the plan.md file
	PromptsDir string // directory containing prompt templates
}

func (c *Checklist) Init() error { return nil }

func (c *Checklist) HasRemaining() (bool, error) {
	content, err := c.readPlan()
	if err != nil {
		return false, nil
	}
	return uncheckedRe.MatchString(content), nil
}

func (c *Checklist) CountCompleted() (int, error) {
	content, err := c.readPlan()
	if err != nil {
		return 0, nil
	}
	return len(checkedRe.FindAllString(content, -1)), nil
}

func (c *Checklist) CountRemaining() (int, error) {
	content, err := c.readPlan()
	if err != nil {
		return 0, nil
	}
	return len(uncheckedRe.FindAllString(content, -1)), nil
}

func (c *Checklist) CountTotal() (int, error) {
	content, err := c.readPlan()
	if err != nil {
		return 0, nil
	}
	return len(anyCheckRe.FindAllString(content, -1)), nil
}

func (c *Checklist) GetNextTask() (string, error) {
	content, err := c.readPlan()
	if err != nil {
		return "", nil
	}
	for _, line := range strings.Split(content, "\n") {
		if m := taskTextRe.FindStringSubmatch(line); m != nil {
			return m[1], nil
		}
	}
	return "", nil
}

func (c *Checklist) GetNextTaskID() (string, error) {
	return "", nil
}

func (c *Checklist) GetNextTaskInfo() (string, string, error) {
	title, err := c.GetNextTask()
	return "", title, err
}

func (c *Checklist) HasTasks() (bool, error) {
	total, err := c.CountTotal()
	if err != nil {
		return false, err
	}
	return total > 0, nil
}

func (c *Checklist) NeedsPlanning() (bool, error) {
	_, err := os.Stat(c.PlanFile)
	return os.IsNotExist(err), nil
}

func (c *Checklist) PlanningSucceeded() (bool, error) {
	return c.HasTasks()
}

func (c *Checklist) CloseTask(_ string, _ string) error {
	return nil
}

func (c *Checklist) ReopenTask(_ string) error { return nil }

func (c *Checklist) SkipTask(_ string, reason string) error {
	if reason == "" {
		reason = "skipped"
	}
	content, err := c.readPlan()
	if err != nil {
		return nil
	}
	task, err := c.GetNextTask()
	if err != nil || task == "" {
		return nil
	}
	// Replace the first matching unchecked task with [s] marker and reason
	old := "- [ ] " + task
	replacement := "- [s] " + task + " (" + reason + ")"
	content = strings.Replace(content, old, replacement, 1)
	return os.WriteFile(c.PlanFile, []byte(content), 0644)
}

func (c *Checklist) ExecutionInstructions() (string, error) {
	path := c.PromptsDir + "/execution-checklist.md"
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading execution instructions: %w", err)
	}
	return string(data), nil
}

func (c *Checklist) PlanningInstructions() string {
	return fmt.Sprintf(`Write tasks to %s in markdown checkbox format:
- [ ] Task 1 description
- [ ] Task 2 description
IMPORTANT: beads/bd is NOT installed in this environment. Do NOT attempt to use bd commands — they will fail. You MUST write the plan file directly. If the user asks to use beads, explain that bd is not installed and they need to install it and restart ralph.`, c.PlanFile)
}

func (c *Checklist) SetState(_, _, _, _ string) error  { return nil }
func (c *Checklist) GetState(_, _ string) (string, error) { return "", nil }

func (c *Checklist) GetDescription(_ string) (string, error) { return "", nil }

func (c *Checklist) ProjectContext() (string, error) { return "", nil }

func (c *Checklist) Label() string { return "checklist" }

func (c *Checklist) readPlan() (string, error) {
	data, err := os.ReadFile(c.PlanFile)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
