// Session logs (loop.log, raw.log) are written to the project's own .ralph/
// directory as daily date-suffixed segments (e.g. loop.2026-06-29.log).
// Old segments are pruned on startup based on a configurable retention window
// (log_retention_days in config.toml, default 30 days). On first startup the
// legacy ~/.ralph/logs tree is removed if it exists.
package logging

import (
	"os"
	"path/filepath"
	"time"
)

// ActiveLogPath returns the path for today's log segment under logDir.
// The file is named <name>.<YYYY-MM-DD>.log so each calendar day gets its
// own segment; old segments age out via PruneLogs.
func ActiveLogPath(logDir, name string) string {
	date := time.Now().Format("2006-01-02")
	return filepath.Join(logDir, name+"."+date+".log")
}

// MigrateLegacyLogsFrom removes the given legacy log directory tree if it
// exists. Call with the path of the old ~/.ralph/logs/<slug> directory on
// first startup to clean up the global location that is no longer used.
func MigrateLegacyLogsFrom(legacyLogsDir string) {
	_ = os.RemoveAll(legacyLogsDir)
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
