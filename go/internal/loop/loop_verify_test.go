package loop

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/verify"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies the extracted onSignal method returns true when both test
// verification and LLM verification pass on the first attempt.
func TestOnSignal_HappyPath(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	// Create prompts dir so loadVerifyPrompt doesn't fail
	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

	logger := logging.New(nil)

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   &stubBackend{remaining: 1, total: 1, description: "test task"},
		VerifyDir:     dir,
	}, st, gm, logger)

	// Stub LLM verify to pass
	l.llmVerifyFunc = func(ctx context.Context, workDir, promptsDir, taskID, headBefore, beadTitle, beadDescription string, gh git.GitHub, queryFn verify.QueryFunc, model ...string) verify.Result {
		return verify.Result{Passed: true, Reason: "looks good"}
	}

	// Stub verify to pass (test suite)
	l.verifyFunc = func(ctx context.Context, dir, headBefore string) (bool, string) {
		return true, ""
	}

	params := signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "test-123",
		nextTask:   "Test task",
	}

	result := l.onSignal(params)
	if !result {
		t.Fatal("expected onSignal to return true when verification passes")
	}
}

// Verifies the extracted onSignal method returns false when LLM verification
// rejects the work and both the fix agent and Sonnet escalation also reject.
func TestOnSignal_LLMReject_SkipsTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	promptsDir := filepath.Join(dir, "prompts")
	os.MkdirAll(promptsDir, 0o755)

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

	logger := logging.New(nil)

	backend := &stubBackend{remaining: 1, total: 1, description: "test task"}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		TaskBackend:   backend,
		VerifyDir:     dir,
	}, st, gm, logger)

	// LLM verify rejects on both attempts (Haiku then Sonnet escalation)
	llmCalls := 0
	l.llmVerifyFunc = func(ctx context.Context, workDir, promptsDir, taskID, headBefore, beadTitle, beadDescription string, gh git.GitHub, queryFn verify.QueryFunc, model ...string) verify.Result {
		llmCalls++
		return verify.Result{Passed: false, Details: "diff doesn't match bead"}
	}

	// Fix agent signals completion (so escalation proceeds)
	l.newRunnerFunc = func() claudeRunner {
		return &stubRunner{result: stubResult(true, "fixed")}
	}

	params := signalParams{
		ctx:        context.Background(),
		headBefore: "abc123",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "test-123",
		nextTask:   "Test task",
	}

	result := l.onSignal(params)
	if result {
		t.Fatal("expected onSignal to return false when LLM verification rejects twice")
	}
	if llmCalls != 2 {
		t.Fatalf("expected 2 LLM verify calls (Haiku + Sonnet), got %d", llmCalls)
	}
}

func stubResult(signal bool, summary string) claude.Result {
	return claude.Result{
		SignalDetected: signal,
		Summary:        summary,
	}
}
