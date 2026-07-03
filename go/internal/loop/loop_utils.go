package loop

import (
	"context"
	"os"
	"time"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/retry"
)

// isNewTask returns true when the next task differs from the last one stored
// in state. Prefers task ID comparison (stable across description edits);
// falls back to description when no ID is available.
func isNewTask(lastID, lastTask, taskID, taskDesc string) bool {
	if taskID != "" {
		return lastID != taskID
	}
	return lastTask != taskDesc
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

// waitForInternet blocks until internet connectivity is restored.
// Shows a single updating line in the terminal log, writes one summary
// line to the log file when restored. Returns false if context is cancelled.
// The logger parameter is the cross-module exception — see Loop.logger.
func waitForInternet(ctx context.Context, logger *logging.Logger, interval, checkTimeout time.Duration) bool {
	if git.IsOnline(checkTimeout) {
		return true
	}

	start := time.Now()
	logger.Emit(logging.Opts{Level: logging.Warn}, "Internet unreachable — waiting for connectivity...")

	err := retry.Retry(ctx, retry.BackoffOpts{Initial: interval, Max: interval}, nil, func() (bool, error) {
		if git.IsOnline(checkTimeout) {
			elapsed := time.Since(start).Truncate(time.Second)
			logger.Emit(logging.Opts{Level: logging.Success}, "Internet restored after %s", elapsed)
			return true, nil
		}
		elapsed := time.Since(start).Truncate(time.Second)
		logger.Emit(logging.Opts{}, "Internet still unreachable (%s elapsed)", elapsed)
		return false, nil
	})
	return err == nil
}
