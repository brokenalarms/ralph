package logging

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// Verifies that LineFormatter places timestamps at the front of lines
// and pads with spaces for same-second lines.
func TestLineFormatter_TimestampAtFront(t *testing.T) {
	fixed := time.Date(2026, 3, 25, 14, 30, 45, 0, time.UTC)
	f := &LineFormatter{Clock: func() time.Time { return fixed }}

	line := f.FormatLine("[o] hello")
	plain := stripANSI(line)
	if !strings.HasPrefix(plain, "14:30:45 ") {
		t.Errorf("first line should start with timestamp, got: %q", plain)
	}
	if !strings.Contains(plain, "[o] hello") {
		t.Errorf("line should contain content, got: %q", plain)
	}

	// Same second: padding instead of timestamp.
	line2 := f.FormatLine("[o] world")
	plain2 := stripANSI(line2)
	if !strings.HasPrefix(plain2, "         ") {
		t.Errorf("same-second should start with 9 spaces, got: %q", plain2)
	}
	if strings.Contains(plain2, "14:30:45") {
		t.Errorf("same-second should not contain timestamp, got: %q", plain2)
	}

	// Different second: timestamp again.
	next := fixed.Add(time.Second)
	f.Clock = func() time.Time { return next }
	line3 := f.FormatLine("[o] next")
	plain3 := stripANSI(line3)
	if !strings.HasPrefix(plain3, "14:30:46 ") {
		t.Errorf("new-second should start with timestamp, got: %q", plain3)
	}
}

// Verifies that the timestamp is dim (ANSI dim code) and followed by reset.
func TestLineFormatter_TimestampIsDim(t *testing.T) {
	fixed := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	f := &LineFormatter{Clock: func() time.Time { return fixed }}

	line := f.FormatLine("content")
	if !strings.HasPrefix(line, Dim+"10:00:00"+Reset+" ") {
		t.Errorf("timestamp should be dim with reset, got prefix: %q", line[:min(len(line), 40)])
	}
}

// Verifies that TSWidth matches the actual visible width of the timestamp prefix.
func TestLineFormatter_TSWidth(t *testing.T) {
	fixed := time.Date(2026, 3, 25, 14, 30, 45, 0, time.UTC)
	f := &LineFormatter{Clock: func() time.Time { return fixed }}

	line := f.FormatLine("x")
	plain := stripANSI(line)
	// "14:30:45 x" — timestamp is 8 chars + 1 space = 9 before content.
	idx := strings.Index(plain, "x")
	if idx != TSWidth {
		t.Errorf("content starts at position %d, want %d (TSWidth)", idx, TSWidth)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
