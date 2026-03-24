package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ScreenshotsForBead returns the absolute paths of all screenshot files
// in ralphDir/screenshots/ that match the given bead ID prefix.
// Files are named {beadID}-{NN}-{slug}.{ext} by the task manager.
func ScreenshotsForBead(ralphDir, beadID string) []string {
	if beadID == "" || ralphDir == "" {
		return nil
	}
	dir := filepath.Join(ralphDir, "screenshots")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	prefix := beadID + "-"
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), prefix) {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	return paths
}

// FormatScreenshotContext builds a prompt section listing screenshot paths
// for the agent to read via multimodal Read. Returns empty string if no
// screenshots exist.
func FormatScreenshotContext(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Screenshots\n\n")
	b.WriteString("The following screenshots are attached to this task. Read each one\n")
	b.WriteString("with the Read tool to see the visual context before starting work.\n\n")
	for i, p := range paths {
		fmt.Fprintf(&b, "%d. `%s`\n", i+1, p)
	}
	return b.String()
}
