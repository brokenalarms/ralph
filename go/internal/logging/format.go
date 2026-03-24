package logging

import (
	"strings"
	"time"
)

// TSWidth is the visible width of the timestamp prefix: "HH:MM:SS " (9 chars).
const TSWidth = 9

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
