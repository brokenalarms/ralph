package logging

import (
	"regexp"
	"strings"
	"time"
)

// TSWidth is the visible width of the timestamp prefix: "HH:MM:SS " (9 chars).
const TSWidth = 9

// MaxLineWidth is the total visible width limit for log lines. Both
// orchestrator and agent output use this as the single source of truth
// for truncation decisions.
const MaxLineWidth = 120

// LineFormatter is the single source of truth for log line formatting.
// Both the orchestrator Logger and the agent StreamFormatter use it to
// ensure consistent timestamp placement, dedup, and color.
type LineFormatter struct {
	lastTS string
	Clock  func() time.Time
}

func (f *LineFormatter) now() time.Time {
	if f.Clock != nil {
		return f.Clock()
	}
	return time.Now()
}

// FormatLine prepends a dim timestamp when the second changes from the
// previous line, or pads with spaces to maintain alignment.
func (f *LineFormatter) FormatLine(content string) string {
	ts := f.now().Format("15:04:05")
	if ts != f.lastTS {
		f.lastTS = ts
		return Dim + ts + Reset + " " + content
	}
	return strings.Repeat(" ", TSWidth) + content
}

// Format applies shared content formatting (markdown stripping) and timestamp,
// producing a complete log line. This is the single formatting path used by
// both orchestrator (Logger) and agent (StreamFormatter) output.
func (f *LineFormatter) Format(content string) string {
	return f.FormatLine(FormatContent(content))
}

var mdBoldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)

// StripMarkdown removes markdown formatting from text for clean terminal output.
func StripMarkdown(s string) string {
	return mdBoldRe.ReplaceAllString(s, "$1")
}

// FormatContent applies shared content formatting that all regular log output
// passes through, regardless of source. Visual separators (Phase, Separator)
// bypass this and use FormatLine directly.
func FormatContent(content string) string {
	return StripMarkdown(content)
}
