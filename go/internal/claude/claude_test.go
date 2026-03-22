package claude

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testLogger discards all output but records calls for verification.
type testLogger struct {
	tasks    []string
	successes []string
}

func (l *testLogger) Log(format string, args ...any)         {}
func (l *testLogger) Warn(format string, args ...any)        {}
func (l *testLogger) Error(format string, args ...any)       {}
func (l *testLogger) Task(format string, args ...any)        { l.tasks = append(l.tasks, fmt.Sprintf(format, args...)) }
func (l *testLogger) TaskSuccess(format string, args ...any) { l.successes = append(l.successes, fmt.Sprintf(format, args...)) }

// --- Signal file tests ---

// Verifies that ClearSignals removes all three signal files, so a new
// iteration starts with a clean slate and doesn't pick up stale signals.
func TestClearSignals(t *testing.T) {
	dir := t.TempDir()
	s := DefaultSignalPaths(dir)

	// Create all signal files.
	for _, p := range []string{s.Complete, s.CurrentTask, s.AllComplete} {
		if err := os.WriteFile(p, []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ClearSignals(s)

	for _, p := range []string{s.Complete, s.CurrentTask, s.AllComplete} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("signal file should be removed: %s", p)
		}
	}
}

// Verifies that DefaultSignalPaths returns paths rooted under the ralph dir,
// matching the convention used by the bash implementation.
func TestDefaultSignalPaths(t *testing.T) {
	s := DefaultSignalPaths("/tmp/.ralph")
	if s.Complete != "/tmp/.ralph/.signal_complete" {
		t.Errorf("unexpected complete path: %s", s.Complete)
	}
	if s.CurrentTask != "/tmp/.ralph/.signal_current_task" {
		t.Errorf("unexpected current task path: %s", s.CurrentTask)
	}
	if s.AllComplete != "/tmp/.ralph/.signal_all_complete" {
		t.Errorf("unexpected all complete path: %s", s.AllComplete)
	}
}

// Verifies that hasSignal returns true only when the file exists, which is
// how the polling loop detects that Claude has written a completion signal.
func TestHasSignal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "signal")

	if hasSignal(path) {
		t.Error("hasSignal should be false for missing file")
	}

	os.WriteFile(path, []byte("done"), 0o644)
	if !hasSignal(path) {
		t.Error("hasSignal should be true after file is created")
	}
}

// Verifies that readFirstLine extracts only the first line of a signal file,
// matching the bash `head -1` behavior used for reading task descriptions.
func TestReadFirstLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "signal")

	os.WriteFile(path, []byte("first line\nsecond line\n"), 0o644)
	got := readFirstLine(path)
	if got != "first line" {
		t.Errorf("readFirstLine = %q, want %q", got, "first line")
	}
}

// Verifies empty/missing files return empty string.
func TestReadFirstLineEmpty(t *testing.T) {
	dir := t.TempDir()
	if v := readFirstLine(filepath.Join(dir, "nonexistent")); v != "" {
		t.Errorf("readFirstLine of missing file = %q, want empty", v)
	}

	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, []byte(""), 0o644)
	if v := readFirstLine(empty); v != "" {
		t.Errorf("readFirstLine of empty file = %q, want empty", v)
	}
}

// Verifies that readSignalSummary prefers the all-complete file over the
// single-task complete file, matching the bash implementation's priority.
func TestReadSignalSummary_PrefersAllComplete(t *testing.T) {
	dir := t.TempDir()
	s := DefaultSignalPaths(dir)

	os.WriteFile(s.Complete, []byte("task done"), 0o644)
	os.WriteFile(s.AllComplete, []byte("everything done"), 0o644)

	got := readSignalSummary(s)
	if got != "everything done" {
		t.Errorf("readSignalSummary = %q, want %q", got, "everything done")
	}
}

// Verifies that readSignalSummary falls back to the complete file when
// no all-complete signal exists.
func TestReadSignalSummary_FallsBackToComplete(t *testing.T) {
	dir := t.TempDir()
	s := DefaultSignalPaths(dir)

	os.WriteFile(s.Complete, []byte("single task done"), 0o644)

	got := readSignalSummary(s)
	if got != "single task done" {
		t.Errorf("readSignalSummary = %q, want %q", got, "single task done")
	}
}

// Verifies that check_current_task (hasSignal) detects the current-task file,
// and read_current_task (readFirstLine) extracts its content.
func TestCheckAndReadCurrentTask(t *testing.T) {
	dir := t.TempDir()
	s := DefaultSignalPaths(dir)

	ClearSignals(s)

	if hasSignal(s.CurrentTask) {
		t.Error("hasSignal should be false after clear")
	}

	os.WriteFile(s.CurrentTask, []byte("Working on auth\n"), 0o644)

	if !hasSignal(s.CurrentTask) {
		t.Error("hasSignal should be true after writing current task")
	}
	got := readFirstLine(s.CurrentTask)
	if got != "Working on auth" {
		t.Errorf("readFirstLine = %q, want %q", got, "Working on auth")
	}
}

// Verifies that the ALL_COMPLETE signal is detected via hasSignal on the
// AllComplete path.
func TestAllCompleteSignalDetected(t *testing.T) {
	dir := t.TempDir()
	s := DefaultSignalPaths(dir)

	ClearSignals(s)

	if hasSignal(s.AllComplete) {
		t.Error("hasSignal should be false for all_complete after clear")
	}

	os.WriteFile(s.AllComplete, []byte("All tasks finished\n"), 0o644)

	if !hasSignal(s.AllComplete) {
		t.Error("hasSignal should be true after writing all_complete")
	}
}

// --- Allowed tools tests ---

// Verifies that IterationAllowedTools contains the core tools Claude needs
// for iteration mode, preventing accidental removal of required tools.
func TestIterationAllowedTools_ContainsCoreTools(t *testing.T) {
	required := []string{
		"Bash(*)", "Read", "Edit", "Write",
		"Glob", "Grep", "Agent",
	}
	toolSet := make(map[string]bool)
	for _, tool := range IterationAllowedTools {
		toolSet[tool] = true
	}
	for _, tool := range required {
		if !toolSet[tool] {
			t.Errorf("IterationAllowedTools missing required tool: %s", tool)
		}
	}
}

// Verifies that IterationAllowedTools does not include --dangerously-skip-permissions
// or any blanket bypass — each tool must be explicitly listed.
func TestIterationAllowedTools_NoBlanketBypass(t *testing.T) {
	for _, tool := range IterationAllowedTools {
		if strings.Contains(strings.ToLower(tool), "dangerously") ||
			strings.Contains(strings.ToLower(tool), "bypass") {
			t.Errorf("IterationAllowedTools should not contain bypass entries: %s", tool)
		}
	}
}

// Verifies that the joined format produces a valid comma-separated list
// suitable for --allowedTools flag.
func TestIterationAllowedTools_JoinFormat(t *testing.T) {
	joined := strings.Join(IterationAllowedTools, ",")
	parts := strings.Split(joined, ",")
	if len(parts) != len(IterationAllowedTools) {
		t.Errorf("joined tools split into %d parts, want %d", len(parts), len(IterationAllowedTools))
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			t.Error("joined tools contain empty entry")
		}
	}
}

// --- Stream text extraction tests ---

// Verifies that extractStreamText pulls text from assistant messages using
// Claude's actual nested content array format: message.content[].text.
func TestExtractStreamText_Assistant(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}`
	got := extractStreamText(line)
	if got != "Hello world" {
		t.Errorf("extractStreamText = %q, want %q", got, "Hello world")
	}
}

// Verifies that tool_use content blocks produce a short summary with the
// tool name and its primary target (file_path, command, etc.).
func TestExtractStreamText_AssistantToolUse(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/foo.go"}}]}}`
	got := extractStreamText(line)
	if got != "[Read] /tmp/foo.go" {
		t.Errorf("extractStreamText = %q, want %q", got, "[Read] /tmp/foo.go")
	}
}

// Verifies that messages with both text and tool_use content blocks are
// concatenated with newlines.
func TestExtractStreamText_AssistantMixed(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"reading file"},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`
	got := extractStreamText(line)
	want := "reading file\n[Bash] ls"
	if got != want {
		t.Errorf("extractStreamText = %q, want %q", got, want)
	}
}

// Verifies text extraction from content_block_delta events (the most
// common streaming event type during Claude output).
func TestExtractStreamText_ContentBlockDelta(t *testing.T) {
	line := `{"type":"content_block_delta","delta":{"text":"partial output"}}`
	got := extractStreamText(line)
	if got != "partial output" {
		t.Errorf("extractStreamText = %q, want %q", got, "partial output")
	}
}

// Verifies that error responses are extracted from result events so users
// see API errors in the log.
func TestExtractStreamText_ResultError(t *testing.T) {
	line := `{"type":"result","subtype":"error_response","error":"rate limited"}`
	got := extractStreamText(line)
	if got != "rate limited" {
		t.Errorf("extractStreamText = %q, want %q", got, "rate limited")
	}
}

// Verifies that non-text JSON events are silently skipped.
func TestExtractStreamText_IgnoresNonTextEvents(t *testing.T) {
	lines := []string{
		`{"type":"message_start"}`,
		`{"type":"content_block_start","index":0}`,
		`not json at all`,
		``,
	}
	for _, line := range lines {
		if got := extractStreamText(line); got != "" {
			t.Errorf("extractStreamText(%q) = %q, want empty", line, got)
		}
	}
}

// --- JSON fragment stripping tests ---

// Verifies that stripJSONFragment removes lines that are entirely JSON,
// preventing raw stream-json events from appearing in log summaries.
func TestStripJSONFragment_PureJSON(t *testing.T) {
	got := stripJSONFragment(`{"type":"result","message":"done"}`)
	if got != "" {
		t.Errorf("stripJSONFragment(pure JSON) = %q, want empty", got)
	}
}

// Verifies that trailing JSON fragments are trimmed from signal summaries,
// which can occur when stream-json output races with signal file writes.
func TestStripJSONFragment_TrailingJSON(t *testing.T) {
	got := stripJSONFragment(`task completed{"type":"result"}`)
	if got != "task completed" {
		t.Errorf("stripJSONFragment(trailing) = %q, want %q", got, "task completed")
	}
}

// Verifies that clean text passes through unchanged.
func TestStripJSONFragment_CleanText(t *testing.T) {
	got := stripJSONFragment("Fixed the login bug")
	if got != "Fixed the login bug" {
		t.Errorf("stripJSONFragment(clean) = %q, want %q", got, "Fixed the login bug")
	}
}

// Verifies that readFirstLine strips JSON fragments from signal files,
// so log messages like "Completed: <summary>" stay human-readable.
func TestReadFirstLine_StripsJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "signal")

	os.WriteFile(path, []byte(`summary text{"type":"result"}`+"\n"), 0o644)
	got := readFirstLine(path)
	if got != "summary text" {
		t.Errorf("readFirstLine = %q, want %q", got, "summary text")
	}
}

// --- Stream filter tailing tests ---

// Verifies that filterStreamJSON follows new data appended to the raw log
// (like tail -f), rather than reading to EOF and exiting.
func TestFilterStreamJSON_TailsFile(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.log")
	logPath := filepath.Join(dir, "loop.log")

	// Create the raw log so the filter can open it.
	os.WriteFile(rawPath, nil, 0o644)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		filterStreamJSON(rawPath, logPath, stop)
	}()

	// Append a stream-json event after the filter has started.
	time.Sleep(200 * time.Millisecond)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"text","text":"hello from claude"}]}}`)
	f.Close()

	// Give the filter time to process, then stop it.
	time.Sleep(300 * time.Millisecond)
	close(stop)
	<-done

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "[claude] hello from claude") {
		t.Errorf("loop.log should contain [claude]-prefixed filtered text, got: %q", string(got))
	}
}

// Verifies that filterStreamJSON prefixes each output line with [claude]
// so loop.log clearly distinguishes Claude's output from ralph's logger output.
func TestFilterStreamJSON_PrefixesWithSource(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.log")
	logPath := filepath.Join(dir, "loop.log")

	os.WriteFile(rawPath, nil, 0o644)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		filterStreamJSON(rawPath, logPath, stop)
	}()

	time.Sleep(200 * time.Millisecond)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Write a tool-use event and a text event.
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/foo.go"}}]}}`)
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"some delta text"}}`)
	f.Close()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	<-done

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(got)
	if !strings.Contains(content, "[claude] [Read] /tmp/foo.go") {
		t.Errorf("tool-use line should have [claude] prefix, got: %q", content)
	}
	if !strings.Contains(content, "[claude] some delta text") {
		t.Errorf("delta text should have [claude] prefix, got: %q", content)
	}
}

// --- Process lifecycle tests ---

// Verifies that Run detects a completion signal written by an external
// process (simulated with a short-lived sleep command), which is the
// core mechanism ralph uses to know when Claude has finished a task.
func TestRun_DetectsCompletionSignal(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Write the completion signal after a short delay. Use write+rename for
	// atomicity — os.WriteFile truncates then writes, creating a window where
	// the file exists but is empty.
	go func() {
		time.Sleep(300 * time.Millisecond)
		tmp := signals.Complete + ".tmp"
		os.WriteFile(tmp, []byte("task finished"), 0o644)
		os.Rename(tmp, signals.Complete)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Quiet:        true,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
	}

	// Use sleep as a long-running stand-in for claude.
	result := runWithCommand(t, &runner, cfg, "sleep", "10")

	if !result.SignalDetected {
		t.Error("expected SignalDetected to be true")
	}
	if result.Summary != "task finished" {
		t.Errorf("Summary = %q, want %q", result.Summary, "task finished")
	}
	if len(log.successes) == 0 {
		t.Error("expected TaskSuccess to be called")
	}
}

// Verifies that Run detects the all-complete signal and sets AllComplete=true,
// which tells the main loop that no more iterations are needed.
func TestRun_DetectsAllCompleteSignal(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	go func() {
		time.Sleep(300 * time.Millisecond)
		os.WriteFile(signals.AllComplete, []byte("all done"), 0o644)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Quiet:        true,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "10")

	if !result.AllComplete {
		t.Error("expected AllComplete to be true")
	}
}

// Verifies that the OnTaskDetected callback fires when the current-task
// signal file appears, which is how the main loop renames branches.
func TestRun_CallsOnTaskDetected(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	var detectedTask string
	runner := Runner{
		Logger: log,
		OnTaskDetected: func(desc string) {
			detectedTask = desc
		},
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		os.WriteFile(signals.CurrentTask, []byte("implement feature X"), 0o644)
		time.Sleep(300 * time.Millisecond)
		os.WriteFile(signals.Complete, []byte("done"), 0o644)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Quiet:        true,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
	}

	runWithCommand(t, &runner, cfg, "sleep", "10")

	if detectedTask != "implement feature X" {
		t.Errorf("OnTaskDetected got %q, want %q", detectedTask, "implement feature X")
	}
	if len(log.tasks) == 0 {
		t.Error("expected Task log to be called")
	}
}

// Verifies that Run handles a process that exits on its own (no signal),
// and still does the final signal check. This covers the case where Claude
// crashes or hits a rate limit.
func TestRun_ProcessExitsWithoutSignal(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Quiet:        true,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
	}

	// Use a command that exits immediately.
	result := runWithCommand(t, &runner, cfg, "true")

	if result.SignalDetected {
		t.Error("expected SignalDetected to be false when no signal written")
	}
}

// Verifies that ClearSignals is called at the start of Run, so stale signals
// from a previous iteration don't cause false detection.
func TestRun_ClearsSignalsBeforeStart(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	// Pre-create stale signals.
	os.WriteFile(signals.Complete, []byte("stale"), 0o644)
	os.WriteFile(signals.CurrentTask, []byte("stale task"), 0o644)

	log := &testLogger{}
	runner := Runner{Logger: log}

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Quiet:        true,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
	}

	// Process exits immediately; stale signals should have been cleared.
	result := runWithCommand(t, &runner, cfg, "true")

	if result.SignalDetected {
		t.Error("stale signals should have been cleared before polling")
	}
}

// --- Idle timeout tests ---

// Verifies that poll kills the session and returns IdleTimeout=true when the
// raw log has no new output for longer than the configured idle timeout.
func TestPoll_IdleTimeoutKillsSession(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Quiet:        true,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		IdleTimeout:  200 * time.Millisecond,
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "10")

	if !result.IdleTimeout {
		t.Error("expected IdleTimeout to be true")
	}
	if result.SignalDetected {
		t.Error("expected SignalDetected to be false on idle timeout")
	}
}

// Verifies that ongoing raw log activity prevents the idle timeout from
// firing, so a busy Claude session is not killed prematurely.
func TestPoll_ActivityResetsIdleTimer(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Keep writing to raw log faster than the idle timeout.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"working"}}`)
				f.Close()
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()

	// Write completion signal after activity keeps it alive past the idle timeout.
	go func() {
		time.Sleep(400 * time.Millisecond)
		os.WriteFile(signals.Complete, []byte("done"), 0o644)
		close(stop)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Quiet:        true,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		IdleTimeout:  200 * time.Millisecond,
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "10")

	if result.IdleTimeout {
		t.Error("expected IdleTimeout to be false when activity resets timer")
	}
	if !result.SignalDetected {
		t.Error("expected SignalDetected to be true")
	}
}

// Verifies that the shorter progress-aware timeout is used when HasProgress
// returns true, catching sessions that did work then went idle.
func TestPoll_ProgressTimeoutShorterThanDefault(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	cfg := RunConfig{
		WorkDir:             dir,
		RalphDir:            dir,
		Prompt:              "echo test",
		RawLog:              rawLog,
		Quiet:               true,
		Signals:             signals,
		PollInterval:        50 * time.Millisecond,
		IdleTimeout:         5 * time.Second,
		IdleTimeoutProgress: 200 * time.Millisecond,
		HasProgress:         func() bool { return true },
	}

	start := time.Now()
	result := runWithCommand(t, &runner, cfg, "sleep", "10")
	elapsed := time.Since(start)

	if !result.IdleTimeout {
		t.Error("expected IdleTimeout to be true with progress timeout")
	}
	if elapsed > 2*time.Second {
		t.Errorf("progress timeout should fire quickly, took %s", elapsed)
	}
}

// Verifies that idle timeout is disabled when IdleTimeout is zero.
func TestPoll_ZeroIdleTimeoutDisablesDetection(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Write completion quickly — with zero timeout, it should complete normally.
	go func() {
		time.Sleep(200 * time.Millisecond)
		os.WriteFile(signals.Complete, []byte("done"), 0o644)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Quiet:        true,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		IdleTimeout:  0,
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "10")

	if result.IdleTimeout {
		t.Error("expected no idle timeout when IdleTimeout is 0")
	}
	if !result.SignalDetected {
		t.Error("expected SignalDetected to be true")
	}
}

// --- Process group cleanup tests ---

// Verifies that stopProcessGroup kills child processes spawned by a bash
// pipeline, not just the top-level bash process. This is the fix for orphaned
// tail/jq/perl/sed processes that accumulated across iterations.
func TestStopProcessGroup_KillsChildProcesses(t *testing.T) {
	// Spawn a bash script that starts a background sleep child, then waits.
	// The child's PID is written to a file so we can check if it's still alive.
	dir := t.TempDir()
	childPidFile := filepath.Join(dir, "child.pid")

	script := fmt.Sprintf(`sleep 60 & echo $! > %s; wait`, childPidFile)
	cmd := exec.Command("bash", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait for the child PID file to appear.
	var childPid string
	for i := 0; i < 50; i++ {
		time.Sleep(50 * time.Millisecond)
		data, err := os.ReadFile(childPidFile)
		if err == nil && len(data) > 0 {
			childPid = strings.TrimSpace(string(data))
			break
		}
	}
	if childPid == "" {
		t.Fatal("child PID file never appeared")
	}

	// Kill the process group.
	stopProcessGroup(cmd)

	// Verify the child process is dead. Signal 0 checks existence.
	checkCmd := exec.Command("kill", "-0", childPid)
	if err := checkCmd.Run(); err == nil {
		t.Errorf("child process %s should be dead after stopProcessGroup, but it's still alive", childPid)
	}
}

// Verifies that stopProcessGroup handles a nil command without panicking.
func TestStopProcessGroup_NilCmd(t *testing.T) {
	stopProcessGroup(nil)
	stopProcessGroup(&exec.Cmd{})
}

// --- Test helpers ---

// runWithCommand replaces the claude command with an arbitrary command for
// testing. It directly manipulates the exec.Cmd instead of going through
// the full Run path (which hardcodes "claude").
func runWithCommand(t *testing.T, r *Runner, cfg RunConfig, name string, args ...string) Result {
	t.Helper()

	if cfg.PollInterval == 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}

	clearSignals(cfg.Signals)

	rawLog, err := os.OpenFile(cfg.RawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer rawLog.Close()

	cmd := exec.Command(name, args...)
	cmd.Dir = cfg.WorkDir
	cmd.Stdin = nil
	cmd.Stdout = rawLog
	cmd.Stderr = rawLog

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	result := r.poll(cmd, cfg)

	_ = cmd.Wait()

	// Final signal check (mirrors Run behavior).
	if !result.SignalDetected {
		if hasSignal(cfg.Signals.Complete) || hasSignal(cfg.Signals.AllComplete) {
			result.SignalDetected = true
			result.AllComplete = hasSignal(cfg.Signals.AllComplete)
			result.Summary = readSignalSummary(cfg.Signals)
		}
	}

	return result
}
