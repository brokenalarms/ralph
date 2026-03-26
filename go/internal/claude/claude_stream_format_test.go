package claude

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/brokenalarms/ralph/internal/logging"
)

func fixedClock(ts string) func() time.Time {
	parsed, _ := time.Parse("15:04:05", ts)
	return func() time.Time { return parsed }
}

// Verifies that timestamps appear at the front of lines, only when the
// second changes from the previous line. First line always gets a timestamp.
func TestStreamFormatter_TimestampAtFront(t *testing.T) {
	sec := 0
	f := &StreamFormatter{Fmt: logging.LineFormatter{Clock: func() time.Time {
		return time.Date(2026, 1, 1, 15, 57, 20+sec, 0, time.UTC)
	}}}

	// First line: always gets timestamp at front.
	lines1 := f.FormatOutput("[Edit] claude_stream.go")
	if len(lines1) != 1 {
		t.Fatalf("first line: expected 1 line, got %d", len(lines1))
	}
	plain1 := ansiRe.ReplaceAllString(lines1[0], "")
	if !strings.HasPrefix(plain1, "15:57:20 ") {
		t.Errorf("first line should start with timestamp, got: %q", plain1)
	}
	if !strings.Contains(plain1, "[r] [Edit] claude_stream.go") {
		t.Errorf("first line should contain content, got: %q", plain1)
	}

	// Second line same second: no timestamp, padded.
	lines2 := f.FormatOutput("[Edit] claude_stream.go")
	if len(lines2) != 1 {
		t.Fatalf("second line: expected 1 line, got %d", len(lines2))
	}
	plain2 := ansiRe.ReplaceAllString(lines2[0], "")
	if strings.Contains(plain2, "15:57") {
		t.Errorf("same-second line should have no timestamp, got: %q", plain2)
	}

	// Third line different second: gets timestamp.
	sec = 3
	lines3 := f.FormatOutput("[Read] claude_stream.go")
	if len(lines3) != 1 {
		t.Fatalf("third line: expected 1 line, got %d", len(lines3))
	}
	plain3 := ansiRe.ReplaceAllString(lines3[0], "")
	if !strings.HasPrefix(plain3, "15:57:23 ") {
		t.Errorf("different-second line should start with timestamp, got: %q", plain3)
	}
}

// Verifies that same-second lines omit the timestamp (only the first
// line in a given second shows it).
func TestStreamFormatter_SameSecond_NoTimestamp(t *testing.T) {
	ts := "14:30:00"
	f := &StreamFormatter{Fmt: logging.LineFormatter{Clock: fixedClock(ts)}}

	lines1 := f.FormatOutput("first line")
	lines2 := f.FormatOutput("second line")

	if len(lines1) != 1 || len(lines2) != 1 {
		t.Fatalf("expected 1 line each, got %d and %d", len(lines1), len(lines2))
	}
	plain1 := ansiRe.ReplaceAllString(lines1[0], "")
	plain2 := ansiRe.ReplaceAllString(lines2[0], "")

	if !strings.HasPrefix(plain1, ts+" ") {
		t.Errorf("first line should start with timestamp, got: %q", plain1)
	}
	if strings.Contains(plain2, ts) {
		t.Errorf("same-second line should not have timestamp, got: %q", plain2)
	}
}

// Verifies that many lines at the same second still only show the
// timestamp on the first line.
func TestStreamFormatter_ManySameSecond(t *testing.T) {
	ts := "14:30:00"
	f := &StreamFormatter{Fmt: logging.LineFormatter{Clock: fixedClock(ts)}}

	for i := 0; i < 5; i++ {
		lines := f.FormatOutput(fmt.Sprintf("line %d", i))
		if len(lines) != 1 {
			t.Fatalf("line %d: expected 1 line, got %d", i, len(lines))
		}
		plain := ansiRe.ReplaceAllString(lines[0], "")
		if i == 0 {
			if !strings.HasPrefix(plain, ts+" ") {
				t.Errorf("first line should start with timestamp, got: %q", plain)
			}
		} else {
			if strings.Contains(plain, ts) {
				t.Errorf("line %d should not have timestamp, got: %q", i, plain)
			}
		}
	}
}

// Verifies that each line with a different second gets a leading timestamp.
func TestStreamFormatter_DifferentSeconds_EachGetsTimestamp(t *testing.T) {
	sec := 0
	f := &StreamFormatter{Fmt: logging.LineFormatter{Clock: func() time.Time {
		sec++
		return time.Date(2026, 1, 1, 14, 30, sec, 0, time.UTC)
	}}}

	lines1 := f.FormatOutput("first line")
	lines2 := f.FormatOutput("second line")

	if len(lines1) != 1 {
		t.Fatalf("first call: expected 1 line, got %d", len(lines1))
	}
	if len(lines2) != 1 {
		t.Fatalf("second call: expected 1 line, got %d", len(lines2))
	}
	plain1 := ansiRe.ReplaceAllString(lines1[0], "")
	plain2 := ansiRe.ReplaceAllString(lines2[0], "")
	if !strings.HasPrefix(plain1, "14:30:01 ") {
		t.Errorf("first line should start with 14:30:01, got: %q", plain1)
	}
	if !strings.HasPrefix(plain2, "14:30:02 ") {
		t.Errorf("second line should start with 14:30:02, got: %q", plain2)
	}
}

// Verifies that FormatOutput returns lines immediately (no buffering).
func TestStreamFormatter_FormatOutput_ReturnsImmediately(t *testing.T) {
	ts := "14:30:00"
	f := &StreamFormatter{Fmt: logging.LineFormatter{Clock: fixedClock(ts)}}

	lines := f.FormatOutput("hello world")
	if len(lines) != 1 {
		t.Fatalf("FormatOutput should return 1 line immediately, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if !strings.Contains(plain, "[r] hello world") {
		t.Errorf("line should contain content, got: %q", plain)
	}
	if !strings.HasPrefix(plain, ts+" ") {
		t.Errorf("line should start with timestamp, got: %q", plain)
	}
}

// Verifies that long prose lines are truncated to fit terminal width,
// preventing overflow and wrapped lines in the stream output.
func TestFormatOutput_TruncatesLongProse(t *testing.T) {
	f := &StreamFormatter{}
	longProse := strings.Repeat("Now I understand the current state and will analyze ", 5)
	lines := f.FormatOutput(longProse)

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	runeCount := utf8.RuneCountInString(plain)
	if runeCount > logging.MaxLineWidth {
		t.Errorf("prose line should be truncated to %d runes, got %d: %q", logging.MaxLineWidth, runeCount, plain)
	}
	if !strings.Contains(plain, "…") {
		t.Errorf("truncated prose should contain ellipsis, got: %q", plain)
	}
}

// Verifies that tool call lines are truncated at the same width as prose,
// preventing terminal overflow from long file paths or commands.
func TestFormatOutput_TruncatesToolLines(t *testing.T) {
	f := &StreamFormatter{}
	longToolLine := "[Edit] " + strings.Repeat("/very/long/path/to/some/deeply/nested/", 5) + "file.go"
	lines := f.FormatOutput(longToolLine)

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	runeCount := utf8.RuneCountInString(plain)
	if runeCount > logging.MaxLineWidth {
		t.Errorf("tool line should be truncated to %d runes, got %d: %q", logging.MaxLineWidth, runeCount, plain)
	}
	if !strings.HasSuffix(plain, "…") {
		t.Errorf("long tool line should have ellipsis, got: %q", plain)
	}
}

// Verifies that diagnosis lines (ISSUE:/FIX:) are NOT truncated since they
// contain critical information about the root cause and fix.
func TestFormatOutput_DoesNotTruncateDiagnosis(t *testing.T) {
	f := &StreamFormatter{}
	longDiag := "ISSUE: " + strings.Repeat("the configuration is completely broken because ", 5)
	lines := f.FormatOutput(longDiag)

	// Diagnosis produces 2 lines: banner + content.
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines for diagnosis, got %d: %v", len(lines), lines)
	}
	plain := ansiRe.ReplaceAllString(lines[1], "")
	if !strings.Contains(plain, "completely broken") {
		t.Errorf("diagnosis content should not be truncated, got: %q", plain)
	}
}

// Verifies that signal lines in FormatOutput get [signal] prefix instead of [r] [Bash].
func TestFormatOutput_SignalLine(t *testing.T) {
	f := &StreamFormatter{}
	lines := f.FormatOutput(`[Bash] echo "Working on feature" > /path/.signal_current_task`)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if !strings.Contains(plain, "[signal]") {
		t.Errorf("signal write should show [signal] prefix, got: %q", plain)
	}
	if strings.Contains(plain, "[Bash]") {
		t.Errorf("signal write should not show [Bash], got: %q", plain)
	}
	if !strings.Contains(plain, "current_task") {
		t.Errorf("signal write should show signal name, got: %q", plain)
	}
}

// Verifies that signal lines get yellow color applied to the [signal] tag.
func TestFormatOutput_SignalLineYellow(t *testing.T) {
	f := &StreamFormatter{}
	lines := f.FormatOutput(`[Bash] echo "task done" > /path/.signal_complete`)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "\033[0;33m") {
		t.Errorf("signal line should contain yellow ANSI code, got: %q", lines[0])
	}
}

// Verifies that short prose lines are not truncated or modified.
func TestFormatOutput_ShortProseUnchanged(t *testing.T) {
	f := &StreamFormatter{}
	lines := f.FormatOutput("Reading the config file")

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if !strings.Contains(plain, "[r] Reading the config file") {
		t.Errorf("short prose should pass through unchanged, got: %q", plain)
	}
	if strings.HasSuffix(plain, "…") {
		t.Errorf("short prose should not have ellipsis, got: %q", plain)
	}
}

// Verifies that consecutive identical signal lines are deduplicated —
// Claude's stream-json emits the same Bash tool call across multiple events,
// so without dedup the same [signal] line appears 2-3 times in the log.
func TestFormatOutput_DeduplicatesSignalLines(t *testing.T) {
	f := &StreamFormatter{}
	signalLine := `[Bash] echo "Working on feature X" > /path/.ralph/.signal_current_task`

	lines1 := f.FormatOutput(signalLine)
	if len(lines1) != 1 {
		t.Fatalf("first signal call: expected 1 line, got %d", len(lines1))
	}

	// Second identical call should be suppressed.
	lines2 := f.FormatOutput(signalLine)
	if len(lines2) != 0 {
		t.Errorf("duplicate signal should be suppressed, got %d lines: %v", len(lines2), lines2)
	}

	// Third identical call should also be suppressed.
	lines3 := f.FormatOutput(signalLine)
	if len(lines3) != 0 {
		t.Errorf("third duplicate signal should be suppressed, got %d lines: %v", len(lines3), lines3)
	}
}

// Verifies that different signal lines are NOT suppressed — only exact
// consecutive duplicates are filtered.
func TestFormatOutput_DifferentSignalsNotSuppressed(t *testing.T) {
	f := &StreamFormatter{}

	lines1 := f.FormatOutput(`[Bash] echo "task A" > /path/.signal_current_task`)
	if len(lines1) != 1 {
		t.Fatalf("first signal: expected 1 line, got %d", len(lines1))
	}

	lines2 := f.FormatOutput(`[Bash] echo "task B" > /path/.signal_current_task`)
	if len(lines2) != 1 {
		t.Fatalf("different signal: expected 1 line, got %d", len(lines2))
	}
}

// Verifies that when workDir is set, absolute paths in tool use lines are
// shortened to relative paths (strips the worktree prefix).
func TestFormatOutput_ShortensAbsolutePaths(t *testing.T) {
	f := &StreamFormatter{
		workDir: "/Users/daniel/Developer/ralph/.ralph/worktrees/ralph-20260323-01",
	}

	lines := f.FormatOutput("[Edit] /Users/daniel/Developer/ralph/.ralph/worktrees/ralph-20260323-01/go/internal/claude/claude_stream.go")

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if strings.Contains(plain, "/Users/daniel") {
		t.Errorf("should not contain absolute path, got: %q", plain)
	}
	if !strings.Contains(plain, "go/internal/claude/claude_stream.go") {
		t.Errorf("should contain relative path, got: %q", plain)
	}
}

// Verifies that path shortening also applies to non-tool prose lines
// (e.g. when Claude mentions a file path in reasoning text).
func TestFormatOutput_ShortensProsePaths(t *testing.T) {
	f := &StreamFormatter{
		workDir: "/tmp/worktree",
	}

	lines := f.FormatOutput("Reading file /tmp/worktree/src/main.go for context")

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if strings.Contains(plain, "/tmp/worktree/") {
		t.Errorf("should strip workDir prefix, got: %q", plain)
	}
	if !strings.Contains(plain, "src/main.go") {
		t.Errorf("should keep relative path, got: %q", plain)
	}
}

// Verifies that when workDir is empty, paths are left unchanged.
func TestFormatOutput_NoWorkDir_PathsUnchanged(t *testing.T) {
	f := &StreamFormatter{}

	lines := f.FormatOutput("[Edit] /absolute/path/to/file.go")

	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if !strings.Contains(plain, "/absolute/path/to/file.go") {
		t.Errorf("path should be unchanged when workDir empty, got: %q", plain)
	}
}

// Verifies that StreamFormatter strips markdown via the shared Format path,
// confirming it uses the same formatting code path as Logger.
func TestFormatOutput_StripsMarkdown(t *testing.T) {
	f := &StreamFormatter{}
	lines := f.FormatOutput("Reading **important** config")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	plain := ansiRe.ReplaceAllString(lines[0], "")
	if strings.Contains(plain, "**important**") {
		t.Error("FormatOutput should strip markdown via shared Format path")
	}
	if !strings.Contains(plain, "Reading important config") {
		t.Errorf("stripped content missing, got: %q", plain)
	}
}

// Verifies that dedup persists across intervening non-signal lines — the
// same Bash command appears in multiple Claude stream events seconds apart,
// often with non-signal content between them.
func TestFormatOutput_SignalDedupPersistsAcrossNonSignal(t *testing.T) {
	f := &StreamFormatter{}
	signalLine := `[Bash] echo "task done" > /path/.signal_complete`

	lines1 := f.FormatOutput(signalLine)
	if len(lines1) != 1 {
		t.Fatalf("first signal: expected 1 line, got %d", len(lines1))
	}

	f.FormatOutput("Reading some file")

	// Same signal again after intervening output — still suppressed.
	lines3 := f.FormatOutput(signalLine)
	if len(lines3) != 0 {
		t.Errorf("same signal after non-signal should still be suppressed, got %d lines", len(lines3))
	}
}
