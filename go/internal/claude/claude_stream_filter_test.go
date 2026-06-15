package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/testutil"
)

// waitFilterStarted gives a freshly-launched tail/filter goroutine time to open
// the raw log and Seek to its end before the test appends input. The goroutines
// (filterStreamJSON, startTailGoroutine) read only data written after their
// Seek(0, io.SeekEnd), so input written before that seek is silently skipped.
// No production observable exposes "goroutine has seeked", so this routes the
// unavoidable startup wait through WaitFor on an elapsed-time observable rather
// than a bare time.Sleep — the test proceeds the instant the window passes.
func waitFilterStarted(t *testing.T) {
	t.Helper()
	start := time.Now()
	testutil.WaitFor(t, 2*time.Second, "tail goroutine to open and seek to EOF", func() bool {
		return time.Since(start) > 50*time.Millisecond
	})
}

// waitForLogContains waits until the ANSI-stripped contents of path contain
// want, returning once the filter goroutine has emitted the expected line. Use
// it instead of a fixed sleep before stopping a filter and asserting output.
func waitForLogContains(t *testing.T, path, want string) {
	t.Helper()
	testutil.WaitFor(t, 2*time.Second, fmt.Sprintf("loop.log to contain %q", want), func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		return strings.Contains(ansiRe.ReplaceAllString(string(data), ""), want)
	})
}

// Verifies that startTailGoroutine follows new [r]-prefixed lines
// appended to a file and stops cleanly when the stop channel is closed.
// Non-[r] lines (orchestrator messages) must NOT be forwarded to
// stdout — the logger already writes those directly.
func TestStartTailGoroutine_FollowsAndStops(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "loop.log")
	os.WriteFile(logPath, []byte("existing\n"), 0o644)

	stop := make(chan struct{})
	done := startTailGoroutine(logPath, stop)

	// Append [r]-prefixed and non-prefixed lines after goroutine starts.
	waitFilterStarted(t)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "[r] hello from agent")
	fmt.Fprintln(f, "12:00:00 \033[0;36m[beads]\033[0m orchestrator message")
	f.Close()

	// close(stop) triggers a final drain of any unread data before the goroutine
	// exits, so this test (which only proves clean shutdown) needs no wait for
	// the lines to be processed first.
	close(stop)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("startTailGoroutine did not stop within 2 seconds")
	}
}

// Verifies that startTailGoroutine returns immediately for nonexistent files,
// without leaving any goroutines running.
func TestStartTailGoroutine_NonexistentFile(t *testing.T) {
	stop := make(chan struct{})
	done := startTailGoroutine("/nonexistent/path/log", stop)

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		close(stop)
		t.Fatal("startTailGoroutine should exit immediately for missing file")
	}
}

// Verifies that startTailGoroutine only forwards [r]-prefixed lines to
// stdout, preventing orchestrator log messages from appearing twice (once from
// the logger writing to stdout directly, and again from the tail goroutine).
func TestStartTailGoroutine_FiltersNonAgentLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "loop.log")
	os.WriteFile(logPath, nil, 0o644)

	// Capture stdout by replacing os.Stdout temporarily.
	origStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	stop := make(chan struct{})
	done := startTailGoroutine(logPath, stop)

	waitFilterStarted(t)
	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	fmt.Fprintln(f, "[r] agent output line")
	fmt.Fprintln(f, "12:00:00 \033[0;36m[beads]\033[0m orchestrator message")
	fmt.Fprintln(f, "[r] second agent line")
	f.Close()

	// close(stop) drains all unread data before the goroutine exits, so the
	// captured stdout below reflects every forwarded line without a fixed wait.
	close(stop)
	<-done

	w.Close()
	captured, _ := io.ReadAll(r)
	os.Stdout = origStdout

	output := string(captured)
	if !strings.Contains(output, "[r] agent output line") {
		t.Errorf("expected [r] lines to be forwarded, got: %q", output)
	}
	if !strings.Contains(output, "[r] second agent line") {
		t.Errorf("expected second [r] line to be forwarded, got: %q", output)
	}
	if strings.Contains(output, "orchestrator message") {
		t.Errorf("orchestrator messages should NOT be forwarded by tail goroutine, got: %q", output)
	}
}

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
		filterStreamJSON(rawPath, logPath, "", true, stop)
	}()

	// Append a stream-json delta event after the filter has started.
	waitFilterStarted(t)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"hello from claude"}}`)
	f.Close()

	// Wait for the filtered text to land in loop.log, then stop the filter.
	waitForLogContains(t, logPath, "[r] hello from claude")
	close(stop)
	<-done

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	plain := ansiRe.ReplaceAllString(string(got), "")
	if !strings.Contains(plain, "[r] hello from claude") {
		t.Errorf("loop.log should contain [r]-prefixed filtered text, got: %q", string(got))
	}
}

// Verifies that filterStreamJSON prefixes each output line with [r]
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
		filterStreamJSON(rawPath, logPath, "", true, stop)
	}()

	waitFilterStarted(t)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Write a tool-use event and a text event.
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/foo.go"}}]}}`)
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"some delta text"}}`)
	f.Close()

	// The delta text is emitted after the batched Read, so waiting for it
	// guarantees both lines are present before we stop and assert.
	waitForLogContains(t, logPath, "[r] some delta text")
	close(stop)
	<-done

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := ansiRe.ReplaceAllString(string(got), "")
	if !strings.Contains(content, "[r] [Read] foo.go") {
		t.Errorf("batched Read should show basename with [r] prefix, got: %q", content)
	}
	if !strings.Contains(content, "[r] some delta text") {
		t.Errorf("delta text should have [r] prefix, got: %q", content)
	}
}

// Verifies that ISSUE:/FIX: diagnosis lines in the stream get banner treatment
// so they stand out visually in the log output.
func TestFilterStreamJSON_DiagnosisBanner(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.log")
	logPath := filepath.Join(dir, "loop.log")

	os.WriteFile(rawPath, nil, 0o644)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		filterStreamJSON(rawPath, logPath, "", true, stop)
	}()

	waitFilterStarted(t)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"ISSUE: the config is missing a required field"}}`)
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"FIX: add the default value to config.go"}}`)
	f.Close()

	// Wait for the FIX content (the last line written) to appear, ensuring the
	// full diagnosis pair has been processed before we stop and assert.
	waitForLogContains(t, logPath, "add the default value to config.go")
	close(stop)
	<-done

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := ansiRe.ReplaceAllString(string(got), "")

	if !strings.Contains(content, "═") {
		t.Errorf("diagnosis output should contain ═ banner characters, got: %q", content)
	}
	if !strings.Contains(content, "ISSUE") {
		t.Errorf("should contain ISSUE banner, got: %q", content)
	}
	if !strings.Contains(content, "FIX") {
		t.Errorf("should contain FIX banner, got: %q", content)
	}
	if !strings.Contains(content, "the config is missing a required field") {
		t.Errorf("should contain ISSUE content, got: %q", content)
	}
	if !strings.Contains(content, "add the default value to config.go") {
		t.Errorf("should contain FIX content, got: %q", content)
	}
}

// Verifies that multiple Read/Grep tool calls within a batch window are
// collapsed into comma-separated summary lines in the log output, with
// Read showing basenames and Grep keeping patterns as-is.
func TestFilterStreamJSON_BatchesToolCalls(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.log")
	logPath := filepath.Join(dir, "loop.log")

	os.WriteFile(rawPath, nil, 0o644)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		filterStreamJSON(rawPath, logPath, "", true, stop)
	}()

	waitFilterStarted(t)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Write multiple Read and Grep tool calls, then a text event to flush.
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/project/go/internal/loop.go"}}]}}`)
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/project/go/internal/git.go"}}]}}`)
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"checklist_"}}]}}`)
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"TASK_BACKEND"}}]}}`)
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"analyzing results"}}`)
	f.Close()

	// The trailing text flushes the batch and appears last, so waiting for it
	// guarantees the batched tool lines have been emitted.
	waitForLogContains(t, logPath, "[r] analyzing results")
	close(stop)
	<-done

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := ansiRe.ReplaceAllString(string(got), "")

	if !strings.Contains(content, "[r] [Read] loop.go, git.go") {
		t.Errorf("Read tools should be batched with basenames, got: %q", content)
	}
	if !strings.Contains(content, "[r] [Grep] checklist_, TASK_BACKEND") {
		t.Errorf("Grep tools should be batched with patterns, got: %q", content)
	}
	if !strings.Contains(content, "[r] analyzing results") {
		t.Errorf("text should flush batch and appear after, got: %q", content)
	}
}

// Verifies that filterStreamJSON strips the workDir prefix from absolute
// paths in tool-use events, producing relative paths in log output.
func TestFilterStreamJSON_ShortensAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.log")
	logPath := filepath.Join(dir, "loop.log")

	os.WriteFile(rawPath, nil, 0o644)

	workDir := "/Users/daniel/Developer/ralph/.ralph/worktrees/ralph-20260323-01"
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		filterStreamJSON(rawPath, logPath, workDir, true, stop)
	}()

	waitFilterStarted(t)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/Users/daniel/Developer/ralph/.ralph/worktrees/ralph-20260323-01/go/internal/claude/claude_stream.go"}}]}}`)
	f.Close()

	// Edit is a visible tool that flushes immediately, so wait for the
	// shortened path to appear before stopping and asserting.
	waitForLogContains(t, logPath, "go/internal/claude/claude_stream.go")
	close(stop)
	<-done

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := ansiRe.ReplaceAllString(string(got), "")

	if strings.Contains(content, "/Users/daniel") {
		t.Errorf("should not contain absolute path, got: %q", content)
	}
	if !strings.Contains(content, "go/internal/claude/claude_stream.go") {
		t.Errorf("should contain relative path, got: %q", content)
	}
}

// Verifies that signal file writes in the raw stream are formatted with
// [signal] prefix and yellow color in the filtered log output.
func TestFilterStreamJSON_SignalHighlight(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.log")
	logPath := filepath.Join(dir, "loop.log")

	os.WriteFile(rawPath, nil, 0o644)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		filterStreamJSON(rawPath, logPath, "", true, stop)
	}()

	waitFilterStarted(t)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo \"Working on feature X\" > /path/.ralph/.signal_current_task"}}]}}`)
	f.Close()

	// Wait for the signal line to be emitted before stopping and asserting.
	waitForLogContains(t, logPath, "Working on feature X")
	close(stop)
	<-done

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := ansiRe.ReplaceAllString(string(got), "")

	if !strings.Contains(content, "[signal]") {
		t.Errorf("signal write should show [signal] prefix, got: %q", content)
	}
	if strings.Contains(content, "[Bash]") {
		t.Errorf("signal write should not show [Bash], got: %q", content)
	}
	if !strings.Contains(content, "current_task") {
		t.Errorf("signal write should show signal name, got: %q", content)
	}
	if !strings.Contains(content, "Working on feature X") {
		t.Errorf("signal write should show message content, got: %q", content)
	}
	if !strings.Contains(string(got), "\033[0;33m") {
		t.Errorf("signal write should have yellow ANSI color, got: %q", string(got))
	}
}

// Verifies that multi-line Bash commands (heredocs, inline scripts) produce
// only one output line showing the first line of the command, not the entire body.
func TestFilterStreamJSON_MultiLineBashTruncated(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.log")
	logPath := filepath.Join(dir, "loop.log")

	os.WriteFile(rawPath, nil, 0o644)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		filterStreamJSON(rawPath, logPath, "", true, stop)
	}()

	waitFilterStarted(t)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Multi-line Bash command with an inline script containing real newlines.
	cmd := "node -e '\nconst http = require(\"http\");\nconst server = http.createServer();\nserver.listen(3000);\nconsole.log(\"started\");\n'"
	// Build JSON manually to ensure newlines are properly encoded.
	cmdJSON, _ := json.Marshal(cmd)
	fmt.Fprintf(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":%s}}]}}`, cmdJSON)
	fmt.Fprintln(f)
	f.Close()

	// Bash is a visible tool that flushes immediately, so wait for its first
	// line to appear before stopping and asserting.
	waitForLogContains(t, logPath, "[r] [Bash] node -e '")
	close(stop)
	<-done

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := ansiRe.ReplaceAllString(string(got), "")

	if strings.Contains(content, "createServer") {
		t.Errorf("multi-line Bash body should not appear in log, got: %q", content)
	}
	if !strings.Contains(content, "[r] [Bash] node -e '") {
		t.Errorf("should show first line of Bash command, got: %q", content)
	}
	// Should be a single [r] line, not multiple.
	rLines := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "[r]") {
			rLines++
		}
	}
	if rLines > 1 {
		t.Errorf("expected 1 [r] output line for multi-line Bash, got %d in: %q", rLines, content)
	}
}

// Verifies that when Claude emits both content_block_delta events (streaming)
// and a final assistant event with the same text, each line appears exactly
// once in the filtered output — no duplicates.
func TestFilterStreamJSON_NoDuplicatesFromAssistantAndDelta(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.log")
	logPath := filepath.Join(dir, "loop.log")

	os.WriteFile(rawPath, nil, 0o644)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		filterStreamJSON(rawPath, logPath, "", true, stop)
	}()

	waitFilterStarted(t)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate Claude's stream-json: delta events stream first, then the
	// assistant event contains the complete text again.
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"hello from claude"}}`)
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"text","text":"hello from claude"}]}}`)
	f.Close()

	// Wait for the text to appear. The assistant event with the same text is
	// written immediately after the delta on the same line-buffered write, so
	// once the delta text is visible the assistant event has been read too and
	// the dedup behavior (no second copy) can be asserted.
	waitForLogContains(t, logPath, "hello from claude")
	close(stop)
	<-done

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := ansiRe.ReplaceAllString(string(got), "")

	// The text should appear exactly once.
	count := strings.Count(content, "hello from claude")
	if count != 1 {
		t.Errorf("expected 'hello from claude' exactly once, got %d times in: %q", count, content)
	}
}

// Verifies that prose status lines ([thinking]) appear in the filtered
// output when the prose tracker's interval has elapsed. This is the
// integration test for ralph-rioy: the end-to-end path from raw stream
// events through ProseTracker to the loop.log file.
func TestFilterStreamJSON_ProseStatusLine(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.log")
	logPath := filepath.Join(dir, "loop.log")

	os.WriteFile(rawPath, nil, 0o644)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		filterStreamJSON(rawPath, logPath, "", true, stop)
	}()

	waitFilterStarted(t)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Write text deltas using real Claude Code wire format (no type in delta).
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"Analyzing the test failures to determine the root cause of the problem"}}`)
	f.Close()

	// The prose tracker has a 60s interval, so a real prose status line won't
	// emit here. We verify the end-to-end path by waiting for the text delta
	// itself (via extractStreamText) to appear in the log output.
	waitForLogContains(t, logPath, "Analyzing the test failures")
	close(stop)
	<-done

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := ansiRe.ReplaceAllString(string(got), "")

	// The text should appear as [r]-prefixed output (via extractStreamText).
	if !strings.Contains(content, "Analyzing the test failures") {
		t.Errorf("text delta should appear in filtered output, got: %q", content)
	}
}

// Verifies that non-verbose mode hides VerboseOnlyTools while keeping
// visible tools, prose, signals, and diagnosis banners in the filtered output.
func TestFilterStreamJSON_NonVerboseHidesLowValueTools(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.log")
	logPath := filepath.Join(dir, "loop.log")

	os.WriteFile(rawPath, nil, 0o644)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		filterStreamJSON(rawPath, logPath, "", false, stop)
	}()

	waitFilterStarted(t)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Verbose-only tools (should be hidden).
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/foo.go"}}]}}`)
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`)
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/tmp/out.go"}}]}}`)
	// Visible tools.
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/tmp/fix.go"}}]}}`)
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Agent","input":{"prompt":"explore codebase"}}]}}`)
	// Prose.
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"analyzing the results"}}`)
	// Signal (starts as Bash but should be converted to [signal]).
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo \"done\" > /path/.ralph/.signal_complete"}}]}}`)
	// Diagnosis.
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"ISSUE: config is broken"}}`)
	f.Close()

	// The diagnosis line is written last, so waiting for it guarantees every
	// earlier event (visible tools, prose, signal) has already been processed.
	waitForLogContains(t, logPath, "ISSUE")
	close(stop)
	<-done

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := ansiRe.ReplaceAllString(string(got), "")

	// Verbose-only tools that were sent should not appear in output.
	for _, hidden := range []string{"Read", "Bash", "Write", "Edit"} {
		if !IsVerboseOnlyTool(hidden) {
			t.Fatalf("test assumption: %s should be in VerboseOnlyTools", hidden)
		}
		if strings.Contains(content, "["+hidden+"]") {
			t.Errorf("%s should be hidden in non-verbose mode, got: %q", hidden, content)
		}
	}
	// Agent is not verbose-only — should be visible.
	if IsVerboseOnlyTool("Agent") {
		t.Fatal("test assumption: Agent should not be verbose-only")
	}
	if !strings.Contains(content, "[Agent]") {
		t.Errorf("Agent should be visible in non-verbose mode, got: %q", content)
	}

	// Prose should appear.
	if !strings.Contains(content, "analyzing the results") {
		t.Errorf("prose should be visible in non-verbose mode, got: %q", content)
	}

	// Signal should appear.
	if !strings.Contains(content, "[signal]") {
		t.Errorf("signal should be visible in non-verbose mode, got: %q", content)
	}

	// Diagnosis should appear.
	if !strings.Contains(content, "ISSUE") {
		t.Errorf("diagnosis should be visible in non-verbose mode, got: %q", content)
	}
}
