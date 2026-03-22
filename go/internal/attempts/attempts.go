package attempts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/git"
)

// Tracker records and retrieves attempt history for tasks, enabling
// the prompt to include context about previous failed attempts.
type Tracker struct {
	RalphDir string
}

// New creates a Tracker that stores attempt logs in ralphDir/attempts/.
func New(ralphDir string) *Tracker {
	return &Tracker{RalphDir: ralphDir}
}

func (t *Tracker) attemptsDir() string {
	return filepath.Join(t.RalphDir, "attempts")
}

func (t *Tracker) attemptFile(taskID, taskName string) string {
	key := taskID
	if key == "" {
		key = git.Slugify(taskName)
	}
	return filepath.Join(t.attemptsDir(), key+".log")
}

// Record appends an attempt entry to the log file for the given task.
// Each attempt is numbered sequentially starting from 1.
func (t *Tracker) Record(taskID, taskName, summary, diffStat, analysis string) error {
	if err := os.MkdirAll(t.attemptsDir(), 0o755); err != nil {
		return err
	}

	path := t.attemptFile(taskID, taskName)
	existing, _ := os.ReadFile(path)
	attemptNum := strings.Count(string(existing), "### Attempt ") + 1

	changes := diffStat
	if changes == "" {
		changes = "none"
	}

	entry := fmt.Sprintf("### Attempt %d\nSummary: %s\nChanges: %s\nAnalysis: %s\n\n",
		attemptNum, summary, changes, analysis)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(entry)
	return err
}

const maxPromptAttempts = 3

// Read returns the most recent attempts for a task (capped at
// maxPromptAttempts). All attempts remain on disk; only the tail
// is returned to keep prompt context small.
func (t *Tracker) Read(taskID, taskName string) string {
	data, err := os.ReadFile(t.attemptFile(taskID, taskName))
	if err != nil {
		return ""
	}
	return lastNAttempts(string(data), maxPromptAttempts)
}

func lastNAttempts(content string, n int) string {
	parts := strings.Split(content, "### Attempt ")
	// First element is empty or preamble before the first attempt header.
	if len(parts) <= 1 {
		return content
	}
	attempts := parts[1:] // each starts with "N\n..."
	if len(attempts) <= n {
		return content
	}
	tail := attempts[len(attempts)-n:]
	var b strings.Builder
	for _, a := range tail {
		b.WriteString("### Attempt ")
		b.WriteString(a)
	}
	return b.String()
}

// Clear removes the attempt file for a task, used when a task is
// resolved and re-attempts should start fresh.
func (t *Tracker) Clear(taskID, taskName string) {
	os.Remove(t.attemptFile(taskID, taskName))
}
