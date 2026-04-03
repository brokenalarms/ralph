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
		{"Log", func(l *Logger) { l.Emit(Opts{Domain: Git}, "msg") }, Cyan, "[o][git]"},
		{"Success", func(l *Logger) { l.Emit(Opts{Domain: Git, Level: Success}, "msg") }, Green, "[o][git]"},
		{"Warn", func(l *Logger) { l.Emit(Opts{Domain: Git, Level: Warn}, "msg") }, Yellow, "[o][git]"},
		{"Error", func(l *Logger) { l.Emit(Opts{Domain: Git, Level: Error}, "msg") }, Red, "[o][git]"},
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

	l.Emit(Opts{}, "first message")
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
	l.Emit(Opts{}, "second message")
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
	l.Emit(Opts{}, "third message")
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
	l.Emit(Opts{}, "dual")
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
	l.Emit(Opts{}, "count=%d name=%s", 42, "test")
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
	l.Emit(Opts{}, "streamed message")
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
	l.Emit(Opts{}, "normal message")

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
	l.Emit(Opts{}, "doing work")
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

// Verifies that Logger strips markdown via the shared Format path —
// proves it goes through FormatContent, not just FormatLine.
func TestLoggerStripsMarkdown(t *testing.T) {
	var buf bytes.Buffer
	l := &Logger{out: &buf, logFile: &buf}
	l.Emit(Opts{}, "hello **world**")
	got := buf.String()
	if strings.Contains(got, "**world**") {
		t.Error("Logger should strip markdown via shared Format path")
	}
	if !strings.Contains(got, "hello world") {
		t.Errorf("stripped content missing: %s", got)
	}
}

// AgentLog emits [r] actor prefix instead of [o], used when the orchestrator
// relays an agent action like task pickup.
func TestAgentLogUsesAgentActor(t *testing.T) {
	var buf bytes.Buffer
	l := &Logger{out: &buf, logFile: &buf}
	l.AgentLog("", "Working on: fix the bug")
	got := buf.String()

	if !strings.Contains(got, "[r]") {
		t.Errorf("AgentLog should emit [r] actor prefix, got: %s", got)
	}
	if strings.Contains(got, "[o]") {
		t.Errorf("AgentLog should NOT emit [o] actor prefix, got: %s", got)
	}
	if !strings.Contains(got, "Working on: fix the bug") {
		t.Errorf("AgentLog should contain message body, got: %s", got)
	}
}

// Hyperlink returns OSC 8 terminal escape sequences wrapping the visible
// text with a clickable URL.
func TestHyperlink(t *testing.T) {
	got := Hyperlink("https://example.com", "click me")
	want := "\033]8;;https://example.com\033\\click me\033]8;;\033\\"
	if got != want {
		t.Errorf("Hyperlink() = %q, want %q", got, want)
	}
}

// PRLink returns a clickable "PR #N" with OSC 8 link to the GitHub PR URL.
func TestPRLink(t *testing.T) {
	got := PRLink("alice/repo", "42")
	if !strings.Contains(got, "PR #42") {
		t.Errorf("PRLink should contain 'PR #42', got: %q", got)
	}
	if !strings.Contains(got, "https://github.com/alice/repo/pull/42") {
		t.Errorf("PRLink should contain GitHub URL, got: %q", got)
	}
	if !strings.Contains(got, "\033]8;;") {
		t.Errorf("PRLink should contain OSC 8 escape, got: %q", got)
	}
}

// PRLink returns plain "PR #N" when nwo is empty (no remote URL available).
func TestPRLink_NoNWO(t *testing.T) {
	got := PRLink("", "42")
	if got != "PR #42" {
		t.Errorf("PRLink with empty nwo should return plain 'PR #42', got: %q", got)
	}
	if strings.Contains(got, "\033") {
		t.Error("PRLink with empty nwo should not contain escape sequences")
	}
}

// PRLinkOpt returns a *Link with Text "PR #N" and the GitHub URL, for use
// in Opts.Link without inline struct construction at each call site.
func TestPRLinkOpt(t *testing.T) {
	link := PRLinkOpt("alice/repo", "42")
	if link == nil {
		t.Fatal("PRLinkOpt should return non-nil for valid inputs")
	}
	if link.Text != "PR #42" {
		t.Errorf("PRLinkOpt Text = %q, want %q", link.Text, "PR #42")
	}
	if link.URL != "https://github.com/alice/repo/pull/42" {
		t.Errorf("PRLinkOpt URL = %q, want %q", link.URL, "https://github.com/alice/repo/pull/42")
	}
}

// PRLinkOpt returns nil when either argument is empty.
func TestPRLinkOpt_EmptyArgs(t *testing.T) {
	if PRLinkOpt("", "42") != nil {
		t.Error("PRLinkOpt with empty nwo should return nil")
	}
	if PRLinkOpt("alice/repo", "") != nil {
		t.Error("PRLinkOpt with empty prNumber should return nil")
	}
}

// Error log with Analyzer domain emits [o][analyzer] tag so operator can
// distinguish analyzer-triggered halts from other error sources.
func TestErrorWithAnalyzerDomain(t *testing.T) {
	var buf strings.Builder
	l := NewWithWriter(&buf)
	l.Emit(Opts{Domain: Analyzer, Level: Error}, "Halting: %s", "stuck_loop")
	out := buf.String()
	if !strings.Contains(out, "[analyzer]") {
		t.Errorf("expected [analyzer] tag in output, got: %q", out)
	}
	if !strings.Contains(out, "Halting: stuck_loop") {
		t.Errorf("expected halt message in output, got: %q", out)
	}
}

// EmitInPlace writes the first segment of an in-place line in append mode —
// no carriage return, no trailing newline. Writes to both stdout and the log file.
func TestEmitInPlace_AppendsWithoutCarriageReturn(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}
	l.EmitInPlace(Opts{Domain: CI}, "CI polled 1s")

	if strings.Contains(stdout.String(), "\r") {
		t.Errorf("EmitInPlace should not write carriage return, got: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "\n") {
		t.Errorf("EmitInPlace should not write newline, got: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "CI polled 1s") {
		t.Errorf("EmitInPlace should write message to stdout, got: %q", stdout.String())
	}
	if !strings.Contains(logFile.String(), "CI polled 1s") {
		t.Errorf("EmitInPlace should write message to log file, got: %q", logFile.String())
	}
}

// EmitAppend appends raw text to the current in-place line — no tag, no
// carriage return, no trailing newline. Writes to both stdout and the log file.
func TestEmitAppend_AppendsRawText(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}
	l.EmitInPlace(Opts{Domain: CI}, "CI polled 1s")
	l.EmitAppend("..2s")
	l.EmitAppend("..4s")

	if strings.Contains(stdout.String(), "\r") || strings.Contains(stdout.String(), "\n") {
		t.Errorf("EmitAppend should not write \\r or \\n, got: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "CI polled 1s..2s..4s") {
		t.Errorf("stdout should contain accumulated text, got: %q", stdout.String())
	}
	if !strings.Contains(logFile.String(), "CI polled 1s..2s..4s") {
		t.Errorf("log file should contain accumulated text, got: %q", logFile.String())
	}
}

// EmitFinalInPlace closes the in-place line with a newline on both stdout
// and the log file — the line content is already accumulated via EmitInPlace
// and EmitAppend.
func TestEmitFinalInPlace_ClosesLineWithNewline(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}
	l.EmitInPlace(Opts{Domain: CI}, "CI polled 1s")
	l.EmitAppend("..2s")
	l.EmitFinalInPlace()

	if !strings.HasSuffix(stdout.String(), "\n") {
		t.Errorf("stdout should end with newline after EmitFinalInPlace, got: %q", stdout.String())
	}
	if !strings.HasSuffix(logFile.String(), "\n") {
		t.Errorf("log file should end with newline after EmitFinalInPlace, got: %q", logFile.String())
	}
	if strings.Contains(stdout.String(), "\r") || strings.Contains(logFile.String(), "\r") {
		t.Errorf("neither stdout nor log file should contain carriage return, stdout: %q logFile: %q", stdout.String(), logFile.String())
	}
}

// EmitInPlace suppresses stdout in streaming mode, same as Emit — the tail
// goroutine owns stdout when streaming is active.
func TestEmitInPlace_RespectsStreamingMode(t *testing.T) {
	var stdout, logFile bytes.Buffer
	l := &Logger{out: &stdout, logFile: &logFile}
	l.SetStreaming(true)
	l.EmitInPlace(Opts{Domain: CI}, "CI polled 1s")
	if stdout.Len() != 0 {
		t.Errorf("EmitInPlace in streaming mode should not write to stdout, got: %q", stdout.String())
	}
	if !strings.Contains(logFile.String(), "CI polled 1s") {
		t.Errorf("EmitInPlace in streaming mode should still write to log file, got: %q", logFile.String())
	}
}

// ModelTag returns a color-coded [short-name] tag for each supported model family,
// with distinct colors for haiku, sonnet, and opus.
func TestModelTag(t *testing.T) {
	tests := []struct {
		model     string
		wantName  string
		wantColor string
	}{
		{"claude-haiku-4-5-20251001", "haiku", BrightBlue},
		{"claude-sonnet-4-5-20241022", "sonnet", Yellow},
		{"claude-opus-4-6", "opus", Magenta},
		{"unknown-model", "unknown-model", Cyan},
	}

	for _, tt := range tests {
		t.Run(tt.wantName, func(t *testing.T) {
			got := ModelTag(tt.model)
			if !strings.Contains(got, "["+tt.wantName+"]") {
				t.Errorf("ModelTag(%q) = %q, want to contain [%s]", tt.model, got, tt.wantName)
			}
			if !strings.Contains(got, tt.wantColor) {
				t.Errorf("ModelTag(%q) = %q, want color %q", tt.model, got, tt.wantColor)
			}
		})
	}

	// Each model family gets a distinct color.
	haikuColor := strings.Split(ModelTag("claude-haiku-4-5"), "[haiku]")[0]
	sonnetColor := strings.Split(ModelTag("claude-sonnet-4-5"), "[sonnet]")[0]
	opusColor := strings.Split(ModelTag("claude-opus-4"), "[opus]")[0]
	if haikuColor == sonnetColor || sonnetColor == opusColor || haikuColor == opusColor {
		t.Errorf("model families must have distinct colors: haiku=%q sonnet=%q opus=%q", haikuColor, sonnetColor, opusColor)
	}
}

// Emit with Opts.Model set renders a color-coded [model] sub-tag between the
// domain tag and the message body, so operators can scan which model is active.
func TestEmit_WithModel_ShowsSubTag(t *testing.T) {
	var buf bytes.Buffer
	l := &Logger{out: &buf, logFile: &buf}
	l.Emit(Opts{Domain: LLM, Model: "claude-haiku-4-5-20251001"}, "Running verification")

	got := buf.String()
	if !strings.Contains(got, "[llm]") {
		t.Errorf("expected [llm] domain tag in output, got: %q", got)
	}
	if !strings.Contains(got, "[haiku]") {
		t.Errorf("expected [haiku] model sub-tag in output, got: %q", got)
	}
	if !strings.Contains(got, "Running verification") {
		t.Errorf("expected message body in output, got: %q", got)
	}
	// Model sub-tag appears after domain tag and before the message body.
	llmIdx := strings.Index(got, "[llm]")
	haikuIdx := strings.Index(got, "[haiku]")
	msgIdx := strings.Index(got, "Running verification")
	if llmIdx < 0 || haikuIdx < 0 || msgIdx < 0 {
		t.Fatal("missing expected content")
	}
	if haikuIdx < llmIdx {
		t.Errorf("[haiku] sub-tag should appear after [llm] domain tag")
	}
	if msgIdx < haikuIdx {
		t.Errorf("message body should appear after [haiku] sub-tag")
	}
}

func intPtr(n int) *int { return &n }
