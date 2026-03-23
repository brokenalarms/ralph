package logging

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ANSI color codes matching ralph.sh.
const (
	Red     = "\033[0;31m"
	Green   = "\033[0;32m"
	Yellow  = "\033[0;33m"
	Blue    = "\033[0;34m"
	Magenta = "\033[0;35m"
	Cyan    = "\033[0;36m"
	Bold    = "\033[1m"
	Reset   = "\033[0m"
)

// Logger provides colored, timestamped logging matching ralph.sh's output.
type Logger struct {
	out       io.Writer
	logFile   io.Writer
	streaming bool
	taskID    string
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

func ts() string {
	return time.Now().Format("15:04:05")
}

// SetStreaming enables or disables streaming mode. In streaming mode, the
// logger writes only to the log file — stdout is handled by a single tail
// goroutine to prevent duplicate output.
func (l *Logger) SetStreaming(on bool) {
	l.streaming = on
}

// SetTaskID sets the current task identifier (e.g. "ralph-xyz") which is
// included in every log line after the timestamp in inline mode.
func (l *Logger) SetTaskID(id string) {
	l.taskID = id
}

// TaskID returns the current task identifier.
func (l *Logger) TaskID() string {
	return l.taskID
}

func (l *Logger) emit(color, prefix, msg string) {
	taskTag := l.taskTag()
	line := fmt.Sprintf("%s %s%s[%s]%s %s\n", ts(), taskTag, color, prefix, Reset, msg)
	if !l.streaming {
		fmt.Fprint(l.out, line)
	}
	fmt.Fprint(l.logFile, line)
}

func (l *Logger) taskTag() string {
	if l.taskID == "" {
		return ""
	}
	return Magenta + "[" + l.taskID + "]" + Reset + " "
}

// Log writes an info-level message with cyan [ralph] prefix.
func (l *Logger) Log(format string, args ...any) {
	l.emit(Cyan, "ralph", fmt.Sprintf(format, args...))
}

// Success writes a success message with green [ralph] prefix.
func (l *Logger) Success(format string, args ...any) {
	l.emit(Green, "ralph", fmt.Sprintf(format, args...))
}

// Warn writes a warning with yellow [ralph] prefix.
func (l *Logger) Warn(format string, args ...any) {
	l.emit(Yellow, "ralph", fmt.Sprintf(format, args...))
}

// Error writes an error with red [ralph] prefix.
func (l *Logger) Error(format string, args ...any) {
	l.emit(Red, "ralph", fmt.Sprintf(format, args...))
}

// Phase writes a bold blue phase header.
func (l *Logger) Phase(format string, args ...any) {
	l.PhaseColor(Blue, format, args...)
}

// PhaseColor writes a bold phase header in the given ANSI color.
func (l *Logger) PhaseColor(color string, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	taskTag := l.taskTag()
	line := fmt.Sprintf("%s %s%s%s[ralph]%s %s%s%s\n", ts(), taskTag, Bold, color, Reset, Bold, msg, Reset)
	if !l.streaming {
		fmt.Fprint(l.out, line)
	}
	fmt.Fprint(l.logFile, line)
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

