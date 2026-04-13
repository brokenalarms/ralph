package loop

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verifier"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// doShip retries the merge loop after a transient CI failure (CIFixNoCommits)
// with exponential backoff, eventually succeeding when CI passes on a later
// attempt. This proves the loop doesn't give up on infrastructure failures.
func TestDoShip_InfraRetry_EventuallySucceeds(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	ciErr := &git.CIFailureError{
		PRNumber: 42,
		Failures: []git.CICheckResult{{Name: "tests", Bucket: "fail", IsRequired: true}},
	}

	var mergeAttempts atomic.Int32
	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, HeadRevValue: "stable"}
	gm.ShipFunc = func(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
		if !opts.AutoMerge {
			return git.ShipResult{PRNumber: 42}, nil
		}
		attempt := mergeAttempts.Add(1)
		if attempt <= 2 {
			// First two merge attempts: transient CI failure, fix agent makes no commits.
			return git.ShipResult{CIFailure: true, CIFailureDetail: ciErr}, nil
		}
		// Third attempt: CI passes, merge succeeds.
		return git.ShipResult{PRNumber: 42, Merged: true}, nil
	}

	cfg := Config{
		Dirs:               workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:      1,
		CallsPerHour:       80,
		AutoMerge:          true,
		InfraRetryBackoffs: []time.Duration{0, 0, 0},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: claude.Result{SignalDetected: true}}
			},
		}),
		Connectivity: onlineStubConnectivity(),
	})

	_, _, merged, ciFailure, _, _ := l.doShip(context.Background(), "ralph-test", "Fix bug", "summary", filepath.Join(ralphDir, "raw.log"), dir)

	if !merged {
		t.Error("expected merged=true after transient CI retries succeeded")
	}
	if ciFailure {
		t.Error("expected ciFailure=false when CI eventually passed")
	}
	if got := mergeAttempts.Load(); got != 3 {
		t.Errorf("expected 3 merge attempts (2 CI failures + 1 success), got %d", got)
	}
}

// doShip gives up after maxInfraRetries consecutive transient CI failures
// (CIFixNoCommits), returning ciFailure=true so the caller can leave the
// task open and leave the PR open for manual investigation.
func TestDoShip_InfraRetry_GivesUpAfterMax(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	ciErr := &git.CIFailureError{
		PRNumber: 42,
		Failures: []git.CICheckResult{{Name: "tests", Bucket: "fail", IsRequired: true}},
	}

	var mergeAttempts atomic.Int32
	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, HeadRevValue: "stable"}
	gm.ShipFunc = func(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
		if !opts.AutoMerge {
			return git.ShipResult{PRNumber: 42}, nil
		}
		mergeAttempts.Add(1)
		return git.ShipResult{CIFailure: true, CIFailureDetail: ciErr}, nil
	}

	const maxRetries = 3
	cfg := Config{
		Dirs:               workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:      1,
		CallsPerHour:       80,
		AutoMerge:          true,
		InfraRetryBackoffs: make([]time.Duration, maxRetries),
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: claude.Result{SignalDetected: true}}
			},
		}),
		Connectivity: onlineStubConnectivity(),
	})

	_, _, merged, ciFailure, _, _ := l.doShip(context.Background(), "ralph-test", "Fix bug", "summary", filepath.Join(ralphDir, "raw.log"), dir)

	if merged {
		t.Error("expected merged=false when CI keeps failing after max retries")
	}
	if !ciFailure {
		t.Error("expected ciFailure=true when CI failures exhausted infra retries")
	}
	// Merge should be attempted once per infra retry attempt + 1 initial.
	if got := mergeAttempts.Load(); got != int32(maxRetries+1) {
		t.Errorf("expected %d merge attempts (1 initial + %d retries), got %d", maxRetries+1, maxRetries, got)
	}
}

// doShip returns ciInfraFailure=true when CI fails with zero job steps and the
// fix agent makes no commits. The loop should close the bead rather than retrying
// — the work is verified locally and CI infrastructure is the blocker.
func TestDoShip_InfrastructureFailure_SkipsRetryAndSignals(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	ciErr := &git.CIFailureError{
		PRNumber: 42,
		Failures: []git.CICheckResult{{Name: "tests", Bucket: "fail", IsRequired: true}},
	}

	var mergeAttempts int
	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, HeadRevValue: "stable"}
	gm.ShipFunc = func(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
		if !opts.AutoMerge {
			return git.ShipResult{PRNumber: 42}, nil
		}
		mergeAttempts++
		return git.ShipResult{
			PRNumber:              42,
			CIFailure:             true,
			CIFailureDetail:       ciErr,
			InfrastructureFailure: true,
		}, nil
	}

	cfg := Config{
		Dirs:               workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:      1,
		CallsPerHour:       80,
		AutoMerge:          true,
		InfraRetryBackoffs: []time.Duration{0, 0, 0},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: claude.Result{SignalDetected: true}}
			},
		}),
		Connectivity: onlineStubConnectivity(),
	})

	_, _, merged, ciFailure, ciInfraFailure, _ := l.doShip(context.Background(), "ralph-test", "Fix bug", "summary", filepath.Join(ralphDir, "raw.log"), dir)

	if merged {
		t.Error("expected merged=false for infrastructure failure")
	}
	if !ciFailure {
		t.Error("expected ciFailure=true for infrastructure failure")
	}
	if !ciInfraFailure {
		t.Error("expected ciInfraFailure=true when InfrastructureFailure=true and CIFixNoCommits")
	}
	if mergeAttempts != 1 {
		t.Errorf("expected 1 merge attempt (no retries for infra failure), got %d", mergeAttempts)
	}
}

// doShip returns ciInfraFailure=false when CI fails with actual test failures
// (job steps executed). The bead should be left open for manual investigation.
func TestDoShip_RealCIFailure_DoesNotSignalInfra(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	ciErr := &git.CIFailureError{
		PRNumber: 42,
		Failures: []git.CICheckResult{{Name: "tests", Bucket: "fail", IsRequired: true}},
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, HeadRevValue: "stable"}
	gm.ShipFunc = func(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
		if !opts.AutoMerge {
			return git.ShipResult{PRNumber: 42}, nil
		}
		return git.ShipResult{
			PRNumber:              42,
			CIFailure:             true,
			CIFailureDetail:       ciErr,
			InfrastructureFailure: false,
		}, nil
	}

	cfg := Config{
		Dirs:               workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:      1,
		CallsPerHour:       80,
		AutoMerge:          true,
		InfraRetryBackoffs: make([]time.Duration, 0), // no retries so it exits quickly
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: claude.Result{SignalDetected: true}}
			},
		}),
		Connectivity: onlineStubConnectivity(),
	})

	_, _, merged, ciFailure, ciInfraFailure, _ := l.doShip(context.Background(), "ralph-test", "Fix bug", "summary", filepath.Join(ralphDir, "raw.log"), dir)

	if merged {
		t.Error("expected merged=false for CI failure")
	}
	if !ciFailure {
		t.Error("expected ciFailure=true for actual CI failure")
	}
	if ciInfraFailure {
		t.Error("expected ciInfraFailure=false when InfrastructureFailure=false (regression guard)")
	}
}

// doShip respects context cancellation during infra retry backoff sleep,
// returning immediately without hanging when the post-signal timeout fires.
func TestDoShip_InfraRetry_RespectsContextCancellation(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	ciErr := &git.CIFailureError{
		PRNumber: 42,
		Failures: []git.CICheckResult{{Name: "tests", Bucket: "fail", IsRequired: true}},
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, HeadRevValue: "stable"}
	gm.ShipFunc = func(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
		if !opts.AutoMerge {
			return git.ShipResult{PRNumber: 42}, nil
		}
		return git.ShipResult{CIFailure: true, CIFailureDetail: ciErr}, nil
	}

	cfg := Config{
		Dirs:               workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:      1,
		CallsPerHour:       80,
		AutoMerge:          true,
		InfraRetryBackoffs: []time.Duration{10 * time.Second},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: &testutil.StubBackend{Remaining: 1, Total: 1},
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: claude.Result{SignalDetected: true}}
			},
		}),
		Connectivity: onlineStubConnectivity(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		l.doShip(ctx, "ralph-test", "Fix bug", "summary", filepath.Join(ralphDir, "raw.log"), dir)
		close(done)
	}()

	select {
	case <-done:
		// Returned promptly — context cancellation was respected.
	case <-time.After(5 * time.Second):
		t.Fatal("doShip hung — infra retry backoff did not respect context cancellation")
	}
}

// completeTask closes the bead when CI fails due to infrastructure (zero job
// steps executed). Work is verified locally; the PR is left open for merge
// when CI infrastructure recovers.
func TestCompleteTask_ClosesBeadOnCIInfraFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix billing bug",
				NextID:    "ralph-infra1",
			},
		},
	}

	ciErr := &git.CIFailureError{
		PRNumber: 42,
		Failures: []git.CICheckResult{{Name: "tests", Bucket: "fail", IsRequired: true}},
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, HeadRevValue: "stable"}
	gm.ShipFunc = func(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
		if !opts.AutoMerge {
			return git.ShipResult{PRNumber: 42, PRURL: "https://github.com/owner/repo/pull/42"}, nil
		}
		return git.ShipResult{
			PRNumber:              42,
			CIFailure:             true,
			CIFailureDetail:       ciErr,
			InfrastructureFailure: true,
		}, nil
	}

	cfg := Config{
		Dirs:               workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:      1,
		CallsPerHour:       80,
		AutoMerge:          true,
		InfraRetryBackoffs: make([]time.Duration, 0),
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: claude.Result{SignalDetected: true}}
			},
		}),
		VerifyHook:   passingVerifyHook(),
		Connectivity: onlineStubConnectivity(),
	})

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-infra1",
		nextTask:   "Fix billing bug",
		ralphDir:   ralphDir,
	})

	if out.action != signalComplete {
		t.Errorf("expected signalComplete, got %d", out.action)
	}
	if out.ct == nil {
		t.Error("expected ct to be set (task handled, not left open)")
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-infra1" {
		t.Errorf("expected CloseTask for ralph-infra1, got %v", backend.ClosedIDs)
	}
	if len(backend.CloseReasons) < 1 || !strings.Contains(backend.CloseReasons[0], "CI recovery") {
		t.Errorf("expected close reason to mention CI recovery, got %v", backend.CloseReasons)
	}
}

// completeTask leaves the bead open when CI fails with actual test failures
// (job steps executed). The bead should remain open for manual investigation.
func TestCompleteTask_LeavesBeadOpenOnCIFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Fix test failures",
				NextID:    "ralph-ci1",
			},
		},
	}

	ciErr := &git.CIFailureError{
		PRNumber: 43,
		Failures: []git.CICheckResult{{Name: "tests", Bucket: "fail", IsRequired: true}},
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, HeadRevValue: "stable"}
	gm.ShipFunc = func(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
		if !opts.AutoMerge {
			return git.ShipResult{PRNumber: 43}, nil
		}
		return git.ShipResult{
			PRNumber:              43,
			CIFailure:             true,
			CIFailureDetail:       ciErr,
			InfrastructureFailure: false,
		}, nil
	}

	cfg := Config{
		Dirs:               workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations:      1,
		CallsPerHour:       80,
		AutoMerge:          true,
		InfraRetryBackoffs: make([]time.Duration, 0),
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				return &stubRunner{result: claude.Result{SignalDetected: true}}
			},
		}),
		VerifyHook:   passingVerifyHook(),
		Connectivity: onlineStubConnectivity(),
	})

	out := l.completeTask(context.Background(), completeTaskParams{
		result:     claude.Result{SignalDetected: true},
		headBefore: "",
		workDir:    dir,
		rawLogPath: filepath.Join(ralphDir, "raw.log"),
		taskID:     "ralph-ci1",
		nextTask:   "Fix test failures",
		ralphDir:   ralphDir,
	})

	if out.action != signalComplete {
		t.Errorf("expected signalComplete, got %d", out.action)
	}
	if out.ct != nil {
		t.Error("expected ct=nil (task left open, not added to completedIDs)")
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Errorf("expected bead left open on CI test failure, got CloseTask for %v", backend.ClosedIDs)
	}
}
