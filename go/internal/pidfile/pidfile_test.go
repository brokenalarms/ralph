package pidfile

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// Write creates a PID file containing the current process PID.
func TestWrite_CreatesPIDFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop.pid")

	if err := Write(path); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatalf("PID file contains non-integer: %q", data)
	}
	if pid != os.Getpid() {
		t.Errorf("PID = %d, want %d", pid, os.Getpid())
	}
}

// Remove deletes the PID file.
func TestRemove_DeletesPIDFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop.pid")

	if err := Write(path); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	Remove(path)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("PID file still exists after Remove")
	}
}

// Check returns 0 and no error when no PID file exists.
func TestCheck_NoPIDFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop.pid")

	pid, err := Check(path)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if pid != 0 {
		t.Errorf("pid = %d, want 0", pid)
	}
}

// Check returns the PID when a different alive process owns the PID file.
func TestCheck_AliveProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop.pid")

	// Start a long-running child process whose PID we can write.
	cmd := exec.Command("sleep", "1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start sleep: %v", err)
	}
	defer cmd.Process.Kill()

	childPID := cmd.Process.Pid
	os.WriteFile(path, []byte(fmt.Sprintf("%d", childPID)), 0o644)

	pid, err := Check(path)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if pid != childPID {
		t.Errorf("pid = %d, want %d (alive process)", pid, childPID)
	}
}

// Check skips self-detection: if the stored PID matches os.Getpid(), return 0.
// This happens after syscall.Exec replaces the process in-place (same PID).
func TestCheck_SelfPIDReturnsZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop.pid")

	if err := Write(path); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	pid, err := Check(path)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if pid != 0 {
		t.Errorf("pid = %d, want 0 (self-PID should be skipped)", pid)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("self-PID file should have been removed")
	}
}

// Check cleans up stale PID files (dead process) and returns 0.
func TestCheck_StaleProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop.pid")

	// Write a PID that almost certainly doesn't exist.
	os.WriteFile(path, []byte("99999999"), 0o644)

	pid, err := Check(path)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if pid != 0 {
		t.Errorf("pid = %d, want 0 (stale process cleaned up)", pid)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale PID file should have been removed")
	}
}

// Check handles corrupt PID file (non-integer content) by cleaning up.
func TestCheck_CorruptPIDFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop.pid")

	os.WriteFile(path, []byte("not-a-pid"), 0o644)

	pid, err := Check(path)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if pid != 0 {
		t.Errorf("pid = %d, want 0 (corrupt file cleaned up)", pid)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("corrupt PID file should have been removed")
	}
}
