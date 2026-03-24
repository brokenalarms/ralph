package logging

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ANSI color codes.
const (
	Red     = "\033[0;31m"
	Green   = "\033[0;32m"
	Yellow  = "\033[0;33m"
	Blue    = "\033[0;34m"
	Magenta = "\033[0;35m"
	Cyan    = "\033[0;36m"
	Orange  = "\033[0;38;5;208m"
	Dim     = "\033[2;37m"
	Bold    = "\033[1m"
	Reset   = "\033[0m"
)

// Actor identifies the source of a log message.
type Actor string

const (
	Orch       Actor = "o" // orchestrator
	AgentActor Actor = "r" // ralph/agent
)

// Domain categorizes what a log message is about.
type Domain = string

const (
	Git   Domain = "git"
	CI    Domain = "ci"
	Beads Domain = "beads"
	Test  Domain = "test"
	LLM   Domain = "llm"
	Shell Domain = "bash"
)

// Tag formats [actor][domain] with ANSI color. If domain is empty,
// only [actor] is emitted. Greppable by actor or domain independently.
func Tag(color string, actor Actor, domain Domain) string {
	if domain == "" {
		return fmt.Sprintf("%s[%s]%s", color, actor, Reset)
	}
	return fmt.Sprintf("%s[%s][%s]%s", color, actor, domain, Reset)
}

// BranchTag formats a branch name as a colored tag for log messages,
// e.g. "[main]" in green.
func BranchTag(branch string) string {
	return Green + "[" + branch + "]" + Reset
}

// Logger provides colored logging with trailing timestamps that appear
// only when the second changes from the previous line.
type Logger struct {
	out       io.Writer
	logFile   io.Writer
	streaming bool
	lastTS    string
	clock     func() time.Time
}

// New creates a Logger that writes to stdout and the given log file writer.
// If logFile is nil, file logging is disabled.
func New(logFile io.Writer) *Logger {
	if logFile == nil {
		logFile = io.Discard
	}
	return &Logger{
		out:     os.Stdout,
		logFile: logFile,
	}
}

// NewWithWriter creates a Logger that writes to the given writer instead of
// stdout, useful for capturing output in tests.
func NewWithWriter(w io.Writer) *Logger {
	return &Logger{
		out:     w,
		logFile: io.Discard,
	}
}

func (l *Logger) now() time.Time {
	if l.clock != nil {
		return l.clock()
	}
	return time.Now()
}

// tsSuffix returns a dim trailing timestamp when the second has changed
// since the last emitted line, or empty string if it hasn't.
func (l *Logger) tsSuffix() string {
	ts := l.now().Format("15:04:05")
	if ts == l.lastTS {
		return ""
	}
	l.lastTS = ts
	return " " + Dim + ts + Reset
}

// SetStreaming enables or disables streaming mode. In streaming mode, the
// logger writes only to the log file — stdout is handled by a single tail
// goroutine to prevent duplicate output.
func (l *Logger) SetStreaming(on bool) {
	l.streaming = on
}

func (l *Logger) emit(color string, domain Domain, msg string) {
	tag := Tag(color, Orch, domain)
	line := fmt.Sprintf("%s %s%s\n", tag, msg, l.tsSuffix())
	if !l.streaming {
		fmt.Fprint(l.out, line)
	}
	fmt.Fprint(l.logFile, line)
}

// Log writes an info-level message with cyan [o][domain] prefix.
func (l *Logger) Log(domain Domain, format string, args ...any) {
	l.emit(Cyan, domain, fmt.Sprintf(format, args...))
}

// Success writes a success message with green [o][domain] prefix.
func (l *Logger) Success(domain Domain, format string, args ...any) {
	l.emit(Green, domain, fmt.Sprintf(format, args...))
}

// Warn writes a warning with yellow [o][domain] prefix.
func (l *Logger) Warn(domain Domain, format string, args ...any) {
	l.emit(Yellow, domain, fmt.Sprintf(format, args...))
}

// Error writes an error with red [o][domain] prefix.
func (l *Logger) Error(domain Domain, format string, args ...any) {
	l.emit(Red, domain, fmt.Sprintf(format, args...))
}

// Phase writes a bold blue phase header.
func (l *Logger) Phase(format string, args ...any) {
	l.PhaseColor(Blue, format, args...)
}

// PhaseColor writes a bold phase header in the given ANSI color.
func (l *Logger) PhaseColor(color string, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s%s[o]%s %s%s%s%s\n", Bold, color, Reset, Bold, msg, Reset, l.tsSuffix())
	if !l.streaming {
		fmt.Fprint(l.out, line)
	}
	fmt.Fprint(l.logFile, line)
}

// PriorityColor returns the ANSI color for a given priority level.
func PriorityColor(priority int) string {
	switch priority {
	case 0:
		return Red
	case 1:
		return Orange
	case 2:
		return Yellow
	case 3:
		return Green
	case 4:
		return Dim
	default:
		return Reset
	}
}

// PriorityTag returns a colored "[P0]"-style tag for the given priority.
// Returns empty string when priority is nil (unset).
func PriorityTag(priority *int) string {
	if priority == nil {
		return ""
	}
	return fmt.Sprintf("%s[P%d]%s", PriorityColor(*priority), *priority, Reset)
}

// TaskBanner writes a bold magenta separator with the task ID and title centered,
// shown once when a new task begins. When priority is non-nil, a colored
// priority tag is included after the separator.
func (l *Logger) TaskBanner(taskID, title string, priority *int) {
	label := taskID
	if title != "" {
		label = taskID + ": " + title
	}
	l.Separator(Magenta, label)
	if priority != nil {
		l.Log("", "%s %s", PriorityTag(priority), title)
	}
}

// DashedSeparator writes a bold, colored full-width dashed line using ─ characters.
func (l *Logger) DashedSeparator(color string) {
	const totalWidth = 72
	line := fmt.Sprintf("\n%s%s%s%s\n\n", Bold, color, strings.Repeat("─", totalWidth), Reset)
	if !l.streaming {
		fmt.Fprint(l.out, line)
	}
	fmt.Fprint(l.logFile, line)
}

// Separator writes a bold, colored full-width separator with a centered label.
func (l *Logger) Separator(color, label string) {
	const totalWidth = 72
	pad := totalWidth - len(label) - 2 // 2 for spaces around label
	if pad < 4 {
		pad = 4
	}
	left := pad / 2
	right := pad - left
	line := fmt.Sprintf("\n%s%s%s %s %s%s\n\n",
		Bold, color,
		strings.Repeat("═", left), label, strings.Repeat("═", right),
		Reset)
	if !l.streaming {
		fmt.Fprint(l.out, line)
	}
	fmt.Fprint(l.logFile, line)
}

