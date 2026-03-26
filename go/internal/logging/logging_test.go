package logging

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func timestampAtFront(plain string) bool {
	// plain should start with "HH:MM:SS " (8 + 1 = 9 chars).
	if len(plain) < 9 {
		return false
	}
	ts := plain[:8]
	return ts[2] == ':' && ts[5] == ':' && plain[8] == ' '
}

// Verifies that each log level emits the correct ANSI color and [o] actor prefix
// with the specified domain tag.
func TestLogLevelColors(t *testing.T) {
	tests := []struct {
		name      string
		call      func(*Logger)
		wantColor string
		wantTag   string
	}{
		{"Log", func(l *Logger) { l.Log(Git, "msg") }, Cyan, "[o][git]"},
		{"Success", func(l *Logger) { l.Success(Git, "msg") }, Green, "[o][git]"},
		{"Warn", func(l *Logger) { l.Warn(Git, "msg") }, Yellow, "[o][git]"},
		{"Error", func(l *Logger) { l.Error(Git, "msg") }, Red, "[o][git]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := &Logger{out: &buf, logFile: &buf}
			tt.call(l)
			got := buf.String()
			if !strings.Contains(got, tt.wantColor+tt.wantTag) {
				t.Errorf("output missing %q color prefix:\n%s", tt.name, got)
			}
			if !strings.Contains(got, "msg") {
				t.Error("output missing message body")
			}
			// Timestamp should be at the front via LineFormatter.
			plain := ansiRe.ReplaceAllString(got, "")
			if !timestampAtFront(plain) {
				t.Errorf("timestamp should be at front of line, got: %q", plain)
			}
		})
	}
}

// Verifies that Tag formats actor+domain tags with ANSI color codes,
// producing the [actor][domain] format greppable by actor or domain.
func TestTagFormat(t *testing.T) {
	tests := []struct {
		name   string
		color  string
		actor  Actor
		domain Domain
		want   string
	}{
		{"orchestrator git", Cyan, Orch, Git, Cyan + "[o][git]" + Reset},
		{"orchestrator ci", Green, Orch, CI, Green + "[o][ci]" + Reset},
		{"agent no domain", Cyan, AgentActor, "", Cyan + "[r]" + Reset},
		{"no domain", Yellow, Orch, "", Yellow + "[o]" + Reset},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Tag(tt.color, tt.actor, tt.domain)
			if got != tt.want {
				t.Errorf("Tag() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Verifies that Phase output includes bold+blue formatting with [o] prefix.
func TestPhaseFormatting(t *testing.T) {
	var buf bytes.Buffer
	l := &Logger{out: &buf, logFile: &buf}
	l.Phase("starting phase %d", 1)
	got := buf.String()
	if !strings.Contains(got, Bold) || !strings.Contains(got, Blue) {
		t.Error("Phase should include bold+blue formatting")
	}
	if !strings.Contains(got, "[o]") {
		t.Error("Phase should use [o] actor prefix")
	}
	if !strings.Contains(got, "starting phase 1") {
		t.Errorf("Phase output missing message: %s", got)
	}
}

// Verifies that timestamps appear at the front of the line and only when
// the second changes from the previous line.
func TestTimestampAtFrontOfLine(t *testing.T) {
	var buf bytes.Buffer
	fixed := time.Date(2026, 3, 25, 14, 30, 45, 0, time.UTC)
	l := &Logger{out: &buf, logFile: &buf}
	l.Fmt.Clock = func() time.Time { return fixed }

	l.Log("", "first message")
	got := buf.String()
	plain := ansiRe.ReplaceAllString(got, "")

	if !strings.HasPrefix(plain, "14:30:45 ") {
		t.Errorf("first line should start with timestamp, got: %q", plain)
	}
	if !strings.Contains(plain, "first message") {
		t.Errorf("first line should include message, got: %q", plain)
	}

	// Second line at same second: padded, no timestamp.
	buf.Reset()
	l.Log("", "second message")
	got = buf.String()
	plain = ansiRe.ReplaceAllString(got, "")
	if strings.Contains(plain, "14:30:45") {
		t.Errorf("same-second line should not include timestamp, got: %q", plain)
	}
	if !strings.HasPrefix(plain, "         ") {
		t.Errorf("same-second line should be padded, got: %q", plain)
	}

	// Third line at a new second: timestamp.
	buf.Reset()
	next := fixed.Add(time.Second)
	l.Fmt.Clock = func() time.Time { return next }
	l.Log("", "third message")
	got = buf.String()
	plain = ansiRe.ReplaceAllString(got, "")
	if !strings.HasPrefix(plain, "14:30:46 ") {
		t.Errorf("new-second line should start with timestamp, got: %q", plain)
	}
}

// Verifies that Phase lines also use front timestamps via LineFormatter.
func TestPhaseTimestampAtFront(t *testing.T) {
	var buf bytes.Buffer
	fixed := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	l := &Logger{out: &buf, logFile: &buf}
	l.Fmt.Clock = func() time.Time { return fixed }
	l.Phase("starting")
	got := buf.String()
	plain := ansiRe.ReplaceAllString(got, "")

	if !strings.HasPrefix(strings.TrimLeft(plain, "\n"), "10:00:00 ") {
		t.Errorf("Phase timestamp should be at front, got: %q", plain)
	}
	if !strings.Contains(plain, "starting") {
		t.Errorf("Phase output missing message, got: %q", plain)
	}
}

// Verifies that log output is written to both stdout and the log file writer,
// matching ralph.sh's `| tee -a "$LOG_FILE"` behavior.
func TestDualOutput(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}
	l.Log("", "dual")
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
	l.Log("", "count=%d name=%s", 42, "test")
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
	l.Log("", "streamed message")
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
	l.Log("", "normal message")

	if !strings.Contains(stdout.String(), "normal message") {
		t.Error("after disabling streaming, stdout should resume")
	}
}

// Verifies that Separator outputs a bold, colored full-width line with
// centered label and ═ characters, written to both stdout and log file,
// with a leading timestamp from LineFormatter.
func TestSeparatorFormatting(t *testing.T) {
	var stdout, logFile bytes.Buffer
	fixed := time.Date(2026, 3, 25, 14, 30, 45, 0, time.UTC)
	l := &Logger{out: &stdout, logFile: &logFile}
	l.Fmt.Clock = func() time.Time { return fixed }
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
	plain := ansiRe.ReplaceAllString(got, "")
	if !strings.Contains(plain, "14:30:45") {
		t.Errorf("Separator should have timestamp from LineFormatter, got: %q", plain)
	}
}

// Verifies that DashedSeparator outputs a full-width dashed line using ─
// characters in the given color, with a timestamp from LineFormatter.
func TestDashedSeparatorFormatting(t *testing.T) {
	var stdout, logFile bytes.Buffer
	fixed := time.Date(2026, 3, 25, 14, 30, 45, 0, time.UTC)
	l := &Logger{out: &stdout, logFile: &logFile}
	l.Fmt.Clock = func() time.Time { return fixed }
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
	plain := ansiRe.ReplaceAllString(got, "")
	if !strings.Contains(plain, "14:30:45") {
		t.Errorf("DashedSeparator should have timestamp from LineFormatter, got: %q", plain)
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

// Verifies BranchTag formats a branch name as a green colored tag
// for use in merge/rebase log messages.
func TestBranchTag(t *testing.T) {
	got := BranchTag("main")
	want := Green + "[main]" + Reset
	if got != want {
		t.Errorf("BranchTag(\"main\") = %q, want %q", got, want)
	}

	got = BranchTag("develop")
	want = Green + "[develop]" + Reset
	if got != want {
		t.Errorf("BranchTag(\"develop\") = %q, want %q", got, want)
	}
}

// Verifies that log lines do NOT include a per-line task ID prefix —
// task identification is handled by a one-time separator banner instead.
func TestNoPerLineTaskIDPrefix(t *testing.T) {
	var buf bytes.Buffer
	l := &Logger{out: &buf, logFile: &buf}
	l.Log("", "doing work")
	got := buf.String()

	if strings.Contains(got, Magenta+"[") {
		t.Errorf("log output should not contain magenta task prefix, got: %s", got)
	}
}

// Verifies that TaskBanner writes a bold separator with the task ID and title centered,
// replacing the old per-line prefix with a single banner per task.
func TestTaskBanner(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}
	l.TaskBanner("ralph-l337", "Fix the widget factory", nil)
	got := stdout.String()

	if !strings.Contains(got, "ralph-l337: Fix the widget factory") {
		t.Errorf("TaskBanner should include task ID and title, got: %s", got)
	}
	if !strings.Contains(got, "═") {
		t.Error("TaskBanner should contain ═ separator characters")
	}
	if !strings.Contains(got, Bold) {
		t.Error("TaskBanner should be bold")
	}
	if !strings.Contains(got, Magenta) {
		t.Error("TaskBanner should use magenta color")
	}
	if !strings.Contains(logFile.String(), "ralph-l337: Fix the widget factory") {
		t.Error("TaskBanner should write to log file")
	}
}

// Verifies that TaskBanner includes a colored priority tag when priority is set.
func TestTaskBannerWithPriority(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}
	p := 0
	l.TaskBanner("ralph-abc", "Critical bug fix", &p)
	got := stdout.String()

	if !strings.Contains(got, "[P0]") {
		t.Errorf("TaskBanner with P0 should include [P0] tag, got: %s", got)
	}
	if !strings.Contains(got, Red) {
		t.Error("P0 priority tag should use red color")
	}
}

// Verifies that TaskBanner respects streaming mode.
func TestTaskBannerStreamingMode(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}
	l.SetStreaming(true)
	l.TaskBanner("ralph-abc", "Some task", nil)

	if stdout.Len() != 0 {
		t.Error("TaskBanner should suppress stdout in streaming mode")
	}
	if !strings.Contains(logFile.String(), "ralph-abc: Some task") {
		t.Error("TaskBanner should still write to log file in streaming mode")
	}
}

// Verifies that PriorityTag returns the correct colored tag for each priority level,
// and returns empty string when priority is nil.
func TestPriorityTag(t *testing.T) {
	tests := []struct {
		name      string
		priority  *int
		wantColor string
		wantTag   string
	}{
		{"P0", intPtr(0), Red, "[P0]"},
		{"P1", intPtr(1), Orange, "[P1]"},
		{"P2", intPtr(2), Yellow, "[P2]"},
		{"P3", intPtr(3), Green, "[P3]"},
		{"P4", intPtr(4), Dim, "[P4]"},
		{"nil", nil, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PriorityTag(tt.priority)
			if tt.priority == nil {
				if got != "" {
					t.Errorf("PriorityTag(nil) = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantTag) {
				t.Errorf("PriorityTag(%d) = %q, want to contain %q", *tt.priority, got, tt.wantTag)
			}
			if !strings.Contains(got, tt.wantColor) {
				t.Errorf("PriorityTag(%d) should use color %q", *tt.priority, tt.wantColor)
			}
		})
	}
}

func intPtr(n int) *int { return &n }
