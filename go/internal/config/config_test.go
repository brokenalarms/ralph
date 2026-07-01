package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Verifies that Parse with no arguments returns defaults derived from the Flags
// registry: cwd project dir, 50 iterations, 80 calls/hr, idle timeouts at 10m/5m.
func TestDefaultValues(t *testing.T) {
	// Clear env vars so defaults are deterministic.
	t.Setenv("RALPH_MAX_ITERATIONS", "")
	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.MaxIterations != 50 {
		t.Errorf("MaxIterations = %d, want 50", cfg.MaxIterations)
	}
	if cfg.ProjectDir != "." {
		t.Errorf("ProjectDir = %q, want \".\"", cfg.ProjectDir)
	}
	if cfg.CallsPerHour != 80 {
		t.Errorf("CallsPerHour = %d, want 80", cfg.CallsPerHour)
	}
	if cfg.IdleTimeout != 10*time.Minute {
		t.Errorf("IdleTimeout = %s, want 10m", cfg.IdleTimeout)
	}
	if cfg.IdleTimeoutProgress != 5*time.Minute {
		t.Errorf("IdleTimeoutProgress = %s, want 5m", cfg.IdleTimeoutProgress)
	}
	if cfg.CIPollTimeout != 5*time.Minute {
		t.Errorf("CIPollTimeout = %s, want 5m", cfg.CIPollTimeout)
	}
}

// Verifies that RALPH_MAX_ITERATIONS env var overrides the Flags registry default.
func TestEnvVarDefaults(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "100")
	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.MaxIterations != 100 {
		t.Errorf("MaxIterations = %d, want 100 (from env)", cfg.MaxIterations)
	}
}

// Verifies that CLI flags override env var defaults for max_iterations.
func TestCLIOverridesEnvVar(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "100")

	cfg, err := Parse([]string{"-n", "25"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.MaxIterations != 25 {
		t.Errorf("MaxIterations = %d, want 25 (CLI override)", cfg.MaxIterations)
	}
}

// Verifies that all short and long flag variants set their respective fields,
// matching the ralph.sh flag interface.
func TestAllFlags(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")

	args := []string{
		"-n", "10",
		"--tmux",
		"--auto-merge",
		"--evolve",
		"--wait",
	}
	cfg, err := Parse(args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ProjectDir != "." {
		t.Errorf("ProjectDir = %q, want \".\" (always cwd)", cfg.ProjectDir)
	}
	if cfg.MaxIterations != 10 {
		t.Errorf("MaxIterations = %d, want 10", cfg.MaxIterations)
	}
	if !cfg.UseTmux {
		t.Error("UseTmux should be true")
	}
	if !cfg.AutoMerge {
		t.Error("AutoMerge should be true")
	}
	if !cfg.Evolve {
		t.Error("Evolve should be true")
	}
	if !cfg.Wait {
		t.Error("Wait should be true")
	}
}

// Verifies that --no-worktree is rejected — worktree isolation is mandatory.
func TestNoWorktreeFlagRejected(t *testing.T) {
	_, err := Parse([]string{"--no-worktree"})
	if err == nil {
		t.Fatal("--no-worktree should be rejected as unknown flag")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("expected 'unknown' error, got: %v", err)
	}
}

// Verifies that --dir/-d flags are rejected — ralph loop must run from the
// project directory, not accept a directory override.
func TestDirFlagRemoved(t *testing.T) {
	for _, flag := range []string{"--dir", "-d"} {
		_, err := Parse([]string{flag, "/tmp/proj"})
		if err == nil {
			t.Errorf("Parse(%q) should reject removed flag", flag)
		}
	}

	for _, f := range Flags {
		if f.Long == "--dir" || f.Short == "-d" {
			t.Errorf("Flags registry should not contain %s/%s", f.Short, f.Long)
		}
	}

	usage := FlagUsage()
	if strings.Contains(usage, "--dir") {
		t.Error("FlagUsage() should not contain --dir")
	}
}

// Verifies that --auto-merge flag defaults to false and is set to true when
// the flag is present, matching ralph.sh's AUTO_MERGE variable.
func TestAutoMergeFlag(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AutoMerge {
		t.Error("AutoMerge should default to false")
	}

	cfg, err = Parse([]string{"--auto-merge"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.AutoMerge {
		t.Error("AutoMerge should be true after --auto-merge")
	}
}

// Verifies that flags previously missing a ConfigKey now have one,
// ensuring they can be loaded from config.toml.
func TestNewConfigKeysExist(t *testing.T) {
	required := map[string]bool{
		"idle_timeout":          false,
		"idle_timeout_progress": false,
		"max_run_duration":      false,
	}
	for _, f := range Flags {
		if _, ok := required[f.ConfigKey]; ok {
			required[f.ConfigKey] = true
		}
	}
	for key, found := range required {
		if !found {
			t.Errorf("ConfigKey %q missing from Flags registry", key)
		}
	}
}

// --merge-admin was removed; passing it should produce a parse error.
func TestMergeAdminFlagRemoved(t *testing.T) {
	_, err := Parse([]string{"--merge-admin"})
	if err == nil {
		t.Error("expected error for removed --merge-admin flag")
	}
}

// Verifies that a positional directory argument is rejected — ralph loop
// uses cwd, not a positional arg.
func TestPositionalDirectoryRejected(t *testing.T) {
	dir := t.TempDir()
	_, err := Parse([]string{dir})
	if err == nil {
		t.Fatal("expected error for positional directory arg")
	}
	if !strings.Contains(err.Error(), "unknown argument") {
		t.Errorf("error should mention 'unknown argument', got %q", err)
	}
}

// Verifies that a bare positional argument that isn't a directory is rejected,
// preventing unknown words (e.g. misspelled subcommands) from silently creating
// orphan project directories.
func TestPositionalNonDirectoryRejected(t *testing.T) {
	_, err := Parse([]string{"notasubcommand"})
	if err == nil {
		t.Fatal("expected error for non-directory positional arg")
	}
	if !strings.Contains(err.Error(), "unknown argument") {
		t.Errorf("error should mention 'unknown argument', got %q", err)
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

// Verifies that idle_timeout and idle_timeout_progress config keys override
// defaults, accepting both Go duration strings and bare seconds.
func TestIdleTimeoutConfigKeys(t *testing.T) {
	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("idle_timeout = 5m\nidle_timeout_progress = 15s\n"), 0o644)

	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := cfg.LoadConfigFile(path); err != nil {
		t.Fatalf("LoadConfigFile failed: %v", err)
	}
	if cfg.IdleTimeout != 5*time.Minute {
		t.Errorf("IdleTimeout = %s, want 5m", cfg.IdleTimeout)
	}
	if cfg.IdleTimeoutProgress != 15*time.Second {
		t.Errorf("IdleTimeoutProgress = %s, want 15s", cfg.IdleTimeoutProgress)
	}

	// Bare integer interpreted as seconds.
	os.WriteFile(path, []byte("idle_timeout = 120\n"), 0o644)
	cfg, err = Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := cfg.LoadConfigFile(path); err != nil {
		t.Fatalf("LoadConfigFile failed: %v", err)
	}
	if cfg.IdleTimeout != 2*time.Minute {
		t.Errorf("IdleTimeout = %s, want 2m0s for bare 120", cfg.IdleTimeout)
	}
}

// Verifies that RALPH_IDLE_TIMEOUT and RALPH_IDLE_TIMEOUT_PROGRESS env vars
// override the Flags registry defaults.
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
	for _, flag := range []string{"-n"} {
		_, err := Parse([]string{flag})
		if err == nil {
			t.Errorf("Parse(%q) should error on missing value", flag)
		}
	}
}

// Verifies that non-numeric values for integer flags produce an error.
func TestInvalidNumericArg(t *testing.T) {
	for _, flag := range []string{"-n"} {
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

	// "attach" is a subcommand
	sub, ok = ParseSubcommand([]string{"attach"})
	if !ok {
		t.Fatal("expected subcommand for 'attach'")
	}
	if sub.Name != "attach" || sub.Dir != "." {
		t.Errorf("got %+v, want Name=attach Dir=.", sub)
	}

	// "command" and "commander" are no longer recognized
	_, ok = ParseSubcommand([]string{"command"})
	if ok {
		t.Error("'command' should no longer be a recognized subcommand")
	}
	_, ok = ParseSubcommand([]string{"commander"})
	if ok {
		t.Error("'commander' should no longer be a recognized subcommand")
	}

	// "task" is a subcommand
	sub, ok = ParseSubcommand([]string{"task"})
	if !ok {
		t.Fatal("expected subcommand for 'task'")
	}
	if sub.Name != "task" || sub.Dir != "." {
		t.Errorf("got %+v, want Name=task Dir=.", sub)
	}

	// "loop" is a subcommand
	sub, ok = ParseSubcommand([]string{"loop"})
	if !ok {
		t.Fatal("expected subcommand for 'loop'")
	}
	if sub.Name != "loop" || sub.Dir != "." {
		t.Errorf("got %+v, want Name=loop Dir=.", sub)
	}

	// "loop" with flags passes them through as Args
	sub, ok = ParseSubcommand([]string{"loop", "--max", "20", "--auto-merge"})
	if !ok {
		t.Fatal("expected subcommand for 'loop' with flags")
	}
	if sub.Name != "loop" {
		t.Errorf("Name = %q, want loop", sub.Name)
	}
	if len(sub.Args) != 3 || sub.Args[0] != "--max" {
		t.Errorf("Args = %v, want [--max 20 --auto-merge]", sub.Args)
	}

	// "review" is a subcommand
	sub, ok = ParseSubcommand([]string{"review"})
	if !ok {
		t.Fatal("expected subcommand for 'review'")
	}
	if sub.Name != "review" || sub.Dir != "." {
		t.Errorf("got %+v, want Name=review Dir=.", sub)
	}

	// Non-subcommand
	_, ok = ParseSubcommand([]string{"/some/path"})
	if ok {
		t.Error("expected no subcommand for a path argument")
	}
}

// Verifies that subcommands always use cwd — positional directory arguments
// are passed through as Args, not consumed as Dir.
func TestSubcommandAlwaysUsesCwd(t *testing.T) {
	dir := t.TempDir()
	sub, ok := ParseSubcommand([]string{"task", dir})
	if !ok {
		t.Fatal("expected subcommand")
	}
	if sub.Dir != "." {
		t.Errorf("Dir = %q, want \".\" (cwd)", sub.Dir)
	}
	if len(sub.Args) != 1 || sub.Args[0] != dir {
		t.Errorf("Args = %v, want [%s]", sub.Args, dir)
	}
}

func TestStopIgnoresDirectoryArg(t *testing.T) {
	dir := t.TempDir()
	sub, ok := ParseSubcommand([]string{"stop", dir})
	if !ok {
		t.Fatal("expected subcommand")
	}
	if sub.Dir != "." {
		t.Errorf("stop should always use current dir, got Dir = %q", sub.Dir)
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

	sub, _ := ParseSubcommand([]string{"stop"})
	// Override Dir for testing since stop always uses "."
	sub.Dir = dir
	stopFile := filepath.Join(sub.Dir, ".ralph", "stop")
	if err := os.WriteFile(stopFile, nil, 0o644); err != nil {
		t.Fatalf("failed to create stop file: %v", err)
	}
	if _, err := os.Stat(stopFile); err != nil {
		t.Errorf("stop file not created: %v", err)
	}
}

// --- Config file (config.toml) tests, ported from config.bats ---

// Verifies that LoadConfigFile sets values from a config.toml file,
// matching bats test "load_config sets variables from config.toml".
func TestLoadConfigSetsValuesFromTOML(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")

	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte("max_iterations = 25\ncalls_per_hour = 40\nstuck_threshold = 10\nstagnation_threshold = 5\n"), 0o644)

	cfg, _ := Parse(nil)
	cfg.LoadConfigFile(tomlPath)

	if cfg.MaxIterations != 25 {
		t.Errorf("MaxIterations = %d, want 25", cfg.MaxIterations)
	}
	if cfg.CallsPerHour != 40 {
		t.Errorf("CallsPerHour = %d, want 40", cfg.CallsPerHour)
	}
	if cfg.StuckThreshold != 10 {
		t.Errorf("StuckThreshold = %d, want 10", cfg.StuckThreshold)
	}
	if cfg.StagnationThreshold != 5 {
		t.Errorf("StagnationThreshold = %d, want 5", cfg.StagnationThreshold)
	}
}

// A boolean flag set to "false" in config.toml must stay OFF. Bool flag Apply
// funcs are presence-based (they ignore the value and set true), so the loader
// must honor the explicit value — otherwise "key = false" enables the flag.
// Regression: bare `ralph loop` failed with
// "--admin-merge-on-ci-infra-failure requires --auto-merge" because
// "admin_merge_on_ci_infra_failure = false" was read as true.
func TestLoadConfigBoolFalseStaysOff(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte("admin_merge_on_ci_infra_failure = false\nnotify = false\n"), 0o644)

	cfg, _ := Parse(nil)
	if err := cfg.LoadConfigFile(tomlPath); err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}

	if cfg.AdminMergeOnCIInfraFailure {
		t.Error("admin_merge_on_ci_infra_failure = false must stay off")
	}
	if cfg.Notify {
		t.Error("notify = false must stay off")
	}
}

// A boolean flag set to a truthy value in config.toml must turn ON.
func TestLoadConfigBoolTrueTurnsOn(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte("admin_merge_on_ci_infra_failure = true\nnotify = true\n"), 0o644)

	cfg, _ := Parse(nil)
	if err := cfg.LoadConfigFile(tomlPath); err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}

	if !cfg.AdminMergeOnCIInfraFailure {
		t.Error("admin_merge_on_ci_infra_failure = true must turn the flag on")
	}
	if !cfg.Notify {
		t.Error("notify = true must turn the flag on")
	}
}

// The presence-based truthy spellings (1/yes/on) must enable a value-ignoring
// bool flag loaded from config — admin_merge uses the presence-based Apply.
func TestLoadConfigBoolTruthySpellings(t *testing.T) {
	for _, v := range []string{"1", "yes", "on", "TRUE"} {
		dir := t.TempDir()
		tomlPath := filepath.Join(dir, "config.toml")
		os.WriteFile(tomlPath, []byte("admin_merge_on_ci_infra_failure = "+v+"\n"), 0o644)

		cfg, _ := Parse(nil)
		if err := cfg.LoadConfigFile(tomlPath); err != nil {
			t.Fatalf("LoadConfigFile(%q): %v", v, err)
		}
		if !cfg.AdminMergeOnCIInfraFailure {
			t.Errorf("admin_merge_on_ci_infra_failure = %q must turn the flag on", v)
		}
	}
}

// Verifies that CLI args take precedence over config file values,
// matching bats test "CLI args override config file values".
func TestCLIArgsOverrideConfigFile(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")

	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte("max_iterations = 25\ncalls_per_hour = 40\n"), 0o644)

	cfg, _ := Parse([]string{"-n", "99"})
	cfg.LoadConfigFile(tomlPath)

	if cfg.MaxIterations != 99 {
		t.Errorf("MaxIterations = %d, want 99 (CLI should override config file)", cfg.MaxIterations)
	}
	if cfg.CallsPerHour != 40 {
		t.Errorf("CallsPerHour = %d, want 40 (from config file)", cfg.CallsPerHour)
	}
}

// Verifies that LoadConfigFile is a no-op when file does not exist,
// matching bats test "load_config is a no-op when file does not exist".
func TestLoadConfigNoOpWhenFileMissing(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")

	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	cfg, _ := Parse(nil)
	origMax := cfg.MaxIterations
	origStuck := cfg.StuckThreshold

	err := cfg.LoadConfigFile("/nonexistent/path/config.toml")
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if cfg.MaxIterations != origMax {
		t.Errorf("MaxIterations changed from %d to %d", origMax, cfg.MaxIterations)
	}
	if cfg.StuckThreshold != origStuck {
		t.Errorf("StuckThreshold changed from %d to %d", origStuck, cfg.StuckThreshold)
	}
}

// Verifies that LoadConfigFile ignores comments and blank lines,
// matching bats test "load_config ignores comments and blank lines".
func TestLoadConfigIgnoresCommentsAndBlankLines(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")

	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	content := "# This is a comment\nmax_iterations = 30\n\n  # indented comment\ncalls_per_hour = 60\n"
	os.WriteFile(tomlPath, []byte(content), 0o644)

	cfg, _ := Parse(nil)
	cfg.LoadConfigFile(tomlPath)

	if cfg.MaxIterations != 30 {
		t.Errorf("MaxIterations = %d, want 30", cfg.MaxIterations)
	}
	if cfg.CallsPerHour != 60 {
		t.Errorf("CallsPerHour = %d, want 60", cfg.CallsPerHour)
	}
}

// Verifies that LoadConfigFile strips inline comments,
// matching bats test "load_config strips inline comments".
func TestLoadConfigStripsInlineComments(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")

	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte("max_iterations = 15 # keep it short\n"), 0o644)

	cfg, _ := Parse(nil)
	cfg.LoadConfigFile(tomlPath)

	if cfg.MaxIterations != 15 {
		t.Errorf("MaxIterations = %d, want 15", cfg.MaxIterations)
	}
}

// Verifies that LoadConfigFile handles quoted values,
// matching bats test "load_config handles quoted values".
func TestLoadConfigHandlesQuotedValues(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")

	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte("max_iterations = \"20\"\n"), 0o644)

	cfg, _ := Parse(nil)
	cfg.LoadConfigFile(tomlPath)

	if cfg.MaxIterations != 20 {
		t.Errorf("MaxIterations = %d, want 20", cfg.MaxIterations)
	}
}

// Verifies that LoadConfigFile handles all 9 config keys,
// matching bats test "load_config handles all config keys".
func TestLoadConfigHandlesAllKeys(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")

	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	content := `max_iterations = 10
calls_per_hour = 20
watcher_interval = 5
stuck_threshold = 8
stuck_confirmation_threshold = 4
stagnation_threshold = 6
test_saturation_threshold = 7
permission_denial_threshold = 9
`
	os.WriteFile(tomlPath, []byte(content), 0o644)

	cfg, _ := Parse(nil)
	cfg.LoadConfigFile(tomlPath)

	checks := []struct {
		name string
		got  int
		want int
	}{
		{"MaxIterations", cfg.MaxIterations, 10},
		{"CallsPerHour", cfg.CallsPerHour, 20},
		{"WatcherInterval", cfg.WatcherInterval, 5},
		{"StuckThreshold", cfg.StuckThreshold, 8},
		{"StuckConfirmationThreshold", cfg.StuckConfirmationThreshold, 4},
		{"StagnationThreshold", cfg.StagnationThreshold, 6},
		{"TestSaturationThreshold", cfg.TestSaturationThreshold, 7},
		{"PermissionDenialThreshold", cfg.PermissionDenialThreshold, 9},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// Verifies that the three stagnation/skip threshold keys (introduced by ralph-tttf)
// are read from config.toml and override the hardcoded defaults (2, 1, 3).
func TestStagnationThresholdKeysFromConfig(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte("max_failed_starts = 5\nmax_compaction_parks = 3\ncascade_skip_limit = 7\n"), 0o644)

	cfg, _ := Parse(nil)
	cfg.LoadConfigFile(tomlPath)

	if cfg.MaxFailedStarts != 5 {
		t.Errorf("MaxFailedStarts = %d, want 5", cfg.MaxFailedStarts)
	}
	if cfg.MaxCompactionParks != 3 {
		t.Errorf("MaxCompactionParks = %d, want 3", cfg.MaxCompactionParks)
	}
	if cfg.CascadeSkipLimit != 7 {
		t.Errorf("CascadeSkipLimit = %d, want 7", cfg.CascadeSkipLimit)
	}
}

// Verifies that the three stagnation/skip threshold keys default to 2, 1, and 2
// when absent from config.toml (no behavior change when unset — ralph-tttf AC2;
// cascade_skip_limit default lowered to 2 by ralph-qlmy since the second
// same-reason skip is now conclusive and a third full attempt is waste).
func TestStagnationThresholdKeyDefaults(t *testing.T) {
	cfg := Defaults()

	if cfg.MaxFailedStarts != 2 {
		t.Errorf("MaxFailedStarts default = %d, want 2", cfg.MaxFailedStarts)
	}
	if cfg.MaxCompactionParks != 1 {
		t.Errorf("MaxCompactionParks default = %d, want 1", cfg.MaxCompactionParks)
	}
	if cfg.CascadeSkipLimit != 2 {
		t.Errorf("CascadeSkipLimit default = %d, want 2", cfg.CascadeSkipLimit)
	}
}

// Verifies that stagnation threshold from config flows into analyzer behavior.
// Matching bats test "analyze_iteration uses configurable stagnation threshold".
// This is tested here at the config level: a threshold of 2 means the config
// correctly stores and returns that value for the analyzer to consume.
func TestStagnationThresholdFromConfig(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")

	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte("stagnation_threshold = 2\n"), 0o644)

	cfg, _ := Parse(nil)
	cfg.LoadConfigFile(tomlPath)

	if cfg.StagnationThreshold != 2 {
		t.Errorf("StagnationThreshold = %d, want 2", cfg.StagnationThreshold)
	}
}

// Verifies that InitConfig generates a config.toml with all expected keys,
// matching bats test "init-config generates config.toml".
func TestInitConfigGeneratesFile(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")

	err := InitConfig(tomlPath)
	if err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}

	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("failed to read generated config: %v", err)
	}

	content := string(data)
	for _, key := range []string{"max_iterations", "stuck_threshold", "calls_per_hour", "stagnation_threshold"} {
		if !strings.Contains(content, key) {
			t.Errorf("generated config missing key %q", key)
		}
	}
}

// Verifies that InitConfig includes the verify key as a commented-out entry,
// so greenfield projects can see the option and uncomment it to skip script detection.
func TestInitConfigIncludesVerifyComment(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")

	if err := InitConfig(tomlPath); err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}

	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("failed to read generated config: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "# verify = ") {
		t.Errorf("generated config should contain commented verify key, got:\n%s", content)
	}
}

// Verifies that InitConfig omits keys with empty defaults (post_task, verify_build)
// so the generated file doesn't contain confusing "post_task = " lines.
func TestInitConfigOmitsEmptyDefaults(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")

	if err := InitConfig(tomlPath); err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}

	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("failed to read generated config: %v", err)
	}

	content := string(data)
	for _, key := range []string{"post_task", "verify_build"} {
		if strings.Contains(content, key) {
			t.Errorf("generated config should not contain empty-default key %q", key)
		}
	}
}

// Verifies that InitConfig writes keys in alphabetical order.
func TestInitConfigKeysAreAlphabetical(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")

	if err := InitConfig(tomlPath); err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}

	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("failed to read generated config: %v", err)
	}

	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if eqIdx := strings.Index(line, " = "); eqIdx >= 0 {
			keys = append(keys, line[:eqIdx])
		}
	}

	for i := 1; i < len(keys); i++ {
		if keys[i] < keys[i-1] {
			t.Errorf("keys not in alphabetical order: %q comes after %q", keys[i], keys[i-1])
		}
	}
}

// Verifies that InitConfig refuses to overwrite an existing config file,
// matching bats test "init-config refuses to overwrite existing config".
func TestInitConfigRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte("existing"), 0o644)

	err := InitConfig(tomlPath)
	if err == nil {
		t.Fatal("expected error when config file already exists")
	}

	data, _ := os.ReadFile(tomlPath)
	if string(data) != "existing" {
		t.Errorf("file was modified: got %q, want %q", string(data), "existing")
	}
}

// Verifies that --evolve flag is parsed and requires --auto-merge.
func TestEvolveFlag(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Evolve {
		t.Error("Evolve should default to false")
	}

	cfg, err = Parse([]string{"--evolve", "--auto-merge"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Evolve {
		t.Error("Evolve should be true after --evolve")
	}
}

// Verifies that --evolve validation rejects missing --auto-merge
// and incompatible --tmux.
func TestEvolveValidation(t *testing.T) {
	cfg, _ := Parse([]string{"--evolve", "--base-branch", "main"})
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error: --evolve without --auto-merge")
	}

	cfg, _ = Parse([]string{"--evolve", "--auto-merge", "--tmux", "--base-branch", "main"})
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error: --evolve with --tmux")
	}

	cfg, _ = Parse([]string{"--evolve", "--auto-merge", "--base-branch", "main"})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid --evolve combo should pass: %v", err)
	}
}

// Verifies that --admin-merge-on-ci-infra-failure validation rejects missing --auto-merge.
func TestAdminMergeOnCIInfraFailureValidation(t *testing.T) {
	cfg, _ := Parse([]string{"--admin-merge-on-ci-infra-failure", "--base-branch", "main"})
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error: --admin-merge-on-ci-infra-failure without --auto-merge")
	}

	cfg, _ = Parse([]string{"--admin-merge-on-ci-infra-failure", "--auto-merge", "--base-branch", "main"})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid --admin-merge-on-ci-infra-failure combo should pass: %v", err)
	}
}

// Verifies that --branch-strategy is rejected as an unknown flag.
func TestBranchStrategyFlag_Rejected(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")


	_, err := Parse([]string{"--branch-strategy", "stacked"})
	if err == nil {
		t.Fatal("expected error for removed --branch-strategy flag")
	}
}

// Verifies that branch_strategy in config.toml is silently ignored
// so existing config files don't cause errors.
func TestBranchStrategyConfigFile_SilentlyIgnored(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")


	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte("branch_strategy = stacked\n"), 0o644)

	cfg, _ := Parse(nil)
	cfg.LoadConfigFile(tomlPath)
	_ = cfg
}

// Verifies base_branch has no default, can be set via --base-branch CLI flag,
// via config.toml, and that CLI takes precedence over config file.
func TestBaseBranch(t *testing.T) {
	t.Setenv("RALPH_BASE_BRANCH", "")

	cfg, _ := Parse(nil)
	if cfg.BaseBranch != "" {
		t.Errorf("BaseBranch = %q, want \"\" (no default)", cfg.BaseBranch)
	}

	cfg, _ = Parse([]string{"--base-branch", "main"})
	if cfg.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want \"main\" from CLI flag", cfg.BaseBranch)
	}

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte("base_branch = staging\n"), 0o644)

	cfg, _ = Parse(nil)
	cfg.LoadConfigFile(tomlPath)
	if cfg.BaseBranch != "staging" {
		t.Errorf("BaseBranch = %q, want \"staging\" from config file", cfg.BaseBranch)
	}

	cfg, _ = Parse([]string{"--base-branch", "main"})
	cfg.LoadConfigFile(tomlPath)
	if cfg.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want \"main\" (CLI should override config file)", cfg.BaseBranch)
	}
}

// Verifies RALPH_BASE_BRANCH env var overrides the hardcoded default.
func TestBaseBranchEnvVar(t *testing.T) {
	t.Setenv("RALPH_BASE_BRANCH", "staging")

	cfg, _ := Parse(nil)
	if cfg.BaseBranch != "staging" {
		t.Errorf("BaseBranch = %q, want \"staging\" from env var", cfg.BaseBranch)
	}
}

// Verifies --wait defaults to false and is set when the flag is present.
func TestWaitFlag(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Wait {
		t.Error("Wait should default to false")
	}

	cfg, err = Parse([]string{"--wait"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Wait {
		t.Error("Wait should be true after --wait")
	}
}

// Verifies --wait-interval flag was removed and is rejected by Parse.
func TestWaitIntervalFlagRemoved(t *testing.T) {
	_, err := Parse([]string{"--wait-interval", "1m"})
	if err == nil {
		t.Fatal("expected error for removed --wait-interval flag, got nil")
	}
}

// Verifies RALPH_WAIT_INTERVAL env var is no longer read.
func TestWaitIntervalEnvVarRemoved(t *testing.T) {
	t.Setenv("RALPH_WAIT_INTERVAL", "2m")

	// Parse should succeed — the env var is simply ignored.
	_, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Verifies ConfigToState captures non-default config values as a map
// suitable for state.json persistence.
func TestConfigToState_CapturesNonDefaults(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")
	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")
	t.Setenv("RALPH_BASE_BRANCH", "")


	cfg, _ := Parse([]string{
		"--max", "20",
		"--auto-merge",
		"--evolve",
	})

	m := ConfigToState(&cfg)

	if m["max"] != "20" {
		t.Errorf("max = %q, want 20", m["max"])
	}
	if m["auto-merge"] != "true" {
		t.Errorf("auto-merge = %q, want true", m["auto-merge"])
	}
	if m["evolve"] != "true" {
		t.Errorf("evolve = %q, want true", m["evolve"])
	}
	// Default values should be omitted.
	if _, ok := m["calls-per-hour"]; ok {
		t.Error("calls-per-hour should be omitted when at default")
	}
}

// Verifies ArgsFromState reconstructs CLI args from a state map,
// only including flags the current binary recognizes.
func TestArgsFromState_ReconstructsArgs(t *testing.T) {
	state := map[string]string{
		"max":        "20",
		"auto-merge": "true",
		"evolve":     "true",
	}

	args := ArgsFromState(state)

	t.Setenv("RALPH_MAX_ITERATIONS", "")
	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")
	t.Setenv("RALPH_BASE_BRANCH", "")

	cfg, err := Parse(args)
	if err != nil {
		t.Fatalf("Parse(ArgsFromState) failed: %v", err)
	}
	if cfg.MaxIterations != 20 {
		t.Errorf("MaxIterations = %d, want 20", cfg.MaxIterations)
	}
	if !cfg.AutoMerge {
		t.Error("AutoMerge should be true")
	}
	if !cfg.Evolve {
		t.Error("Evolve should be true")
	}
}

// Verifies that ArgsFromState silently ignores unknown state keys from old
// binary versions — the core acceptance criterion for evolve restart safety.
func TestArgsFromState_IgnoresUnknownKeys(t *testing.T) {
	state := map[string]string{
		"dir":         "/tmp/project",
		"auto-merge":  "true",
		"evolve":      "true",
		"refactor":    "true", // removed flag from old binary
		"some-future": "value",
	}

	args := ArgsFromState(state)

	t.Setenv("RALPH_MAX_ITERATIONS", "")
	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")
	t.Setenv("RALPH_BASE_BRANCH", "")

	cfg, err := Parse(args)
	if err != nil {
		t.Fatalf("Parse should succeed after ignoring unknown keys, got: %v", err)
	}
	if !cfg.AutoMerge {
		t.Error("AutoMerge should be true")
	}
	if !cfg.Evolve {
		t.Error("Evolve should be true")
	}
}

// Verifies the full round-trip: Config → ConfigToState → ArgsFromState → Parse
// produces an equivalent Config (for the fields that matter to evolve restart).
func TestConfigToState_ArgsFromState_Roundtrip(t *testing.T) {
	t.Setenv("RALPH_MAX_ITERATIONS", "")
	t.Setenv("RALPH_IDLE_TIMEOUT", "")
	t.Setenv("RALPH_IDLE_TIMEOUT_PROGRESS", "")
	t.Setenv("RALPH_BASE_BRANCH", "")


	original, _ := Parse([]string{
		"--max", "30",
		"--auto-merge",
		"--evolve",
		"--wait",
		"--base-branch", "main",
	})

	state := ConfigToState(&original)
	args := ArgsFromState(state)
	restored, err := Parse(args)
	if err != nil {
		t.Fatalf("Parse(ArgsFromState(ConfigToState)) failed: %v", err)
	}

	if restored.MaxIterations != original.MaxIterations {
		t.Errorf("MaxIterations = %d, want %d", restored.MaxIterations, original.MaxIterations)
	}
	if restored.AutoMerge != original.AutoMerge {
		t.Errorf("AutoMerge = %v, want %v", restored.AutoMerge, original.AutoMerge)
	}
	if restored.Evolve != original.Evolve {
		t.Errorf("Evolve = %v, want %v", restored.Evolve, original.Evolve)
	}
	if restored.Wait != original.Wait {
		t.Errorf("Wait = %v, want %v", restored.Wait, original.Wait)
	}
	if restored.BaseBranch != original.BaseBranch {
		t.Errorf("BaseBranch = %q, want %q", restored.BaseBranch, original.BaseBranch)
	}
}

// Proves: working_model in config.toml is loaded into cfg.WorkingModel.
func TestWorkingModelLoadedFromTOML(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte("working_model = "+ModelHaiku+"\n"), 0o644)

	cfg, _ := Parse(nil)
	if err := cfg.LoadConfigFile(tomlPath); err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if cfg.WorkingModel != ModelHaiku {
		t.Errorf("cfg.WorkingModel = %q, want %q", cfg.WorkingModel, ModelHaiku)
	}
}

// Verifies Validate() fails when BaseBranch is empty and succeeds when set,
// proving ralph loop exits before the first iteration when no base branch is configured.
func TestBaseBranchMandatoryValidation(t *testing.T) {
	t.Setenv("RALPH_BASE_BRANCH", "")

	// No flag, env, or config: BaseBranch is "" → Validate must error
	cfg, _ := Parse(nil)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() should error when BaseBranch is not set")
	}
	for _, want := range []string{"--base-branch", "RALPH_BASE_BRANCH", "base_branch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %v", want, err)
		}
	}

	// Set via --base-branch flag: should pass
	cfg, _ = Parse([]string{"--base-branch", "main"})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() should not error when --base-branch is set: %v", err)
	}
}

// Proves AC4 of ralph-55ww: log_retention_days is configurable via config.toml
// and defaults to 30. Zero disables pruning.
func TestLogRetentionDays(t *testing.T) {
	cfg, _ := Parse(nil)
	if cfg.LogRetentionDays != 30 {
		t.Errorf("LogRetentionDays default = %d, want 30", cfg.LogRetentionDays)
	}

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	os.WriteFile(tomlPath, []byte("log_retention_days = 90\n"), 0o644)

	cfg, _ = Parse(nil)
	cfg.LoadConfigFile(tomlPath)
	if cfg.LogRetentionDays != 90 {
		t.Errorf("LogRetentionDays = %d, want 90 from config file", cfg.LogRetentionDays)
	}

	os.WriteFile(tomlPath, []byte("log_retention_days = 0\n"), 0o644)
	cfg, _ = Parse(nil)
	cfg.LoadConfigFile(tomlPath)
	if cfg.LogRetentionDays != 0 {
		t.Errorf("LogRetentionDays = %d, want 0 (disabled)", cfg.LogRetentionDays)
	}
}


