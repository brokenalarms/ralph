package logging

import (
	"bytes"
	"strings"
	"testing"
)

// Verifies that each log level emits the correct ANSI color and [ralph] prefix,
// matching ralph.sh's log, log_success, log_warn, log_error output format.
func TestLogLevelColors(t *testing.T) {
	tests := []struct {
		name      string
		call      func(*Logger)
		wantColor string
	}{
		{"Log", func(l *Logger) { l.Log("msg") }, Cyan},
		{"Success", func(l *Logger) { l.Success("msg") }, Green},
		{"Warn", func(l *Logger) { l.Warn("msg") }, Yellow},
		{"Error", func(l *Logger) { l.Error("msg") }, Red},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := &Logger{out: &buf, logFile: &buf}
			tt.call(l)
			got := buf.String()
			if !strings.Contains(got, tt.wantColor+"[ralph]") {
				t.Errorf("output missing %q color prefix:\n%s", tt.name, got)
			}
			if !strings.Contains(got, "msg") {
				t.Error("output missing message body")
			}
		})
	}
}

// Verifies that Phase output includes bold+blue formatting and the message,
// matching ralph.sh's log_phase format.
func TestPhaseFormatting(t *testing.T) {
	var buf bytes.Buffer
	l := &Logger{out: &buf, logFile: &buf}
	l.Phase("starting phase %d", 1)
	got := buf.String()
	if !strings.Contains(got, Bold) || !strings.Contains(got, Blue) {
		t.Error("Phase should include bold+blue formatting")
	}
	if !strings.Contains(got, "starting phase 1") {
		t.Errorf("Phase output missing message: %s", got)
	}
}

// Verifies that log output includes HH:MM:SS timestamps, matching ralph.sh's _ts().
func TestTimestamp(t *testing.T) {
	var buf bytes.Buffer
	l := &Logger{out: &buf, logFile: &buf}
	l.Log("test")
	got := buf.String()
	// Timestamp format is HH:MM:SS — check for two colons in first 8 chars.
	if len(got) < 8 || got[2] != ':' || got[5] != ':' {
		t.Errorf("expected HH:MM:SS timestamp prefix, got: %q", got[:min(20, len(got))])
	}
}

// Verifies that log output is written to both stdout and the log file writer,
// matching ralph.sh's `| tee -a "$LOG_FILE"` behavior.
func TestDualOutput(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}
	l.Log("dual")
	if !strings.Contains(stdout.String(), "dual") {
		t.Error("message missing from stdout")
	}
	if !strings.Contains(logFile.String(), "dual") {
		t.Error("message missing from log file")
	}
}

// Verifies that format strings with arguments are properly interpolated.
func TestFormatArgs(t *testing.T) {
	var buf bytes.Buffer
	l := &Logger{out: &buf, logFile: &buf}
	l.Log("count=%d name=%s", 42, "test")
	got := buf.String()
	if !strings.Contains(got, "count=42 name=test") {
		t.Errorf("format interpolation failed: %s", got)
	}
}

// Verifies that streaming mode suppresses stdout writes while continuing
// to write to the log file. This prevents duplicate lines when a tail
// goroutine is the sole stdout writer during a Claude Run().
func TestStreamingModeSuppressesStdout(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}

	l.SetStreaming(true)
	l.Log("streamed message")
	l.Phase("streamed phase")

	if stdout.Len() != 0 {
		t.Errorf("streaming mode should suppress stdout, got: %q", stdout.String())
	}
	if !strings.Contains(logFile.String(), "streamed message") {
		t.Error("streaming mode should still write to log file")
	}
	if !strings.Contains(logFile.String(), "streamed phase") {
		t.Error("streaming mode should still write phase to log file")
	}

	l.SetStreaming(false)
	l.Log("normal message")

	if !strings.Contains(stdout.String(), "normal message") {
		t.Error("after disabling streaming, stdout should resume")
	}
}

// Verifies that Separator outputs a bold, colored full-width line with
// centered label and ═ characters, written to both stdout and log file.
func TestSeparatorFormatting(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}
	l.Separator(Magenta, "RALPH EVOLVED")
	got := stdout.String()

	if !strings.Contains(got, Bold) || !strings.Contains(got, Magenta) {
		t.Error("Separator should include bold+magenta formatting")
	}
	if !strings.Contains(got, "RALPH EVOLVED") {
		t.Errorf("Separator missing label: %s", got)
	}
	if !strings.Contains(got, "═") {
		t.Error("Separator should contain ═ characters")
	}
	if !strings.Contains(got, Reset) {
		t.Error("Separator should reset ANSI at end")
	}
	if !strings.Contains(logFile.String(), "RALPH EVOLVED") {
		t.Error("Separator should write to log file")
	}
}

// Verifies that DashedSeparator outputs a full-width dashed line using ─
// characters in the given color, distinct from Separator's ═ characters.
func TestDashedSeparatorFormatting(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}
	l.DashedSeparator(Yellow)
	got := stdout.String()

	if !strings.Contains(got, Bold) || !strings.Contains(got, Yellow) {
		t.Error("DashedSeparator should include bold+yellow formatting")
	}
	if !strings.Contains(got, "─") {
		t.Error("DashedSeparator should contain ─ characters")
	}
	if !strings.Contains(got, Reset) {
		t.Error("DashedSeparator should reset ANSI at end")
	}
	if !strings.Contains(logFile.String(), "─") {
		t.Error("DashedSeparator should write to log file")
	}
}

// Verifies that DashedSeparator respects streaming mode.
func TestDashedSeparatorStreamingMode(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}
	l.SetStreaming(true)
	l.DashedSeparator(Yellow)

	if stdout.Len() != 0 {
		t.Error("DashedSeparator should suppress stdout in streaming mode")
	}
	if !strings.Contains(logFile.String(), "─") {
		t.Error("DashedSeparator should still write to log file in streaming mode")
	}
}

// Verifies that Separator respects streaming mode.
func TestSeparatorStreamingMode(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}
	l.SetStreaming(true)
	l.Separator(Magenta, "TEST")

	if stdout.Len() != 0 {
		t.Error("Separator should suppress stdout in streaming mode")
	}
	if !strings.Contains(logFile.String(), "TEST") {
		t.Error("Separator should still write to log file in streaming mode")
	}
}
