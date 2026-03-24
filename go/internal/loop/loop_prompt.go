package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/prompt"
)

const maxCrossTaskReflections = 3
const maxCrossTaskAttempts = 3

func (l *Loop) buildTaskPrompt(nextTask, taskID string) string {
	if taskID == "" {
		return fmt.Sprintf("Complete this task: %s", nextTask)
	}
	if l.cfg.TaskBackend != nil {
		full, err := l.cfg.TaskBackend.GetFullContext(taskID)
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

func (l *Loop) buildPrompt(taskPrompt, attemptHistory, testStatus string) (string, error) {
	beadsContext, err := l.cfg.TaskBackend.ProjectContext()
	if err != nil {
		l.logger.Warn("beads", "ProjectContext: %v", err)
	}

	return prompt.BuildPrompt(prompt.Vars{
		PromptsDir:       l.cfg.Dirs.PromptsDir,
		ProjectDir:       l.cfg.Dirs.ProjectDir,
		WorkDir:          l.git.WorkDir,
		RalphDir:         l.cfg.Dirs.RalphDir,
		PlanFile:         l.cfg.PlanFile,
		SignalToken:      l.signals.Complete,
		CurrentTaskToken: l.signals.CurrentTask,
		AllCompleteToken: l.signals.AllComplete,
		TaskPrompt:       taskPrompt,
		AttemptHistory:   attemptHistory,
		TestStatus:       testStatus,
		BeadsContext:     beadsContext,
		TaskBackend:      prompt.BackendBD,
	})
}

// buildAttemptContext assembles attempt history, reflections, and cross-task
// learnings into a single block for the prompt. Returns empty string if no
// prior context exists.
func (l *Loop) buildAttemptContext(taskID, taskName string) string {
	var parts []string

	// Same-task attempt history (retries of this specific task)
	if history := l.attempts.Read(taskID, taskName); history != "" {
		parts = append(parts, "## Previous attempts on this task\n"+history)
	}

	// Same-task reflection
	if reflection := l.readReflection(taskID, taskName); reflection != "" {
		parts = append(parts, "### Previous reflection\n"+reflection)
	}

	// Cross-task learnings: recent reflections from other completed tasks
	excludeKey := taskID
	if excludeKey == "" {
		excludeKey = git.Slugify(taskName)
	}

	var crossParts []string

	reflections := l.attempts.RecentReflections(excludeKey, maxCrossTaskReflections)
	for _, r := range reflections {
		crossParts = append(crossParts, fmt.Sprintf("### %s\n%s", r.TaskID, r.Content))
	}

	recentAttempts := l.attempts.RecentAttemptEntries(excludeKey, maxCrossTaskAttempts)
	if recentAttempts != "" {
		crossParts = append(crossParts, "### Recent attempt outcomes\n"+recentAttempts)
	}

	if len(crossParts) > 0 {
		parts = append(parts, "## Recent learnings from previous tasks\n"+strings.Join(crossParts, "\n"))
	}

	return strings.Join(parts, "\n")
}

func (l *Loop) readFeedback() string {
	feedbackFile := filepath.Join(l.cfg.Dirs.RalphDir, "feedback")
	data, err := os.ReadFile(feedbackFile)
	if err != nil || len(data) == 0 {
		return ""
	}
	return string(data)
}

// readReflection returns the content of a previous reflection file for a task.
// Uses task ID if available, falls back to slugified task name.
func (l *Loop) readReflection(taskID, taskName string) string {
	key := taskID
	if key == "" {
		key = git.Slugify(taskName)
	}
	path := filepath.Join(l.cfg.Dirs.RalphDir, "reflections", key+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
