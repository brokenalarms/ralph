package loop

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// blockingOps wraps git.Ops with a Ship that blocks until the context is
// cancelled, simulating a hung gh CLI invocation during the Ship phase.
type blockingOps struct {
	git.Ops
}

func (b *blockingOps) Ship(ctx context.Context, _ git.ShipOpts) (git.ShipResult, error) {
	<-ctx.Done()
	return git.ShipResult{}, ctx.Err()
}

// PostSignalTimeout cancellation propagates through completeTask into Ship,
// killing an in-flight call within 1 second of the deadline expiry.
//
// The stub's Ship blocks forever; only the PostSignalTimeout-derived ctx
// cancellation can unblock it. The assertion confirms the loop does not
// wait for the stub's indefinite blocking duration.
func TestCompleteTask_PostSignalTimeout_KillsHungShip(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1, NextTask: "Fix login", NextID: "ralph-hung"}},
	}

	inner := git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir, HeadRev: "after"})
	gm := &blockingOps{Ops: inner}

	const postSignalTimeout = 150 * time.Millisecond
	cfg := Config{
		Dirs:              workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:     1,
		CallsPerHour:      80,
		PostSignalTimeout: postSignalTimeout,
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

	start := time.Now()
	l.completeTask(context.Background(), completeTaskParams{
		result:            claude.Result{SignalDetected: true},
		headBefore:        "",
		workDir:           dir,
		rawLogPath:        filepath.Join(ralphDir, "raw.log"),
		taskID:            "ralph-hung",
		nextTask:          "Fix login",
		postSignalTimeout: postSignalTimeout,
		ralphDir:          ralphDir,
	})
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("completeTask took %v — PostSignalTimeout did not propagate into Ship (expected < 1s)", elapsed)
	}
}
