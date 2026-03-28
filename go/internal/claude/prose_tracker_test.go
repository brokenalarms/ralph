package claude

import (
	"testing"
	"time"
)

// Verifies that text_delta content_block_delta events are captured as prose,
// while input_json_delta events (tool call arguments) are ignored.
func TestProseTracker_OnlyTracksTextDeltas(t *testing.T) {
	pt := NewProseTracker(60 * time.Second)

	// Text delta — should be captured.
	pt.Observe(`{"type":"content_block_delta","delta":{"text":"Let me read the configuration file to understand the setup"}}`)

	// Tool input delta — should be ignored (partial_json has no "text" field).
	pt.Observe(`{"type":"content_block_delta","delta":{"partial_json":"{\"file_path\":\"/tmp/foo.go\"}"}}`)

	got := pt.lastProse
	want := "Let me read the configuration file to understand the setup"
	if got != want {
		t.Errorf("lastProse = %q, want %q", got, want)
	}
}

// Verifies that short text fragments (< 20 chars) are not stored as the
// last prose — they're usually incomplete fragments, not meaningful status.
func TestProseTracker_IgnoresShortText(t *testing.T) {
	pt := NewProseTracker(60 * time.Second)

	pt.Observe(`{"type":"content_block_delta","delta":{"text":"I'll check the code now and look at the stream handling"}}`)
	pt.Observe(`{"type":"content_block_delta","delta":{"text":"short"}}`)

	if pt.lastProse != "I'll check the code now and look at the stream handling" {
		t.Errorf("short text should not replace lastProse, got %q", pt.lastProse)
	}
}

// Verifies that StatusLine returns empty before the interval has elapsed.
func TestProseTracker_NoStatusBeforeInterval(t *testing.T) {
	pt := NewProseTracker(60 * time.Second)
	pt.Observe(`{"type":"content_block_delta","delta":{"text":"Working on the implementation now"}}`)

	if got := pt.StatusLine(); got != "" {
		t.Errorf("StatusLine should be empty before interval, got %q", got)
	}
}

// Verifies that StatusLine emits a [thinking] line after the interval.
func TestProseTracker_EmitsAfterInterval(t *testing.T) {
	pt := NewProseTracker(60 * time.Second)
	pt.lastEmit = time.Now().Add(-61 * time.Second)
	pt.Observe(`{"type":"content_block_delta","delta":{"text":"Analyzing the test failures to find the root cause"}}`)

	got := pt.StatusLine()
	if got == "" {
		t.Fatal("StatusLine should return text after interval")
	}
	want := "[thinking] Analyzing the test failures to find the root cause"
	if got != want {
		t.Errorf("StatusLine = %q, want %q", got, want)
	}
}

// Verifies that StatusLine resets the timer and clears prose after emitting.
func TestProseTracker_ResetsAfterEmit(t *testing.T) {
	pt := NewProseTracker(60 * time.Second)
	pt.lastEmit = time.Now().Add(-61 * time.Second)
	pt.Observe(`{"type":"content_block_delta","delta":{"text":"First pass at the implementation"}}`)

	pt.StatusLine()

	if pt.lastProse != "" {
		t.Errorf("lastProse should be cleared after emit, got %q", pt.lastProse)
	}
	if got := pt.StatusLine(); got != "" {
		t.Errorf("second StatusLine should be empty, got %q", got)
	}
}

// Verifies that non-content_block_delta events are ignored entirely.
func TestProseTracker_IgnoresOtherEvents(t *testing.T) {
	pt := NewProseTracker(60 * time.Second)

	pt.Observe(`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world this is long enough"}]}}`)
	pt.Observe(`{"type":"result","subtype":"success"}`)
	pt.Observe(`{"type":"message_start"}`)

	if pt.lastProse != "" {
		t.Errorf("non-delta events should not set lastProse, got %q", pt.lastProse)
	}
}

// Verifies that the status line is truncated to a single line for long prose.
func TestProseTracker_TruncatesLongProse(t *testing.T) {
	pt := NewProseTracker(60 * time.Second)
	pt.lastEmit = time.Now().Add(-61 * time.Second)

	long := "This is a very long line of agent reasoning that goes on and on and on and describes what the agent is thinking about in great detail which should be truncated to fit in a single terminal line"
	pt.Observe(`{"type":"content_block_delta","delta":{"text":"` + long + `"}}`)

	got := pt.StatusLine()
	runes := []rune(got)
	maxLen := maxProseStatusLen + len("[thinking] ")
	if len(runes) > maxLen+1 { // +1 for ellipsis
		t.Errorf("StatusLine too long: %d runes, max %d", len(runes), maxLen)
	}
}

// Verifies that accumulated text across multiple small deltas produces
// a coherent status line from the last substantial fragment.
func TestProseTracker_AccumulatesFragments(t *testing.T) {
	pt := NewProseTracker(60 * time.Second)
	pt.lastEmit = time.Now().Add(-61 * time.Second)

	// Simulate small streaming fragments that together form a sentence.
	pt.Observe(`{"type":"content_block_delta","delta":{"text":"I need to "}}`)
	pt.Observe(`{"type":"content_block_delta","delta":{"text":"check the configuration "}}`)
	pt.Observe(`{"type":"content_block_delta","delta":{"text":"and verify the stream format"}}`)

	got := pt.StatusLine()
	if got == "" {
		t.Fatal("StatusLine should return accumulated text")
	}
	// The accumulated buffer should produce a meaningful line.
	want := "[thinking] I need to check the configuration and verify the stream format"
	if got != want {
		t.Errorf("StatusLine = %q, want %q", got, want)
	}
}

// Verifies that the real Claude Code wire format (no "type" inside delta)
// is captured correctly. This is the regression test for ralph-rioy: the
// original code checked delta.type == "text_delta" which never matched
// because Claude Code's stream-json omits that field.
func TestProseTracker_RealWireFormat(t *testing.T) {
	pt := NewProseTracker(60 * time.Second)
	pt.lastEmit = time.Now().Add(-61 * time.Second)

	// Real Claude Code format: no "type" inside delta, just "text".
	pt.Observe(`{"type":"content_block_delta","delta":{"text":"Reading the configuration to understand the current setup"}}`)

	got := pt.StatusLine()
	if got == "" {
		t.Fatal("StatusLine should capture text from real wire format (no delta.type field)")
	}
	if got != "[thinking] Reading the configuration to understand the current setup" {
		t.Errorf("StatusLine = %q, want [thinking] prefix with captured text", got)
	}
}

// Verifies that [thinking] tag gets dim color in formatted output.
func TestColorTag_Thinking(t *testing.T) {
	tag := colorTag("[thinking]")
	if tag != "\033[2;37m[thinking]\033[0m" {
		t.Errorf("colorTag([thinking]) should use Dim, got %q", tag)
	}
}
