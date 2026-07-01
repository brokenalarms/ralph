package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verifier"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// stubFixCheck is a test-only fixCheck: it returns the next outcome from a
// fixed sequence (repeating the last entry once exhausted) and records how
// many times evaluate was called.
type stubFixCheck struct {
	calls    *int
	outcomes []checkOutcome
}

func (s stubFixCheck) name() string { return "stub" }

func (s stubFixCheck) evaluate(context.Context) checkOutcome {
	i := *s.calls
	*s.calls++
	if i < len(s.outcomes) {
		return s.outcomes[i]
	}
	return s.outcomes[len(s.outcomes)-1]
}

// newFixLoopTestLoop builds a minimal Loop wired with a stub git module and
// a verifier whose fix-agent spawns are counted via spawnCount.
func newFixLoopTestLoop(t *testing.T, gitCfg git.StubRepoConfig, spawnCount *int) *Loop {
	t.Helper()
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gitCfg.ProjectDir = dir
	gitCfg.WorkDir = dir
	gm := git.NewStub(gitCfg)
	logger := logging.New(nil)
	backend := &testutil.StubBackend{Remaining: 1, Total: 1}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	return New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				*spawnCount++
				return &stubRunner{result: claude.Result{SignalDetected: true}}
			},
		}),
	})
}

// Proves the baked-in per-attempt movement guard: when plan.signalTimeHead
// is no longer an ancestor of HEAD (worktree reset between attempts),
// runFixLoop aborts as an infrastructure error without ever evaluating a
// check or spawning a fix agent, and reports no skip reason so the task is
// not skipped — it retries on the next iteration instead.
func TestRunFixLoop_MovementGuard_AbortsWithoutEvaluatingOrSpawning(t *testing.T) {
	spawnCount := 0
	l := newFixLoopTestLoop(t, git.StubRepoConfig{CommitDroppedFromBranch: true}, &spawnCount)

	evalCalls := 0
	passed, skipReason := l.runFixLoop(context.Background(), fixPlan{
		checks:          []fixCheck{stubFixCheck{calls: &evalCalls, outcomes: []checkOutcome{{Passed: false, Failure: "boom"}}}},
		spawnTemplate:   "verify-tests.md",
		maxAttempts:     3,
		exhaustedFormat: "still failing after %d attempts",
		signalTimeHead:  "signal-sha",
		logDomain:       logging.Test,
	})

	if passed {
		t.Error("expected runFixLoop to fail when the movement guard trips")
	}
	if skipReason != "" {
		t.Errorf("movement-guard abort must not produce a skip reason, got %q", skipReason)
	}
	if evalCalls != 0 {
		t.Errorf("expected checks never evaluated once the guard trips, got %d calls", evalCalls)
	}
	if spawnCount != 0 {
		t.Errorf("expected no fix agent spawned once the guard trips, got %d spawns", spawnCount)
	}
}

// Proves runFixLoop returns success immediately, with no fix-agent spawn,
// when every check already passes.
func TestRunFixLoop_ChecksPass_NoSpawn(t *testing.T) {
	spawnCount := 0
	l := newFixLoopTestLoop(t, git.StubRepoConfig{}, &spawnCount)

	evalCalls := 0
	passed, skipReason := l.runFixLoop(context.Background(), fixPlan{
		checks:          []fixCheck{stubFixCheck{calls: &evalCalls, outcomes: []checkOutcome{{Passed: true}}}},
		spawnTemplate:   "verify-tests.md",
		maxAttempts:     3,
		exhaustedFormat: "still failing after %d attempts",
		logDomain:       logging.Test,
	})

	if !passed {
		t.Errorf("expected runFixLoop to pass when checks already pass, got skipReason=%q", skipReason)
	}
	if spawnCount != 0 {
		t.Errorf("expected no fix agent spawned when checks already pass, got %d spawns", spawnCount)
	}
	if evalCalls != 1 {
		t.Errorf("expected exactly one evaluation, got %d", evalCalls)
	}
}

// Proves runFixLoop spawns exactly maxAttempts fix agents before giving up,
// and formats the returned skip reason from plan.exhaustedFormat.
func TestRunFixLoop_ExhaustsAttempts_ReturnsFormattedSkipReason(t *testing.T) {
	spawnCount := 0
	l := newFixLoopTestLoop(t, git.StubRepoConfig{}, &spawnCount)

	evalCalls := 0
	passed, skipReason := l.runFixLoop(context.Background(), fixPlan{
		checks:          []fixCheck{stubFixCheck{calls: &evalCalls, outcomes: []checkOutcome{{Passed: false, Failure: "still broken"}}}},
		spawnTemplate:   "verify-tests.md",
		maxAttempts:     2,
		exhaustedFormat: "still failing after %d attempts",
		logDomain:       logging.Test,
	})

	if passed {
		t.Error("expected runFixLoop to fail once attempts are exhausted")
	}
	want := "still failing after 2 attempts"
	if skipReason != want {
		t.Errorf("expected skip reason %q, got %q", want, skipReason)
	}
	if spawnCount != 2 {
		t.Errorf("expected 2 fix agent spawns, got %d", spawnCount)
	}
}
