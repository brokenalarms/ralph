package claude

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var rateLimitRe = regexp.MustCompile(`(?i)hit your limit`)
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
// and checks for rate limit messages.
func ScanRawLogForRateLimit(logContent string, now time.Time) (resetAt time.Time, found bool) {
	lines := strings.Split(logContent, "\n")
	// Check last 50 lines — the rate limit message appears near the end
	start := len(lines) - 50
	if start < 0 {
		start = 0
	}
	for _, line := range lines[start:] {
		text := extractStreamText(line)
		if text == "" {
			continue
		}
		if resetAt, found := ParseRateLimitReset(text, now); found {
			return resetAt, true
		}
	}
	// Also check raw lines (non-JSON stderr output from Claude CLI)
	for _, line := range lines[start:] {
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
