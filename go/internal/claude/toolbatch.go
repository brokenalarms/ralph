package claude

import (
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var batchableToolRe = regexp.MustCompile(`^\[(Read|Grep|Glob)\] (.+)$`)

// ToolBatcher accumulates read-only tool calls (Read, Grep, Glob) within a
// time window and emits them as collapsed summary lines. Edit, Bash, and
// text output pass through immediately, flushing any pending batch.
type ToolBatcher struct {
	order       []string
	batches     map[string][]string
	windowStart time.Time
	window      time.Duration
}

// NewToolBatcher creates a batcher with the given window duration.
// Tool calls arriving within a window are collapsed into one summary line
// per tool type.
func NewToolBatcher(window time.Duration) *ToolBatcher {
	return &ToolBatcher{
		batches: make(map[string][]string),
		window:  window,
	}
}

// ProcessLine takes raw extracted stream text (e.g. "[Read] /path/file.go"
// or "some reasoning text") and returns formatted output lines. Batchable
// tool calls may return nothing (accumulated), while non-batchable content
// flushes any pending batch before passing through.
func (b *ToolBatcher) ProcessLine(text string) []string {
	var flushed []string
	if len(b.batches) > 0 && !b.windowStart.IsZero() && time.Since(b.windowStart) >= b.window {
		flushed = b.flush()
	}

	m := batchableToolRe.FindStringSubmatch(text)
	if m != nil {
		tool, arg := m[1], m[2]
		if len(b.batches) == 0 {
			b.windowStart = time.Now()
		}
		if tool == "Read" {
			arg = filepath.Base(arg)
		}
		if _, ok := b.batches[tool]; !ok {
			b.order = append(b.order, tool)
		}
		b.batches[tool] = append(b.batches[tool], arg)
		return flushed
	}

	flushed = append(flushed, b.flush()...)
	flushed = append(flushed, FormatStreamOutput(text)...)
	return flushed
}

// Flush emits any pending batched tool calls as summary lines.
func (b *ToolBatcher) Flush() []string {
	return b.flush()
}

// FlushIfExpired emits pending batches only if the window has elapsed.
func (b *ToolBatcher) FlushIfExpired() []string {
	if len(b.batches) > 0 && !b.windowStart.IsZero() && time.Since(b.windowStart) >= b.window {
		return b.flush()
	}
	return nil
}

func (b *ToolBatcher) flush() []string {
	if len(b.batches) == 0 {
		return nil
	}
	var lines []string
	for _, tool := range b.order {
		args := b.batches[tool]
		summary := "[" + tool + "] " + strings.Join(args, ", ")
		lines = append(lines, FormatStreamOutput(summary)...)
	}
	b.batches = make(map[string][]string)
	b.order = nil
	b.windowStart = time.Time{}
	return lines
}
