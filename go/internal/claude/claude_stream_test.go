package claude

import (
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/logging"
)

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

// Verifies that multi-line Bash commands (heredocs, inline scripts) show
// only the first line of the command, not the entire body.
func TestFormatToolUse_MultiLineBashTruncated(t *testing.T) {
	cmd := "node -e '\nconst x = 1;\nconsole.log(x);\nprocess.exit(0);\n'"
	c := streamContent{
		Type:  "tool_use",
		Name:  "Bash",
		Input: map[string]interface{}{"command": cmd},
	}
	got := formatToolUse(c)
	if strings.Contains(got, "const x") {
		t.Errorf("multi-line Bash should not include body lines, got: %q", got)
	}
	if !strings.HasPrefix(got, "[Bash] node -e '") {
		t.Errorf("should show first line with [Bash] prefix, got: %q", got)
	}
}

// Verifies that single-line Bash commands pass through unchanged.
func TestFormatToolUse_SingleLineBash(t *testing.T) {
	c := streamContent{
		Type:  "tool_use",
		Name:  "Bash",
		Input: map[string]interface{}{"command": "go test ./..."},
	}
	got := formatToolUse(c)
	if got != "[Bash] go test ./..." {
		t.Errorf("single-line Bash should pass through, got: %q", got)
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
		got := logging.StripMarkdown(tt.input)
		if got != tt.want {
			t.Errorf("logging.StripMarkdown(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// Verifies that FormatStreamLine adds a leading timestamp and ANSI color codes.
func TestFormatStreamLine(t *testing.T) {
	line := FormatStreamLine("[r] [Read] /tmp/foo.go")
	plain := ansiRe.ReplaceAllString(line, "")

	// Should have HH:MM:SS timestamp prefix.
	if len(plain) < 9 {
		t.Fatalf("FormatStreamLine too short, got: %q", plain)
	}
	prefix := plain[:8]
	if prefix[2] != ':' || prefix[5] != ':' {
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
	if !strings.Contains(line, "\033[0;94m") {
		t.Error("FormatStreamLine should apply bright blue to [Read]")
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

// Verifies that tool tags like [Edit], [Read], [Bash] use bright blue
// so they stand out clearly in the stream log on dark terminals and mobile.
func TestColorTag_ToolUsesBrightBlue(t *testing.T) {
	for _, name := range []string{"[Edit]", "[Read]", "[Bash]", "[Grep]", "[Write]"} {
		tag := colorTag(name)
		if !strings.Contains(tag, logging.BrightBlue) {
			t.Errorf("colorTag(%q) should use BrightBlue, got: %q", name, tag)
		}
		if strings.Contains(tag, logging.Blue+name) {
			t.Errorf("colorTag(%q) should NOT use dim Blue", name)
		}
	}
}
