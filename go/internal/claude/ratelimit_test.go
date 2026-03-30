package claude

import (
	"encoding/json"
	"testing"
	"time"
)

// Verify that "hit your limit · resets 12am" is detected and parsed to
// midnight (next day if currently past midnight).
func TestParseRateLimitReset_Midnight(t *testing.T) {
	now := time.Date(2026, 3, 24, 22, 30, 0, 0, time.UTC)
	text := "You've hit your limit · resets 12am"
	resetAt, found := ParseRateLimitReset(text, now)
	if !found {
		t.Fatal("expected rate limit to be detected")
	}
	expected := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)
	if !resetAt.Equal(expected) {
		t.Fatalf("expected reset at %v, got %v", expected, resetAt)
	}
}

// Verify afternoon reset times like "resets 3pm" parse correctly.
func TestParseRateLimitReset_Afternoon(t *testing.T) {
	now := time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC)
	text := "You've hit your limit · resets 3pm"
	resetAt, found := ParseRateLimitReset(text, now)
	if !found {
		t.Fatal("expected rate limit to be detected")
	}
	expected := time.Date(2026, 3, 24, 15, 0, 0, 0, time.UTC)
	if !resetAt.Equal(expected) {
		t.Fatalf("expected reset at %v, got %v", expected, resetAt)
	}
}

// Verify that if the reset hour has already passed today, it wraps to tomorrow.
func TestParseRateLimitReset_WrapToNextDay(t *testing.T) {
	now := time.Date(2026, 3, 24, 16, 0, 0, 0, time.UTC)
	text := "You've hit your limit · resets 3pm"
	resetAt, found := ParseRateLimitReset(text, now)
	if !found {
		t.Fatal("expected rate limit to be detected")
	}
	expected := time.Date(2026, 3, 25, 15, 0, 0, 0, time.UTC)
	if !resetAt.Equal(expected) {
		t.Fatalf("expected reset at %v (next day), got %v", expected, resetAt)
	}
}

// Verify case-insensitive matching of the rate limit message.
func TestParseRateLimitReset_CaseInsensitive(t *testing.T) {
	now := time.Date(2026, 3, 24, 22, 0, 0, 0, time.UTC)
	text := "you've HIT YOUR LIMIT · Resets 1AM"
	_, found := ParseRateLimitReset(text, now)
	if !found {
		t.Fatal("expected case-insensitive match")
	}
}

// Verify that normal text without rate limit keywords returns false.
func TestParseRateLimitReset_NoMatch(t *testing.T) {
	now := time.Date(2026, 3, 24, 22, 0, 0, 0, time.UTC)
	_, found := ParseRateLimitReset("Claude completed successfully", now)
	if found {
		t.Fatal("expected no match on normal text")
	}
}

// Verify that "hit your limit" without a parseable reset time falls back
// to the next hour boundary.
func TestParseRateLimitReset_NoResetTime(t *testing.T) {
	now := time.Date(2026, 3, 24, 22, 45, 0, 0, time.UTC)
	text := "You've hit your limit"
	resetAt, found := ParseRateLimitReset(text, now)
	if !found {
		t.Fatal("expected rate limit to be detected even without reset time")
	}
	expected := time.Date(2026, 3, 24, 23, 0, 0, 0, time.UTC)
	if !resetAt.Equal(expected) {
		t.Fatalf("expected fallback to next hour %v, got %v", expected, resetAt)
	}
}

// Verify ScanRawLogForRateLimit detects rate limit in stream-json format.
func TestScanRawLogForRateLimit_JSON(t *testing.T) {
	now := time.Date(2026, 3, 24, 22, 0, 0, 0, time.UTC)

	ev := streamEvent{
		Type:    "result",
		Subtype: "error_response",
		Error:   "You've hit your limit · resets 12am",
	}
	jsonLine, _ := json.Marshal(ev)
	logContent := "some earlier output\n" + string(jsonLine) + "\n"

	resetAt, found := ScanRawLogForRateLimit(logContent, now)
	if !found {
		t.Fatal("expected rate limit detection in JSON log")
	}
	expected := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)
	if !resetAt.Equal(expected) {
		t.Fatalf("expected %v, got %v", expected, resetAt)
	}
}

// Verify ScanRawLogForRateLimit detects rate limit in plain text (stderr).
func TestScanRawLogForRateLimit_PlainText(t *testing.T) {
	now := time.Date(2026, 3, 24, 22, 0, 0, 0, time.UTC)
	logContent := "some output\nYou've hit your limit · resets 12am\n"

	resetAt, found := ScanRawLogForRateLimit(logContent, now)
	if !found {
		t.Fatal("expected rate limit detection in plain text")
	}
	expected := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)
	if !resetAt.Equal(expected) {
		t.Fatalf("expected %v, got %v", expected, resetAt)
	}
}

// Verify FormatWaitDuration produces human-readable output.
func TestFormatWaitDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h30m"},
		{2*time.Hour + 5*time.Minute, "2h05m"},
	}
	for _, tt := range tests {
		got := FormatWaitDuration(tt.d)
		if got != tt.want {
			t.Errorf("FormatWaitDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// Verify to24Hour converts 12-hour clock to 24-hour correctly.
func TestTo24Hour(t *testing.T) {
	tests := []struct {
		hour int
		ampm string
		want int
	}{
		{12, "am", 0},
		{1, "am", 1},
		{11, "am", 11},
		{12, "pm", 12},
		{1, "pm", 13},
		{11, "pm", 23},
	}
	for _, tt := range tests {
		got := to24Hour(tt.hour, tt.ampm)
		if got != tt.want {
			t.Errorf("to24Hour(%d, %q) = %d, want %d", tt.hour, tt.ampm, got, tt.want)
		}
	}
}
