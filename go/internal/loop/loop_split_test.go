package loop

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Proves that loop_helpers.go has been split into focused files, each under
// the target line count. This prevents the file from growing back.
func TestLoopHelpersSplitFileStructure(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)

	// loop_helpers.go must exist and be under 300 lines.
	assertFileUnderLines(t, filepath.Join(dir, "loop_helpers.go"), 300)

	// Split files must exist.
	for _, name := range []string{"loop_git.go", "loop_refactor.go", "loop_utils.go"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected %s to exist after split", name)
		}
	}
}

func assertFileUnderLines(t *testing.T, path string, maxLines int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("cannot open %s: %v", filepath.Base(path), err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	lines := 0
	for scanner.Scan() {
		lines++
	}
	if lines > maxLines {
		t.Errorf("%s has %d lines, want <= %d", filepath.Base(path), lines, maxLines)
	}
}
