package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/attempts"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/prompt"
	"github.com/brokenalarms/ralph/internal/tasks"
)

const maxCrossTaskReflections = 3

func buildTaskPrompt(nextTask, taskID string, backend tasks.Backend, promptsDir, ralphDir string) string {
	if taskID == "" {
		return fmt.Sprintf("Complete this task: %s", nextTask)
	}
	if backend != nil {
		full, err := backend.GetFullContext(taskID)
		if err == nil && full != "" {
			tmplPath := filepath.Join(promptsDir, "task-assignment.md")
			if data, readErr := os.ReadFile(tmplPath); readErr == nil {
				p := string(data)
				p = strings.ReplaceAll(p, "{{TASK_ID}}", taskID)
				p = strings.ReplaceAll(p, "{{TASK_CONTEXT}}", full)

				screenshots := prompt.ScreenshotsForBead(ralphDir, taskID)
				p += prompt.FormatScreenshotContext(screenshots)

				return p
			}
		}
	}
	return fmt.Sprintf("Complete this task (bd id: %s): %s", taskID, nextTask)
}

func buildPrompt(taskPrompt, attemptHistory, testStatus string, backend tasks.Backend, promptsDir, projectDir, workDir, ralphDir, planFile string, signals claude.SignalPaths) (string, error) {
	tasksContext, err := backend.ProjectContext()
	if err != nil {
		logging.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "ProjectContext: %v", err)
	}

	return prompt.BuildPrompt(prompt.Vars{
		PromptsDir:       promptsDir,
		ProjectDir:       projectDir,
		WorkDir:          workDir,
		RalphDir:         ralphDir,
		PlanFile:         planFile,
		SignalToken:      signals.Complete,
		CurrentTaskToken: signals.CurrentTask,
		AllCompleteToken: signals.AllComplete,
		TaskPrompt:       taskPrompt,
		AttemptHistory:   attemptHistory,
		TestStatus:       testStatus,
		TasksContext:     tasksContext,
		TaskBackend:      prompt.BackendBD,
	})
}

// buildAttemptContext assembles attempt history, reflections, and cross-task
// learnings into a single block for the prompt. Returns empty string if no
// prior context exists.
func buildAttemptContext(taskID, taskName string, tracker *attempts.Tracker, ralphDir string) string {
	var parts []string

	// Same-task attempt history (retries of this specific task)
	if history := tracker.Read(taskID, taskName); history != "" {
		parts = append(parts, "## Previous attempts on this task\n"+history)
	}

	// Same-task reflection
	if reflection := readReflection(ralphDir, taskID, taskName); reflection != "" {
		parts = append(parts, "### Previous reflection\n"+reflection)
	}

	// Cross-task learnings: recent reflections from other completed tasks
	excludeKey := taskID
	if excludeKey == "" {
		excludeKey = git.Slugify(taskName)
	}

	reflections := tracker.RecentReflections(excludeKey, maxCrossTaskReflections)
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
// Uses task ID if available, falls back to slugified task name.
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
