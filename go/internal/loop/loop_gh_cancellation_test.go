package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// deadlineRecordingOps wraps git.Ops with a Ship that records whether the
// context it received carried a deadline, then returns immediately.
type deadlineRecordingOps struct {
	git.Ops
	shipCtxHadDeadline bool
}

func (d *deadlineRecordingOps) Ship(ctx context.Context, _ git.ShipOpts) (git.ShipResult, error) {
	_, d.shipCtxHadDeadline = ctx.Deadline()
	return git.ShipResult{}, nil
}

// completeTask must NOT impose an iteration-wide deadline on post-signal work.
// Each agent (verify, CI fix, conflict, review) is bounded independently by its
// own idle timeout and per-agent wall-clock inside claude.go; a shared
// post-signal deadline would guillotine a healthy, progressing agent when a
// legitimate multi-agent chain exceeds an arbitrary cap. This test pins that
// invariant: given a parent context with no deadline, the context that reaches
// Ship also has no deadline.
func TestCompleteTask_AddsNoIterationDeadline(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix login", NextID: "ralph-nodl"}},
	}

	inner := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, HeadRev: "after"})
	gm := &deadlineRecordingOps{Ops: inner}

	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 1,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		VerifyHook:  passingVerifyHook(),
	})
	l.runner = &stubRunner{}

	l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-nodl",
		nextTask:   "Fix login",
		ralphDir:   ralphDir,
	})

	if gm.shipCtxHadDeadline {
		t.Error("Ship received a context with a deadline — completeTask must not impose an iteration-wide post-signal timeout (agents are bounded by their own idle/run timeouts)")
	}
}
