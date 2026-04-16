package claude

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var rateLimitRe = regexp.MustCompile(`(?i)hit your limit`)

type rateLimitEventPayload struct {
	Type          string `json:"type"`
	RateLimitInfo struct {
		Status        string  `json:"status"`
		ResetsAt      int64   `json:"resetsAt"`
		RateLimitType string  `json:"rateLimitType"`
		Utilization   float64 `json:"utilization"`
	} `json:"rate_limit_info"`
}

// ParseRateLimitEvent parses a single raw log line as a rate_limit_event JSON
// event. Returns ok=false for malformed JSON or non-rate_limit_event lines.
// Throttled statuses (throttled, exceeded, blocked) set throttled=true.
// allowed_warning sets warning=true. allowed and unknown statuses return
// ok=true with both flags false (safe no-op).
func ParseRateLimitEvent(line string) (resetAt time.Time, throttled bool, warning bool, rateLimitType string, utilization float64, ok bool) {
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var ev rateLimitEventPayload
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return
	}
	if ev.Type != "rate_limit_event" {
		return
	}
	ok = true
	info := ev.RateLimitInfo
	resetAt = time.Unix(info.ResetsAt, 0)
	rateLimitType = info.RateLimitType
	utilization = info.Utilization
	switch info.Status {
	case "throttled", "exceeded", "blocked":
		throttled = true
	case "allowed_warning":
		warning = true
	}
	return
}
var resetTimeRe = regexp.MustCompile(`(?i)resets?\s+(\d{1,2})\s*(am|pm)`)

// ParseRateLimitReset scans text for Claude's rate limit message and
// returns the parsed reset time. The message format is:
// "You've hit your limit · resets 12am"
func ParseRateLimitReset(text string, now time.Time) (resetAt time.Time, found bool) {
	if !rateLimitRe.MatchString(text) {
		return time.Time{}, false
	}

	m := resetTimeRe.FindStringSubmatch(text)
	if m == nil {
		return nextHourBoundary(now), true
	}

	hour, _ := strconv.Atoi(m[1])
	ampm := strings.ToLower(m[2])
	resetHour := to24Hour(hour, ampm)

	resetAt = time.Date(now.Year(), now.Month(), now.Day(), resetHour, 0, 0, 0, now.Location())
	if !resetAt.After(now) {
		resetAt = resetAt.Add(24 * time.Hour)
	}
	return resetAt, true
}

func to24Hour(hour int, ampm string) int {
	if ampm == "am" {
		if hour == 12 {
			return 0
		}
		return hour
	}
	if hour == 12 {
		return 12
	}
	return hour + 12
}

func nextHourBoundary(now time.Time) time.Time {
	return now.Truncate(time.Hour).Add(time.Hour)
}

// ScanRawLogForRateLimit reads the last portion of a raw log file's content
// and checks for rate limit messages. JSON rate_limit_event entries are checked
// first; plaintext 'hit your limit' regex is the fallback.
func ScanRawLogForRateLimit(logContent string, now time.Time) (resetAt time.Time, found bool) {
	lines := strings.Split(logContent, "\n")
	// Check last 50 lines — the rate limit message appears near the end
	start := len(lines) - 50
	if start < 0 {
		start = 0
	}
	tail := lines[start:]

	// JSON rate_limit_event scan first.
	for _, line := range tail {
		resetAt, throttled, _, _, _, ok := ParseRateLimitEvent(line)
		if ok && throttled {
			return resetAt, true
		}
	}

	// Plaintext / stream-json text extraction fallback.
	for _, line := range tail {
		text := extractStreamText(line)
		if text == "" {
			continue
		}
		if resetAt, found := ParseRateLimitReset(text, now); found {
			return resetAt, true
		}
	}
	// Also check raw lines (non-JSON stderr output from Claude CLI)
	for _, line := range tail {
		if resetAt, found := ParseRateLimitReset(line, now); found {
			return resetAt, true
		}
	}
	return time.Time{}, false
}

// FormatWaitDuration returns a human-readable duration string like "2h15m".
func FormatWaitDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
