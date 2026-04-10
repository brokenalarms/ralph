package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/prompt"
)

const maxCrossTaskReflections = 3

// buildTaskPrompt constructs the per-task assignment prompt from the task
// backend's full context. Reads l.taskBackend, l.cfg.Dirs.PromptsDir,
// and l.cfg.Dirs.RalphDir via the receiver.
func (l *Loop) buildTaskPrompt(nextTask, taskID string) string {
	if taskID == "" {
		return fmt.Sprintf("Complete this task: %s", nextTask)
	}
	if l.taskBackend != nil {
		full, err := l.taskBackend.GetFullContext(taskID)
		if err == nil && full != "" {
			tmplPath := filepath.Join(l.cfg.Dirs.PromptsDir, "task-assignment.md")
			if data, readErr := os.ReadFile(tmplPath); readErr == nil {
				p := string(data)
				p = strings.ReplaceAll(p, "{{TASK_ID}}", taskID)
				p = strings.ReplaceAll(p, "{{TASK_CONTEXT}}", full)

				screenshots := prompt.ScreenshotsForBead(l.cfg.Dirs.RalphDir, taskID)
				p += prompt.FormatScreenshotContext(screenshots)

				return p
			}
		}
	}
	return fmt.Sprintf("Complete this task (bd id: %s): %s", taskID, nextTask)
}

// buildPrompt assembles the full prompt for the agent from the task prompt,
// attempt history, test status, and tasks context fetched from the backend.
// Reads l.taskBackend, l.cfg.Dirs.*, l.cfg.PlanFile, and l.signals via
// the receiver.
func (l *Loop) buildPrompt(taskPrompt, attemptHistory, testStatus string) (string, error) {
	tasksContext, err := l.taskBackend.ProjectContext()
	if err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "ProjectContext: %v", err)
	}

	return prompt.BuildPrompt(prompt.Vars{
		PromptsDir:       l.cfg.Dirs.PromptsDir,
		ProjectDir:       l.cfg.Dirs.ProjectDir,
		WorkDir:          l.git.GetWorkDir(),
		RalphDir:         l.cfg.Dirs.RalphDir,
		PlanFile:         l.cfg.PlanFile,
		SignalToken:      l.signals.Complete,
		CurrentTaskToken: l.signals.CurrentTask,
		AllCompleteToken: l.signals.AllComplete,
		TaskPrompt:       taskPrompt,
		AttemptHistory:   attemptHistory,
		TestStatus:       testStatus,
		TasksContext:     tasksContext,
		TaskBackend:      prompt.BackendBD,
	})
}

// attemptContext assembles attempt history, reflections, and cross-task
// learnings into a single block for the prompt. Returns empty string if no
// prior context exists. Reads l.attempts and l.cfg.Dirs.RalphDir via the
// receiver.
func (l *Loop) attemptContext(taskID, taskName string) string {
	var parts []string

	// Same-task attempt history (retries of this specific task)
	if history := l.attempts.Read(taskID, taskName); history != "" {
		parts = append(parts, "## Previous attempts on this task\n"+history)
	}

	// Same-task reflection
	if reflection := readReflection(l.cfg.Dirs.RalphDir, taskID, taskName); reflection != "" {
		parts = append(parts, "### Previous reflection\n"+reflection)
	}

	// Cross-task learnings: recent reflections from other completed tasks
	excludeKey := taskID
	if excludeKey == "" {
		excludeKey = git.Slugify(taskName)
	}

	reflections := l.attempts.RecentReflections(excludeKey, maxCrossTaskReflections)
	if len(reflections) > 0 {
		var crossParts []string
		for _, r := range reflections {
			crossParts = append(crossParts, fmt.Sprintf("### %s\n%s", r.TaskID, r.Content))
		}
		parts = append(parts, "## Recent learnings from previous tasks\n"+strings.Join(crossParts, "\n"))
	}

	return strings.Join(parts, "\n")
}

// readReflection returns the content of a previous reflection file for a task.
// Uses task ID if available, falls back to slugified task name. Pure data
// helper — takes only paths and string keys.
func readReflection(ralphDir, taskID, taskName string) string {
	key := taskID
	if key == "" {
		key = git.Slugify(taskName)
	}
	path := filepath.Join(ralphDir, "reflections", key+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
