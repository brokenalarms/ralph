package loop

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
)

// isNewTask returns true when the next task differs from the last one stored
// in state. Prefers task ID comparison (stable across description edits);
// falls back to description when no ID is available.
func isNewTask(st *state.Store, taskID, taskDesc string) bool {
	if taskID != "" {
		lastID, _ := st.Read("last_task_id")
		return lastID != taskID
	}
	lastTask, _ := st.Read("last_task")
	return lastTask != taskDesc
}

// persistCompletedTask writes a completed task entry to state.json so
// ralph-task can verify tasks weren't falsely closed and setStackHead can
// find unmerged branches for stacking.
func persistCompletedTask(st *state.Store, logger *logging.Logger, taskID string, merged bool) {
	if taskID == "" {
		return
	}
	if err := st.AddCompletedTask(taskID, merged); err != nil {
		logger.Warn("state", "AddCompletedTask: %v", err)
	}
}

func fileLineCount(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	count := 0
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}
	return count
}

func readLogFrom(path string, startLine int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := 0
	offset := 0
	for i, b := range data {
		if b == '\n' {
			lines++
			if lines >= startLine {
				offset = i + 1
				break
			}
		}
	}
	if offset >= len(data) {
		return ""
	}
	return string(data[offset:])
}

// skipTask sets the task back to open in bd, records the reason as a
// comment, and adds the ID to both the backend's in-memory skip set
// and the state.json skipped_tasks list so it stays excluded from
// future selection.
func skipTask(backend tasks.Backend, st *state.Store, logger *logging.Logger, id, reason string) {
	if id == "" {
		return
	}
	logger.Warn("beads", "Skipping task %s: %s", id, reason)
	if err := backend.SkipTask(id, reason); err != nil {
		logger.Warn("beads", "Failed to skip task %s in backend: %v", id, err)
	}
	if st != nil {
		if err := st.AddSkippedTask(id); err != nil {
			logger.Warn("beads", "Failed to persist skip for %s: %v", id, err)
		}
		skipped, _ := st.GetSkippedTasks()
		backend.SetSkippedIDs(skipped)
	}
}

// isOnline checks internet connectivity with a quick DNS lookup.
func isOnline() bool {
	conn, err := net.DialTimeout("tcp", "api.anthropic.com:443", 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// waitForInternet blocks until internet connectivity is restored.
// Shows a single updating line in the terminal log, writes one summary
// line to the log file when restored. Returns false if context is cancelled.
func waitForInternet(ctx context.Context, logger *logging.Logger) bool {
	if isOnline() {
		return true
	}

	start := time.Now()
	logger.Warn("", "Internet unreachable — waiting for connectivity...")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if isOnline() {
				elapsed := time.Since(start).Truncate(time.Second)
				logger.Success("", "Internet restored after %s", elapsed)
				return true
			}
			elapsed := time.Since(start).Truncate(time.Second)
			logger.Log("", "Internet still unreachable (%s elapsed)", elapsed)
		}
	}
}

// runPostTask executes the --post-task script if configured. Runs in the
// project directory with RALPH_TASK_ID, RALPH_PR_NUMBER, and RALPH_MERGED
// env vars. Non-zero exit warns and continues.
func (l *Loop) runPostTask(ctx context.Context, taskID, prNumber string, merged bool) {
	if l.cfg.PostTask == "" {
		return
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", l.cfg.PostTask)
	cmd.Dir = l.cfg.Dirs.ProjectDir
	cmd.Env = append(os.Environ(),
		"RALPH_TASK_ID="+taskID,
		"RALPH_PR_NUMBER="+prNumber,
		"RALPH_MERGED="+fmt.Sprintf("%t", merged),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	l.logger.Log("post-task", "Running %s (task=%s pr=%s merged=%t)", l.cfg.PostTask, taskID, prNumber, merged)
	if err := cmd.Run(); err != nil {
		l.logger.Warn("post-task", "Script exited with error: %v", err)
	}
}
