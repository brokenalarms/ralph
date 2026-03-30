package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Verifies that Available() returns a boolean reflecting whether tmux is
// on the PATH, so the caller can fall back to inline mode.
func TestAvailable(t *testing.T) {
	got := Available()
	// Just verify it doesn't panic — result depends on test host.
	_ = got
}

// Verifies that filterStreamCmd builds a command using the ralph binary
// and raw log path, so the tmux stream pane runs the Go filter.
func TestFilterStreamCmd(t *testing.T) {
	s := &Session{
		ScriptPath: "/usr/local/bin/ralph",
		RawLogPath: "/tmp/project/.ralph/raw.log",
	}

	cmd := s.filterStreamCmd()
	if !strings.Contains(cmd, "filter-stream") {
		t.Error("filterStreamCmd should include 'filter-stream' subcommand")
	}
	if !strings.Contains(cmd, "/usr/local/bin/ralph") {
		t.Error("filterStreamCmd should include the ralph binary path")
	}
	if !strings.Contains(cmd, "raw.log") {
		t.Error("filterStreamCmd should include the raw log path")
	}
}

// Verifies that writePlanWatcher generates a bd-style plan script when
// TaskBackend is "bd", showing current task, ready queue, and progress.
func TestWritePlanWatcher_BD(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		RalphDir:    dir,
			}

	if err := s.writePlanWatcher(); err != nil {
		t.Fatalf("writePlanWatcher() error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".plan-watch.sh"))
	content := string(data)

	if !strings.Contains(content, "bd list --status in_progress") {
		t.Error("bd plan watcher missing in_progress query")
	}
	if !strings.Contains(content, "bd ready --json") {
		t.Error("bd plan watcher missing JSON-based ready queue")
	}
	if !strings.Contains(content, `select(.id != $cid)`) {
		t.Error("bd plan watcher missing jq filter for current task ID")
	}
	if !strings.Contains(content, "bd count --status closed") {
		t.Error("bd plan watcher missing progress counter")
	}
	if !strings.Contains(content, "bd count --status open") {
		t.Error("bd plan watcher should compute total from status-filtered counts")
	}
}

// Verifies that BuildRalphCmd strips --tmux and appends --quiet,
// matching the shell behavior where the re-exec'd ralph runs headless.
func TestBuildRalphCmd(t *testing.T) {
	cmd := BuildRalphCmd("/usr/local/bin/ralph.sh", []string{
		"--tmux",
		"-n", "10",
	})

	if strings.Contains(cmd, "--tmux") {
		t.Error("BuildRalphCmd should strip --tmux")
	}
	if !strings.Contains(cmd, "--quiet") {
		t.Error("BuildRalphCmd should append --quiet")
	}
	if !strings.Contains(cmd, "/usr/local/bin/ralph.sh") {
		t.Error("BuildRalphCmd should include script path")
	}
	if !strings.Contains(cmd, " loop ") {
		t.Error("BuildRalphCmd should include 'loop' subcommand")
	}
}

// Verifies that shellQuote handles empty strings, simple strings,
// and strings with single quotes correctly.
func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "''"},
		{"simple", "'simple'"},
		{"it's", "'it'\"'\"'s'"},
		{"/path/to/file", "'/path/to/file'"},
	}

	for _, tt := range tests {
		got := shellQuote(tt.input)
		if got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// Verifies that the plan watcher disables terminal echo so arrow key
// presses and other escape sequences don't clutter the display-only pane.
func TestWritePlanWatcher_DisablesEcho(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		RalphDir:    dir,
			}

	if err := s.writePlanWatcher(); err != nil {
		t.Fatalf("writePlanWatcher() error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".plan-watch.sh"))
	if !strings.Contains(string(data), "stty -echo") {
		t.Error("plan watcher should disable terminal echo to suppress escape sequences")
	}
}

// Verifies that touchFile creates the .plan-refresh sentinel so the plan
// pane renders on startup without waiting for the first iteration.
func TestTouchFile_CreatesPlanRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".plan-refresh")

	touchFile(path)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error(".plan-refresh should exist after touchFile")
	}
}

// Verifies that Setup removes a stale .stream-task file from a previous run,
// so the stream pane doesn't briefly show the old task before the loop writes
// the current one.
func TestSetup_ClearsStaleStreamTask(t *testing.T) {
	dir := t.TempDir()
	staleFile := filepath.Join(dir, ".stream-task")
	os.WriteFile(staleFile, []byte("ralph-old: Previous task"), 0o644)

	s := &Session{
		Name:        "test-session",
		RalphDir:    dir,
			}

	// Setup will fail on createSession (no tmux), but the stale file
	// cleanup happens before that.
	_ = s.Setup()

	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Error(".stream-task should be removed on Setup to prevent showing stale task")
	}
}

// Verifies that SessionName derives the session name from the project
// directory basename with "-loop" suffix, making concurrent ralph
// sessions distinguishable in tmux ls.
func TestSessionName_BasenameLoop(t *testing.T) {
	// Stub sessionExists so test doesn't depend on live tmux state.
	orig := sessionExists
	sessionExists = func(string) bool { return false }
	defer func() { sessionExists = orig }()

	got := SessionName("/home/user/projects/tabi")
	if got != "tabi-loop" {
		t.Errorf("SessionName(/home/user/projects/tabi) = %q, want %q", got, "tabi-loop")
	}
}

// Verifies that sanitizeSessionName replaces dots and colons (which are
// invalid in tmux session names) with hyphens.
func TestSanitizeSessionName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"my.project", "my-project"},
		{"host:path", "host-path"},
		{"normal", "normal"},
		{"dots.and:colons", "dots-and-colons"},
	}

	for _, tt := range tests {
		got := sanitizeSessionName(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeSessionName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// Verifies that completed tasks are rendered as a comma-separated inline
// list with a "Done:" prefix in dim styling, providing a compact summary
// of finished work that stays visually receded.
func TestWritePlanWatcher_BD_ShowsCompletedTasks(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		RalphDir:    dir,
			}

	if err := s.writePlanWatcher(); err != nil {
		t.Fatalf("writePlanWatcher() error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".plan-watch.sh"))
	content := string(data)

	if !strings.Contains(content, ".completed-tasks") {
		t.Error("bd plan watcher should reference .completed-tasks file")
	}
	if !strings.Contains(content, "DIM") {
		t.Error("bd plan watcher should use dim styling for completed tasks")
	}
	if strings.Contains(content, "while IFS= read") {
		t.Error("completed tasks should be comma-separated inline, not one-per-line")
	}
	if !strings.Contains(content, "Done:") {
		t.Error("completed tasks should be labeled with 'Done:' prefix")
	}
}

// Verifies that Setup clears stale .completed-tasks from a previous run
// so the plan pane doesn't show old completions.
func TestSetup_ClearsStaleCompletedTasks(t *testing.T) {
	dir := t.TempDir()
	staleFile := filepath.Join(dir, ".completed-tasks")
	os.WriteFile(staleFile, []byte("ralph-old\n"), 0o644)

	s := &Session{
		Name:        "test-session",
		RalphDir:    dir,
			}

	_ = s.Setup()

	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Error(".completed-tasks should be removed on Setup to prevent showing stale completions")
	}
}

// Verifies that applySessionOptions installs a pane-died hook that auto-kills
// the session when pane 0 (ralph loop) dies, replacing the old root-level q
// binding that stole keypresses from all panes.
func TestApplySessionOptions_PaneDiedHook(t *testing.T) {
	orig := tmuxCmd
	var calls [][]string
	tmuxCmd = func(args ...string) error {
		calls = append(calls, args)
		return nil
	}
	defer func() { tmuxCmd = orig }()

	s := &Session{Name: "test-loop"}
	s.applySessionOptions()

	var hookArgs []string
	for _, args := range calls {
		if len(args) >= 4 && args[0] == "set-hook" && args[3] == "pane-died" {
			hookArgs = args
			break
		}
	}
	if hookArgs == nil {
		t.Fatal("applySessionOptions should install a pane-died hook")
	}

	hookCmd := strings.Join(hookArgs[4:], " ")
	if !strings.Contains(hookCmd, "pane_dead") {
		t.Error("pane-died hook should check #{pane_dead} on pane 0")
	}
	if !strings.Contains(hookCmd, "kill-session") {
		t.Error("pane-died hook should kill the session when pane 0 is dead")
	}
	if !strings.Contains(hookCmd, "test-loop") {
		t.Error("pane-died hook should target the correct session")
	}

	// Regression: root-level key bindings steal keypresses from all panes.
	for _, args := range calls {
		if len(args) >= 3 && args[0] == "bind-key" && args[1] == "-T" && args[2] == "root" {
			t.Errorf("applySessionOptions must not use root-level bind-key, found: %v", args)
		}
	}
}

// Verifies that the bd plan watcher renders a simple "X/Y done" counter
// in green, so progress is visible at a glance without visual noise.
func TestWritePlanWatcher_BD_DoneCounter(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		RalphDir:    dir,
			}

	if err := s.writePlanWatcher(); err != nil {
		t.Fatalf("writePlanWatcher() error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".plan-watch.sh"))
	content := string(data)

	if !strings.Contains(content, "done${NC}") {
		t.Error("bd plan watcher should show done counter with green color reset")
	}
	if strings.Contains(content, "bar_w=") {
		t.Error("bd plan watcher should use simple counter, not progress bar")
	}
}

// Verifies that the plan watcher checks for a .plan-flash signal file and
// briefly highlights the pane border green when a task completes.
func TestWritePlanWatcher_FlashSignal(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		RalphDir:    dir,
			}

	if err := s.writePlanWatcher(); err != nil {
		t.Fatalf("writePlanWatcher() error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".plan-watch.sh"))
	content := string(data)

	if !strings.Contains(content, ".plan-flash") {
		t.Error("plan watcher should check for .plan-flash signal file")
	}
	if !strings.Contains(content, "pane-border-style") {
		t.Error("plan watcher should set pane-border-style for flash effect")
	}
}

// Verifies that BuildRalphCmd strips "loop" and --tmux but preserves other args.
func TestBuildRalphCmd_StripsLoopAndTmux(t *testing.T) {
	cmd := BuildRalphCmd("/usr/local/bin/ralph", []string{
		"loop",
		"--tmux",
		"-n", "10",
	})

	if strings.Contains(cmd, "--tmux") {
		t.Error("BuildRalphCmd should strip --tmux")
	}
	if !strings.Contains(cmd, "--quiet") {
		t.Error("BuildRalphCmd should append --quiet")
	}
	if !strings.Contains(cmd, " loop ") {
		t.Error("BuildRalphCmd should include 'loop' subcommand")
	}
	if !strings.Contains(cmd, "'-n'") {
		t.Error("BuildRalphCmd should preserve -n flag")
	}
}

// Verifies that the plan watcher periodically re-renders the bd ready list
// without requiring a .plan-refresh signal, so new beads added during a long
// iteration (e.g. from ralph task in another pane) appear within ~45 seconds.
func TestWritePlanWatcher_BD_PeriodicRefresh(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		RalphDir:    dir,
			}

	if err := s.writePlanWatcher(); err != nil {
		t.Fatalf("writePlanWatcher() error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".plan-watch.sh"))
	content := string(data)

	if !strings.Contains(content, "poll_counter") {
		t.Error("plan watcher should use a poll_counter for periodic refresh")
	}
	if !strings.Contains(content, "poll_interval") {
		t.Error("plan watcher should define a poll_interval for the refresh period")
	}
}

// Verifies that applySessionOptions disables set-titles so that pane border
// title updates don't propagate to the iTerm window title, preserving
// Claude's /rename of the terminal window.
func TestApplySessionOptions_DisablesSetTitles(t *testing.T) {
	orig := tmuxCmd
	var calls [][]string
	tmuxCmd = func(args ...string) error {
		calls = append(calls, args)
		return nil
	}
	defer func() { tmuxCmd = orig }()

	s := &Session{Name: "test-loop"}
	s.applySessionOptions()

	found := false
	for _, args := range calls {
		if len(args) >= 5 && args[0] == "set-option" && args[3] == "set-titles" && args[4] == "off" {
			found = true
			break
		}
	}
	if !found {
		t.Error("applySessionOptions should set 'set-titles off' to prevent tmux from overwriting iTerm window title")
	}
}

// Verifies that .plan-refresh signal path is correctly embedded in the
// plan watcher script, so the pane redraws when signaled.
func TestPlanWatcher_SignalPath(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		RalphDir:    dir,
			}

	s.writePlanWatcher() //nolint:errcheck
	data, _ := os.ReadFile(filepath.Join(dir, ".plan-watch.sh"))
	content := string(data)

	expectedSignal := dir + "/.plan-refresh"
	if !strings.Contains(content, expectedSignal) {
		t.Errorf("plan watcher missing signal path %q", expectedSignal)
	}
}
