package claude

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// testLogger discards all output but records calls for verification.
type testLogger struct {
	logs      []string
	successes []string
	opts      []logging.Opts
}

func (l *testLogger) Emit(o logging.Opts, format string, args ...any) {
	l.opts = append(l.opts, o)
	msg := fmt.Sprintf(format, args...)
	if o.Level == logging.Success {
		l.successes = append(l.successes, msg)
	} else {
		l.logs = append(l.logs, msg)
	}
}
func (l *testLogger) AgentLog(_ logging.Domain, format string, args ...any) {
	l.logs = append(l.logs, fmt.Sprintf(format, args...))
}

// --- Signal file tests ---

// Verifies that ClearSignals removes all three signal files, so a new
// iteration starts with a clean slate and doesn't pick up stale signals.
func TestClearSignals(t *testing.T) {
	dir := t.TempDir()
	s := DefaultSignalPaths(dir)

	// Create all signal files.
	for _, p := range []string{s.Complete, s.CurrentTask, s.AllComplete, s.NoCodeNeeded} {
		if err := os.WriteFile(p, []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ClearSignals(s)

	for _, p := range []string{s.Complete, s.CurrentTask, s.AllComplete, s.NoCodeNeeded} {
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
	if s.NoCodeNeeded != "/tmp/.ralph/.signal_no_code_needed" {
		t.Errorf("unexpected no-code-needed path: %s", s.NoCodeNeeded)
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

// --- Stdin injection tests ---

// Verifies UserInputMessage produces valid JSON with the stream-json
// input format expected by Claude Code's --input-format stream-json.
func TestUserInputMessage_Format(t *testing.T) {
	got := UserInputMessage("fix the tests")
	want := `{"type":"user_input_text","content":"fix the tests"}`
	if got != want {
		t.Errorf("UserInputMessage = %q, want %q", got, want)
	}
}

// Verifies that UserInputMessage correctly JSON-escapes multiline content
// with special characters, which is typical for test failure output.
func TestUserInputMessage_EscapesSpecialChars(t *testing.T) {
	msg := "test failed:\n\texpected \"foo\"\n\tgot \"bar\""
	got := UserInputMessage(msg)
	// The content should be JSON-escaped inside the JSON string.
	if !strings.Contains(got, `\n`) {
		t.Errorf("expected JSON-escaped newlines, got: %s", got)
	}
	if !strings.Contains(got, `\"foo\"`) {
		t.Errorf("expected JSON-escaped quotes, got: %s", got)
	}
}

// Verifies that InjectMessage returns an error when no pipe is available,
// which is the expected state before Run() is called or after it returns.
func TestInjectMessage_NoPipe(t *testing.T) {
	runner := &Runner{Logger: &testLogger{}}
	err := runner.InjectMessage("hello")
	if err == nil {
		t.Error("InjectMessage should fail when no stdin pipe is available")
	}
}

// Verifies that InjectMessage writes the message as a JSON line to the
// stdin pipe, which the running agent reads as a follow-up user message.
func TestInjectMessage_WritesToPipe(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()

	runner := &Runner{Logger: &testLogger{}}
	runner.stdinPipe = pw

	if err := runner.InjectMessage("user feedback here"); err != nil {
		t.Fatalf("InjectMessage failed: %v", err)
	}
	pw.Close()

	buf := make([]byte, 4096)
	n, _ := pr.Read(buf)
	got := strings.TrimSpace(string(buf[:n]))
	want := `{"type":"user_input_text","content":"user feedback here"}`
	if got != want {
		t.Errorf("pipe content = %q, want %q", got, want)
	}
}

// Verifies that when a feedback signal file exists, the agent is killed
// and FeedbackKill is returned so the orchestrator can restart it.
// Content is already on the bead — the file is a signal only.
func TestPoll_FeedbackSignalKillsAgent(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)
	feedbackFile := filepath.Join(dir, "feedback")

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Empty file — signal only, no content.
	os.WriteFile(feedbackFile, nil, 0o644)

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
		FeedbackFile: feedbackFile,
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "0.1")

	if !result.FeedbackKill {
		t.Error("expected FeedbackKill when feedback signal file exists")
	}
	// Feedback file should be removed after detection.
	if _, err := os.Stat(feedbackFile); !os.IsNotExist(err) {
		t.Error("feedback signal file should be removed after detection")
	}
}

// Verifies that a feedback file written DURING an active agent run (after
// poll starts) is detected and produces FeedbackKill=true. This covers the
// real-world scenario where a user sends feedback while the agent is working.
func TestPoll_FeedbackDuringRunKillsAgent(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)
	feedbackFile := filepath.Join(dir, "feedback")

	log := &testLogger{}
	runner := Runner{Logger: log}

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
		FeedbackFile: feedbackFile,
	}

	// Write the feedback file once the poll loop has started (raw log created
	// after clearSignals), simulating a user sending feedback while the agent is
	// already running. Gating on poll start instead of a fixed delay keeps the
	// write from being wiped by clearSignals without waiting a magic duration.
	go func() {
		waitPollStarted(t, cfg.RawLog)
		os.WriteFile(feedbackFile, nil, 0o644)
	}()

	result := runWithCommand(t, &runner, cfg, "sleep", "1")

	if !result.FeedbackKill {
		t.Error("expected FeedbackKill when feedback file written during agent run")
	}
	if _, err := os.Stat(feedbackFile); !os.IsNotExist(err) {
		t.Error("feedback signal file should be removed after detection")
	}
}

// Verifies that feedback takes priority over a completion signal. When both
// exist simultaneously (e.g. agent signals completion while user sends
// feedback), the feedback file must be detected first so the agent restarts
// with the user's corrections rather than proceeding through verification.
func TestPoll_FeedbackTakesPriorityOverCompletion(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)
	feedbackFile := filepath.Join(dir, "feedback")

	log := &testLogger{}
	runner := Runner{Logger: log}

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
		FeedbackFile: feedbackFile,
		OnSignal: func(summary string) bool {
			t.Error("OnSignal should not be called when feedback takes priority")
			return true
		},
	}

	// Create both files after poll starts so clearSignals doesn't remove them.
	go func() {
		waitPollStarted(t, cfg.RawLog)
		os.WriteFile(feedbackFile, nil, 0o644)
		os.WriteFile(signals.Complete, []byte("task done"), 0o644)
	}()

	result := runWithCommand(t, &runner, cfg, "sleep", "1")

	if !result.FeedbackKill {
		t.Error("expected FeedbackKill to take priority over completion signal")
	}
	if result.SignalDetected {
		t.Error("expected SignalDetected=false when feedback takes priority")
	}
}

// Verifies that feedback is detected while OnSignal verification is running.
// This is the regression scenario: the agent signals completion, OnSignal
// starts running tests/LLM verification (blocking the poll loop), and the
// user sends feedback during that blocking period. The feedback must still
// be detected and cause the agent to restart.
func TestPoll_FeedbackDuringOnSignalBlockKillsAgent(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)
	feedbackFile := filepath.Join(dir, "feedback")

	log := &testLogger{}
	runner := Runner{Logger: log}

	// onSignalEntered closes the instant OnSignal begins, giving the feedback
	// producer a precise observable for "OnSignal is now blocking the poll loop".
	onSignalEntered := make(chan struct{})
	var once sync.Once
	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
		FeedbackFile: feedbackFile,
		OnSignal: func(summary string) bool {
			once.Do(func() { close(onSignalEntered) })
			// Block for 5s (as a real verification would) to validate that
			// feedback detection interrupts the callback rather than waiting for
			// it to return. Routed through an elapsed-time wait instead of a bare
			// sleep so the blocking window is an observable, not a magic sleep.
			waitElapsed(t, time.Now(), 5*time.Second, "OnSignal verification block to elapse")
			return false
		},
	}

	// Write completion signal after poll starts to trigger OnSignal.
	go func() {
		waitPollStarted(t, cfg.RawLog)
		os.WriteFile(signals.Complete, []byte("task done"), 0o644)
	}()

	// Write feedback file once OnSignal is actually blocking the poll loop.
	go func() {
		<-onSignalEntered
		os.WriteFile(feedbackFile, nil, 0o644)
	}()

	start := time.Now()
	result := runWithCommand(t, &runner, cfg, "sleep", "1")
	elapsed := time.Since(start)

	if !result.FeedbackKill {
		t.Errorf("expected FeedbackKill when feedback written during OnSignal block, got %+v", result)
	}
	// Must detect feedback quickly, not wait for OnSignal to return.
	if elapsed > 3*time.Second {
		t.Errorf("feedback detection took %s — should be detected within ~1s, not after OnSignal returns", elapsed)
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

// Verifies that startStreamFilter uses the pure Go filter (no bash/tail/jq
// child processes), which eliminates the orphaned process accumulation bug.
func TestStartStreamFilter_NoExternalProcesses(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.log")
	logPath := filepath.Join(dir, "loop.log")
	os.WriteFile(rawPath, nil, 0o644)

	runner := &Runner{Logger: &testLogger{}}
	stop := make(chan struct{})
	done := runner.startStreamFilter(RunConfig{
		RalphDir: dir,
		RawLog:   rawPath,
		LogFile:  logPath,
	}, stop)

	// Append a stream event and verify it's filtered.
	waitFilterStarted(t)
	f, _ := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"go filter works"}}`)
	f.Close()

	// Wait for the filtered line to land in loop.log, then stop the filter.
	waitForLogContains(t, logPath, "[r] go filter works")
	close(stop)
	<-done

	got, _ := os.ReadFile(logPath)
	plain := ansiRe.ReplaceAllString(string(got), "")
	if !strings.Contains(plain, "[r] go filter works") {
		t.Errorf("expected Go filter output, got: %q", string(got))
	}

	// Verify no bash/tail child processes were spawned. If any child
	// processes exist, the old external filter codepath was used.
	out, _ := exec.Command("pgrep", "-P", fmt.Sprintf("%d", os.Getpid())).Output()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// Check if it's a test-related process (go test runner) — ignore those.
		nameOut, _ := exec.Command("ps", "-p", line, "-o", "comm=").Output()
		name := strings.TrimSpace(string(nameOut))
		if strings.Contains(name, "tail") || strings.Contains(name, "jq") ||
			strings.Contains(name, "perl") || strings.Contains(name, "sed") {
			t.Errorf("found orphaned %s process (PID %s) — stream filter should use pure Go", name, line)
		}
	}
}

// Proves that StopStreaming drains active goroutines so a subsequent Run()
// on the same Runner cannot accumulate duplicate filter/tail writers on the
// same log files. Simulates the OnSignal verification pattern where
// StopStreaming is called before spawning a verification agent.
func TestStopStreaming_PreventsGoroutineAccumulation(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.log")
	logPath := filepath.Join(dir, "loop.log")
	os.WriteFile(rawPath, nil, 0o644)

	runner := &Runner{Logger: &testLogger{}}

	// Start streaming goroutines via startStreamFilter.
	filterStop := make(chan struct{})
	filterDone := runner.startStreamFilter(RunConfig{
		RalphDir: dir,
		RawLog:   rawPath,
		LogFile:  logPath,
	}, filterStop)

	tailStop := make(chan struct{})
	tailDone := startTailGoroutine(logPath, tailStop)

	runner.mu.Lock()
	runner.filterStop = filterStop
	runner.filterDone = filterDone
	runner.tailStop = tailStop
	runner.tailDone = tailDone
	runner.mu.Unlock()

	// Let the goroutines open their files and seek to EOF before stopping them.
	waitFilterStarted(t)

	// StopStreaming should drain both goroutines.
	runner.StopStreaming()

	// After StopStreaming, channels should be nil — no active goroutines.
	runner.mu.Lock()
	if runner.filterStop != nil || runner.tailStop != nil {
		t.Error("StopStreaming should nil out channels after draining")
	}
	runner.mu.Unlock()

	// Write data AFTER StopStreaming — old goroutines must not process it.
	// StopStreaming joined both goroutines (<-filterDone, <-tailDone) before
	// returning, so they have provably exited by now; no wait is needed before
	// asserting the negative — nothing remains that could process this line.
	f, _ := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"after stop"}}`)
	f.Close()

	got, _ := os.ReadFile(logPath)
	if strings.Contains(string(got), "after stop") {
		t.Error("stopped filter should not process data written after StopStreaming")
	}

	// Calling StopStreaming again should be safe (no-op).
	runner.StopStreaming()
}

// Proves that StopStreaming is idempotent — calling it when no goroutines
// are active does not panic or block.
func TestStopStreaming_Idempotent(t *testing.T) {
	runner := &Runner{Logger: &testLogger{}}
	done := make(chan struct{})
	go func() {
		runner.StopStreaming()
		runner.StopStreaming()
		runner.StopStreaming()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StopStreaming should not block when no goroutines are active")
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

	// Write the completion signal once the poll loop has started (raw log
	// created after clearSignals). Use write+rename for atomicity — os.WriteFile
	// truncates then writes, creating a window where the file exists but is empty.
	go func() {
		waitPollStarted(t, rawLog)
		tmp := signals.Complete + ".tmp"
		os.WriteFile(tmp, []byte("task finished"), 0o644)
		os.Rename(tmp, signals.Complete)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
	}

	// Use sleep as a long-running stand-in for claude.
	result := runWithCommand(t, &runner, cfg, "sleep", "1")

	if !result.SignalDetected {
		t.Error("expected SignalDetected to be true")
	}
	if result.Summary != "task finished" {
		t.Errorf("Summary = %q, want %q", result.Summary, "task finished")
	}
	if len(log.successes) == 0 {
		t.Error("expected Success to be called")
	}
}

// Verifies that the completion log message includes the bead ID when TaskID
// is set, so operators can identify which task completed.
func TestRun_CompletionMessageIncludesBeadID(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	go func() {
		waitPollStarted(t, rawLog)
		tmp := signals.Complete + ".tmp"
		os.WriteFile(tmp, []byte("fixed the bug"), 0o644)
		os.Rename(tmp, signals.Complete)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
		TaskID:       "ralph-xyz",
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "1")

	if !result.SignalDetected {
		t.Error("expected SignalDetected to be true")
	}
	if len(log.successes) == 0 {
		t.Fatal("expected Success to be called")
	}
	got := log.successes[0]
	if !strings.Contains(got, "ralph-xyz") {
		t.Errorf("completion message should include bead ID, got: %q", got)
	}
	if !strings.Contains(got, "fixed the bug") {
		t.Errorf("completion message should include summary, got: %q", got)
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
		waitPollStarted(t, rawLog)
		os.WriteFile(signals.AllComplete, []byte("all done"), 0o644)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "1")

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
	var detectedTask atomic.Value
	taskDetected := make(chan struct{})
	var once sync.Once
	runner := Runner{
		Logger: log,
		OnTaskDetected: func(desc string) {
			detectedTask.Store(desc)
			once.Do(func() { close(taskDetected) })
		},
	}

	// Write the current-task signal after poll starts, then write completion
	// only once OnTaskDetected has fired — this preserves the ordering the test
	// depends on (task seen before completion ends the run) without fixed sleeps.
	go func() {
		waitPollStarted(t, rawLog)
		os.WriteFile(signals.CurrentTask, []byte("implement feature X"), 0o644)
		<-taskDetected
		os.WriteFile(signals.Complete, []byte("done"), 0o644)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
	}

	runWithCommand(t, &runner, cfg, "sleep", "1")

	if got, _ := detectedTask.Load().(string); got != "implement feature X" {
		t.Errorf("OnTaskDetected got %q, want %q", got, "implement feature X")
	}
	// The poller must NOT emit a "Working on" log line — the stream
	// formatter already shows the signal in real-time.
	for _, msg := range log.logs {
		if strings.Contains(msg, "Working on") {
			t.Errorf("poller should not emit 'Working on' log, got: %s", msg)
		}
	}
}

// Verifies that the poller does not emit a duplicate "Working on" log line
// when the agent writes .signal_current_task — the stream formatter already
// shows the signal in real-time, so the poller only sets taskLogged and
// fires OnTaskDetected.
func TestPoll_NoWorkingOnLogLine(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	// OnTaskDetected is used purely as a synchronization observable: it fires
	// when the poller has processed the current-task signal, which is exactly
	// when the (unwanted) "Working on" log would be emitted. It does not affect
	// what the test asserts (only log.logs is checked).
	taskDetected := make(chan struct{})
	var once sync.Once
	runner := Runner{
		Logger:         log,
		OnTaskDetected: func(string) { once.Do(func() { close(taskDetected) }) },
	}

	// Write the current-task signal after poll starts, then write completion
	// only once the poller has processed it — guaranteeing the poller had its
	// chance to emit a "Working on" line before the run ends.
	go func() {
		waitPollStarted(t, rawLog)
		os.WriteFile(signals.CurrentTask, []byte("fix duplicate log"), 0o644)
		<-taskDetected
		os.WriteFile(signals.Complete, []byte("done"), 0o644)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		TaskID:       "ralph-ez87",
		PollInterval: 100 * time.Millisecond,
	}

	runWithCommand(t, &runner, cfg, "sleep", "1")

	for _, msg := range log.logs {
		if strings.Contains(msg, "Working on") {
			t.Errorf("poller emitted duplicate 'Working on' log: %s", msg)
		}
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
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Idle: 200 * time.Millisecond},
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "1")

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

	// Keep writing to raw log faster than the idle timeout. The 50ms cadence is
	// a genuine load-generation interval (not a sync proxy), so it is paced via
	// an elapsed-time wait rather than a bare sleep.
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
				waitElapsed(t, time.Now(), 50*time.Millisecond, "activity write cadence")
			}
		}
	}()

	// Keep the activity alive well past the 200ms idle timeout before completing,
	// proving the idle timer was reset by activity rather than firing.
	go func() {
		start := time.Now()
		waitElapsed(t, start, 400*time.Millisecond, "activity to outlast idle timeout")
		os.WriteFile(signals.Complete, []byte("done"), 0o644)
		close(stop)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Idle: 200 * time.Millisecond},
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "1")

	if result.IdleTimeout {
		t.Error("expected IdleTimeout to be false when activity resets timer")
	}
	if !result.SignalDetected {
		t.Error("expected SignalDetected to be true")
	}
}

// Regression test: an oversized raw log line (e.g. a full CSS file returned as
// a tool result) must not break the idle watchdog. Before the fix, the default
// 64KB bufio.Scanner buffer would error on such lines, causing scanNewLines to
// stop detecting activity entirely — the idle timer would fire even though the
// agent was working.
func TestPoll_OversizedRawLogLineDoesNotBreakIdleDetection(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Write a mix of normal activity, an oversized line (100KB), and more
	// activity. The idle timer should stay reset throughout.
	stop := make(chan struct{})
	go func() {
		// Normal activity line, written after poll has seeded its raw-log read
		// offset (short elapsed wait, not a bare sleep) so the line is scanned.
		waitElapsed(t, time.Now(), 30*time.Millisecond, "poll to seed raw-log offset")
		f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, `{"type":"content_block_start","content_block":{"type":"text"}}`)
		f.Close()

		waitElapsed(t, time.Now(), 50*time.Millisecond, "gap before oversized line")
		// Oversized line — simulates a large file read result (100KB).
		f, _ = os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		bigContent := strings.Repeat("x", 100*1024)
		fmt.Fprintf(f, `{"type":"user","message":{"content":"%s"}}`+"\n", bigContent)
		f.Close()

		// Continue writing normal activity after the oversized line, paced via an
		// elapsed-time wait rather than a bare sleep.
		for {
			select {
			case <-stop:
				return
			default:
				f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"working"}}`)
				f.Close()
				waitElapsed(t, time.Now(), 50*time.Millisecond, "activity write cadence")
			}
		}
	}()

	// Complete after the activity has lived past the 200ms idle timeout.
	go func() {
		waitElapsed(t, time.Now(), 500*time.Millisecond, "activity to outlast idle timeout")
		os.WriteFile(signals.Complete, []byte("done"), 0o644)
		close(stop)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Idle: 200 * time.Millisecond},
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "1")

	if result.IdleTimeout {
		t.Error("expected no idle timeout — oversized raw log line should not break activity detection")
	}
	if !result.SignalDetected {
		t.Error("expected SignalDetected to be true")
	}
}

// TestRun_LogsSystemStatusEvents verifies that when a system event with
// subtype=status appears in the raw log, poll emits a 'Claude system status'
// log entry — needed to capture SDK status signals in the loop log.
func TestRun_LogsSystemStatusEvents(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Inject the status event after poll has seeded its raw-log read offset, then
	// signal completion. poll seeds logOffset from the raw log's size at startup,
	// so a line appended before that seed would be skipped; this short elapsed
	// wait (not a bare sleep) lets the seed happen first. A single poll iteration
	// scans new lines (logging the status) before it checks the completion
	// signal, so the status is logged before the run ends — no second delay.
	go func() {
		waitElapsed(t, time.Now(), 50*time.Millisecond, "poll to seed raw-log offset")
		f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, `{"type":"system","subtype":"status","status":"connected"}`)
		f.Close()
		os.WriteFile(signals.Complete, []byte("done"), 0o644)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "1")
	if !result.SignalDetected {
		t.Error("expected SignalDetected to be true")
	}

	found := false
	for _, msg := range log.logs {
		if strings.Contains(msg, "Claude system status") && strings.Contains(msg, "connected") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'Claude system status: connected' log entry, got: %v", log.logs)
	}
}

// TestRun_CompactingEventKillsAgent verifies that when a system event with
// status=compacting appears in the raw log, the agent is killed immediately
// and Result{Compacted: true} is returned — compaction indicates a context
// leak and continuing the run would be wasteful.
func TestRun_CompactingEventKillsAgent(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Inject the compacting event after poll has seeded its raw-log read offset
	// (short elapsed wait, not a bare sleep), so the line is scanned rather than
	// skipped.
	go func() {
		waitElapsed(t, time.Now(), 50*time.Millisecond, "poll to seed raw-log offset")
		f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, `{"type":"system","subtype":"status","status":"compacting"}`)
		f.Close()
	}()

	const pollInterval = 50 * time.Millisecond
	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: pollInterval,
	}

	start := time.Now()
	result := runWithCommand(t, &runner, cfg, "sleep", "5")
	elapsed := time.Since(start)

	if !result.Compacted {
		t.Errorf("expected Compacted=true, got result=%+v", result)
	}
	if result.SignalDetected {
		t.Error("expected SignalDetected=false when killed by compaction")
	}
	if elapsed > 2*pollInterval+200*time.Millisecond {
		t.Errorf("expected kill within 2 poll intervals, took %s", elapsed)
	}
}

// Verifies that the shorter progress-aware timeout fires once the agent has
// produced content output in the raw log (text activity flips activitySeen).
func TestPoll_ProgressTimeoutShorterThanDefault(t *testing.T) {
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
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Idle: 5 * time.Second, IdleProgress: 200 * time.Millisecond},
	}

	// Write a content activity line after poll has seeded its raw-log read offset
	// (short elapsed wait, not a bare sleep), simulating the agent producing
	// output. This causes activitySeen to flip true, switching the idle timer to
	// IdleTimeoutProgress (200ms) instead of IdleTimeout (5s).
	go func() {
		waitElapsed(t, time.Now(), 60*time.Millisecond, "poll to seed raw-log offset")
		os.WriteFile(rawLog, []byte("agent output line\n"), 0o644)
	}()

	start := time.Now()
	result := runWithCommand(t, &runner, cfg, "sleep", "1")
	elapsed := time.Since(start)

	if !result.IdleTimeout {
		t.Error("expected IdleTimeout to be true after content activity seen")
	}
	if elapsed > 2*time.Second {
		t.Errorf("progress timeout should fire quickly after content, took %s", elapsed)
	}
}

// Verifies that idle timeout is disabled when IdleTimeout is zero.
func TestPoll_ZeroIdleTimeoutDisablesDetection(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Write completion once poll starts — with zero timeout, it should complete
	// normally.
	go func() {
		waitPollStarted(t, rawLog)
		os.WriteFile(signals.Complete, []byte("done"), 0o644)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Idle: 0},
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "1")

	if result.IdleTimeout {
		t.Error("expected no idle timeout when IdleTimeout is 0")
	}
	if !result.SignalDetected {
		t.Error("expected SignalDetected to be true")
	}
}

// Verifies that poll kills the session and returns WallClockTimeout=true when
// MaxRunDuration elapses, even when the raw log is continuously written to
// (so idle detection never fires).
func TestPoll_WallClockTimeoutKillsSession(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Keep writing content lines so idle detection never fires. The 30ms cadence
	// is load generation, paced via an elapsed-time wait rather than a bare sleep.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"working"}}`)
				f.Close()
				waitElapsed(t, time.Now(), 30*time.Millisecond, "activity write cadence")
			}
		}
	}()
	defer close(stop)

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Idle: 5 * time.Second, MaxRun: 200 * time.Millisecond},
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "2")

	if !result.WallClockTimeout {
		t.Error("expected WallClockTimeout to be true")
	}
	if result.IdleTimeout {
		t.Error("expected IdleTimeout to be false (activity was continuous)")
	}
	if result.SignalDetected {
		t.Error("expected SignalDetected to be false on wall-clock timeout")
	}
}

// Verifies that the wall-clock cap is disabled when MaxRunDuration is zero,
// so long-running legitimate sessions complete normally.
func TestPoll_ZeroMaxRunDurationDisables(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Write completion once poll starts — with zero wall-clock cap, it should complete.
	go func() {
		waitPollStarted(t, rawLog)
		os.WriteFile(signals.Complete, []byte("done"), 0o644)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{MaxRun: 0},
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "1")

	if result.WallClockTimeout {
		t.Error("expected no wall-clock timeout when MaxRunDuration is 0")
	}
	if !result.SignalDetected {
		t.Error("expected SignalDetected to be true")
	}
}

// --- Content activity classification tests ---

// Verifies that rate_limit_event is classified as non-content so it does not
// reset the idle watchdog. This is the root cause of the 45-minute stall.
func TestIsContentActivity_RateLimitEventIsFalse(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","rateLimitType":"seven_day"}}`
	if isContentActivity(line) {
		t.Error("rate_limit_event should not be classified as content activity")
	}
}

// Verifies that system events do not reset the idle watchdog.
func TestIsContentActivity_SystemEventIsFalse(t *testing.T) {
	if isContentActivity(`{"type":"system","subtype":"init"}`) {
		t.Error("system event should not be classified as content activity")
	}
}

// Verifies that ping/keepalive events do not reset the idle watchdog.
func TestIsContentActivity_PingEventIsFalse(t *testing.T) {
	if isContentActivity(`{"type":"ping"}`) {
		t.Error("ping event should not be classified as content activity")
	}
}

// Verifies that result events do not reset the idle watchdog.
func TestIsContentActivity_ResultEventIsFalse(t *testing.T) {
	if isContentActivity(`{"type":"result","subtype":"success"}`) {
		t.Error("result event should not be classified as content activity")
	}
}

// Verifies that error events do not reset the idle watchdog.
func TestIsContentActivity_ErrorEventIsFalse(t *testing.T) {
	if isContentActivity(`{"type":"error","error":"something went wrong"}`) {
		t.Error("error event should not be classified as content activity")
	}
}

// Verifies that user message echoes do not reset the idle watchdog.
func TestIsContentActivity_UserMessageIsFalse(t *testing.T) {
	if isContentActivity(`{"type":"user","message":{"role":"user","content":"fix the bug"}}`) {
		t.Error("user message should not be classified as content activity")
	}
}

// Verifies that plaintext/stderr lines (not JSON) conservatively reset the
// idle watchdog — better to miss an idle than kill a live session.
func TestIsContentActivity_PlaintextIsTrue(t *testing.T) {
	if !isContentActivity("some stderr plaintext line") {
		t.Error("plaintext line should be treated as content activity (conservative)")
	}
}

// Verifies that unparseable JSON conservatively resets the idle watchdog.
func TestIsContentActivity_UnparseableJSONIsTrue(t *testing.T) {
	if !isContentActivity(`{"type":"broken_json`) {
		t.Error("unparseable JSON should be treated as content activity (conservative)")
	}
}

// Verifies that content_block_delta with a text delta resets the idle watchdog.
func TestIsContentActivity_TextDeltaIsTrue(t *testing.T) {
	line := `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`
	if !isContentActivity(line) {
		t.Error("content_block_delta with text_delta should be content activity")
	}
}

// Verifies that content_block_delta with a thinking delta resets the idle watchdog.
func TestIsContentActivity_ThinkingDeltaIsTrue(t *testing.T) {
	line := `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"reasoning..."}}`
	if !isContentActivity(line) {
		t.Error("content_block_delta with thinking_delta should be content activity")
	}
}

// Verifies that content_block_delta for tool input (not text/thinking) does NOT
// reset the idle watchdog — tool input streaming is infrastructure, not content.
func TestIsContentActivity_InputJsonDeltaIsFalse(t *testing.T) {
	line := `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"key\":\"val\"}"}}`
	if isContentActivity(line) {
		t.Error("content_block_delta with input_json_delta should not be content activity")
	}
}

// Verifies that content_block_start for a text block resets the idle watchdog.
func TestIsContentActivity_TextBlockStartIsTrue(t *testing.T) {
	if !isContentActivity(`{"type":"content_block_start","content_block":{"type":"text"}}`) {
		t.Error("content_block_start for text should be content activity")
	}
}

// Verifies that content_block_start for a thinking block resets the idle watchdog.
func TestIsContentActivity_ThinkingBlockStartIsTrue(t *testing.T) {
	if !isContentActivity(`{"type":"content_block_start","content_block":{"type":"thinking"}}`) {
		t.Error("content_block_start for thinking should be content activity")
	}
}

// tool_use is infrastructure (signal writes, file reads) — not thinking work.
func TestIsContentActivity_ToolUseBlockStartIsFalse(t *testing.T) {
	if isContentActivity(`{"type":"content_block_start","content_block":{"type":"tool_use"}}`) {
		t.Error("content_block_start for tool_use should NOT be content activity")
	}
}

// Verifies that message_start with role=assistant resets the idle watchdog.
func TestIsContentActivity_MessageStartAssistantIsTrue(t *testing.T) {
	if !isContentActivity(`{"type":"message_start","message":{"role":"assistant","id":"msg_123"}}`) {
		t.Error("message_start with role=assistant should be content activity")
	}
}

// Verifies that message_start with role=user does NOT reset the idle watchdog.
func TestIsContentActivity_MessageStartUserIsFalse(t *testing.T) {
	if isContentActivity(`{"type":"message_start","message":{"role":"user"}}`) {
		t.Error("message_start with role=user should not be content activity")
	}
}

// Verifies that message_delta with a stop_reason resets the idle watchdog.
func TestIsContentActivity_MessageDeltaWithStopReasonIsTrue(t *testing.T) {
	if !isContentActivity(`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`) {
		t.Error("message_delta with stop_reason should be content activity")
	}
}

// Verifies that message_delta without stop_reason does NOT reset the idle watchdog.
func TestIsContentActivity_MessageDeltaNoStopReasonIsFalse(t *testing.T) {
	if isContentActivity(`{"type":"message_delta","delta":{"stop_reason":null}}`) {
		t.Error("message_delta without stop_reason should not be content activity")
	}
}

// Regression test: proves that rate_limit_event lines written repeatedly to the
// raw log do NOT prevent the idle timeout from firing. This was the root cause
// of the 45-minute stall — periodic non-content events reset the mtime-based
// watchdog before this fix.
func TestPoll_RateLimitEventsDoNotPreventIdleTimeout(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Write rate_limit_event lines faster than the idle timeout — with the old
	// mtime-based detection these would have prevented the timeout from firing.
	// The 50ms cadence is load generation, paced via an elapsed-time wait.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				fmt.Fprintln(f, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning"}}`)
				f.Close()
				waitElapsed(t, time.Now(), 50*time.Millisecond, "rate-limit event cadence")
			}
		}
	}()
	defer close(stop)

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Idle: 200 * time.Millisecond},
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "2")

	if !result.IdleTimeout {
		t.Error("expected IdleTimeout to fire even when rate_limit_events are written continuously")
	}
}

// Verifies that a throttled rate_limit_event in the stream causes poll to kill
// the agent and return RateLimited=true with the correct ResetAt.
func TestPoll_ThrottledRateLimitEventKillsSession(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
	}

	resetsAt := int64(1776412800)
	// Write the throttled event after poll has seeded its raw-log read offset
	// (short elapsed wait, not a bare sleep) so it is scanned before the process
	// exits.
	go func() {
		waitElapsed(t, time.Now(), 80*time.Millisecond, "poll to seed raw-log offset")
		f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, `{"type":"rate_limit_event","rate_limit_info":{"status":"throttled","resetsAt":%d,"rateLimitType":"daily","utilization":1.0}}%s`, resetsAt, "\n")
		f.Close()
	}()

	result := runWithCommand(t, &runner, cfg, "sleep", "5")

	if !result.RateLimited {
		t.Error("expected RateLimited=true for throttled rate_limit_event")
	}
	expected := time.Unix(resetsAt, 0)
	if !result.ResetAt.Equal(expected) {
		t.Errorf("expected ResetAt=%v, got %v", expected, result.ResetAt)
	}
}

// Verifies that an allowed_warning rate_limit_event emits exactly one info log
// per run and does not kill or interrupt the agent.
func TestPoll_AllowedWarningRateLimitEventLogsOnce(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Idle: 400 * time.Millisecond},
	}

	// Write three allowed_warning events — only the first should log. Each is
	// spaced by an elapsed-time wait (> the 50ms poll interval) so the events
	// land in distinct poll iterations; the first wait also lets poll seed its
	// raw-log read offset before any event is written.
	go func() {
		for i := 0; i < 3; i++ {
			waitElapsed(t, time.Now(), 60*time.Millisecond, "spacing between rate-limit events")
			f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			fmt.Fprintln(f, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1776412800,"rateLimitType":"seven_day","utilization":0.84}}`)
			f.Close()
		}
	}()

	result := runWithCommand(t, &runner, cfg, "sleep", "5")

	if result.RateLimited {
		t.Error("expected RateLimited=false for allowed_warning events")
	}
	if !result.IdleTimeout {
		t.Error("expected IdleTimeout — agent should not have been killed by warning")
	}
	warningLogs := 0
	for _, msg := range log.logs {
		if strings.Contains(msg, "rate limit warning") {
			warningLogs++
		}
	}
	if warningLogs != 1 {
		t.Errorf("expected exactly 1 rate limit warning log, got %d: %v", warningLogs, log.logs)
	}
}

// Verifies that after an allowed_warning event, a subsequent allowed event with the
// same resetsAt emits exactly one "allowed back in" log and no "consuming extended usage" log.
func TestPoll_RateLimitAllowedBackIn_LogsOnce(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Idle: 500 * time.Millisecond},
	}

	const resetsAt = int64(1776412800)
	// The first elapsed wait lets poll seed its raw-log read offset; the second
	// (> the 50ms poll interval) ensures the warning event is scanned in its own
	// poll iteration before the allowed event is written. Both replace bare
	// sleeps with elapsed-time observables.
	go func() {
		waitElapsed(t, time.Now(), 60*time.Millisecond, "poll to seed raw-log offset")
		f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":%d,"rateLimitType":"five_hour","utilization":1.0,"isUsingOverage":false}}`+"\n", resetsAt)
		f.Close()
		waitElapsed(t, time.Now(), 80*time.Millisecond, "warning event to be scanned before allowed event")
		f, _ = os.OpenFile(rawLog, os.O_APPEND|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":%d,"rateLimitType":"five_hour","utilization":0.5,"isUsingOverage":false}}`+"\n", resetsAt)
		f.Close()
	}()

	result := runWithCommand(t, &runner, cfg, "sleep", "5")

	if result.RateLimited {
		t.Error("expected RateLimited=false")
	}
	if !result.IdleTimeout {
		t.Error("expected IdleTimeout")
	}
	var allowedBackCount, extendedUsageCount int
	for _, msg := range log.logs {
		if strings.Contains(msg, "allowed back in") {
			allowedBackCount++
		}
		if strings.Contains(msg, "consuming extended usage") {
			extendedUsageCount++
		}
	}
	if allowedBackCount != 1 {
		t.Errorf("expected exactly 1 'allowed back in' log, got %d: %v", allowedBackCount, log.logs)
	}
	if extendedUsageCount != 0 {
		t.Errorf("expected 0 'consuming extended usage' logs, got %d", extendedUsageCount)
	}
}

// Verifies that after an allowed_warning event, a subsequent allowed event with an advanced
// resetsAt emits exactly one "consuming extended usage" log and no "allowed back in" log.
func TestPoll_RateLimitConsumingExtendedUsage_LogsOnce(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Idle: 500 * time.Millisecond},
	}

	const warningResetsAt = int64(1776412800)
	const advancedResetsAt = int64(1776430800) // 5 hours later
	// The first elapsed wait lets poll seed its raw-log read offset; the second
	// (> the 50ms poll interval) ensures the warning event is scanned in its own
	// poll iteration before the allowed event is written. Both replace bare
	// sleeps with elapsed-time observables.
	go func() {
		waitElapsed(t, time.Now(), 60*time.Millisecond, "poll to seed raw-log offset")
		f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":%d,"rateLimitType":"five_hour","utilization":1.0,"isUsingOverage":false}}`+"\n", warningResetsAt)
		f.Close()
		waitElapsed(t, time.Now(), 80*time.Millisecond, "warning event to be scanned before allowed event")
		f, _ = os.OpenFile(rawLog, os.O_APPEND|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":%d,"rateLimitType":"five_hour","utilization":0.2,"isUsingOverage":false}}`+"\n", advancedResetsAt)
		f.Close()
	}()

	result := runWithCommand(t, &runner, cfg, "sleep", "5")

	if result.RateLimited {
		t.Error("expected RateLimited=false")
	}
	if !result.IdleTimeout {
		t.Error("expected IdleTimeout")
	}
	var allowedBackCount, extendedUsageCount int
	for _, msg := range log.logs {
		if strings.Contains(msg, "allowed back in") {
			allowedBackCount++
		}
		if strings.Contains(msg, "consuming extended usage") {
			extendedUsageCount++
		}
	}
	if extendedUsageCount != 1 {
		t.Errorf("expected exactly 1 'consuming extended usage' log, got %d: %v", extendedUsageCount, log.logs)
	}
	if allowedBackCount != 0 {
		t.Errorf("expected 0 'allowed back in' logs, got %d", allowedBackCount)
	}
}

// Verifies that after an allowed_warning with isUsingOverage=false, a subsequent event
// with isUsingOverage=true emits exactly one "now using overage" log.
func TestPoll_RateLimitNowUsingOverage_LogsOnce(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Idle: 500 * time.Millisecond},
	}

	const resetsAt = int64(1776412800)
	// The first elapsed wait lets poll seed its raw-log read offset; the second
	// (> the 50ms poll interval) ensures the first warning event is scanned in
	// its own poll iteration before the overage event is written. Both replace
	// bare sleeps with elapsed-time observables.
	go func() {
		waitElapsed(t, time.Now(), 60*time.Millisecond, "poll to seed raw-log offset")
		f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":%d,"rateLimitType":"five_hour","utilization":1.0,"isUsingOverage":false}}`+"\n", resetsAt)
		f.Close()
		waitElapsed(t, time.Now(), 80*time.Millisecond, "first warning to be scanned before overage event")
		f, _ = os.OpenFile(rawLog, os.O_APPEND|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":%d,"rateLimitType":"five_hour","utilization":1.0,"isUsingOverage":true}}`+"\n", resetsAt)
		f.Close()
	}()

	result := runWithCommand(t, &runner, cfg, "sleep", "5")

	if result.RateLimited {
		t.Error("expected RateLimited=false")
	}
	if !result.IdleTimeout {
		t.Error("expected IdleTimeout")
	}
	var overageCount int
	for _, msg := range log.logs {
		if strings.Contains(msg, "now using overage") {
			overageCount++
		}
	}
	if overageCount != 1 {
		t.Errorf("expected exactly 1 'now using overage' log, got %d: %v", overageCount, log.logs)
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

	script := fmt.Sprintf(`sleep 1 & echo $! > %s; wait`, childPidFile)
	cmd := exec.Command("bash", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait for the child PID file to appear and be non-empty.
	childPid := testutil.WaitForFile(t, 3*time.Second, childPidFile)

	// Kill the process group.
	stopProcessGroup(cmd)

	// Verify the child process becomes dead — poll kill -0 until it fails rather
	// than sleeping a fixed window and hoping the signal has propagated.
	testutil.WaitProcessGone(t, 3*time.Second, childPid)
}

// Verifies that stopProcessGroup kills all processes in a bash pipeline,
// which is the actual pattern used by the stream filter script
// (tail -f | cat | cat). Each pipeline process must be killed, not just bash.
func TestStopProcessGroup_KillsPipelineProcesses(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")

	// Simulate the stream filter: a bash pipeline where each stage writes
	// its PID to a file, then blocks on read so it stays alive.
	script := fmt.Sprintf(`set +m
(echo $$ >> %s; sleep 1) | (echo $$ >> %s; sleep 1) | (echo $$ >> %s; sleep 1)
`, pidFile, pidFile, pidFile)

	cmd := exec.Command("bash", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Wait for all 3 pipeline PIDs to appear.
	var pids []string
	testutil.WaitFor(t, 5*time.Second, "all 3 pipeline PIDs to be written", func() bool {
		data, _ := os.ReadFile(pidFile)
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) >= 3 && lines[0] != "" {
			pids = lines[:3]
			return true
		}
		return false
	})

	stopProcessGroup(cmd)

	// Each pipeline process must become dead — poll kill -0 until it fails
	// instead of pausing a fixed window for the kernel to clean up.
	for _, pid := range pids {
		pid = strings.TrimSpace(pid)
		if pid == "" {
			continue
		}
		testutil.WaitProcessGone(t, 3*time.Second, pid)
	}
}

// Verifies that stopProcessGroup handles a nil command without panicking.
func TestStopProcessGroup_NilCmd(t *testing.T) {
	stopProcessGroup(nil)
	stopProcessGroup(&exec.Cmd{})
}

// --- Test helpers ---

// waitPollStarted blocks until runWithCommand has cleared stale signals and
// created the raw log — the observable precondition that the poll loop is about
// to begin. runWithCommand clears signals (line order matters) and only then
// creates rawLog, so the raw log's existence proves clearSignals has already
// run. A producer goroutine that writes a signal file after this point cannot be
// wiped by clearSignals, removing the need for an arbitrary startup delay.
func waitPollStarted(t *testing.T, rawLog string) {
	t.Helper()
	testutil.WaitForSignalFile(t, 2*time.Second, rawLog)
}

// waitElapsed blocks until at least d has passed since start. Use it to model a
// genuine time-based delay (e.g. "stay quiet long enough to trip the idle
// timeout") as an elapsed-time observable rather than a bare time.Sleep.
func waitElapsed(t *testing.T, start time.Time, d time.Duration, desc string) {
	t.Helper()
	testutil.WaitFor(t, d+2*time.Second, desc, func() bool {
		return time.Since(start) >= d
	})
}

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
	// Skip if feedback was detected — the agent should restart, not complete.
	if !result.SignalDetected && !result.FeedbackKill {
		if cfg.Signals.NoCodeNeeded != "" && hasSignal(cfg.Signals.NoCodeNeeded) {
			result.SignalDetected = true
			result.NoCodeNeeded = true
			result.Summary = readFirstLine(cfg.Signals.NoCodeNeeded)
			if result.Summary == "" {
				result.Summary = "confirmed no code changes needed"
			}
		} else if hasSignal(cfg.Signals.Complete) || hasSignal(cfg.Signals.AllComplete) {
			result.SignalDetected = true
			result.AllComplete = hasSignal(cfg.Signals.AllComplete)
			result.Summary = readSignalSummary(cfg.Signals)
		}
	}

	return result
}

// Verifies that IterationDisallowedTools contains bd close so the agent
// cannot close beads — the orchestrator owns that lifecycle.
func TestDisallowedTools_ContainsBdBlock(t *testing.T) {
	found := false
	for _, tool := range IterationDisallowedTools {
		if strings.Contains(tool, "bd ") {
			found = true
			break
		}
	}
	if !found {
		t.Error("IterationDisallowedTools must block bd commands — orchestrator owns all bd operations")
	}
}

// --- Run() cleanup tests ---

// Verifies that Run() kills the claude process and stops streaming goroutines
// before returning. Uses a long-running process and context cancellation to
// exercise the kill path, then checks the process is actually dead.
func TestRun_CleansUpStreamingOnReturn(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	logFile := filepath.Join(dir, "loop.log")
	signals := DefaultSignalPaths(dir)

	runner := &Runner{
		Logger: &testLogger{},
		CmdFactory: func(cfg RunConfig, raw *os.File) *exec.Cmd {
			cmd := exec.Command("sleep", "1")
			cmd.Dir = cfg.WorkDir
			cmd.Stdout = raw
			cmd.Stderr = raw
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			return cmd
		},
	}

	// Cancel once Run() is underway — its creation of the raw log is the
	// observable that the process and streaming goroutines have started, so we
	// exercise the kill/cleanup path rather than cancelling on a fixed timer.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		testutil.WaitForSignalFile(t, 2*time.Second, rawLog)
		cancel()
	}()

	_, err := runner.Run(RunConfig{
		Ctx:          ctx,
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "test",
		RawLog:       rawLog,
		LogFile:      logFile,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Since Run() returned without hanging, cmd.Wait() must have completed,
	// confirming Kill+Wait were called on the process.

	// Verify streaming goroutines were cleaned up.
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.filterStop != nil {
		t.Error("filterStop should be nil after Run() returns")
	}
	if runner.filterDone != nil {
		t.Error("filterDone should be nil after Run() returns")
	}
	if runner.tailStop != nil {
		t.Error("tailStop should be nil after Run() returns")
	}
	if runner.tailDone != nil {
		t.Error("tailDone should be nil after Run() returns")
	}
}

// Verifies that Run() kills a long-running process and the process is actually
// dead after Run() returns. Tracks the PID from CmdFactory and uses kill -0 to
// confirm the process was killed.
func TestRun_KillsProcessOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	logFile := filepath.Join(dir, "loop.log")
	signals := DefaultSignalPaths(dir)

	pidFile := filepath.Join(dir, "cmd.pid")
	runner := &Runner{
		Logger: &testLogger{},
		CmdFactory: func(cfg RunConfig, raw *os.File) *exec.Cmd {
			script := fmt.Sprintf(`echo $$ > %s; sleep 1`, pidFile)
			cmd := exec.Command("bash", "-c", script)
			cmd.Dir = cfg.WorkDir
			cmd.Stdout = raw
			cmd.Stderr = raw
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			return cmd
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel only after the spawned process has started and written its PID
	// file. Cancelling on a fixed timer races process startup: under load the
	// kill can arrive before bash runs `echo $$ > pidFile`, leaving the file
	// absent. The script's `sleep 1` keeps the process alive long enough to be
	// killed once we cancel.
	go func() {
		testutil.WaitForFile(t, 5*time.Second, pidFile)
		cancel()
	}()

	_, err := runner.Run(RunConfig{
		Ctx:          ctx,
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "test",
		RawLog:       rawLog,
		LogFile:      logFile,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Read the captured PID. By the time Run() returns the cancel goroutine
	// has confirmed the PID file is present and non-empty.
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal("PID file not written — process may not have started")
	}
	pid := strings.TrimSpace(string(pidData))

	// The process must be gone once Run() returns. Poll kill -0 until it fails
	// rather than asserting after a fixed sleep — the kill may take a moment to
	// propagate to the process group.
	testutil.WaitProcessGone(t, 2*time.Second, pid)
}

// Verifies that a second Run() call stops streaming goroutines left by the
// first Run() before starting new ones, preventing goroutine accumulation.
// Also verifies the second Run()'s process is killed on context cancellation.
func TestRun_SecondCallStopsPreviousStreaming(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	logFile := filepath.Join(dir, "loop.log")
	signals := DefaultSignalPaths(dir)

	var callCount atomic.Int32
	pidFile1 := filepath.Join(dir, "cmd1.pid")
	pidFile2 := filepath.Join(dir, "cmd2.pid")

	runner := &Runner{
		Logger: &testLogger{},
		CmdFactory: func(cfg RunConfig, raw *os.File) *exec.Cmd {
			n := callCount.Add(1)
			pidFile := pidFile1
			if n == 2 {
				pidFile = pidFile2
			}
			script := fmt.Sprintf(`echo $$ > %s; sleep 1`, pidFile)
			cmd := exec.Command("bash", "-c", script)
			cmd.Dir = cfg.WorkDir
			cmd.Stdout = raw
			cmd.Stderr = raw
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			return cmd
		},
	}

	// Simulate a previous Run() that left active streaming goroutines by
	// manually starting them and registering on the Runner.
	os.WriteFile(rawLog, nil, 0o644)
	os.WriteFile(logFile, nil, 0o644)

	prevFilterStop := make(chan struct{})
	prevFilterDone := runner.startStreamFilter(RunConfig{
		RalphDir: dir,
		RawLog:   rawLog,
		LogFile:  logFile,
	}, prevFilterStop)
	prevTailStop := make(chan struct{})
	prevTailDone := startTailGoroutine(logFile, prevTailStop)

	runner.mu.Lock()
	runner.filterStop = prevFilterStop
	runner.filterDone = prevFilterDone
	runner.tailStop = prevTailStop
	runner.tailDone = prevTailDone
	runner.mu.Unlock()

	// Run() with context cancellation so the long-running process gets killed.
	// Cancel once Run() has started its new process — Run writes .stream-start
	// right after launching it — so the cancel exercises the kill path on a live
	// process instead of racing a fixed timer.
	streamStart := filepath.Join(dir, ".stream-start")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		testutil.WaitForSignalFile(t, 2*time.Second, streamStart)
		cancel()
	}()

	_, err := runner.Run(RunConfig{
		Ctx:          ctx,
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "test",
		RawLog:       rawLog,
		LogFile:      logFile,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Previous goroutines must have been drained.
	select {
	case <-prevFilterDone:
	default:
		t.Error("previous filter goroutine should have been stopped by second Run()")
	}
	select {
	case <-prevTailDone:
	default:
		t.Error("previous tail goroutine should have been stopped by second Run()")
	}

	// Runner state should be clean after the second Run() completes.
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.filterStop != nil || runner.tailStop != nil {
		t.Error("streaming channels should be nil after Run() returns")
	}
}

// Proves that after detecting a completion signal, the orchestrator waits for
// the raw log to stop being modified before killing the agent. This prevents
// truncation of the agent's final output (e.g., completion summary).
func TestRun_WaitsForOutputAfterSignal(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Simulate: signal file appears once poll is running, then the raw log keeps
	// being written for ~500ms (5 writes paced 100ms apart). The post-signal
	// cadence is genuine load that must keep the raw log fresh, so it is paced
	// via elapsed-time waits rather than bare sleeps.
	go func() {
		waitPollStarted(t, rawLog)
		tmp := signals.Complete + ".tmp"
		os.WriteFile(tmp, []byte("task finished"), 0o644)
		os.Rename(tmp, signals.Complete)

		// Agent still writing output after signal.
		for i := 0; i < 5; i++ {
			waitElapsed(t, time.Now(), 100*time.Millisecond, "post-signal output cadence")
			f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_WRONLY, 0o644)
			fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"finishing up"}}`)
			f.Close()
		}
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
	}

	start := time.Now()
	result := runWithCommand(t, &runner, cfg, "sleep", "1")
	elapsed := time.Since(start)

	if !result.SignalDetected {
		t.Error("expected SignalDetected to be true")
	}
	// Should have waited at least 500ms after signal (5 writes * 100ms) plus
	// the settle period, not killed immediately on signal detection.
	if elapsed < 700*time.Millisecond {
		t.Errorf("expected grace period after signal, but killed in %s", elapsed)
	}
	// But shouldn't wait forever — should finish within a few seconds.
	if elapsed > 5*time.Second {
		t.Errorf("grace period took too long: %s", elapsed)
	}
}

// Verifies that all orchestrator-owned operations are disallowed:
// bd (bead lifecycle), gh (GitHub operations), and destructive git commands.
// Each must be blocked both bare and with a leading wildcard for cd-prefixed variants.
func TestDisallowedTools_BlocksOrchestratorOwnedOps(t *testing.T) {
	required := []string{
		"bd ",
		"gh ",
		"git checkout",
		"git branch",
		"git push",
	}

	joined := strings.Join(IterationDisallowedTools, "\n")

	for _, cmd := range required {
		barePattern := "Bash(" + cmd
		if !strings.Contains(joined, barePattern) {
			t.Errorf("IterationDisallowedTools must block bare %q", cmd)
		}
		wildcardPattern := "Bash(*" + cmd
		if !strings.Contains(joined, wildcardPattern) {
			t.Errorf("IterationDisallowedTools must block wildcard-prefixed %q", cmd)
		}
	}
}

// Verifies that IterationDisallowedTools blocks SQL clients (mysql, sqlite3, psql)
// that could directly access the dolt sql-server, .beads/ file reads via shell
// commands, and Read-tool path denials for .beads/. Each command pattern must
// appear both bare and with a leading wildcard for cd-prefixed command variants.
func TestDisallowedTools_BlocksSQLClientsAndBeadsReads(t *testing.T) {
	sqlClients := []string{"mysql ", "sqlite3 ", "psql "}
	beadsReaders := []string{"cat *.beads", "sed *.beads", "awk *.beads", "less *.beads"}

	joined := strings.Join(IterationDisallowedTools, "\n")

	for _, cmd := range sqlClients {
		barePattern := "Bash(" + cmd
		if !strings.Contains(joined, barePattern) {
			t.Errorf("IterationDisallowedTools must block bare SQL client %q", cmd)
		}
		wildcardPattern := "Bash(*" + cmd
		if !strings.Contains(joined, wildcardPattern) {
			t.Errorf("IterationDisallowedTools must block wildcard-prefixed SQL client %q", cmd)
		}
	}

	for _, cmd := range beadsReaders {
		barePattern := "Bash(" + cmd
		if !strings.Contains(joined, barePattern) {
			t.Errorf("IterationDisallowedTools must block bare .beads reader %q", cmd)
		}
		wildcardPattern := "Bash(*" + cmd
		if !strings.Contains(joined, wildcardPattern) {
			t.Errorf("IterationDisallowedTools must block wildcard-prefixed .beads reader %q", cmd)
		}
	}

	if !strings.Contains(joined, "Read(*.beads*") {
		t.Error("IterationDisallowedTools must include a Read deny pattern for .beads/ paths")
	}
}

// Verifies that IterationDisallowedTools blocks absolute-path cd escapes and
// orchestrator-owned ralph:* npm scripts. Agents must stay in their worktree
// CWD and cannot invoke package.json scripts reserved for the orchestrator.
func TestDisallowedTools_BlocksCdEscapeAndRalphScripts(t *testing.T) {
	joined := strings.Join(IterationDisallowedTools, "\n")

	// AC4: chained absolute-path cd (e.g. "cd /Users/... && tsx ...") is blocked
	if !strings.Contains(joined, "Bash(cd /Users/") {
		t.Error("IterationDisallowedTools must block bare 'cd /Users/*'")
	}
	if !strings.Contains(joined, "Bash(*cd /Users/") {
		t.Error("IterationDisallowedTools must block wildcard-prefixed 'cd /Users/*'")
	}

	// AC5: direct and chained npm run ralph:* invocations are blocked
	if !strings.Contains(joined, "Bash(npm run ralph:") {
		t.Error("IterationDisallowedTools must block bare 'npm run ralph:*'")
	}
	if !strings.Contains(joined, "Bash(*npm run ralph:") {
		t.Error("IterationDisallowedTools must block wildcard-prefixed 'npm run ralph:*'")
	}

	// AC4: the specific incident command must be blocked
	cmd := "cd /Users/daniel/Developer/tabi && tsx tests/foo.ts"
	if !isDisallowedBash(IterationDisallowedTools, cmd) {
		t.Errorf("IterationDisallowedTools must reject %q", cmd)
	}

	// AC5: direct ralph:verify invocation must be blocked
	ralphVerify := "npm run ralph:verify"
	if !isDisallowedBash(IterationDisallowedTools, ralphVerify) {
		t.Errorf("IterationDisallowedTools must reject %q", ralphVerify)
	}

	// AC6: relative cd within worktree must NOT be blocked
	relativeCD := "cd subdir && ls"
	if isDisallowedBash(IterationDisallowedTools, relativeCD) {
		t.Errorf("IterationDisallowedTools must NOT reject relative-path cd %q", relativeCD)
	}

	// AC7: 'npm test' must NOT be blocked
	npmTest := "npm test"
	if isDisallowedBash(IterationDisallowedTools, npmTest) {
		t.Errorf("IterationDisallowedTools must NOT reject %q", npmTest)
	}
}

// isDisallowedBash reports whether cmd matches any Bash(...) pattern in the
// disallowed list using the same glob semantics claude CLI applies.
// Pattern "Bash(cd /Users/*)" → must match cmd starting with "cd /Users/".
// Pattern "Bash(*cd /Users/*)" → must match cmd containing "cd /Users/".
func isDisallowedBash(disallowed []string, cmd string) bool {
	for _, pattern := range disallowed {
		if !strings.HasPrefix(pattern, "Bash(") || !strings.HasSuffix(pattern, ")") {
			continue
		}
		inner := pattern[len("Bash(") : len(pattern)-1]
		if matchesGlob(inner, cmd) {
			return true
		}
	}
	return false
}

// Verifies that IterationDisallowedTools blocks Skill invocations for all
// claude-mem:* skills. The Skill tool can invoke any registered slash command;
// claude-mem:* skills waste iterations on memory-retrieval loops instead of
// writing code, and their MCP tool calls would be blocked anyway.
func TestDisallowedTools_BlocksClaudeMemSkills(t *testing.T) {
	// AC1: IterationDisallowedTools must contain a Skill(claude-mem:*) pattern.
	joined := strings.Join(IterationDisallowedTools, "\n")
	if !strings.Contains(joined, "Skill(claude-mem:") {
		t.Error("IterationDisallowedTools must contain a Skill(claude-mem:*) pattern")
	}

	// AC2: specific claude-mem skills must be blocked.
	for _, skill := range []string{"claude-mem:mem-search", "claude-mem:do"} {
		if !isDisallowedSkill(IterationDisallowedTools, skill) {
			t.Errorf("IterationDisallowedTools must block Skill invocation for %q", skill)
		}
	}

	// Non-claude-mem skills must not be blocked.
	if isDisallowedSkill(IterationDisallowedTools, "simplify") {
		t.Error("IterationDisallowedTools must NOT block non-claude-mem Skill invocations")
	}
}

// isDisallowedSkill reports whether skillName matches any Skill(...) pattern in
// the disallowed list using the same glob semantics the claude CLI applies.
// Pattern "Skill(claude-mem:*)" → matches any skill name starting with "claude-mem:".
func isDisallowedSkill(disallowed []string, skillName string) bool {
	for _, pattern := range disallowed {
		if !strings.HasPrefix(pattern, "Skill(") || !strings.HasSuffix(pattern, ")") {
			continue
		}
		inner := pattern[len("Skill(") : len(pattern)-1]
		if matchesGlob(inner, skillName) {
			return true
		}
	}
	return false
}

// matchesGlob performs simple prefix/suffix glob matching for patterns
// that may start with '*' (match anywhere) or not (match from start).
// A trailing '*' matches any suffix; no trailing '*' requires exact suffix match.
func matchesGlob(pattern, s string) bool {
	leadWild := strings.HasPrefix(pattern, "*")
	trailWild := strings.HasSuffix(pattern, "*")

	core := pattern
	if leadWild {
		core = core[1:]
	}
	if trailWild {
		core = core[:len(core)-1]
	}

	if leadWild && trailWild {
		return strings.Contains(s, core)
	}
	if leadWild {
		return strings.HasSuffix(s, core)
	}
	if trailWild {
		return strings.HasPrefix(s, core)
	}
	return s == core
}

// Verifies that every Emit call with Domain: logging.LLM includes the Model
// from RunConfig. Uses CmdFactory to run a real process so the full Run()
// path fires, including the "Claude started (PID: ...)" log line.
func TestRun_LLMEmitsIncludeModel(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)
	const model = "claude-sonnet-4-5-20241022"

	log := &testLogger{}
	runner := &Runner{
		Logger: log,
		CmdFactory: func(cfg RunConfig, raw *os.File) *exec.Cmd {
			cmd := exec.Command("true")
			cmd.Dir = cfg.WorkDir
			cmd.Stdout = raw
			cmd.Stderr = raw
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			return cmd
		},
	}

	runner.Run(RunConfig{
		Ctx:          context.Background(),
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
		Model:        model,
	})

	var hasLLMLine bool
	for _, o := range log.opts {
		if o.Domain == logging.LLM {
			hasLLMLine = true
			if o.Model != model {
				t.Errorf("LLM-domain log line emitted with Model=%q, want %q", o.Model, model)
			}
		}
	}
	if !hasLLMLine {
		t.Error("expected at least one LLM-domain log line to be emitted")
	}
}

// Verifies that writing .signal_no_code_needed causes the poll loop to return
// Result{SignalDetected: true, NoCodeNeeded: true} without calling OnSignal.
// This is the "already fixed / not a bug" close path — no commit check.
func TestRun_DetectsNoCodeNeededSignal(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	onSignalCalled := false
	go func() {
		waitPollStarted(t, rawLog)
		tmp := signals.NoCodeNeeded + ".tmp"
		os.WriteFile(tmp, []byte("bug already fixed in main"), 0o644)
		os.Rename(tmp, signals.NoCodeNeeded)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 100 * time.Millisecond,
		OnSignal: func(summary string) bool {
			onSignalCalled = true
			return true
		},
	}

	result := runWithCommand(t, &runner, cfg, "sleep", "1")

	if !result.SignalDetected {
		t.Error("expected SignalDetected to be true")
	}
	if !result.NoCodeNeeded {
		t.Error("expected NoCodeNeeded to be true")
	}
	if result.Summary != "bug already fixed in main" {
		t.Errorf("Summary = %q, want %q", result.Summary, "bug already fixed in main")
	}
	if result.OnSignalUsed {
		t.Error("expected OnSignalUsed to be false — no-code-needed bypasses OnSignal")
	}
	if onSignalCalled {
		t.Error("OnSignal should not be called when NoCodeNeeded signal is detected")
	}
}

// Verifies that .signal_no_code_needed is detected after process exit (final
// signal check in runWithCommand), matching how .signal_complete is handled.
// The process writes the signal file itself before exiting so it's present
// when the final check runs.
func TestRun_DetectsNoCodeNeededSignalAfterExit(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	cfg := RunConfig{
		WorkDir:  dir,
		RalphDir: dir,
		Prompt:   "echo test",
		RawLog:   rawLog,
		Signals:  signals,
		// Long poll interval so the signal is not caught mid-run.
		PollInterval: 2 * time.Second,
	}

	// The process writes the signal file and exits immediately. The poll loop
	// won't fire before exit (PollInterval > process lifetime), so detection
	// must happen in the final signal check.
	result := runWithCommand(t, &runner, cfg, "bash", "-c",
		"echo 'confirmed not a bug' > "+signals.NoCodeNeeded)

	if !result.SignalDetected {
		t.Error("expected SignalDetected to be true after process exit")
	}
	if !result.NoCodeNeeded {
		t.Error("expected NoCodeNeeded to be true after process exit")
	}
	if result.Summary != "confirmed not a bug" {
		t.Errorf("Summary = %q, want %q", result.Summary, "confirmed not a bug")
	}
}

// Verifies that the agent subprocess environment includes the project venv bin on
// PATH when .venv/bin exists in the workdir, enabling bare `python`, `pytest`,
// `ruff` commands to resolve to the project venv without explicit activation.
func TestBuildAgentEnv_PrependsvenvBin(t *testing.T) {
	dir := t.TempDir()
	venvBin := filepath.Join(dir, ".venv", "bin")
	if err := os.MkdirAll(venvBin, 0o755); err != nil {
		t.Fatal(err)
	}

	env := buildAgentEnv(dir)
	if env == nil {
		t.Fatal("expected non-nil env when .venv/bin exists")
	}

	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			pathVal := strings.TrimPrefix(entry, "PATH=")
			parts := filepath.SplitList(pathVal)
			if len(parts) == 0 || parts[0] != venvBin {
				t.Errorf("first PATH entry = %q, want %q", parts[0], venvBin)
			}
			return
		}
	}
	t.Error("no PATH entry found in returned env")
}

// Verifies that buildAgentEnv always disables Claude auto-memory (hermetic
// context) even when no .venv/bin exists, and does not prepend a venv PATH.
func TestBuildAgentEnv_NoVenv(t *testing.T) {
	dir := t.TempDir()
	env := buildAgentEnv(dir)
	if env == nil {
		t.Fatal("expected non-nil env (auto-memory must be disabled) even without .venv/bin")
	}
	venvBin := filepath.Join(dir, ".venv", "bin")
	found := false
	for _, entry := range env {
		if entry == "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1" {
			found = true
		}
		if strings.HasPrefix(entry, "PATH=") && strings.Contains(entry, venvBin) {
			t.Errorf("no .venv/bin exists but PATH references it: %q", entry)
		}
	}
	if !found {
		t.Error("expected CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 in env")
	}
}

// Verifies that when an agent produces no visible output for longer than
// AgentHeartbeatInterval, the poll loop emits an agent-liveness heartbeat
// line that includes the last activity text seen from the agent.
func TestPoll_HeartbeatEmittedDuringQuietRun(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Write an agent text line after poll has seeded its raw-log read offset
	// (short elapsed wait, not a bare sleep), then stay quiet for longer than the
	// heartbeat interval — the quiet window during which a heartbeat must fire is
	// itself an elapsed-time observable — before signaling completion.
	go func() {
		waitElapsed(t, time.Now(), 30*time.Millisecond, "poll to seed raw-log offset")
		f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"Investigating the bug"}}`)
		f.Close()
		// Quiet for 300ms (> 100ms heartbeat interval) before signaling completion.
		waitElapsed(t, time.Now(), 300*time.Millisecond, "quiet window for heartbeat to fire")
		os.WriteFile(signals.Complete, []byte("done"), 0o644)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Heartbeat: 100 * time.Millisecond},
	}

	runWithCommand(t, &runner, cfg, "sleep", "1")

	var found bool
	for _, msg := range log.logs {
		if strings.Contains(msg, "Agent alive") && strings.Contains(msg, "Investigating the bug") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected heartbeat log with last text, logs were: %v", log.logs)
	}
}

// Verifies that the heartbeat does not fire while the agent is actively
// producing output — the timer resets on each activity scan.
func TestPoll_NoHeartbeatDuringActiveOutput(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// Write activity every 40ms (faster than the 120ms heartbeat interval), then
	// complete. The heartbeat should never fire. The 40ms cadence is load
	// generation, paced via an elapsed-time wait rather than a bare sleep.
	go func() {
		for i := 0; i < 8; i++ {
			waitElapsed(t, time.Now(), 40*time.Millisecond, "active output cadence")
			f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"working"}}`)
			f.Close()
		}
		os.WriteFile(signals.Complete, []byte("done"), 0o644)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Heartbeat: 120 * time.Millisecond},
	}

	runWithCommand(t, &runner, cfg, "sleep", "1")

	for _, msg := range log.logs {
		if strings.Contains(msg, "Agent alive") {
			t.Errorf("unexpected heartbeat during active output: %s", msg)
		}
	}
}

// Verifies that when an idle timeout is configured, the heartbeat reports the
// idle-kill countdown — so the line states how long the agent has been quiet
// AND when it will be killed, rather than vaguely asserting it is "working".
func TestPoll_HeartbeatReportsIdleKillCountdown(t *testing.T) {
	dir := t.TempDir()
	rawLog := filepath.Join(dir, "raw.log")
	signals := DefaultSignalPaths(dir)

	log := &testLogger{}
	runner := Runner{Logger: log}

	// One activity line (written after poll seeds its raw-log read offset), then
	// quiet long enough for a heartbeat (but well under the 5s idle timeout, so
	// the run is not killed) before completing. Both the seed wait and the quiet
	// window are elapsed-time observables rather than bare sleeps.
	go func() {
		waitElapsed(t, time.Now(), 30*time.Millisecond, "poll to seed raw-log offset")
		f, _ := os.OpenFile(rawLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"thinking"}}`)
		f.Close()
		waitElapsed(t, time.Now(), 300*time.Millisecond, "quiet window for heartbeat to fire")
		os.WriteFile(signals.Complete, []byte("done"), 0o644)
	}()

	cfg := RunConfig{
		WorkDir:      dir,
		RalphDir:     dir,
		Prompt:       "echo test",
		RawLog:       rawLog,
		Signals:      signals,
		PollInterval: 50 * time.Millisecond,
		Timeouts:     Timeouts{Idle: 5 * time.Second, Heartbeat: 100 * time.Millisecond},
	}

	runWithCommand(t, &runner, cfg, "sleep", "1")

	var found bool
	for _, msg := range log.logs {
		if strings.Contains(msg, "Agent alive") && strings.Contains(msg, "idle-kill in") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected heartbeat reporting idle-kill countdown, logs were: %v", log.logs)
	}
}
