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

// Verifies that writeStreamFilter creates an executable script that pipes
// raw JSON log through jq, perl, and sed for colored output.
func TestWriteStreamFilter(t *testing.T) {
	dir := t.TempDir()
	s := &Session{RalphDir: dir}

	if err := s.writeStreamFilter(); err != nil {
		t.Fatalf("writeStreamFilter() error: %v", err)
	}

	path := filepath.Join(dir, ".stream-filter.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "tail -f") {
		t.Error("stream filter missing tail -f")
	}
	if !strings.Contains(content, "jq --raw-input") {
		t.Error("stream filter missing jq invocation")
	}

	info, _ := os.Stat(path)
	if info.Mode()&0o111 == 0 {
		t.Error("stream filter is not executable")
	}
}

// Verifies that writePlanWatcher generates a bd-style plan script when
// TaskBackend is "bd", showing current task, ready queue, and progress.
func TestWritePlanWatcher_BD(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		RalphDir:    dir,
		TaskBackend: "bd",
	}

	if err := s.writePlanWatcher(); err != nil {
		t.Fatalf("writePlanWatcher() error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".plan-watch.sh"))
	content := string(data)

	if !strings.Contains(content, "bd list --status in_progress") {
		t.Error("bd plan watcher missing in_progress query")
	}
	if !strings.Contains(content, "bd ready") {
		t.Error("bd plan watcher missing ready queue")
	}
	if !strings.Contains(content, "bd count --status closed") {
		t.Error("bd plan watcher missing progress counter")
	}
}

// Verifies that writePlanWatcher generates a checklist-style plan script
// that cats the plan file when TaskBackend is not "bd".
func TestWritePlanWatcher_Checklist(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		RalphDir:    dir,
		TaskBackend: "checklist",
		PlanFile:    "/tmp/test-plan.md",
	}

	if err := s.writePlanWatcher(); err != nil {
		t.Fatalf("writePlanWatcher() error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".plan-watch.sh"))
	content := string(data)

	if !strings.Contains(content, "cat '/tmp/test-plan.md'") {
		t.Error("checklist plan watcher missing plan file cat")
	}
}

// Verifies that BuildRalphCmd strips --tmux and appends --quiet,
// matching the shell behavior where the re-exec'd ralph runs headless.
func TestBuildRalphCmd(t *testing.T) {
	cmd := BuildRalphCmd("/usr/local/bin/ralph.sh", []string{
		"--dir", "/projects/myapp",
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
	if !strings.Contains(cmd, "/projects/myapp") {
		t.Error("BuildRalphCmd should include original args")
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

// Verifies that .plan-refresh signal path is correctly embedded in the
// plan watcher script, so the pane redraws when signaled.
func TestPlanWatcher_SignalPath(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		RalphDir:    dir,
		TaskBackend: "bd",
	}

	s.writePlanWatcher() //nolint:errcheck
	data, _ := os.ReadFile(filepath.Join(dir, ".plan-watch.sh"))
	content := string(data)

	expectedSignal := dir + "/.plan-refresh"
	if !strings.Contains(content, expectedSignal) {
		t.Errorf("plan watcher missing signal path %q", expectedSignal)
	}
}
