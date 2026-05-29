// Session logs (loop.log, raw.log) are written to a stable per-project
// location at ~/.ralph/logs/<slug>/ where <slug> is derived from the
// project directory name and a hash of its full path. This location
// survives worktree recreation, loop restarts, and evolve re-execs.
// Old log files are pruned on startup based on a configurable retention
// window (log_retention_days in config.toml, default 30 days).
package logging

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"time"
)

// StableLogDir returns a per-project log directory under ~/.ralph/logs/
// that survives worktree recreation. The slug embeds the project base
// name (human-readable) and a hash of the full path (collision-safe).
// Returns an error only when the user home directory is unavailable.
func StableLogDir(projectDir string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("StableLogDir: home dir unavailable: %w", err)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(projectDir))
	slug := filepath.Base(projectDir) + "-" + fmt.Sprintf("%08x", h.Sum32())[:6]
	return filepath.Join(home, ".ralph", "logs", slug), nil
}

// PruneLogs deletes files in logDir whose modification time is older than
// retentionDays days. Non-positive retentionDays disables pruning. Missing
// logDir is a no-op.
func PruneLogs(logDir string, retentionDays int) error {
	if retentionDays <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(logDir, e.Name()))
		}
	}
	return nil
}
