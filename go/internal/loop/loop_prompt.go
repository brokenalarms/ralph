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

	result, err := prompt.BuildPrompt(prompt.Vars{
		PromptsDir:        l.cfg.Dirs.PromptsDir,
		ProjectDir:        l.cfg.Dirs.ProjectDir,
		WorkDir:           l.git.GetWorkDir(),
		RalphDir:          l.cfg.Dirs.RalphDir,
		PlanFile:          l.cfg.PlanFile,
		SignalToken:       l.signals.Complete,
		CurrentTaskToken:  l.signals.CurrentTask,
		AllCompleteToken:  l.signals.AllComplete,
		NoCodeNeededToken: l.signals.NoCodeNeeded,
		TaskPrompt:        taskPrompt,
		AttemptHistory:    attemptHistory,
		TestStatus:        testStatus,
		TasksContext:      tasksContext,
		TaskBackend:       prompt.BackendBD,
	})
	if err != nil {
		return "", err
	}

	inputSum := len(taskPrompt) + len(attemptHistory) + len(testStatus) + len(tasksContext)
	l.logger.Emit(logging.Opts{Domain: logging.LLM},
		"Prompt sizes — taskPrompt: %dB, attemptHistory: %dB, testStatus: %dB, tasksContext: %dB, total: %dB (template overhead: %dB)",
		len(taskPrompt), len(attemptHistory), len(testStatus), len(tasksContext), len(result), len(result)-inputSum)

	return result, nil
}

// attemptContext assembles attempt history, reflections, and cross-task
// learnings into a single block for the prompt. Returns empty string if no
// prior context exists. Reads l.taskAttempts and l.cfg.Dirs.RalphDir via
// the receiver.
func (l *Loop) attemptContext(taskID, taskName string) string {
	var parts []string

	history := renderAttemptHistory(l.taskAttempts, l.cfg.MaxPromptAttempts)
	if history != "" {
		parts = append(parts, "## Previous attempts on this task\n"+history)
	}

	ownReflection := readReflection(l.cfg.Dirs.RalphDir, taskID, taskName)
	if ownReflection != "" {
		parts = append(parts, "### Previous reflection\n"+ownReflection)
	}

	excludeKey := taskID
	if excludeKey == "" {
		excludeKey = git.Slugify(taskName)
	}

	var crossTaskLearnings string
	reflections := recentReflections(l.cfg.Dirs.RalphDir, excludeKey, maxCrossTaskReflections)
	if len(reflections) > 0 {
		var crossParts []string
		for _, r := range reflections {
			crossParts = append(crossParts, fmt.Sprintf("### %s\n%s", r.TaskID, r.Content))
		}
		crossTaskLearnings = strings.Join(crossParts, "\n")
		parts = append(parts, "## Recent learnings from previous tasks\n"+crossTaskLearnings)
	}

	result := strings.Join(parts, "\n")
	l.logger.Emit(logging.Opts{Domain: logging.LLM},
		"attemptContext sizes — history: %dB, ownReflection: %dB, crossTaskLearnings: %dB, total: %dB",
		len(history), len(ownReflection), len(crossTaskLearnings), len(result))

	return result
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
