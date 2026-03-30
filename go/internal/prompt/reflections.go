package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadReflections reads all reflection markdown files from ralphDir/reflections/
// and returns their concatenated content. Returns empty string if the directory
// does not exist or contains no files.
func ReadReflections(ralphDir string) (string, error) {
	dir := filepath.Join(ralphDir, "reflections")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading reflections: %w", err)
	}

	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n---\n\n")
		}
		b.WriteString(string(data))
	}
	return b.String(), nil
}

// ArchiveReflections moves all .md files from reflections/ to reflections/archived/.
// Returns the list of task IDs whose reflections were archived.
func ArchiveReflections(ralphDir string) ([]string, error) {
	dir := filepath.Join(ralphDir, "reflections")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading reflections: %w", err)
	}

	archiveDir := filepath.Join(dir, "archived")
	var archived []string

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if err := os.MkdirAll(archiveDir, 0o755); err != nil {
			return archived, fmt.Errorf("creating archive dir: %w", err)
		}
		src := filepath.Join(dir, e.Name())
		dst := filepath.Join(archiveDir, e.Name())
		if err := os.Rename(src, dst); err != nil {
			return archived, fmt.Errorf("archiving %s: %w", e.Name(), err)
		}
		taskID := strings.TrimSuffix(e.Name(), ".md")
		archived = append(archived, taskID)
	}
	return archived, nil
}
