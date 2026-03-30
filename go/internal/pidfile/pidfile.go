package pidfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const FileName = "loop.pid"

// Path returns the full path to the PID file inside the given ralph directory.
func Path(ralphDir string) string {
	return filepath.Join(ralphDir, FileName)
}

// Write writes the current process PID to the PID file.
func Write(ralphDir string) error {
	return os.WriteFile(Path(ralphDir), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// Remove deletes the PID file if it exists.
func Remove(ralphDir string) {
	os.Remove(Path(ralphDir))
}

// Check reads the PID file and checks if the process is still alive.
// Returns (pid, alive, err). If the file doesn't exist, returns (0, false, nil).
// If the file exists but the process is dead (stale), the file is removed.
func Check(ralphDir string) (int, bool, error) {
	data, err := os.ReadFile(Path(ralphDir))
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading PID file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		// Corrupt PID file — remove it.
		Remove(ralphDir)
		return 0, false, nil
	}

	if !processAlive(pid) {
		Remove(ralphDir)
		return pid, false, nil
	}

	return pid, true, nil
}

// processAlive checks whether a process with the given PID exists.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks existence without sending a real signal.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}
