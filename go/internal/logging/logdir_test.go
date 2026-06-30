package logging_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// ActiveLogPath returns a date-suffixed path under logDir for the given log name.
func TestActiveLogPath(t *testing.T) {
	logDir := "/some/project/.ralph"
	path := logging.ActiveLogPath(logDir, "loop")

	if !strings.HasPrefix(path, logDir) {
		t.Errorf("path should be under logDir: %s", path)
	}
	if !strings.HasSuffix(path, ".log") {
		t.Errorf("path should end in .log: %s", path)
	}
	date := time.Now().Format("2006-01-02")
	if !strings.Contains(filepath.Base(path), "loop."+date) {
		t.Errorf("path should contain 'loop.%s': %s", date, path)
	}
}

// PruneLogs deletes date-suffixed segments older than retentionDays and keeps recent ones.
func TestPruneLogs_DateSegments(t *testing.T) {
	dir := t.TempDir()

	now := time.Now()
	oldTime := now.Add(-40 * 24 * time.Hour)
	recentTime := now.Add(-5 * 24 * time.Hour)

	oldSegment := filepath.Join(dir, "loop.2026-05-01.log")
	recentSegment := filepath.Join(dir, "loop.2026-06-24.log")
	activeSegment := filepath.Join(dir, "loop.2026-06-29.log")

	for _, f := range []string{oldSegment, recentSegment, activeSegment} {
		if err := os.WriteFile(f, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(oldSegment, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(recentSegment, recentTime, recentTime); err != nil {
		t.Fatal(err)
	}
	// activeSegment mtime is current (default after WriteFile)

	if err := logging.PruneLogs(dir, 30); err != nil {
		t.Fatalf("PruneLogs: %v", err)
	}

	if _, err := os.Stat(oldSegment); !os.IsNotExist(err) {
		t.Error("old segment should have been pruned")
	}
	if _, err := os.Stat(recentSegment); os.IsNotExist(err) {
		t.Error("recent segment should have been kept")
	}
	if _, err := os.Stat(activeSegment); os.IsNotExist(err) {
		t.Error("active segment should have been kept")
	}
}

// MigrateLegacyLogs removes the legacy ~/.ralph/logs tree so nothing remains under the home dir.
func TestMigrateLegacyLogs(t *testing.T) {
	fakeHome := t.TempDir()
	legacyLogs := filepath.Join(fakeHome, ".ralph", "logs")
	if err := os.MkdirAll(legacyLogs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyLogs, "old.log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	logging.MigrateLegacyLogsFrom(legacyLogs)

	if _, err := os.Stat(legacyLogs); !os.IsNotExist(err) {
		t.Error("legacy logs directory should have been removed")
	}
	// Parent .ralph dir should not be removed
	parent := filepath.Join(fakeHome, ".ralph")
	if _, err := os.Stat(parent); os.IsNotExist(err) {
		t.Error(".ralph parent dir should remain")
	}
}

// MigrateLegacyLogs is a no-op when the legacy directory does not exist.
func TestMigrateLegacyLogs_NoOp(t *testing.T) {
	logging.MigrateLegacyLogsFrom("/nonexistent/path/logs")
	// no panic, no error
}

// PruneLogs deletes files older than retentionDays and leaves newer ones.
func TestPruneLogs(t *testing.T) {
	dir := t.TempDir()

	now := time.Now()
	old := now.Add(-40 * 24 * time.Hour)
	recent := now.Add(-5 * 24 * time.Hour)

	oldFile := filepath.Join(dir, "old.log")
	recentFile := filepath.Join(dir, "recent.log")

	if err := os.WriteFile(oldFile, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recentFile, []byte("recent"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldFile, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(recentFile, recent, recent); err != nil {
		t.Fatal(err)
	}

	if err := logging.PruneLogs(dir, 30); err != nil {
		t.Fatalf("PruneLogs: %v", err)
	}

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("old log file should have been pruned")
	}
	if _, err := os.Stat(recentFile); os.IsNotExist(err) {
		t.Error("recent log file should have been kept")
	}
}

// PruneLogs with non-positive retention is a no-op.
func TestPruneLogs_Disabled(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-100 * 24 * time.Hour)
	f := filepath.Join(dir, "old.log")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f, old, old); err != nil {
		t.Fatal(err)
	}

	if err := logging.PruneLogs(dir, 0); err != nil {
		t.Fatalf("PruneLogs(0): %v", err)
	}
	if _, err := os.Stat(f); os.IsNotExist(err) {
		t.Error("file should not be pruned when retentionDays=0")
	}
}

// PruneLogs on a missing directory returns nil.
func TestPruneLogs_MissingDir(t *testing.T) {
	if err := logging.PruneLogs("/nonexistent/path/xyz", 30); err != nil {
		t.Errorf("missing dir should not return error: %v", err)
	}
}
