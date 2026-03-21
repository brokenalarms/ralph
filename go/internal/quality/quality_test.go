package quality

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupWorkDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func writeFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	absPath := filepath.Join(dir, relPath)
	os.MkdirAll(filepath.Dir(absPath), 0o755)
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Proves: assess_quality detects any type usage in TypeScript and
// scores >= 9 for 3 occurrences (3 * 3 = 9).
func TestAssess_DetectsAnyTypeUsageInTypeScript(t *testing.T) {
	workDir := setupWorkDir(t)
	writeFile(t, workDir, "src/test.ts", `function parse(data: any): any {
  return data as any;
}
`)
	findingsFile := filepath.Join(workDir, ".quality-findings")
	score, err := Assess(workDir, findingsFile, "src/test.ts")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score < 9 {
		t.Errorf("expected score >= 9, got %d", score)
	}
	data, _ := os.ReadFile(findingsFile)
	if len(data) == 0 {
		t.Error("expected non-empty findings file")
	}
	if !strings.Contains(string(data), "untyped") {
		t.Error("findings should mention 'untyped'")
	}
}

// Proves: assess_quality detects oversized files (over 500 lines)
// and includes "over 500-line" in the findings.
func TestAssess_DetectsOversizedFiles(t *testing.T) {
	workDir := setupWorkDir(t)
	var lines []string
	for i := 0; i < 550; i++ {
		lines = append(lines, "echo 'line'")
	}
	writeFile(t, workDir, "big.sh", strings.Join(lines, "\n"))

	findingsFile := filepath.Join(workDir, ".quality-findings")
	score, err := Assess(workDir, findingsFile, "big.sh")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score <= 0 {
		t.Errorf("expected score > 0, got %d", score)
	}
	data, _ := os.ReadFile(findingsFile)
	if !strings.Contains(string(data), "over 500-line") {
		t.Errorf("findings should mention 'over 500-line', got: %s", data)
	}
}

// Proves: assess_quality detects silent catch blocks and scores >= 5.
func TestAssess_DetectsSilentCatches(t *testing.T) {
	workDir := setupWorkDir(t)
	writeFile(t, workDir, "src/handler.ts", `try {
  doSomething();
} catch (e) {}
`)
	findingsFile := filepath.Join(workDir, ".quality-findings")
	score, err := Assess(workDir, findingsFile, "src/handler.ts")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score < 5 {
		t.Errorf("expected score >= 5, got %d", score)
	}
	data, _ := os.ReadFile(findingsFile)
	if !strings.Contains(string(data), "silent catch") {
		t.Errorf("findings should mention 'silent catch', got: %s", data)
	}
}

// Proves: assess_quality returns 0 for clean files with no quality issues.
func TestAssess_ReturnsZeroForCleanFiles(t *testing.T) {
	workDir := setupWorkDir(t)
	writeFile(t, workDir, "clean.sh", "#!/bin/bash\necho \"hello\"\n")

	findingsFile := filepath.Join(workDir, ".quality-findings")
	score, err := Assess(workDir, findingsFile, "clean.sh")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score != 0 {
		t.Errorf("expected score 0, got %d", score)
	}
	data, _ := os.ReadFile(findingsFile)
	if len(data) > 0 {
		t.Errorf("expected empty findings file, got: %s", data)
	}
}

// Proves: assess_quality detects console.log ghosts in JS/TS files
// and scores >= 6.
func TestAssess_DetectsConsoleLogGhosts(t *testing.T) {
	workDir := setupWorkDir(t)
	writeFile(t, workDir, "src/debug.js", `console.log('here');
console.debug('test');
console.warn('????');
`)
	findingsFile := filepath.Join(workDir, ".quality-findings")
	score, err := Assess(workDir, findingsFile, "src/debug.js")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score < 6 {
		t.Errorf("expected score >= 6, got %d", score)
	}
	data, _ := os.ReadFile(findingsFile)
	if !strings.Contains(string(data), "console.log") {
		t.Errorf("findings should mention 'console.log', got: %s", data)
	}
}

// Proves: REFACTOR_THRESHOLD defaults to 20.
func TestDefaultRefactorThreshold(t *testing.T) {
	if DefaultRefactorThreshold != 20 {
		t.Errorf("expected 20, got %d", DefaultRefactorThreshold)
	}
}
