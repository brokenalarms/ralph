package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Screenshot struct {
	Path        string
	Description string
}

// ScreenshotsForBead returns screenshots in ralphDir/screenshots/ matching
// the given bead ID prefix. Each screenshot may have a companion .desc
// sidecar file containing a text description of the visual issue.
func ScreenshotsForBead(ralphDir, beadID string) []Screenshot {
	if beadID == "" || ralphDir == "" {
		return nil
	}
	dir := filepath.Join(ralphDir, "screenshots")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	prefix := beadID + "-"
	var result []Screenshot
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".desc") {
			continue
		}
		if strings.HasPrefix(name, prefix) {
			p := filepath.Join(dir, name)
			desc := ""
			if data, err := os.ReadFile(p + ".desc"); err == nil {
				desc = strings.TrimSpace(string(data))
			}
			result = append(result, Screenshot{Path: p, Description: desc})
		}
	}
	return result
}

// FormatScreenshotContext builds a prompt section listing screenshot paths
// and descriptions for the agent to read via multimodal Read. Returns empty
// string if no screenshots exist.
func FormatScreenshotContext(screenshots []Screenshot) string {
	if len(screenshots) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Screenshots\n\n")
	b.WriteString("The following screenshots are attached to this task. Read each one\n")
	b.WriteString("with the Read tool to see the visual context before starting work.\n\n")
	for i, s := range screenshots {
		if s.Description != "" {
			fmt.Fprintf(&b, "%d. `%s` — %s\n", i+1, s.Path, s.Description)
		} else {
			fmt.Fprintf(&b, "%d. `%s`\n", i+1, s.Path)
		}
	}
	return b.String()
}

// SaveScreenshot writes imageData to ralphDir/screenshots/{beadID}-{NN}-{slug}.png
// and creates a companion .desc sidecar file with the description. The sequence
// number auto-increments based on existing screenshots for the bead.
func SaveScreenshot(ralphDir, beadID string, imageData []byte, slug, description string) (string, error) {
	dir := filepath.Join(ralphDir, "screenshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create screenshots dir: %w", err)
	}

	seq := nextSequence(dir, beadID)
	filename := fmt.Sprintf("%s-%02d-%s.png", beadID, seq, slug)
	path := filepath.Join(dir, filename)

	if err := os.WriteFile(path, imageData, 0o644); err != nil {
		return "", fmt.Errorf("write screenshot: %w", err)
	}

	if description != "" {
		if err := os.WriteFile(path+".desc", []byte(description), 0o644); err != nil {
			return "", fmt.Errorf("write description: %w", err)
		}
	}

	return path, nil
}

func nextSequence(dir, beadID string) int {
	prefix := beadID + "-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 1
	}
	max := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := name[len(prefix):]
		if idx := strings.Index(rest, "-"); idx >= 0 {
			if n, err := strconv.Atoi(rest[:idx]); err == nil && n > max {
				max = n
			}
		}
	}
	return max + 1
}
