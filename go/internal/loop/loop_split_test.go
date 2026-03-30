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

// Proves that loop_test.go has been split into focused test files, each under
// 1000 lines. This prevents the monolithic test file from growing back.
func TestLoopTestSplitFileStructure(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)

	assertFileUnderLines(t, filepath.Join(dir, "loop_test.go"), 400)

	testFiles := []string{
		"loop_lifecycle_test.go",
		"loop_prompt_test.go",
		"loop_refactor_test.go",
		"loop_branch_test.go",
		"loop_completion_test.go",
		"loop_verification_test.go",
		"loop_push_test.go",
		"loop_merge_test.go",
		"loop_signal_test.go",
		"loop_finalize_test.go",
		"loop_posttask_test.go",
		"loop_display_test.go",
	}

	for _, name := range testFiles {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected %s to exist after split", name)
		}
		assertFileUnderLines(t, path, 1000)
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
