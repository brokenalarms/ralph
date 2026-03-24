package claude

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func fixedClock(ts string) func() time.Time {
	parsed, _ := time.Parse("15:04:05", ts)
	return func() time.Time { return parsed }
}

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

// Verifies that result events produce [done] output so users can see
// when Claude finishes processing a request.
func TestExtractStreamText_ResultDone(t *testing.T) {
	line := `{"type":"result","subtype":"success"}`
	got := extractStreamText(line)
	if got != "[done]" {
		t.Errorf("extractStreamText = %q, want %q", got, "[done]")
	}
}

// Verifies that Write to a reflections/ path shows a summary of the
// reflection content instead of the raw file path.
func TestFormatToolUse_ReflectionSummary(t *testing.T) {
	content := "# Fixed is-ancestor arg order\n\n## What was discovered\n- Was comparing HEAD to main instead of main to HEAD"
	c := streamContent{
		Type: "tool_use",
		Name: "Write",
		Input: map[string]interface{}{
			"file_path": "/Users/daniel/.ralph/reflections/ralph-259.md",
			"content":   content,
		},
	}
	got := formatToolUse(c)
	want := "[Write] Reflection: Fixed is-ancestor arg order"
	if got != want {
		t.Errorf("formatToolUse reflection = %q, want %q", got, want)
	}
}

// Verifies that reflection summary works with content that has no heading.
func TestFormatToolUse_ReflectionNoHeading(t *testing.T) {
	content := "Some reflection content without a heading\nSecond line"
	c := streamContent{
		Type: "tool_use",
		Name: "Write",
		Input: map[string]interface{}{
			"file_path": "/Users/daniel/.ralph/reflections/ralph-abc.md",
			"content":   content,
		},
	}
	got := formatToolUse(c)
	want := "[Write] Reflection: Some reflection content without a heading"
	if got != want {
		t.Errorf("formatToolUse reflection = %q, want %q", got, want)
	}
}

// Verifies that reflection summary truncates long first lines.
func TestFormatToolUse_ReflectionTruncatesLong(t *testing.T) {
	content := "# " + strings.Repeat("A very long reflection title ", 5) + "\n\n## Details"
	c := streamContent{
		Type: "tool_use",
		Name: "Write",
		Input: map[string]interface{}{
			"file_path": "/tmp/.ralph/reflections/task.md",
			"content":   content,
		},
	}
	got := formatToolUse(c)
	if !strings.HasPrefix(got, "[Write] Reflection: ") {
		t.Errorf("expected reflection prefix, got %q", got)
	}
	// [Write] prefix (8) + "Reflection: " (12) + 80 max summary = 100 max
	if len([]rune(got)) > 102 {
		t.Errorf("expected truncated output, got %d chars: %q", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation ellipsis, got %q", got)
	}
}

// Verifies that non-reflection Write calls still show the file path.
func TestFormatToolUse_NonReflectionWrite(t *testing.T) {
	c := streamContent{
		Type: "tool_use",
		Name: "Write",
		Input: map[string]interface{}{
			"file_path": "/tmp/some/other/file.go",
			"content":   "package main",
		},
	}
	got := formatToolUse(c)
	want := "[Write] /tmp/some/other/file.go"
	if got != want {
		t.Errorf("formatToolUse non-reflection = %q, want %q", got, want)
	}
}

// Verifies that markdown bold is stripped from output so the terminal
// shows clean text without literal asterisks.
func TestStripMarkdown(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"**bold text**", "bold text"},
		{"normal text", "normal text"},
		{"some **bold** and **more bold**", "some bold and more bold"},
		{"**nested** middle **end**", "nested middle end"},
	}
	for _, tt := range tests {
		got := stripMarkdown(tt.input)
		if got != tt.want {
			t.Errorf("stripMarkdown(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// Verifies that FormatStreamLine adds a timestamp prefix and ANSI color codes
// so output matches the format previously provided by the bash stream filter.
func TestFormatStreamLine(t *testing.T) {
	line := FormatStreamLine("[r] [Read] /tmp/foo.go")
	plain := ansiRe.ReplaceAllString(line, "")

	// Should have HH:MM:SS timestamp prefix.
	if len(plain) < 8 || plain[2] != ':' || plain[5] != ':' {
		t.Errorf("FormatStreamLine missing timestamp prefix, got: %q", plain)
	}

	// Should contain the text content after stripping ANSI.
	if !strings.Contains(plain, "[r] [Read] /tmp/foo.go") {
		t.Errorf("FormatStreamLine should preserve content, got: %q", plain)
	}

	// Should contain ANSI color codes for [r] and [Read].
	if !strings.Contains(line, "\033[0;36m") {
		t.Error("FormatStreamLine should apply cyan to [r]")
	}
	if !strings.Contains(line, "\033[0;34m") {
		t.Error("FormatStreamLine should apply blue to [Read]")
	}
}

// Verifies that [done] gets green color in the formatted output.
func TestFormatStreamLine_DoneColor(t *testing.T) {
	line := FormatStreamLine("[r] [done]")
	if !strings.Contains(line, "\033[0;32m") {
		t.Error("FormatStreamLine should apply green to [done]")
	}
}

// Verifies that FormatStreamLine does NOT include a per-line task ID prefix —
// task identification is handled by a one-time separator banner instead.
func TestFormatStreamLine_NoTaskIDPrefix(t *testing.T) {
	line := FormatStreamLine("[r] [Read] /tmp/foo.go")
	plain := ansiRe.ReplaceAllString(line, "")

	if strings.Contains(plain, "ralph-") {
		t.Errorf("FormatStreamLine should not include task ID, got: %q", plain)
	}
	if !strings.Contains(plain, "[r]") {
		t.Errorf("FormatStreamLine should include [r] tag, got: %q", plain)
	}
}

// Verifies that ISSUE: lines are detected and produce a banner.
func TestParseDiagnosis_Issue(t *testing.T) {
	label, content, ok := parseDiagnosis("ISSUE: the build is broken")
	if !ok {
		t.Fatal("parseDiagnosis should detect ISSUE: prefix")
	}
	if label != "ISSUE" {
		t.Errorf("label = %q, want ISSUE", label)
	}
	if content != "the build is broken" {
		t.Errorf("content = %q, want 'the build is broken'", content)
	}
}

// Verifies that FIX: lines are detected and produce a banner.
func TestParseDiagnosis_Fix(t *testing.T) {
	label, content, ok := parseDiagnosis("FIX: update the config")
	if !ok {
		t.Fatal("parseDiagnosis should detect FIX: prefix")
	}
	if label != "FIX" {
		t.Errorf("label = %q, want FIX", label)
	}
	if content != "update the config" {
		t.Errorf("content = %q, want 'update the config'", content)
	}
}

// Verifies that ordinary lines are not treated as diagnosis.
func TestParseDiagnosis_NormalLine(t *testing.T) {
	_, _, ok := parseDiagnosis("Reading file /tmp/foo.go")
	if ok {
		t.Error("parseDiagnosis should not match ordinary lines")
	}
}

// Verifies that partial matches like "ISSUES:" or "FIXED:" are not detected.
func TestParseDiagnosis_NoFalsePositives(t *testing.T) {
	for _, line := range []string{"ISSUES: plural", "FIXED: past tense", "issue: lowercase"} {
		if _, _, ok := parseDiagnosis(line); ok {
			t.Errorf("parseDiagnosis should not match %q", line)
		}
	}
}

// Verifies that diagnosisBanner produces a centered banner with ═ characters.
func TestDiagnosisBanner(t *testing.T) {
	banner := diagnosisBanner("ISSUE")
	plain := ansiRe.ReplaceAllString(banner, "")

	if !strings.Contains(plain, "═") {
		t.Error("diagnosisBanner should contain ═ separator characters")
	}
	if !strings.Contains(plain, " ISSUE ") {
		t.Errorf("diagnosisBanner should contain centered label, got: %q", plain)
	}
}

// Verifies that diagnosis lines get banner treatment in FormatStreamOutput.
func TestFormatStreamOutput_DiagnosisBanner(t *testing.T) {
	lines := FormatStreamOutput("ISSUE: something is wrong")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (banner + content), got %d", len(lines))
	}
	plainBanner := ansiRe.ReplaceAllString(lines[0], "")
	if !strings.Contains(plainBanner, "ISSUE") || !strings.Contains(plainBanner, "═") {
		t.Errorf("first line should be banner, got: %q", plainBanner)
	}
	plainContent := ansiRe.ReplaceAllString(lines[1], "")
	if !strings.Contains(plainContent, "something is wrong") {
		t.Errorf("second line should contain content, got: %q", plainContent)
	}
}

// Verifies that non-diagnosis lines pass through normally in FormatStreamOutput.
func TestFormatStreamOutput_NormalLine(t *testing.T) {
	lines := FormatStreamOutput("Reading file /tmp/foo.go")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line for normal text, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if !strings.Contains(plain, "[r] Reading file /tmp/foo.go") {
		t.Errorf("normal line should have [r] prefix, got: %q", plain)
	}
}

// Verifies that under 3 lines sharing a timestamp, each gets its own
// timestamp prefix (no grouping).
func TestStreamFormatter_Under3_EachGetsTimestamp(t *testing.T) {
	ts := "14:30:00"
	f := &StreamFormatter{clock: fixedClock(ts)}

	f.FormatOutput("first line")
	f.FormatOutput("second line")
	lines := f.FlushPending()

	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	for i, line := range lines {
		plain := ansiRe.ReplaceAllString(line, "")
		if !strings.HasPrefix(plain, ts+" ") {
			t.Errorf("line %d should have timestamp prefix %q, got: %q", i, ts, plain)
		}
	}
}

// Verifies that 3+ lines sharing a timestamp produce a header-style
// timestamp on its own line, with content lines at root level (no indent).
func TestStreamFormatter_3Plus_TimestampOnOwnLine(t *testing.T) {
	ts := "14:30:00"
	f := &StreamFormatter{clock: fixedClock(ts)}

	f.FormatOutput("first line")
	f.FormatOutput("second line")
	f.FormatOutput("third line")
	lines := f.FlushPending()

	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (1 timestamp + 3 content), got %d: %v", len(lines), lines)
	}

	// First line is the standalone timestamp (dim).
	plainTS := ansiRe.ReplaceAllString(lines[0], "")
	if plainTS != ts {
		t.Errorf("first line should be standalone timestamp %q, got: %q", ts, plainTS)
	}

	// Content lines should NOT have timestamp prefix or indentation.
	for i := 1; i < len(lines); i++ {
		plain := ansiRe.ReplaceAllString(lines[i], "")
		if strings.HasPrefix(plain, ts) {
			t.Errorf("content line %d should not have timestamp prefix, got: %q", i, plain)
		}
		if strings.HasPrefix(plain, " ") {
			t.Errorf("content line %d should not be indented, got: %q", i, plain)
		}
	}
}

// Verifies that a new timestamp flushes the previous group.
func TestStreamFormatter_NewTimestampFlushes(t *testing.T) {
	sec := 0
	f := &StreamFormatter{clock: func() time.Time {
		sec++
		return time.Date(2026, 1, 1, 14, 30, sec, 0, time.UTC)
	}}

	// Each call gets a different second, so each flushes the previous.
	lines1 := f.FormatOutput("first line")  // buffers (sec=1), returns nil
	lines2 := f.FormatOutput("second line") // flushes first (sec=2), buffers second

	if len(lines1) != 0 {
		t.Errorf("first call should buffer, got %d lines", len(lines1))
	}
	if len(lines2) != 1 {
		t.Fatalf("second call should flush 1 line, got %d", len(lines2))
	}
	plain := ansiRe.ReplaceAllString(lines2[0], "")
	if !strings.HasPrefix(plain, "14:30:01") {
		t.Errorf("flushed line should have first timestamp, got: %q", plain)
	}
}

// Verifies that FormatOutput buffers lines and FlushPending emits them.
func TestStreamFormatter_FormatOutput_BuffersAndFlushes(t *testing.T) {
	ts := "14:30:00"
	f := &StreamFormatter{clock: fixedClock(ts)}

	result := f.FormatOutput("hello world")
	if len(result) != 0 {
		t.Errorf("FormatOutput should buffer, got %d lines", len(result))
	}

	lines := f.FlushPending()
	if len(lines) != 1 {
		t.Fatalf("FlushPending should return 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if !strings.Contains(plain, "[r] hello world") {
		t.Errorf("flushed line should contain content, got: %q", plain)
	}
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
		filterStreamJSON(rawPath, logPath, "", stop)
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
		filterStreamJSON(rawPath, logPath, "", stop)
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
		filterStreamJSON(rawPath, logPath, "", stop)
	}()

	time.Sleep(200 * time.Millisecond)
	f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"text","text":"ISSUE: the config is missing a required field"}]}}`)
	fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"text","text":"FIX: add the default value to config.go"}]}}`)
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
		filterStreamJSON(rawPath, logPath, "", stop)
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
		filterStreamJSON(rawPath, logPath, workDir, stop)
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

// Verifies that long prose lines are truncated to fit terminal width,
// preventing overflow and wrapped lines in the stream output.
func TestFormatOutput_TruncatesLongProse(t *testing.T) {
	f := &StreamFormatter{}
	longProse := strings.Repeat("Now I understand the current state and will analyze ", 5)
	f.FormatOutput(longProse)
	lines := f.FlushPending()

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	// Line should be truncated: tsWidth(9) + "[r] "(4) + content + "…"
	// Total rune count should not exceed maxLineWidth.
	runeCount := utf8.RuneCountInString(plain)
	if runeCount > maxLineWidth {
		t.Errorf("prose line should be truncated to %d runes, got %d: %q", maxLineWidth, runeCount, plain)
	}
	if !strings.HasSuffix(plain, "…") {
		t.Errorf("truncated prose should end with ellipsis, got: %q", plain)
	}
}

// Verifies that tool call lines are NOT truncated — they contain important
// file paths, commands, and patterns that users need to see in full.
func TestFormatOutput_DoesNotTruncateToolLines(t *testing.T) {
	f := &StreamFormatter{}
	longToolLine := "[Edit] " + strings.Repeat("/very/long/path/to/some/deeply/nested/", 5) + "file.go"
	f.FormatOutput(longToolLine)
	lines := f.FlushPending()

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if !strings.Contains(plain, "file.go") {
		t.Errorf("tool line should not be truncated, got: %q", plain)
	}
	if strings.HasSuffix(plain, "…") {
		t.Errorf("tool line should not have ellipsis, got: %q", plain)
	}
}

// Verifies that diagnosis lines (ISSUE:/FIX:) are NOT truncated since they
// contain critical information about the root cause and fix.
func TestFormatOutput_DoesNotTruncateDiagnosis(t *testing.T) {
	f := &StreamFormatter{}
	longDiag := "ISSUE: " + strings.Repeat("the configuration is completely broken because ", 5)
	lines := f.FormatOutput(longDiag)
	lines = append(lines, f.FlushPending()...)

	// Diagnosis produces 2 lines: banner + content.
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines for diagnosis, got %d: %v", len(lines), lines)
	}
	plain := ansiRe.ReplaceAllString(lines[1], "")
	if !strings.Contains(plain, "completely broken") {
		t.Errorf("diagnosis content should not be truncated, got: %q", plain)
	}
}

// Verifies that Bash commands writing to .signal_* files are detected and
// reformatted with a [signal] prefix instead of [Bash], so signal writes
// stand out visually as task boundary markers in the stream output.
func TestParseSignalLine_CurrentTask(t *testing.T) {
	name, msg, ok := parseSignalLine(`[Bash] echo "Implement feature X" > /path/to/.ralph/.signal_current_task`)
	if !ok {
		t.Fatal("should detect signal_current_task write")
	}
	if name != "current_task" {
		t.Errorf("name = %q, want current_task", name)
	}
	if msg != "Implement feature X" {
		t.Errorf("msg = %q, want 'Implement feature X'", msg)
	}
}

// Verifies that .signal_complete writes are detected.
func TestParseSignalLine_Complete(t *testing.T) {
	name, msg, ok := parseSignalLine(`[Bash] echo "Done with the task" > /some/path/.signal_complete`)
	if !ok {
		t.Fatal("should detect signal_complete write")
	}
	if name != "complete" {
		t.Errorf("name = %q, want complete", name)
	}
	if msg != "Done with the task" {
		t.Errorf("msg = %q, want 'Done with the task'", msg)
	}
}

// Verifies that .signal_all_complete writes are detected.
func TestParseSignalLine_AllComplete(t *testing.T) {
	name, _, ok := parseSignalLine(`[Bash] echo "All done" > /some/path/.signal_all_complete`)
	if !ok {
		t.Fatal("should detect signal_all_complete write")
	}
	if name != "all_complete" {
		t.Errorf("name = %q, want all_complete", name)
	}
}

// Verifies that normal Bash commands are NOT treated as signal writes.
func TestParseSignalLine_NormalBash(t *testing.T) {
	for _, line := range []string{
		`[Bash] ls -la`,
		`[Bash] echo "hello" > /tmp/output.txt`,
		`[Bash] cat .signal_complete`,
	} {
		if _, _, ok := parseSignalLine(line); ok {
			t.Errorf("should not match %q", line)
		}
	}
}

// Verifies that [signal] tags get yellow color in formatted output.
func TestColorTag_Signal(t *testing.T) {
	tag := colorTag("[signal]")
	if !strings.Contains(tag, "\033[0;33m") {
		t.Error("colorTag should apply yellow to [signal]")
	}
}

// Verifies that signal lines in FormatOutput get [signal] prefix instead of [r] [Bash].
func TestFormatOutput_SignalLine(t *testing.T) {
	f := &StreamFormatter{}
	f.FormatOutput(`[Bash] echo "Working on feature" > /path/.signal_current_task`)
	lines := f.FlushPending()
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if !strings.Contains(plain, "[signal]") {
		t.Errorf("signal write should show [signal] prefix, got: %q", plain)
	}
	if strings.Contains(plain, "[Bash]") {
		t.Errorf("signal write should not show [Bash], got: %q", plain)
	}
	if !strings.Contains(plain, "current_task") {
		t.Errorf("signal write should show signal name, got: %q", plain)
	}
}

// Verifies that signal lines get yellow color applied to the [signal] tag.
func TestFormatOutput_SignalLineYellow(t *testing.T) {
	f := &StreamFormatter{}
	f.FormatOutput(`[Bash] echo "task done" > /path/.signal_complete`)
	lines := f.FlushPending()
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "\033[0;33m") {
		t.Errorf("signal line should contain yellow ANSI code, got: %q", lines[0])
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
		filterStreamJSON(rawPath, logPath, "", stop)
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

// Verifies that short prose lines are not truncated or modified.
func TestFormatOutput_ShortProseUnchanged(t *testing.T) {
	f := &StreamFormatter{}
	f.FormatOutput("Reading the config file")
	lines := f.FlushPending()

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if !strings.Contains(plain, "[r] Reading the config file") {
		t.Errorf("short prose should pass through unchanged, got: %q", plain)
	}
	if strings.HasSuffix(plain, "…") {
		t.Errorf("short prose should not have ellipsis, got: %q", plain)
	}
}

// Verifies that consecutive identical signal lines are deduplicated —
// Claude's stream-json emits the same Bash tool call across multiple events,
// so without dedup the same [signal] line appears 2-3 times in the log.
func TestFormatOutput_DeduplicatesSignalLines(t *testing.T) {
	f := &StreamFormatter{}
	signalLine := `[Bash] echo "Working on feature X" > /path/.ralph/.signal_current_task`

	// First call buffers the signal line.
	f.FormatOutput(signalLine)
	lines1 := f.FlushPending()
	if len(lines1) != 1 {
		t.Fatalf("first signal call: expected 1 line, got %d", len(lines1))
	}

	// Second identical call should be suppressed.
	lines2 := f.FormatOutput(signalLine)
	lines2 = append(lines2, f.FlushPending()...)
	if len(lines2) != 0 {
		t.Errorf("duplicate signal should be suppressed, got %d lines: %v", len(lines2), lines2)
	}

	// Third identical call should also be suppressed.
	lines3 := f.FormatOutput(signalLine)
	lines3 = append(lines3, f.FlushPending()...)
	if len(lines3) != 0 {
		t.Errorf("third duplicate signal should be suppressed, got %d lines: %v", len(lines3), lines3)
	}
}

// Verifies that different signal lines are NOT suppressed — only exact
// consecutive duplicates are filtered.
func TestFormatOutput_DifferentSignalsNotSuppressed(t *testing.T) {
	f := &StreamFormatter{}

	f.FormatOutput(`[Bash] echo "task A" > /path/.signal_current_task`)
	lines1 := f.FlushPending()
	if len(lines1) != 1 {
		t.Fatalf("first signal: expected 1 line, got %d", len(lines1))
	}

	f.FormatOutput(`[Bash] echo "task B" > /path/.signal_current_task`)
	lines2 := f.FlushPending()
	if len(lines2) != 1 {
		t.Fatalf("different signal: expected 1 line, got %d", len(lines2))
	}
}

// Verifies that when workDir is set, absolute paths in tool use lines are
// shortened to relative paths (strips the worktree prefix).
func TestFormatOutput_ShortensAbsolutePaths(t *testing.T) {
	f := &StreamFormatter{
		workDir: "/Users/daniel/Developer/ralph/.ralph/worktrees/ralph-20260323-01",
	}

	lines := f.FormatOutput("[Edit] /Users/daniel/Developer/ralph/.ralph/worktrees/ralph-20260323-01/go/internal/claude/claude_stream.go")
	lines = append(lines, f.FlushPending()...)

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if strings.Contains(plain, "/Users/daniel") {
		t.Errorf("should not contain absolute path, got: %q", plain)
	}
	if !strings.Contains(plain, "go/internal/claude/claude_stream.go") {
		t.Errorf("should contain relative path, got: %q", plain)
	}
}

// Verifies that path shortening also applies to non-tool prose lines
// (e.g. when Claude mentions a file path in reasoning text).
func TestFormatOutput_ShortensProsePaths(t *testing.T) {
	f := &StreamFormatter{
		workDir: "/tmp/worktree",
	}

	lines := f.FormatOutput("Reading file /tmp/worktree/src/main.go for context")
	lines = append(lines, f.FlushPending()...)

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if strings.Contains(plain, "/tmp/worktree/") {
		t.Errorf("should strip workDir prefix, got: %q", plain)
	}
	if !strings.Contains(plain, "src/main.go") {
		t.Errorf("should keep relative path, got: %q", plain)
	}
}

// Verifies that when workDir is empty, paths are left unchanged.
func TestFormatOutput_NoWorkDir_PathsUnchanged(t *testing.T) {
	f := &StreamFormatter{}

	lines := f.FormatOutput("[Edit] /absolute/path/to/file.go")
	lines = append(lines, f.FlushPending()...)

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if !strings.Contains(plain, "/absolute/path/to/file.go") {
		t.Errorf("path should be unchanged when workDir empty, got: %q", plain)
	}
}

// Verifies that dedup persists across intervening non-signal lines — the
// same Bash command appears in multiple Claude stream events seconds apart,
// often with non-signal content between them.
func TestFormatOutput_SignalDedupPersistsAcrossNonSignal(t *testing.T) {
	f := &StreamFormatter{}
	signalLine := `[Bash] echo "task done" > /path/.signal_complete`

	f.FormatOutput(signalLine)
	lines1 := f.FlushPending()
	if len(lines1) != 1 {
		t.Fatalf("first signal: expected 1 line, got %d", len(lines1))
	}

	f.FormatOutput("Reading some file")
	f.FlushPending()

	// Same signal again after intervening output — still suppressed.
	lines3 := f.FormatOutput(signalLine)
	lines3 = append(lines3, f.FlushPending()...)
	if len(lines3) != 0 {
		t.Errorf("same signal after non-signal should still be suppressed, got %d lines", len(lines3))
	}
}
