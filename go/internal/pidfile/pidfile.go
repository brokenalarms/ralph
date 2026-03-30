package pidfile

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Write creates a PID file at the given path containing the current process PID.
func Write(path string) error {
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// Remove deletes the PID file. No error if it doesn't exist.
func Remove(path string) {
	os.Remove(path)
}

// Check reads a PID file and determines if the referenced process is alive.
// Returns the PID if the process is alive, 0 if no file exists or the process
// is dead (stale files are cleaned up automatically).
func Check(path string) (int, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading PID file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		os.Remove(path)
		return 0, nil
	}

	if processAlive(pid) {
		return pid, nil
	}

	os.Remove(path)
	return 0, nil
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
