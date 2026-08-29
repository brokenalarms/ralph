package loop

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/tasks"
)

const (
	// storeCompactInterval is how often the task backend's history is collapsed.
	storeCompactInterval = 7 * 24 * time.Hour

	// storeCompactRetainDays is how much recent commit history survives a compaction.
	storeCompactRetainDays = 30

	// storeCompactStampFile records the last compaction, under the .ralph directory
	// alongside the loop's other run state.
	storeCompactStampFile = ".last-compact"
)

// maybeCompactStore collapses the task backend's accumulated history if it has not
// been done recently.
//
// Called only from the idle path. Compaction rewrites the backend's storage —
// squash, cherry-pick, swap the main branch, garbage collect — which must not
// overlap a running task. That requirement is also why this is not a cron job
// or a launchd timer: only the loop knows when it is between tasks. A
// wall-clock scheduler would eventually fire mid-write.
//
// Every failure here is non-fatal. Maintenance that cannot run is worth a
// warning, never an interrupted session.
func (l *Loop) maybeCompactStore() {
	compactor, ok := l.taskBackend.(tasks.Compactor)
	if !ok {
		return
	}

	interval := l.cfg.StoreCompactInterval
	if interval == 0 {
		interval = storeCompactInterval
	}
	if interval < 0 {
		return // negative disables maintenance entirely
	}

	stamp := l.storeCompactStampPath()
	if stamp == "" {
		return
	}
	if !dueForStoreCompaction(stamp, interval) {
		return
	}

	l.logger.Emit(logging.Opts{Domain: logging.Beads}, "Compacting task history (keeping %d days)...", storeCompactRetainDays)
	start := time.Now()
	if err := compactor.Compact(storeCompactRetainDays); err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Compaction failed: %v", err)
		return
	}
	l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Success}, "Compaction complete in %s", time.Since(start).Round(time.Millisecond))

	// Stamped only on success, so a failing compaction is retried on the next
	// idle rather than being skipped for another interval.
	if err := writeStoreCompactStamp(stamp); err != nil {
		l.logger.Emit(logging.Opts{Domain: logging.Beads, Level: logging.Warn}, "Could not record compaction time: %v", err)
	}
}

func (l *Loop) storeCompactStampPath() string {
	if l.cfg.Dirs.RalphDir == "" {
		return ""
	}
	return filepath.Join(l.cfg.Dirs.RalphDir, storeCompactStampFile)
}

// dueForStoreCompaction reports whether interval has elapsed since the stamp was
// written. A missing or unreadable stamp counts as due: on a store that has
// never been compacted, running once is the point.
func dueForStoreCompaction(stampPath string, interval time.Duration) bool {
	data, err := os.ReadFile(stampPath)
	if err != nil {
		return true
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return true
	}
	return time.Since(time.Unix(secs, 0)) >= interval
}

func writeStoreCompactStamp(stampPath string) error {
	return os.WriteFile(stampPath, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
}
