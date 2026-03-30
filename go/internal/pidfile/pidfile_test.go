package pidfile

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Verifies that Write creates a PID file containing the current process PID.
func TestWriteCreatesPIDFile(t *testing.T) {
	dir := t.TempDir()

	if err := Write(dir); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	data, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("Reading PID file: %v", err)
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatalf("PID file contains non-integer: %q", data)
	}

	if pid != os.Getpid() {
		t.Errorf("PID file contains %d, want %d", pid, os.Getpid())
	}
}

// Verifies that Remove deletes the PID file.
func TestRemoveDeletesPIDFile(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	Remove(dir)

	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Error("PID file still exists after Remove")
	}
}

// Verifies that Remove is a no-op when the file doesn't exist.
func TestRemoveNoopWhenMissing(t *testing.T) {
	dir := t.TempDir()
	Remove(dir) // should not panic
}

// Verifies that Check returns alive=true for the current process PID.
func TestCheckDetectsLiveProcess(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	pid, alive, err := Check(dir)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if !alive {
		t.Error("Check reported current process as dead")
	}
	if pid != os.Getpid() {
		t.Errorf("Check returned PID %d, want %d", pid, os.Getpid())
	}
}

// Verifies that Check returns alive=false when no PID file exists.
func TestCheckNoPIDFile(t *testing.T) {
	dir := t.TempDir()

	pid, alive, err := Check(dir)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if alive {
		t.Error("Check reported alive with no PID file")
	}
	if pid != 0 {
		t.Errorf("Check returned PID %d, want 0", pid)
	}
}

// Verifies that a stale PID file (dead process) is cleaned up by Check.
func TestCheckCleansUpStalePID(t *testing.T) {
	dir := t.TempDir()

	// Write a PID that almost certainly doesn't exist.
	stalePID := 2147483647
	os.WriteFile(Path(dir), []byte(strconv.Itoa(stalePID)), 0o644)

	pid, alive, err := Check(dir)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if alive {
		t.Error("Check reported stale PID as alive")
	}
	if pid != stalePID {
		t.Errorf("Check returned PID %d, want %d", pid, stalePID)
	}

	// PID file should have been removed.
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Error("Stale PID file was not cleaned up")
	}
}

// Verifies that a corrupt PID file is cleaned up by Check.
func TestCheckCleansUpCorruptPIDFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(Path(dir), []byte("not-a-number"), 0o644)

	_, alive, err := Check(dir)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if alive {
		t.Error("Check reported corrupt PID as alive")
	}

	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Error("Corrupt PID file was not cleaned up")
	}
}

// Verifies that Path returns the expected location.
func TestPathReturnsCorrectLocation(t *testing.T) {
	want := filepath.Join("/tmp/test-ralph", FileName)
	got := Path("/tmp/test-ralph")
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}
