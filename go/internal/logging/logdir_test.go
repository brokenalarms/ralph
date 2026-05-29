package logging_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// StableLogDir returns a deterministic path under ~/.ralph/logs/ for each project dir.
func TestStableLogDir(t *testing.T) {
	home, _ := os.UserHomeDir()

	dir1, err := logging.StableLogDir("/Users/alice/projects/myapp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dir2, err := logging.StableLogDir("/Users/alice/projects/myapp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir1 != dir2 {
		t.Errorf("same projectDir should give same log dir: %s vs %s", dir1, dir2)
	}

	dir3, err := logging.StableLogDir("/Users/alice/projects/otherapp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir1 == dir3 {
		t.Errorf("different projectDirs should give different log dirs")
	}

	if !strings.HasPrefix(dir1, filepath.Join(home, ".ralph", "logs")) {
		t.Errorf("log dir should be under ~/.ralph/logs/: %s", dir1)
	}
	if !strings.Contains(filepath.Base(dir1), "myapp") {
		t.Errorf("log dir slug should include project base name: %s", dir1)
	}
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
