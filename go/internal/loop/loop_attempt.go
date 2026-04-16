package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AttemptEvent records one failed agent run within the current iteration.
// Scoped to the iteration — lost on re-exec.
type AttemptEvent struct {
	Summary  string
	DiffStat string
	Analysis string
}

// renderAttemptHistory formats in-memory attempt events as markdown.
// Returns empty string when events is empty.
func renderAttemptHistory(events []AttemptEvent, maxAttempts int) string {
	if len(events) == 0 {
		return ""
	}
	tail := events
	if maxAttempts > 0 && len(tail) > maxAttempts {
		tail = tail[len(tail)-maxAttempts:]
	}
	var b strings.Builder
	for i, ev := range tail {
		changes := ev.DiffStat
		if changes == "" {
			changes = "none"
		}
		fmt.Fprintf(&b, "### Attempt %d\nSummary: %s\nChanges: %s\nAnalysis: %s\n\n",
			i+1, ev.Summary, changes, ev.Analysis)
	}
	return b.String()
}

// reflectionEntry holds a single reflection file's content and identity.
type reflectionEntry struct {
	TaskID  string
	Content string
}

// recentReflections returns the n most recent reflection files from
// ralphDir/reflections, sorted by modification time (oldest first),
// excluding the file matching excludeKey.
func recentReflections(ralphDir, excludeKey string, n int) []reflectionEntry {
	refDir := filepath.Join(ralphDir, "reflections")
	entries, err := os.ReadDir(refDir)
	if err != nil {
		return nil
	}

	type fileEntry struct {
		taskID  string
		path    string
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

	result := make([]reflectionEntry, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		result = append(result, reflectionEntry{
			TaskID:  f.taskID,
			Content: string(data),
		})
	}
	return result
}

// recordAttempt appends an event to the in-memory attempt slice for the current task.
func (l *Loop) recordAttempt(ev AttemptEvent) {
	l.taskAttempts = append(l.taskAttempts, ev)
}

// clearAttempts resets the in-memory attempt state after a task completes.
func (l *Loop) clearAttempts() {
	l.taskAttempts = nil
	l.taskIdleTimeouts = 0
}

// maxIdleTimeoutFailures returns the configured limit (defaulting to 3).
func (l *Loop) maxIdleTimeoutFailures() int {
	if l.cfg.MaxIdleTimeoutFailures > 0 {
		return l.cfg.MaxIdleTimeoutFailures
	}
	return 3
}
