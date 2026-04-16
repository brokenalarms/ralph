package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Proves: renderAttemptHistory returns empty string for an empty event slice.
func TestRenderAttemptHistory_EmptyReturnsEmpty(t *testing.T) {
	if got := renderAttemptHistory(nil, 3); got != "" {
		t.Errorf("expected empty string for nil events, got %q", got)
	}
	if got := renderAttemptHistory([]AttemptEvent{}, 3); got != "" {
		t.Errorf("expected empty string for empty slice, got %q", got)
	}
}

// Proves: renderAttemptHistory formats events with sequential ### Attempt headers.
func TestRenderAttemptHistory_FormatsEvents(t *testing.T) {
	events := []AttemptEvent{
		{Summary: "first try", DiffStat: "2 files", Analysis: "keep going"},
		{Summary: "second try", DiffStat: "", Analysis: "still failing"},
	}
	got := renderAttemptHistory(events, 0)
	if !strings.Contains(got, "### Attempt 1") {
		t.Error("expected '### Attempt 1' header")
	}
	if !strings.Contains(got, "### Attempt 2") {
		t.Error("expected '### Attempt 2' header")
	}
	if !strings.Contains(got, "Changes: none") {
		t.Error("expected empty DiffStat to render as 'none'")
	}
}

// Proves: renderAttemptHistory caps output at maxAttempts (returns only the tail).
func TestRenderAttemptHistory_CapsAtMaxAttempts(t *testing.T) {
	events := make([]AttemptEvent, 5)
	for i := range events {
		events[i] = AttemptEvent{Summary: "try", Analysis: "continue"}
	}
	got := renderAttemptHistory(events, 3)
	count := strings.Count(got, "### Attempt ")
	if count != 3 {
		t.Errorf("expected 3 attempt headers (capped), got %d", count)
	}
}

// Proves AC 6: in-memory attempts are rendered and passed to a retry agent within
// the same iteration. After a simulated re-exec (new Loop instance), the next
// iteration's initial agent sees no prior-attempt context.
func TestLoop_InMemoryAttemptsNotPresentAfterReexec(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)

	// First iteration: record an in-memory attempt
	l1 := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  &testutil.StubBackend{},
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l1.currentTaskID = "ralph-abc"
	l1.taskAttempts = append(l1.taskAttempts, AttemptEvent{
		Summary:  "first iteration failed",
		Analysis: "verification_failed",
	})

	// Verify the attempt context renders the in-memory event for a retry
	ctx1 := l1.attemptContext("ralph-abc", "Fix bug")
	if !strings.Contains(ctx1, "### Attempt 1") {
		t.Error("expected in-memory attempt to appear in attempt context for retry within same iteration")
	}

	// Simulate re-exec: new Loop instance for the same task
	_, st2 := setupTestDir(t)
	l2 := New(cfg, Modules{
		State:        st2,
		Git:          gm,
		TaskBackend:  &testutil.StubBackend{},
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	// New instance starts with empty taskAttempts — no cross-iteration memory
	ctx2 := l2.attemptContext("ralph-abc", "Fix bug")
	if strings.Contains(ctx2, "### Attempt") {
		t.Error("new Loop instance after re-exec should see no prior in-memory attempt context")
	}
}

// Proves AC 7: pre-existing .ralph/attempts/<id>.log files on disk do not
// affect ralph behavior — they are neither read nor cause errors.
func TestLoop_DiskAttemptFilesIgnored(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	// Create a legacy disk attempt file as would exist from an older ralph version
	attemptsDir := filepath.Join(ralphDir, "attempts")
	if err := os.MkdirAll(attemptsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	legacyContent := "### Attempt 1\nSummary: old attempt from legacy disk subsystem\nChanges: none\nAnalysis: old_analysis\n\n"
	if err := os.WriteFile(filepath.Join(attemptsDir, "ralph-abc.log"), []byte(legacyContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	gm := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir})
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  &testutil.StubBackend{},
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	// Attempt context should NOT include legacy disk content
	ctx := l.attemptContext("ralph-abc", "Fix bug")
	if strings.Contains(ctx, "old attempt from legacy disk subsystem") {
		t.Error("legacy disk attempt file should not affect ralph behavior")
	}
	if strings.Contains(ctx, "old_analysis") {
		t.Error("legacy disk attempt file content should not appear in attempt context")
	}

	// Disk file must still exist (ralph does not delete legacy files)
	if _, err := os.Stat(filepath.Join(attemptsDir, "ralph-abc.log")); os.IsNotExist(err) {
		t.Error("ralph should not delete legacy disk attempt files")
	}
}
