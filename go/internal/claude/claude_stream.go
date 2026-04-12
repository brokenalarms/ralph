package claude

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

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

// extractAssistantText extracts tool-use summaries from the content array
// in an assistant message. Text content is deliberately skipped because
// Claude's stream-json emits text via content_block_delta events during
// streaming, then repeats the same text in the final assistant event.
// Extracting text from both would produce duplicate log lines.
func extractAssistantText(msg *streamMsg) string {
	if msg == nil || len(msg.Content) == 0 {
		return ""
	}

	var parts []string
	for _, c := range msg.Content {
		if c.Type == "tool_use" {
			parts = append(parts, formatToolUse(c))
		}
	}
	return strings.Join(parts, "\n")
}

// formatToolUse returns a short summary of a tool invocation.
// Multi-line values (e.g. Bash commands with heredocs or inline scripts)
// are truncated to just the first line.
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
				return "[" + c.Name + "] " + firstLine(s)
			}
		}
	}
	return "[" + c.Name + "]"
}

// firstLine returns everything up to the first newline, or the whole
// string if it contains no newlines.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
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

// VerboseOnlyTools lists tool names that are hidden from the stream log by
// default and only shown with --verbose. Adding/removing a tool is a one-line
// change in this map. This is the single source of truth for tool visibility.
var VerboseOnlyTools = map[string]bool{
	"Bash":       true,
	"Edit":       true,
	"Read":       true,
	"Write":      true,
	"Grep":       true,
	"Glob":       true,
	"ToolSearch": true,
	"TodoWrite":  true,
	"TaskOutput": true,
}

// toolNameRe extracts the tool name from a bracketed tool line like "[Read] foo".
var toolNameRe = regexp.MustCompile(`^\[([A-Za-z]+)\]`)

// IsVerboseOnlyTool returns true if the named tool is hidden from the stream
// log by default (only shown with --verbose). This is the single entry point
// for tool visibility checks — both StreamFormatter and ToolBatcher use it.
func IsVerboseOnlyTool(name string) bool {
	return VerboseOnlyTools[name]
}

// isVerboseOnlyLine returns true if the line is a tool call for a verbose-only
// tool. Non-tool lines (prose, diagnosis) always return false.
func isVerboseOnlyLine(text string) bool {
	m := toolNameRe.FindStringSubmatch(text)
	if m == nil {
		return false
	}
	return IsVerboseOnlyTool(m[1])
}

// colorTags applies ANSI color to bracketed tags like [r] or [Edit].
// This is the agent-specific coloring step applied before the shared
// Format path (which handles markdown stripping and timestamps).

// colorTag applies ANSI color to a bracketed tag like [r] or [Read].
func colorTag(tag string) string {
	switch {
	case tag == "[done]":
		return logging.Green + tag + logging.Reset
	case tag == "[r]":
		return logging.Cyan + tag + logging.Reset
	case tag == "[signal]":
		return logging.Yellow + tag + logging.Reset
	case tag == "[thinking]":
		return logging.Dim + tag + logging.Reset
	default:
		return logging.BrightBlue + tag + logging.Reset
	}
}

var tagRe = regexp.MustCompile(`\[([A-Za-z][A-Za-z]*)\]`)

func colorTags(text string) string {
	return tagRe.ReplaceAllStringFunc(text, colorTag)
}

// FormatStreamLine takes raw extracted text from a stream event and returns
// a fully formatted output line with timestamp, ANSI colors, and markdown stripped.
func FormatStreamLine(text string) string {
	f := &logging.LineFormatter{}
	return f.Format(colorTags(text))
}

// StreamFormatter emits lines immediately, prepending a dim timestamp
// only when the second changes from the previous line:
//
//	15:57:20 [Edit] claude_stream.go
//	         [Edit] claude_stream.go
//	15:57:23 [Read] claude_stream.go
type StreamFormatter struct {
	lastSignal       string // dedup: suppress consecutive identical signal lines
	Fmt              logging.LineFormatter
	workDir          string // when set, strip this prefix from absolute paths
	hideVerboseOnly  bool   // when true, suppress VerboseOnlyTools lines
}

func (f *StreamFormatter) shortenPaths(text string) string {
	if f.workDir == "" {
		return text
	}
	return strings.ReplaceAll(text, f.workDir+"/", "")
}

// FlushPending is a no-op — lines are emitted immediately, not buffered.
// Kept for API compatibility with ToolBatcher.
func (f *StreamFormatter) FlushPending() []string {
	return nil
}

// FlushIfStale is a no-op — lines are emitted immediately, not buffered.
// Kept for API compatibility with ToolBatcher.
func (f *StreamFormatter) FlushIfStale() []string {
	return nil
}

// emitLine formats content via the shared Format path (markdown stripping +
// timestamp), returning it as a single-element slice for caller convenience.
func (f *StreamFormatter) emitLine(content string) []string {
	return []string{f.Fmt.Format(content)}
}

// FormatLine formats text with a front timestamp via the shared Format path.
func (f *StreamFormatter) FormatLine(text string) string {
	return f.Fmt.Format(colorTags(text))
}

// FormatOutput formats a stream text line, prepending a dim timestamp
// only when the second changes. Diagnosis lines (ISSUE:/FIX:) get a banner
// above the content.
func (f *StreamFormatter) FormatOutput(text string) []string {
	text = f.shortenPaths(text)
	if name, msg, ok := parseSignalLine(text); ok {
		key := name + ":" + msg
		if key == f.lastSignal {
			return nil
		}
		f.lastSignal = key
		return f.emitLine(colorTags("[signal] " + name + ": " + msg))
	}
	if label, content, ok := parseDiagnosis(text); ok {
		var result []string
		result = append(result, diagnosisBanner(label))
		result = append(result, f.emitLine(colorTags("[r] "+content))...)
		return result
	}
	if f.hideVerboseOnly && isVerboseOnlyLine(text) {
		return nil
	}
	return f.emitLine(colorTags("[r] " + text))
}

var signalRe = regexp.MustCompile(`^\[Bash\] echo ["'](.+?)["'] > .+/\.signal_(current_task|complete|all_complete|no_code_needed)$`)

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

// FormatStreamOutput formats a stream text line using a fresh formatter,
// so the first line always gets a leading timestamp.
func FormatStreamOutput(text string) []string {
	f := &StreamFormatter{}
	return f.FormatOutput(text)
}

