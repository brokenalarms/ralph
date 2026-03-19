package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Verifies that Parse with no arguments returns ralph.sh-compatible defaults:
// cwd project dir, 20 iterations, worktree enabled, 80 calls/hr, refactor every 5.
func TestDefaultValues(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := Defaults()
	if cfg != want {
		t.Errorf("defaults mismatch:\ngot  %+v\nwant %+v", cfg, want)
	}
}

// Verifies that all short and long flag variants set their respective fields,
// matching the ralph.sh flag interface.
func TestAllFlags(t *testing.T) {
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
	for _, flag := range []string{"-d", "-n", "-p", "--plan-file", "--calls-per-hour", "--refactor-every"} {
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
