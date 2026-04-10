package logging

import (
	"io"
	"os"
)

// defaultLogger is the package-level logger that all package-level Emit/Phase/etc.
// functions delegate to. It's configured once at startup by SetDefault (typically
// from cmd/ralph/main.go after parsing flags) and read by every callsite that
// imports this package.
//
// Holding the logger as a package-level value lets every package call
// `logging.Emit(...)` directly instead of threading a *Logger field or parameter
// through every struct and helper. The logger is constructed once with the
// configured output writers and from then on it's an ambient utility, not a
// dependency that needs injection.
//
// Tests that need to capture log output for assertions use SetDefault to
// install a buffer-backed logger and restore the original on teardown.
var defaultLogger = New(io.Discard)

// SetDefault installs l as the package-level logger that all package-level
// functions in this package delegate to. Returns the previously-installed
// logger so callers can restore it (e.g. in test teardown via defer).
//
// SetDefault is typically called once at process startup from cmd/ralph/main.go
// with a logger configured for stdout + log file. Tests call SetDefault with
// a buffer-backed logger to capture output for assertions.
func SetDefault(l *Logger) *Logger {
	prev := defaultLogger
	if l == nil {
		defaultLogger = New(os.Stdout)
	} else {
		defaultLogger = l
	}
	return prev
}

// Default returns the currently-installed package-level logger. Useful when
// a caller needs to pass the logger to a function that still takes *Logger
// (e.g. legacy code being migrated). Prefer the package-level Emit/Phase/etc.
// functions instead of reaching for Default().
func Default() *Logger {
	return defaultLogger
}

// SetStreaming enables or disables streaming mode on the default logger.
func SetStreaming(on bool) {
	defaultLogger.SetStreaming(on)
}

// Emit writes a log message via the default logger. See (*Logger).Emit.
func Emit(o Opts, format string, args ...any) {
	defaultLogger.Emit(o, format, args...)
}

// EmitInPlace writes the first segment of an in-place log line via the
// default logger. See (*Logger).EmitInPlace.
func EmitInPlace(o Opts, format string, args ...any) {
	defaultLogger.EmitInPlace(o, format, args...)
}

// EmitAppend appends raw text to the current in-place log line via the
// default logger. See (*Logger).EmitAppend.
func EmitAppend(format string, args ...any) {
	defaultLogger.EmitAppend(format, args...)
}

// EmitFinalInPlace closes an in-place log line via the default logger.
// See (*Logger).EmitFinalInPlace.
func EmitFinalInPlace() {
	defaultLogger.EmitFinalInPlace()
}

// AgentLog writes an info-level message with [r] actor prefix via the
// default logger. See (*Logger).AgentLog.
func AgentLog(domain Domain, format string, args ...any) {
	defaultLogger.AgentLog(domain, format, args...)
}

// Phase writes a bold blue phase header via the default logger.
// See (*Logger).Phase.
func Phase(format string, args ...any) {
	defaultLogger.Phase(format, args...)
}

// PhaseColor writes a bold phase header in the given ANSI color via the
// default logger. See (*Logger).PhaseColor.
func PhaseColor(color string, format string, args ...any) {
	defaultLogger.PhaseColor(color, format, args...)
}

// TaskBanner writes a bold magenta separator with the task ID and title
// centered via the default logger. See (*Logger).TaskBanner.
func TaskBanner(taskID, title string, priority *int) {
	defaultLogger.TaskBanner(taskID, title, priority)
}

// EmitDescription writes a task description via the default logger.
// See (*Logger).EmitDescription.
func EmitDescription(description string) {
	defaultLogger.EmitDescription(description)
}

// IterationBanner renders the task banner and phase line via the default
// logger. See (*Logger).IterationBanner.
func IterationBanner(o BannerOpts) {
	defaultLogger.IterationBanner(o)
}

// DashedSeparator writes a bold, colored full-width dashed line via the
// default logger. See (*Logger).DashedSeparator.
func DashedSeparator(color string) {
	defaultLogger.DashedSeparator(color)
}

// Separator writes a bold, colored full-width separator with a centered
// label via the default logger. See (*Logger).Separator.
func Separator(color, label string) {
	defaultLogger.Separator(color, label)
}
