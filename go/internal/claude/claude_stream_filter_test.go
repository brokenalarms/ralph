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
)

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
	time.Sleep(200 * time.Millisecond)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "[r] hello from agent")
	fmt.Fprintln(f, "12:00:00 \033[0;36m[beads]\033[0m orchestrator message")
	f.Close()

	time.Sleep(200 * time.Millisecond)
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

	time.Sleep(200 * time.Millisecond)
	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	fmt.Fprintln(f, "[r] agent output line")
	fmt.Fprintln(f, "12:00:00 \033[0;36m[beads]\033[0m orchestrator message")
	fmt.Fprintln(f, "[r] second agent line")
	f.Close()

	time.Sleep(300 * time.Millisecond)
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
	time.Sleep(200 * time.Millisecond)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"hello from claude"}}`)
	f.Close()

	// Give the filter time to process, then stop it.
	time.Sleep(300 * time.Millisecond)
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

	time.Sleep(200 * time.Millisecond)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"ISSUE: the config is missing a required field"}}`)
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"FIX: add the default value to config.go"}}`)
	f.Close()

	time.Sleep(300 * time.Millisecond)
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

	time.Sleep(200 * time.Millisecond)
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

	time.Sleep(300 * time.Millisecond)
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

	time.Sleep(200 * time.Millisecond)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/Users/daniel/Developer/ralph/.ralph/worktrees/ralph-20260323-01/go/internal/claude/claude_stream.go"}}]}}`)
	f.Close()

	time.Sleep(300 * time.Millisecond)
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

	time.Sleep(200 * time.Millisecond)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo \"Working on feature X\" > /path/.ralph/.signal_current_task"}}]}}`)
	f.Close()

	time.Sleep(300 * time.Millisecond)
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

	time.Sleep(200 * time.Millisecond)
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

	time.Sleep(300 * time.Millisecond)
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

	time.Sleep(200 * time.Millisecond)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate Claude's stream-json: delta events stream first, then the
	// assistant event contains the complete text again.
	fmt.Fprintln(f, `{"type":"content_block_delta","delta":{"text":"hello from claude"}}`)
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"text","text":"hello from claude"}]}}`)
	f.Close()

	time.Sleep(300 * time.Millisecond)
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

// Verifies that non-verbose mode hides Read/Bash/Write but keeps Edit, Agent,
// prose, signals, and diagnosis banners visible in the filtered output.
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

	time.Sleep(200 * time.Millisecond)
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

	time.Sleep(300 * time.Millisecond)
	close(stop)
	<-done

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := ansiRe.ReplaceAllString(string(got), "")

	// Hidden tools should NOT appear.
	if strings.Contains(content, "[Read]") {
		t.Errorf("Read should be hidden in non-verbose mode, got: %q", content)
	}
	if strings.Contains(content, "[Bash] go test") {
		t.Errorf("Bash should be hidden in non-verbose mode, got: %q", content)
	}
	if strings.Contains(content, "[Write]") {
		t.Errorf("Write should be hidden in non-verbose mode, got: %q", content)
	}

	// Visible tools should appear.
	if !strings.Contains(content, "[Edit]") {
		t.Errorf("Edit should be visible in non-verbose mode, got: %q", content)
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
