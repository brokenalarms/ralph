package claude

import (
	"strings"
	"testing"
	"time"
)

// strip ANSI codes for assertions
func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// Batchable tool calls are not emitted immediately — they accumulate
// until flushed by non-batchable content or window expiry.
func TestToolBatcher_AccumulatesReadOnlyTools(t *testing.T) {
	b := NewToolBatcher(5 * time.Second)
	out := b.ProcessLine("[Read] /some/path/loop.go")
	if len(out) != 0 {
		t.Errorf("batchable tool should not emit immediately, got %v", out)
	}
	out = b.ProcessLine("[Grep] checklist")
	if len(out) != 0 {
		t.Errorf("second batchable tool should not emit immediately, got %v", out)
	}
}

// Non-batchable text flushes the accumulated batch before emitting itself.
func TestToolBatcher_FlushesOnText(t *testing.T) {
	b := NewToolBatcher(5 * time.Second)
	b.ProcessLine("[Read] /path/to/loop.go")
	b.ProcessLine("[Read] /path/to/git.go")
	b.ProcessLine("[Grep] checklist")

	out := b.ProcessLine("Now analyzing the code")
	// Should have: [Read] summary, [Grep] summary, text line = 3 lines
	if len(out) != 3 {
		t.Fatalf("expected 3 output lines, got %d: %v", len(out), out)
	}
	plain0 := stripANSI(out[0])
	if !strings.Contains(plain0, "[Read] loop.go, git.go") {
		t.Errorf("Read summary should have basenames, got: %s", plain0)
	}
	plain1 := stripANSI(out[1])
	if !strings.Contains(plain1, "[Grep] checklist") {
		t.Errorf("Grep summary expected, got: %s", plain1)
	}
	plain2 := stripANSI(out[2])
	if !strings.Contains(plain2, "[agent] Now analyzing the code") {
		t.Errorf("text should pass through with [agent] prefix, got: %s", plain2)
	}
}

// Read and Glob args show just the filename, not the full path.
func TestToolBatcher_ReadGlobBasename(t *testing.T) {
	b := NewToolBatcher(5 * time.Second)
	b.ProcessLine("[Read] /very/long/path/to/config.go")
	b.ProcessLine("[Glob] /some/dir/**/*.ts")

	out := b.Flush()
	if len(out) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(out), out)
	}
	plain0 := stripANSI(out[0])
	if !strings.Contains(plain0, "[Read] config.go") {
		t.Errorf("Read should show basename, got: %s", plain0)
	}
	plain1 := stripANSI(out[1])
	if !strings.Contains(plain1, "[Glob] /some/dir/**/*.ts") {
		t.Errorf("Glob should keep pattern as-is, got: %s", plain1)
	}
}

// Grep args pass through as-is (they're search patterns, not paths).
func TestToolBatcher_GrepKeepsPattern(t *testing.T) {
	b := NewToolBatcher(5 * time.Second)
	b.ProcessLine("[Grep] checklist_")
	b.ProcessLine("[Grep] TASK_BACKEND")

	out := b.Flush()
	if len(out) != 1 {
		t.Fatalf("expected 1 Grep summary line, got %d", len(out))
	}
	plain := stripANSI(out[0])
	if !strings.Contains(plain, "[Grep] checklist_, TASK_BACKEND") {
		t.Errorf("Grep args should be comma-separated, got: %s", plain)
	}
}

// Edit, Bash, and other non-batchable tools pass through immediately.
func TestToolBatcher_EditPassesThrough(t *testing.T) {
	b := NewToolBatcher(5 * time.Second)
	out := b.ProcessLine("[Edit] /path/to/file.go")
	if len(out) != 1 {
		t.Fatalf("Edit should pass through immediately, got %d lines", len(out))
	}
	plain := stripANSI(out[0])
	if !strings.Contains(plain, "[Edit] /path/to/file.go") {
		t.Errorf("Edit line should be unmodified, got: %s", plain)
	}
}

// Bash commands pass through immediately, flushing any pending batch.
func TestToolBatcher_BashFlushesAndPassesThrough(t *testing.T) {
	b := NewToolBatcher(5 * time.Second)
	b.ProcessLine("[Read] /path/file.go")
	out := b.ProcessLine("[Bash] go test ./...")
	// Should have: Read flush + Bash passthrough
	if len(out) != 2 {
		t.Fatalf("expected 2 lines (flush + bash), got %d: %v", len(out), out)
	}
	plain0 := stripANSI(out[0])
	if !strings.Contains(plain0, "[Read] file.go") {
		t.Errorf("batch should flush first, got: %s", plain0)
	}
	plain1 := stripANSI(out[1])
	if !strings.Contains(plain1, "[Bash] go test ./...") {
		t.Errorf("Bash should pass through, got: %s", plain1)
	}
}

// Different tool types within a batch get separate summary lines.
func TestToolBatcher_SeparateLinePerToolType(t *testing.T) {
	b := NewToolBatcher(5 * time.Second)
	b.ProcessLine("[Read] /path/a.go")
	b.ProcessLine("[Grep] pattern1")
	b.ProcessLine("[Glob] /dir/**/*.go")
	b.ProcessLine("[Read] /path/b.go")

	out := b.Flush()
	if len(out) != 3 {
		t.Fatalf("expected 3 lines (Read, Grep, Glob), got %d: %v", len(out), out)
	}
	plain0 := stripANSI(out[0])
	if !strings.Contains(plain0, "[Read] a.go, b.go") {
		t.Errorf("Read should group both files, got: %s", plain0)
	}
	plain1 := stripANSI(out[1])
	if !strings.Contains(plain1, "[Grep] pattern1") {
		t.Errorf("Grep should be on its own line, got: %s", plain1)
	}
	plain2 := stripANSI(out[2])
	if !strings.Contains(plain2, "[Glob] /dir/**/*.go") {
		t.Errorf("Glob should be on its own line, got: %s", plain2)
	}
}

// FlushIfExpired only emits when the window has elapsed.
func TestToolBatcher_FlushIfExpired(t *testing.T) {
	b := NewToolBatcher(50 * time.Millisecond)
	b.ProcessLine("[Read] /path/file.go")

	// Immediately — should not flush.
	out := b.FlushIfExpired()
	if len(out) != 0 {
		t.Errorf("should not flush before window expires, got %v", out)
	}

	time.Sleep(60 * time.Millisecond)
	out = b.FlushIfExpired()
	if len(out) == 0 {
		t.Error("should flush after window expires")
	}
	plain := stripANSI(out[0])
	if !strings.Contains(plain, "[Read] file.go") {
		t.Errorf("flushed line should contain batched Read, got: %s", plain)
	}
}

// Flush on empty batcher returns nil.
func TestToolBatcher_FlushEmpty(t *testing.T) {
	b := NewToolBatcher(5 * time.Second)
	out := b.Flush()
	if out != nil {
		t.Errorf("flush on empty batcher should return nil, got %v", out)
	}
}

// Window expiry during ProcessLine flushes old batch before accumulating new tool.
func TestToolBatcher_WindowExpiryDuringProcess(t *testing.T) {
	b := NewToolBatcher(50 * time.Millisecond)
	b.ProcessLine("[Read] /path/old.go")

	time.Sleep(60 * time.Millisecond)

	out := b.ProcessLine("[Read] /path/new.go")
	// Should flush old batch
	if len(out) != 1 {
		t.Fatalf("expected 1 flushed line, got %d: %v", len(out), out)
	}
	plain := stripANSI(out[0])
	if !strings.Contains(plain, "[Read] old.go") {
		t.Errorf("should flush old batch, got: %s", plain)
	}

	// new.go should still be pending
	out = b.Flush()
	if len(out) != 1 {
		t.Fatalf("expected 1 pending line, got %d", len(out))
	}
	plain = stripANSI(out[0])
	if !strings.Contains(plain, "[Read] new.go") {
		t.Errorf("new tool should be in next batch, got: %s", plain)
	}
}
