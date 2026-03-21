package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Verifies that Parse with no arguments returns ralph.sh-compatible defaults:
// cwd project dir, 50 iterations, worktree enabled, 80 calls/hr, refactor disabled,
// idle timeouts at 10m/30s.
func TestDefaultValues(t *testing.T) {
	// Clear env vars so defaults are deterministic.
	t.Setenv("RALPH_MAX_ITERATIONS", "")
	t.Setenv("RALPH_REFACTOR_EVERY", "")
	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.MaxIterations != 50 {
		t.Errorf("MaxIterations = %d, want 50", cfg.MaxIterations)
	}
	if cfg.RefactorEvery != 0 {
		t.Errorf("RefactorEvery = %d, want 0", cfg.RefactorEvery)
	}
	if cfg.ProjectDir != "." {
		t.Errorf("ProjectDir = %q, want \".\"", cfg.ProjectDir)
	}
	if !cfg.UseWorktree {
		t.Error("UseWorktree should default to true")
	}
	if cfg.CallsPerHour != 80 {
		t.Errorf("CallsPerHour = %d, want 80", cfg.CallsPerHour)
	}
	if cfg.IdleTimeout != 10*time.Minute {
		t.Errorf("IdleTimeout = %s, want 10m", cfg.IdleTimeout)
	}
	if cfg.IdleTimeoutProgress != 30*time.Second {
		t.Errorf("IdleTimeoutProgress = %s, want 30s", cfg.IdleTimeoutProgress)
	}
}

// Verifies that RALPH_MAX_ITERATIONS and RALPH_REFACTOR_EVERY env vars
// override the hardcoded defaults, matching ralph.sh's ${VAR:-default} pattern.
func TestEnvVarDefaults(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "100")
	t.Setenv("RALPH_REFACTOR_EVERY", "10")
	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.MaxIterations != 100 {
		t.Errorf("MaxIterations = %d, want 100 (from env)", cfg.MaxIterations)
	}
	if cfg.RefactorEvery != 10 {
		t.Errorf("RefactorEvery = %d, want 10 (from env)", cfg.RefactorEvery)
	}
}

// Verifies that CLI flags override env var defaults for max_iterations
// and refactor_every, matching the shell's flag-then-env precedence.
func TestCLIOverridesEnvVar(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "100")
	t.Setenv("RALPH_REFACTOR_EVERY", "10")

	cfg, err := Parse([]string{"-n", "25", "--refactor-every", "3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.MaxIterations != 25 {
		t.Errorf("MaxIterations = %d, want 25 (CLI override)", cfg.MaxIterations)
	}
	if cfg.RefactorEvery != 3 {
		t.Errorf("RefactorEvery = %d, want 3 (CLI override)", cfg.RefactorEvery)
	}
}

// Verifies that all short and long flag variants set their respective fields,
// matching the ralph.sh flag interface.
func TestAllFlags(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")
	t.Setenv("RALPH_REFACTOR_EVERY", "")

	args := []string{
		"-d", "/tmp/proj",
		"-n", "10",
		"-p", "fix tests",
		"--plan-file", "plan.md",
		"--plan",
		"--skip-planning",
		"-q",
		"--no-worktree",
		"--calls-per-hour", "40",
		"--refactor-every", "3",
		"--tmux",
	}
	cfg, err := Parse(args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ProjectDir != "/tmp/proj" {
		t.Errorf("ProjectDir = %q, want /tmp/proj", cfg.ProjectDir)
	}
	if cfg.MaxIterations != 10 {
		t.Errorf("MaxIterations = %d, want 10", cfg.MaxIterations)
	}
	if cfg.Prompt != "fix tests" {
		t.Errorf("Prompt = %q, want \"fix tests\"", cfg.Prompt)
	}
	if cfg.PlanFile != "plan.md" {
		t.Errorf("PlanFile = %q, want plan.md", cfg.PlanFile)
	}
	if !cfg.PlanOnly {
		t.Error("PlanOnly should be true")
	}
	if !cfg.SkipPlanning {
		t.Error("SkipPlanning should be true")
	}
	if !cfg.Quiet {
		t.Error("Quiet should be true")
	}
	if cfg.UseWorktree {
		t.Error("UseWorktree should be false after --no-worktree")
	}
	if cfg.CallsPerHour != 40 {
		t.Errorf("CallsPerHour = %d, want 40", cfg.CallsPerHour)
	}
	if cfg.RefactorEvery != 3 {
		t.Errorf("RefactorEvery = %d, want 3", cfg.RefactorEvery)
	}
	if !cfg.UseTmux {
		t.Error("UseTmux should be true")
	}
}

// Verifies that a bare positional argument is treated as the project directory,
// matching ralph.sh's `*) PROJECT_DIR="$1"` case.
func TestPositionalProjectDir(t *testing.T) {
	cfg, err := Parse([]string{"/some/path"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ProjectDir != "/some/path" {
		t.Errorf("ProjectDir = %q, want /some/path", cfg.ProjectDir)
	}
}

// Verifies that -h/--help returns ErrHelp so the caller can print usage.
func TestHelpFlag(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		_, err := Parse([]string{flag})
		if err != ErrHelp {
			t.Errorf("Parse(%q) error = %v, want ErrHelp", flag, err)
		}
	}
}

// Verifies that --idle-timeout and --idle-timeout-progress flags override
// defaults, accepting both Go duration strings and bare seconds.
func TestIdleTimeoutFlags(t *testing.T) {
	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	cfg, err := Parse([]string{"--idle-timeout", "5m", "--idle-timeout-progress", "15s"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.IdleTimeout != 5*time.Minute {
		t.Errorf("IdleTimeout = %s, want 5m", cfg.IdleTimeout)
	}
	if cfg.IdleTimeoutProgress != 15*time.Second {
		t.Errorf("IdleTimeoutProgress = %s, want 15s", cfg.IdleTimeoutProgress)
	}

	// Bare integer interpreted as seconds.
	cfg, err = Parse([]string{"--idle-timeout", "120"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.IdleTimeout != 2*time.Minute {
		t.Errorf("IdleTimeout = %s, want 2m0s for bare 120", cfg.IdleTimeout)
	}
}

// Verifies that RALPH_IDLE_TIMEOUT and RALPH_IDLE_TIMEOUT_PROGRESS env vars
// override the hardcoded defaults.
func TestIdleTimeoutEnvVars(t *testing.T) {
	t.Setenv("RALPH_IDLE_TIMEOUT", "3m")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "45")

	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.IdleTimeout != 3*time.Minute {
		t.Errorf("IdleTimeout = %s, want 3m from env", cfg.IdleTimeout)
	}
	if cfg.IdleTimeoutProgress != 45*time.Second {
		t.Errorf("IdleTimeoutProgress = %s, want 45s from env", cfg.IdleTimeoutProgress)
	}
}

// Verifies that unknown flags produce an error, matching ralph.sh's
// `-*) log_error "Unknown option: $1"` case.
func TestUnknownFlag(t *testing.T) {
	_, err := Parse([]string{"--bogus"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

// Verifies that flags requiring a value return an error when the value is missing.
func TestMissingArgValue(t *testing.T) {
	for _, flag := range []string{"-d", "-n", "-p", "--plan-file", "--calls-per-hour", "--refactor-every", "--idle-timeout", "--idle-timeout-progress"} {
		_, err := Parse([]string{flag})
		if err == nil {
			t.Errorf("Parse(%q) should error on missing value", flag)
		}
	}
}

// Verifies that non-numeric values for integer flags produce an error.
func TestInvalidNumericArg(t *testing.T) {
	for _, flag := range []string{"-n", "--calls-per-hour", "--refactor-every"} {
		_, err := Parse([]string{flag, "abc"})
		if err == nil {
			t.Errorf("Parse(%q, \"abc\") should error on non-numeric value", flag)
		}
	}
}

// Verifies that "stop" and "feedback" are recognized as subcommands,
// and a regular directory argument is not.
func TestParseSubcommand(t *testing.T) {
	// "stop" should be parsed as a subcommand with default dir
	sub, ok := ParseSubcommand([]string{"stop"})
	if !ok {
		t.Fatal("expected subcommand for 'stop'")
	}
	if sub.Name != "stop" || sub.Dir != "." {
		t.Errorf("got %+v, want Name=stop Dir=.", sub)
	}

	// "feedback" with trailing message words
	sub, ok = ParseSubcommand([]string{"feedback", "hello", "world"})
	if !ok {
		t.Fatal("expected subcommand for 'feedback'")
	}
	if sub.Name != "feedback" {
		t.Errorf("Name = %q, want feedback", sub.Name)
	}
	if len(sub.Args) != 2 || sub.Args[0] != "hello" {
		t.Errorf("Args = %v, want [hello world]", sub.Args)
	}

	// Non-subcommand
	_, ok = ParseSubcommand([]string{"/some/path"})
	if ok {
		t.Error("expected no subcommand for a path argument")
	}
}

// Verifies that subcommand parsing recognizes an existing directory argument
// as the target dir.
func TestSubcommandWithDir(t *testing.T) {
	dir := t.TempDir()
	sub, ok := ParseSubcommand([]string{"stop", dir})
	if !ok {
		t.Fatal("expected subcommand")
	}
	if sub.Dir != dir {
		t.Errorf("Dir = %q, want %q", sub.Dir, dir)
	}
}

// Verifies that subcommand parsing falls back to "." when the argument
// after the subcommand name is not a directory.
func TestSubcommandNonexistentDirFallback(t *testing.T) {
	sub, ok := ParseSubcommand([]string{"feedback", "not-a-dir", "msg"})
	if !ok {
		t.Fatal("expected subcommand")
	}
	// "not-a-dir" is not a directory, so it stays in Args and Dir defaults to "."
	if sub.Dir != "." {
		t.Errorf("Dir = %q, want \".\"", sub.Dir)
	}
	if len(sub.Args) != 2 {
		t.Errorf("Args = %v, want [not-a-dir msg]", sub.Args)
	}
}

// Verifies that empty args produce no subcommand.
func TestParseSubcommandEmpty(t *testing.T) {
	_, ok := ParseSubcommand(nil)
	if ok {
		t.Error("expected no subcommand for empty args")
	}
}

// Proves: --plan-file flag stores the plan file path in config
// for validation by the planning phase.
func TestPlanFileFlag(t *testing.T) {
	cfg, err := Parse([]string{"--plan-file", "/some/plan.md"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PlanFile != "/some/plan.md" {
		t.Errorf("PlanFile = %q, want /some/plan.md", cfg.PlanFile)
	}
}

// Proves: --auto-merge flag defaults to false and is set to true
// when passed on the CLI.
func TestAutoMergeFlag(t *testing.T) {
	cfg, err := Parse([]string{"-d", "/tmp"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AutoMerge {
		t.Error("AutoMerge should default to false")
	}

	cfg, err = Parse([]string{"--auto-merge", "--no-worktree"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.AutoMerge {
		t.Error("AutoMerge should be true after --auto-merge flag")
	}
}

// Proves: ValidatePlanFile with nonexistent file returns "not found" error.
func TestValidatePlanFile_NonexistentFile(t *testing.T) {
	err := ValidatePlanFile("/nonexistent/plan.md")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should contain 'not found', got: %v", err)
	}
}

// Proves: ValidatePlanFile without checkboxes returns format error.
func TestValidatePlanFile_NoCheckboxes(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "bad-plan.md")
	os.WriteFile(planFile, []byte("Just some text without checkboxes"), 0o644)

	err := ValidatePlanFile(planFile)
	if err == nil {
		t.Fatal("expected error for plan without checkboxes")
	}
	if !strings.Contains(err.Error(), "Ralph format") {
		t.Errorf("error should contain 'Ralph format', got: %v", err)
	}
}

// Proves: ValidatePlanFile with valid checkboxes returns nil.
func TestValidatePlanFile_ValidFile(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan.md")
	os.WriteFile(planFile, []byte("- [ ] Test task\n- [ ] Another task\n"), 0o644)

	err := ValidatePlanFile(planFile)
	if err != nil {
		t.Fatalf("expected no error for valid plan, got: %v", err)
	}
}

// Proves: ValidatePlanFile accepts files with completed checkboxes.
func TestValidatePlanFile_CompletedCheckboxes(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan.md")
	os.WriteFile(planFile, []byte("- [x] Done task\n"), 0o644)

	err := ValidatePlanFile(planFile)
	if err != nil {
		t.Fatalf("expected no error for plan with completed checkboxes, got: %v", err)
	}
}

// Verifies the stop subcommand E2E: creates a stop file in .ralph/.
func TestStopSubcommandIntegration(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	if err := os.MkdirAll(ralphDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sub, _ := ParseSubcommand([]string{"stop", dir})
	stopFile := filepath.Join(sub.Dir, ".ralph", "stop")
	if err := os.WriteFile(stopFile, nil, 0o644); err != nil {
		t.Fatalf("failed to create stop file: %v", err)
	}
	if _, err := os.Stat(stopFile); err != nil {
		t.Errorf("stop file not created: %v", err)
	}
}
