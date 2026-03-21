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
	score, err := Assess(workDir, findingsFile, nil, "src/test.ts")
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
	score, err := Assess(workDir, findingsFile, nil, "big.sh")
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
	score, err := Assess(workDir, findingsFile, nil, "src/handler.ts")
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
	score, err := Assess(workDir, findingsFile, nil, "clean.sh")
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
	score, err := Assess(workDir, findingsFile, nil, "src/debug.js")
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

// Proves: disabling a check via Options.DisabledChecks prevents it from
// contributing to the score, allowing users to suppress noisy checks.
func TestAssess_DisabledCheckSkipped(t *testing.T) {
	workDir := setupWorkDir(t)
	writeFile(t, workDir, "src/test.ts", `function parse(data: any): any {
  return data as any;
}
`)
	findingsFile := filepath.Join(workDir, ".quality-findings")

	opts := &Options{DisabledChecks: map[string]bool{CheckAnyType: true}}
	score, err := Assess(workDir, findingsFile, opts, "src/test.ts")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score != 0 {
		t.Errorf("expected score 0 with any-type disabled, got %d", score)
	}
}

// Proves: disabling multiple checks suppresses all of them while leaving
// other checks active.
func TestAssess_MultipleDisabledChecks(t *testing.T) {
	workDir := setupWorkDir(t)
	writeFile(t, workDir, "src/messy.ts", `console.log('debug');
try { x(); } catch (e) {}
const v: any = 1;
`)
	findingsFile := filepath.Join(workDir, ".quality-findings")

	opts := &Options{DisabledChecks: map[string]bool{
		CheckConsoleLog:  true,
		CheckSilentCatch: true,
	}}
	score, err := Assess(workDir, findingsFile, opts, "src/messy.ts")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	// Only any-type check should fire (1 occurrence * 3 = 3).
	if score != 3 {
		t.Errorf("expected score 3 (only any-type), got %d", score)
	}
}

// Proves: AllChecks contains every defined check name so users can
// enumerate valid check names for --disable-check.
func TestAllChecksContainsEveryCheck(t *testing.T) {
	expected := map[string]bool{
		CheckAnyType:       true,
		CheckOversizedFile: true,
		CheckSilentCatch:   true,
		CheckConsoleLog:    true,
	}
	if len(AllChecks) != len(expected) {
		t.Fatalf("AllChecks has %d entries, want %d", len(AllChecks), len(expected))
	}
	for _, name := range AllChecks {
		if !expected[name] {
			t.Errorf("unexpected check name in AllChecks: %q", name)
		}
	}
}
