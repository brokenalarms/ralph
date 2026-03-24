package health

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/logging"
)

// Snapshot captures orchestrator resource usage at a point in time.
type Snapshot struct {
	TailProcesses   int
	FilterProcesses int
	RalphDirSizeMB  float64
	StateFields     int
	SignalFiles     []string
	WorktreeBranch  string
}

// Collect gathers a health snapshot from the running system.
func Collect(ralphDir, workDir string) Snapshot {
	var s Snapshot

	s.TailProcesses = countProcesses("tail")
	s.FilterProcesses = countProcesses("stream_filter")
	s.RalphDirSizeMB = dirSizeMB(ralphDir)
	s.StateFields = stateFieldCount(filepath.Join(ralphDir, "state.json"))
	s.SignalFiles = findSignalFiles(ralphDir)
	s.WorktreeBranch = currentBranch(workDir)

	return s
}

// Log writes the health snapshot to the logger as a single compact line.
func Log(logger *logging.Logger, s Snapshot) {
	signals := "none"
	if len(s.SignalFiles) > 0 {
		signals = strings.Join(s.SignalFiles, ", ")
	}

	logger.Log("health",
		"procs: tail=%d filter=%d | .ralph: %.1fMB, %d state fields | signals: %s | branch: %s",
		s.TailProcesses, s.FilterProcesses,
		s.RalphDirSizeMB, s.StateFields,
		signals, s.WorktreeBranch)
}

func countProcesses(name string) int {
	out, err := exec.Command("pgrep", "-f", name).Output()
	if err != nil {
		return 0
	}
	lines := strings.TrimSpace(string(out))
	if lines == "" {
		return 0
	}
	return len(strings.Split(lines, "\n"))
}

func dirSizeMB(dir string) float64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return float64(total) / (1024 * 1024)
}

func stateFieldCount(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	count := 0
	for _, b := range data {
		if b == ':' {
			count++
		}
	}
	return count
}

func findSignalFiles(ralphDir string) []string {
	var found []string
	entries, err := os.ReadDir(ralphDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".signal_") {
			found = append(found, e.Name())
		}
	}
	return found
}

func currentBranch(workDir string) string {
	out, err := exec.Command("git", "-C", workDir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return strings.TrimSpace(string(out))
}
