package claude

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/logging"
)

// streamEvent is the top-level envelope for Claude's stream-json output.
type streamEvent struct {
	Type    string       `json:"type"`
	Subtype string       `json:"subtype"`
	Message *streamMsg   `json:"message"`
	Delta   *streamDelta `json:"delta"`
	Error   string       `json:"error"`
}

type streamMsg struct {
	Content []streamContent `json:"content"`
}

type streamContent struct {
	Type  string                 `json:"type"`
	Text  string                 `json:"text"`
	Name  string                 `json:"name"`
	Input map[string]interface{} `json:"input"`
}

type streamDelta struct {
	Text string `json:"text"`
}

// extractStreamText pulls human-readable text from a stream-json line.
// Uses encoding/json to properly parse Claude's nested message format.
func extractStreamText(line string) string {
	if len(line) == 0 || line[0] != '{' {
		return ""
	}

	var ev streamEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return ""
	}

	switch ev.Type {
	case "assistant":
		return extractAssistantText(ev.Message)

	case "content_block_delta":
		if ev.Delta != nil {
			return ev.Delta.Text
		}

	case "result":
		if ev.Subtype == "error_response" && ev.Error != "" {
			return ev.Error
		}
		return "[done]"
	}

	return ""
}

// extractAssistantText extracts text and tool-use summaries from the
// content array in an assistant message.
func extractAssistantText(msg *streamMsg) string {
	if msg == nil || len(msg.Content) == 0 {
		return ""
	}

	var parts []string
	for _, c := range msg.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		case "tool_use":
			parts = append(parts, formatToolUse(c))
		}
	}
	return strings.Join(parts, "\n")
}

// formatToolUse returns a short summary of a tool invocation.
func formatToolUse(c streamContent) string {
	if summary, ok := reflectionSummary(c); ok {
		return "[" + c.Name + "] " + summary
	}
	for _, key := range []string{
		"file_path", "command", "pattern", "query", "url",
		"description", "task_id", "skill", "prompt",
	} {
		if v, ok := c.Input[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return "[" + c.Name + "] " + s
			}
		}
	}
	return "[" + c.Name + "]"
}

const maxReflectionSummary = 80

// reflectionSummary detects Write calls to reflections/ and returns a
// one-line summary of the content (e.g. "Reflection: Fixed arg order").
func reflectionSummary(c streamContent) (string, bool) {
	if c.Name != "Write" {
		return "", false
	}
	fp, _ := c.Input["file_path"].(string)
	if !strings.Contains(fp, "/reflections/") {
		return "", false
	}
	content, _ := c.Input["content"].(string)
	if content == "" {
		return "", false
	}
	line := firstMeaningfulLine(content)
	line = strings.TrimPrefix(line, "# ")
	if len([]rune(line)) > maxReflectionSummary {
		line = string([]rune(line)[:maxReflectionSummary-1]) + "…"
	}
	return "Reflection: " + line, true
}

// firstMeaningfulLine returns the first non-empty line from text.
func firstMeaningfulLine(text string) string {
	for _, line := range strings.SplitN(text, "\n", 10) {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

var mdBoldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)

// stripMarkdown removes markdown formatting from text for clean terminal output.
func stripMarkdown(s string) string {
	return mdBoldRe.ReplaceAllString(s, "$1")
}

// colorTag applies ANSI color to a bracketed tag like [r] or [Read].
func colorTag(tag string) string {
	switch {
	case tag == "[done]":
		return logging.Green + tag + logging.Reset
	case tag == "[r]":
		return logging.Cyan + tag + logging.Reset
	case tag == "[signal]":
		return logging.Yellow + tag + logging.Reset
	default:
		return logging.Blue + tag + logging.Reset
	}
}

var tagRe = regexp.MustCompile(`\[([A-Za-z][A-Za-z]*)\]`)

// formatContent applies markdown stripping and ANSI tag coloring to text
// without adding a timestamp prefix.
func formatContent(text string) string {
	text = stripMarkdown(text)
	return tagRe.ReplaceAllStringFunc(text, colorTag)
}

// FormatStreamLine takes raw extracted text from a stream event and returns
// a fully formatted output line with timestamp, ANSI colors, and markdown stripped.
func FormatStreamLine(text string) string {
	return time.Now().Format("15:04:05") + " " + formatContent(text)
}

// StreamFormatter buffers consecutive lines that share the same timestamp
// and emits them as a group when the second changes:
//   - Under 3 lines: each line gets its own timestamp prefix
//   - 3+ lines: timestamp on its own line, content lines at root level
type StreamFormatter struct {
	lastSignal string // dedup: suppress consecutive identical signal lines
	pendingTS  string
	pending    []string          // formatted content without timestamp prefix
	clock      func() time.Time  // injectable clock; nil defaults to time.Now
	workDir    string            // when set, strip this prefix from absolute paths
}

const tsWidth = 9       // "HH:MM:SS" (8) + space (1)
const agentPrefix = 4 // "[r] " (4)
const maxLineWidth = 120

func (f *StreamFormatter) shortenPaths(text string) string {
	if f.workDir == "" {
		return text
	}
	return strings.ReplaceAll(text, f.workDir+"/", "")
}

func (f *StreamFormatter) now() time.Time {
	if f.clock != nil {
		return f.clock()
	}
	return time.Now()
}

// FlushPending emits any buffered lines with appropriate timestamp formatting.
func (f *StreamFormatter) FlushPending() []string {
	if len(f.pending) == 0 {
		return nil
	}
	ts := f.pendingTS
	lines := f.pending
	f.pending = nil
	f.pendingTS = ""

	if len(lines) < 3 {
		result := make([]string, len(lines))
		for i, c := range lines {
			result[i] = ts + " " + c
		}
		return result
	}

	result := make([]string, 0, len(lines)+1)
	result = append(result, logging.Dim+ts+logging.Reset)
	for _, c := range lines {
		result = append(result, c)
	}
	return result
}

// FlushIfStale emits pending lines only if the current second has changed.
func (f *StreamFormatter) FlushIfStale() []string {
	if len(f.pending) == 0 {
		return nil
	}
	ts := f.now().Format("15:04:05")
	if ts != f.pendingTS {
		return f.FlushPending()
	}
	return nil
}

// bufferLine adds a formatted content line to the pending buffer.
// If the timestamp has changed, flushes the previous group first.
func (f *StreamFormatter) bufferLine(content string) []string {
	ts := f.now().Format("15:04:05")

	var flushed []string
	if ts != f.pendingTS && len(f.pending) > 0 {
		flushed = f.FlushPending()
	}

	f.pendingTS = ts
	f.pending = append(f.pending, content)
	return flushed
}

// FormatLine formats text with a timestamp prefix. Kept for simple
// single-line formatting; does not participate in buffered grouping.
func (f *StreamFormatter) FormatLine(text string) string {
	content := formatContent(text)
	ts := f.now().Format("15:04:05")
	return ts + " " + content
}

// FormatOutput formats a stream text line using timestamp grouping,
// returning one or more output lines. Diagnosis lines (ISSUE:/FIX:)
// get a banner above the content. Prose lines (not tool calls or
// diagnosis) are truncated to maxLineWidth to prevent terminal overflow.
func (f *StreamFormatter) FormatOutput(text string) []string {
	text = f.shortenPaths(text)
	if name, msg, ok := parseSignalLine(text); ok {
		key := name + ":" + msg
		if key == f.lastSignal {
			return nil
		}
		f.lastSignal = key
		return f.bufferLine(formatContent("[signal] " + name + ": " + msg))
	}
	if label, content, ok := parseDiagnosis(text); ok {
		var result []string
		result = append(result, f.FlushPending()...)
		result = append(result, diagnosisBanner(label))
		result = append(result, f.bufferLine(formatContent("[r] "+content))...)
		return result
	}
	if !isToolLine(text) {
		text = truncateProse(text, maxLineWidth-tsWidth-agentPrefix)
	}
	return f.bufferLine(formatContent("[r] " + text))
}

// isToolLine returns true if the line starts with a bracketed tool name.
func isToolLine(text string) bool {
	return len(text) > 0 && text[0] == '['
}

// truncateProse shortens text to maxLen runes, appending "…" if truncated.
func truncateProse(text string, maxLen int) string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	return string(runes[:maxLen-1]) + "…"
}

var signalRe = regexp.MustCompile(`^\[Bash\] echo ["'](.+?)["'] > .+/\.signal_(current_task|complete|all_complete)$`)

// parseSignalLine detects Bash commands that write to .signal_* files.
// Returns the signal name (e.g. "current_task") and message content.
func parseSignalLine(text string) (name, msg string, ok bool) {
	m := signalRe.FindStringSubmatch(text)
	if m == nil {
		return "", "", false
	}
	return m[2], m[1], true
}

var diagnosisRe = regexp.MustCompile(`^(ISSUE|FIX):\s+(.+)`)

// parseDiagnosis checks if a line is an ISSUE: or FIX: diagnosis line.
// Returns the label, content, and whether it matched.
func parseDiagnosis(line string) (label, content string, ok bool) {
	m := diagnosisRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// diagnosisBanner returns a bold yellow centered banner like ═══ ISSUE ═══.
func diagnosisBanner(label string) string {
	const totalWidth = 72
	pad := totalWidth - len(label) - 2
	if pad < 4 {
		pad = 4
	}
	left := pad / 2
	right := pad - left
	return fmt.Sprintf("%s%s%s %s %s%s",
		logging.Bold, logging.Yellow,
		strings.Repeat("═", left), label, strings.Repeat("═", right),
		logging.Reset)
}

// FormatStreamOutput formats a stream text line without timestamp grouping.
// Each call uses a fresh formatter, so every line gets its own timestamp.
// Used by ToolBatcher for backward-compatible output when no grouping is needed.
func FormatStreamOutput(text string) []string {
	f := &StreamFormatter{}
	result := f.FormatOutput(text)
	result = append(result, f.FlushPending()...)
	return result
}

