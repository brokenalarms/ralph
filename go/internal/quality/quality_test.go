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

// Proves: REFACTOR_THRESHOLD defaults to 100.
func TestDefaultRefactorThreshold(t *testing.T) {
	if DefaultRefactorThreshold != 100 {
		t.Errorf("expected 100, got %d", DefaultRefactorThreshold)
	}
}

// Proves: test files (_test.go, .bats, test/ prefix) are excluded from scoring
// so test fixture strings and naturally large test files don't inflate quality scores.
func TestAssess_ExcludesTestFiles(t *testing.T) {
	workDir := setupWorkDir(t)
	// Go test file with debug prints — should be excluded
	writeFile(t, workDir, "internal/loop/loop_test.go", `package loop
import "fmt"
func TestSomething() { fmt.Println("debug") }
`)
	// Bats test file — should be excluded
	writeFile(t, workDir, "test/refactor.bats", strings.Repeat("echo line\n", 600))
	// Test helper in test/ directory — should be excluded
	writeFile(t, workDir, "test/helper.sh", strings.Repeat("echo line\n", 600))

	score, err := Assess(workDir, "", nil,
		"internal/loop/loop_test.go",
		"test/refactor.bats",
		"test/helper.sh",
	)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score != 0 {
		t.Errorf("expected score 0 for test files, got %d", score)
	}
}

// Proves: legacy scripts (ralph.sh) are excluded from scoring so their
// oversized-file score doesn't dominate the quality assessment.
func TestAssess_ExcludesLegacyScripts(t *testing.T) {
	workDir := setupWorkDir(t)
	writeFile(t, workDir, "ralph.sh", strings.Repeat("echo line\n", 2000))

	score, err := Assess(workDir, "", nil, "ralph.sh")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score != 0 {
		t.Errorf("expected score 0 for legacy ralph.sh, got %d", score)
	}
}

// Proves: CLI entry points (cmd/*/main.go) are excluded from scoring so
// intentional fmt.Print in CLI output code doesn't trigger debug-print checks.
func TestAssess_ExcludesCLIEntryPoints(t *testing.T) {
	workDir := setupWorkDir(t)
	writeFile(t, workDir, "go/cmd/ralph/main.go", `package main
import "fmt"
func main() {
	fmt.Println("ralph v1.0")
	fmt.Printf("running...\n")
}
`)
	score, err := Assess(workDir, "", nil, "go/cmd/ralph/main.go")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score != 0 {
		t.Errorf("expected score 0 for CLI entry point, got %d", score)
	}
}

// Proves: non-excluded Go files still get scored normally.
func TestAssess_NonExcludedGoFilesStillScored(t *testing.T) {
	workDir := setupWorkDir(t)
	writeFile(t, workDir, "internal/loop/loop.go", `package loop
import "fmt"
func Run() { fmt.Println("debug") }
`)
	score, err := Assess(workDir, "", nil, "internal/loop/loop.go")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score < 2 {
		t.Errorf("expected score >= 2 for non-excluded Go file with debug print, got %d", score)
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
		CheckDebugPrint:    true,
		CheckTodoCount:     true,
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

// Proves: assess_quality detects fmt.Println/Printf debug prints in Go files
// and scores >= 4 for 2 occurrences (2 * 2 = 4).
func TestAssess_DetectsDebugPrintsInGo(t *testing.T) {
	workDir := setupWorkDir(t)
	writeFile(t, workDir, "internal/debug.go", `package internal

import "fmt"

func debug() {
	fmt.Println("debug here")
	fmt.Printf("value: %d\n", 42)
}
`)
	findingsFile := filepath.Join(workDir, ".quality-findings")
	score, err := Assess(workDir, findingsFile, nil, "internal/debug.go")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score < 4 {
		t.Errorf("expected score >= 4, got %d", score)
	}
	data, _ := os.ReadFile(findingsFile)
	if !strings.Contains(string(data), "fmt.Print") {
		t.Errorf("findings should mention 'fmt.Print', got: %s", data)
	}
}

// Proves: debug-print check only fires for Go files, not JS/TS.
func TestAssess_DebugPrintSkipsNonGoFiles(t *testing.T) {
	workDir := setupWorkDir(t)
	writeFile(t, workDir, "src/app.ts", `fmt.Println("not go");`)
	score, err := Assess(workDir, "", nil, "src/app.ts")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score != 0 {
		t.Errorf("expected score 0 for non-Go file, got %d", score)
	}
}

// Proves: assess_quality detects TODO accumulation (>= 3 TODOs per file)
// and scores equal to the TODO count.
func TestAssess_DetectsTodoAccumulation(t *testing.T) {
	workDir := setupWorkDir(t)
	writeFile(t, workDir, "internal/handler.go", `package handler

// TODO: implement auth
// TODO: add logging
// TODO: handle errors
func Handle() {}
`)
	findingsFile := filepath.Join(workDir, ".quality-findings")
	score, err := Assess(workDir, findingsFile, nil, "internal/handler.go")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score < 3 {
		t.Errorf("expected score >= 3, got %d", score)
	}
	data, _ := os.ReadFile(findingsFile)
	if !strings.Contains(string(data), "TODO") {
		t.Errorf("findings should mention 'TODO', got: %s", data)
	}
}

// Proves: fewer than 3 TODOs in a file do not trigger the TODO accumulation check.
func TestAssess_FewTodosDoNotTrigger(t *testing.T) {
	workDir := setupWorkDir(t)
	writeFile(t, workDir, "lib/util.go", `package lib

// TODO: clean up later
func Util() {}
`)
	score, err := Assess(workDir, "", nil, "lib/util.go")
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if score != 0 {
		t.Errorf("expected score 0 for < 3 TODOs, got %d", score)
	}
}

// Proves: FormatFindings returns file content when findings exist,
// empty string when file is missing or empty.
func TestFormatFindings(t *testing.T) {
	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "empty")
	os.WriteFile(emptyPath, nil, 0o644)
	if got := FormatFindings(emptyPath); got != "" {
		t.Errorf("expected empty for empty file, got: %q", got)
	}

	if got := FormatFindings(filepath.Join(dir, "nonexistent")); got != "" {
		t.Errorf("expected empty for missing file, got: %q", got)
	}

	filledPath := filepath.Join(dir, "findings")
	os.WriteFile(filledPath, []byte("src/auth.ts:\n  - 3x untyped `any`\n"), 0o644)
	got := FormatFindings(filledPath)
	if !strings.Contains(got, "untyped") {
		t.Errorf("expected findings content, got: %q", got)
	}
}
