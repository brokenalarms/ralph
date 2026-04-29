package claude

import (
	"encoding/json"
	"testing"
	"time"
)

// Verify allowed_warning parses as warning=true, throttled=false, with utilization and type.
func TestParseRateLimitEvent_AllowedWarning(t *testing.T) {
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1776412800,"rateLimitType":"seven_day","utilization":0.84,"isUsingOverage":false,"surpassedThreshold":0.75}}`
	resetAt, throttled, warning, _, rateLimitType, utilization, ok := ParseRateLimitEvent(line)
	if !ok {
		t.Fatal("expected ok=true for allowed_warning")
	}
	if throttled {
		t.Error("expected throttled=false for allowed_warning")
	}
	if !warning {
		t.Error("expected warning=true for allowed_warning")
	}
	if rateLimitType != "seven_day" {
		t.Errorf("expected rateLimitType=seven_day, got %q", rateLimitType)
	}
	if utilization != 0.84 {
		t.Errorf("expected utilization=0.84, got %f", utilization)
	}
	expected := time.Unix(1776412800, 0)
	if !resetAt.Equal(expected) {
		t.Errorf("expected resetAt=%v, got %v", expected, resetAt)
	}
}

// Verify throttled status returns throttled=true with correct resetAt.
func TestParseRateLimitEvent_Throttled(t *testing.T) {
	for _, status := range []string{"throttled", "exceeded", "blocked"} {
		line := `{"type":"rate_limit_event","rate_limit_info":{"status":"` + status + `","resetsAt":1776412800,"rateLimitType":"daily","utilization":1.0}}`
		resetAt, throttled, warning, _, _, _, ok := ParseRateLimitEvent(line)
		if !ok {
			t.Fatalf("status=%q: expected ok=true", status)
		}
		if !throttled {
			t.Errorf("status=%q: expected throttled=true", status)
		}
		if warning {
			t.Errorf("status=%q: expected warning=false", status)
		}
		expected := time.Unix(1776412800, 0)
		if !resetAt.Equal(expected) {
			t.Errorf("status=%q: expected resetAt=%v, got %v", status, expected, resetAt)
		}
	}
}

// Verify allowed status and unknown statuses return ok=true with both flags false.
func TestParseRateLimitEvent_AllowedAndUnknown(t *testing.T) {
	for _, status := range []string{"allowed", "some_future_status"} {
		line := `{"type":"rate_limit_event","rate_limit_info":{"status":"` + status + `","resetsAt":1776412800,"rateLimitType":"daily","utilization":0.5}}`
		_, throttled, warning, _, _, _, ok := ParseRateLimitEvent(line)
		if !ok {
			t.Fatalf("status=%q: expected ok=true (safe no-op)", status)
		}
		if throttled {
			t.Errorf("status=%q: expected throttled=false", status)
		}
		if warning {
			t.Errorf("status=%q: expected warning=false", status)
		}
	}
}

// Verify malformed JSON returns ok=false so caller falls through to plaintext.
func TestParseRateLimitEvent_MalformedJSON(t *testing.T) {
	_, _, _, _, _, _, ok := ParseRateLimitEvent("not json at all")
	if ok {
		t.Error("expected ok=false for non-JSON input")
	}
}

// Verify non-rate_limit_event JSON type returns ok=false.
func TestParseRateLimitEvent_WrongType(t *testing.T) {
	line := `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`
	_, _, _, _, _, _, ok := ParseRateLimitEvent(line)
	if ok {
		t.Error("expected ok=false for non-rate_limit_event JSON")
	}
}

// Verify ScanRawLogForRateLimit detects throttled JSON event and falls back to plaintext.
func TestScanRawLogForRateLimit_JSONRateLimit_Throttled(t *testing.T) {
	now := time.Unix(1776412700, 0)
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"throttled","resetsAt":1776412800,"rateLimitType":"daily","utilization":1.0}}`
	logContent := "some output\n" + line + "\n"
	resetAt, found := ScanRawLogForRateLimit(logContent, now)
	if !found {
		t.Fatal("expected rate limit detection from JSON throttled event")
	}
	expected := time.Unix(1776412800, 0)
	if !resetAt.Equal(expected) {
		t.Errorf("expected resetAt=%v, got %v", expected, resetAt)
	}
}

// Verify ScanRawLogForRateLimit still detects plaintext rate limit when no JSON event is present.
func TestScanRawLogForRateLimit_PlaintextFallback(t *testing.T) {
	now := time.Date(2026, 3, 24, 22, 0, 0, 0, time.UTC)
	logContent := "some output\nYou've hit your limit · resets 12am\n"
	resetAt, found := ScanRawLogForRateLimit(logContent, now)
	if !found {
		t.Fatal("expected plaintext rate limit to still be detected")
	}
	expected := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)
	if !resetAt.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, resetAt)
	}
}

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
