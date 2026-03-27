package claude

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	minProseLen       = 20
	maxProseStatusLen = 100
)

// ProseTracker captures the agent's most recent reasoning text from
// content_block_delta stream events (text_delta only, not tool input).
// Periodically returns a status line showing what the agent is thinking.
type ProseTracker struct {
	lastProse string
	buf       strings.Builder
	lastEmit  time.Time
	interval  time.Duration
}

func NewProseTracker(interval time.Duration) *ProseTracker {
	return &ProseTracker{
		interval: interval,
		lastEmit: time.Now(),
	}
}

// Observe parses a raw stream-json line. If it's a text_delta
// content_block_delta, the text is accumulated and the last fragment
// longer than minProseLen is stored as the current reasoning status.
func (p *ProseTracker) Observe(rawLine string) {
	if len(rawLine) == 0 || rawLine[0] != '{' {
		return
	}

	var ev struct {
		Type  string `json:"type"`
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal([]byte(rawLine), &ev); err != nil {
		return
	}

	if ev.Type != "content_block_delta" || ev.Delta == nil || ev.Delta.Type != "text_delta" {
		return
	}

	text := ev.Delta.Text
	p.buf.WriteString(text)

	if len([]rune(text)) >= minProseLen {
		p.lastProse = strings.TrimSpace(text)
	}
}

// StatusLine returns a formatted status line if the interval has elapsed
// and there is meaningful prose to show. Resets state after emitting.
func (p *ProseTracker) StatusLine() string {
	if time.Since(p.lastEmit) < p.interval {
		return ""
	}

	// Prefer the accumulated buffer over the last single fragment.
	text := strings.TrimSpace(p.buf.String())
	if len([]rune(text)) < minProseLen {
		text = p.lastProse
	}

	if text == "" {
		return ""
	}

	// Take the last meaningful chunk from the buffer.
	text = lastMeaningfulChunk(text)

	if len([]rune(text)) > maxProseStatusLen {
		text = string([]rune(text)[:maxProseStatusLen-1]) + "…"
	}

	p.lastProse = ""
	p.buf.Reset()
	p.lastEmit = time.Now()
	return "[thinking] " + text
}

// lastMeaningfulChunk returns the trailing portion of text, trimmed to the
// last sentence boundary or the last maxProseStatusLen*2 chars.
func lastMeaningfulChunk(text string) string {
	runes := []rune(text)
	if len(runes) > maxProseStatusLen*2 {
		text = string(runes[len(runes)-maxProseStatusLen*2:])
	}

	// Find the last sentence-ending punctuation followed by a space.
	for _, sep := range []string{". ", ".\n", "? ", "!\n"} {
		if idx := strings.LastIndex(text, sep); idx >= 0 {
			after := strings.TrimSpace(text[idx+len(sep):])
			if len([]rune(after)) >= minProseLen {
				return after
			}
		}
	}

	return text
}
