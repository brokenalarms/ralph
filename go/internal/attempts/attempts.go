package attempts

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// ReflectionEntry holds a single reflection file's content and identity.
type ReflectionEntry struct {
	TaskID  string
	Content string
}

// RecentReflections returns the n most recent reflection files sorted by
// modification time (oldest first), excluding the file matching excludeKey.
func (t *Tracker) RecentReflections(excludeKey string, n int) []ReflectionEntry {
	refDir := filepath.Join(t.RalphDir, "reflections")
	entries, err := os.ReadDir(refDir)
	if err != nil {
		return nil
	}

	type fileEntry struct {
		taskID string
		path   string
		modTime int64
	}

	var files []fileEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		taskID := strings.TrimSuffix(e.Name(), ".md")
		if taskID == excludeKey {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileEntry{
			taskID:  taskID,
			path:    filepath.Join(refDir, e.Name()),
			modTime: info.ModTime().UnixNano(),
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime < files[j].modTime
	})

	if len(files) > n {
		files = files[len(files)-n:]
	}

	result := make([]ReflectionEntry, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		result = append(result, ReflectionEntry{
			TaskID:  f.taskID,
			Content: string(data),
		})
	}
	return result
}

// RecentAttemptEntries returns the last attempt entry from each of the n
// most recently modified attempt logs, excluding the file matching excludeKey.
// This provides cross-task context about recent halts, timeouts, and failures.
func (t *Tracker) RecentAttemptEntries(excludeKey string, n int) string {
	entries, err := os.ReadDir(t.attemptsDir())
	if err != nil {
		return ""
	}

	type fileEntry struct {
		taskID string
		path   string
		modTime int64
	}

	var files []fileEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		taskID := strings.TrimSuffix(e.Name(), ".log")
		if taskID == excludeKey {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileEntry{
			taskID:  taskID,
			path:    filepath.Join(t.attemptsDir(), e.Name()),
			modTime: info.ModTime().UnixNano(),
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime < files[j].modTime
	})

	if len(files) > n {
		files = files[len(files)-n:]
	}

	var b strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		lastEntry := lastNAttempts(string(data), 1)
		if lastEntry == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("#### Task: %s\n", f.taskID))
		b.WriteString(lastEntry)
	}
	return b.String()
}

// Clear removes the attempt file for a task, used when a task is
// resolved and re-attempts should start fresh.
func (t *Tracker) Clear(taskID, taskName string) {
	os.Remove(t.attemptFile(taskID, taskName))
}
