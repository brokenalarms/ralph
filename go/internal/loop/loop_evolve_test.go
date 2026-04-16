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

// Verifies that when Evolve is enabled and the binary changed, the loop exits
// with "evolve_restart" status after a merged PR, and that post-task ran
// before the evolve signal. Ordering is proven by capturing the hasher's Calls
// counter inside the post-task hook: startup hash is call #1, and maybeEvolve
// calls Hash() only after execRunPostTask returns, so post-task must observe
// Calls == 1 if it fired before the iteration's evolve check.
func TestLoop_EvolveRestartsAfterMerge(t *testing.T) {
	cases := []struct {
		name string
		ship git.ShipResult
	}{
		{"merged", git.ShipResult{PRNumber: 42, Merged: true}},
		{"stacked (not merged)", git.ShipResult{PRNumber: 42, Merged: false, Stacked: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, st := setupTestDir(t)
			ralphDir := filepath.Join(dir, ".ralph")
			promptsDir := filepath.Join(dir, "prompts")
			createPromptTemplates(t, promptsDir)

			backend := &testutil.StubBackend{
				Remaining: 1,
				Total:     1,
				NextTask:  "Improve feature X",
				NextID:    "ralph-imp",
			}

			gm := git.NewStub(git.StubRepoConfig{
				ProjectDir:     dir,
				WorkDir:        dir,
				WorktreeBranch: "ralph/wip-branch",
				Ship:           tc.ship,
			})
			cfg := Config{
				Dirs: workctx.WorkContext{
					ProjectDir: dir,
					WorkDir:    dir,
					RalphDir:   ralphDir,
					PromptsDir: promptsDir,
				},
				MaxIterations: 5,
				CallsPerHour:  80,
				AutoMerge:     true,
				Evolve:        true,
			}
			logger := logging.New(nil)

			hasher := changedBinaryHasher()
			postTaskFired := 0
			hasherCallsAtPostTask := -1
			postTaskHook := &stubPostTaskHook{fn: func(_ context.Context, _ string, _ int, _ bool) {
				postTaskFired++
				hasherCallsAtPostTask = hasher.Calls
			}}

			l := New(cfg, Modules{
				State:        st,
				Git:          gm,
				TaskBackend:  backend,
				Logger:       logger,
				Verifier:     newTestVerifier(t, cfg, logger),
				Connectivity: onlineStubConnectivity(),
				PostTaskHook: postTaskHook,
				BinaryHasher: hasher,
			})

			l.runner = &stubRunner{
				onRun:  func() { gm.CommitAll("simulated agent commit") },
				result: claude.Result{SignalDetected: true},
			}

			err := l.Run(context.Background())
			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}

			finalState, _ := st.Load()
			if finalState.Status != "evolve_restart" {
				t.Errorf("[%s] expected status 'evolve_restart', got %q", tc.name, finalState.Status)
			}
			if postTaskFired == 0 {
				t.Fatalf("[%s] post-task hook must run before evolve restart", tc.name)
			}
			// Proof of ordering: at the moment post-task fired, only the startup
			// hash had been computed (Calls == 1). maybeEvolve's Hash() call
			// happens after execRunPostTask returns; if post-task saw Calls > 1
			// for iteration 1, it would mean maybeEvolve ran first.
			if hasherCallsAtPostTask != 1 {
				t.Errorf("[%s] post-task must run BEFORE maybeEvolve: expected hasher.Calls=1 at post-task, got %d", tc.name, hasherCallsAtPostTask)
			}
			// Final state: maybeEvolve must have run after post-task, so Calls
			// must have advanced past the post-task observation.
			if hasher.Calls <= hasherCallsAtPostTask {
				t.Errorf("[%s] maybeEvolve must run AFTER post-task: final Calls=%d, post-task saw %d", tc.name, hasher.Calls, hasherCallsAtPostTask)
			}
		})
	}
}

// Verifies that when Evolve is enabled and the binary changed, the loop exits
// with "evolve_restart" even when no new commits were produced (verified
// complete, no-op run). Post-task ordering is proven by capturing hasher.Calls
// inside the hook: maybeEvolve's Hash() call happens only after execRunPostTask
// returns, so post-task must observe Calls == 1 (startup-only).
func TestLoop_EvolveRestartsOnNoCommitsWhenBinaryChanged(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Improve feature X",
		NextID:    "ralph-imp",
	}

	// HeadRev must be non-empty so the no-commits guard (`headBefore != ""`)
	// is satisfied and the early-exit path fires when the runner makes no commit.
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/wip-branch",
		HeadRev:        "initial-sha",
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		Evolve:        true,
	}
	logger := logging.New(nil)

	hasher := changedBinaryHasher()
	postTaskFired := 0
	hasherCallsAtPostTask := -1
	postTaskHook := &stubPostTaskHook{fn: func(_ context.Context, _ string, _ int, _ bool) {
		postTaskFired++
		hasherCallsAtPostTask = hasher.Calls
	}}

	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		PostTaskHook: postTaskHook,
		BinaryHasher: hasher,
	})

	// Runner signals without committing — exercises the no-commits evolve path.
	l.runner = &stubRunner{
		result: claude.Result{SignalDetected: true},
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "evolve_restart" {
		t.Errorf("expected status 'evolve_restart' on no-commits + binary changed, got %q", finalState.Status)
	}
	if postTaskFired == 0 {
		t.Fatal("post-task hook must run before evolve restart")
	}
	if hasherCallsAtPostTask != 1 {
		t.Errorf("post-task must run BEFORE maybeEvolve: expected hasher.Calls=1 at post-task, got %d", hasherCallsAtPostTask)
	}
	if hasher.Calls <= hasherCallsAtPostTask {
		t.Errorf("maybeEvolve must run AFTER post-task: final Calls=%d, post-task saw %d", hasher.Calls, hasherCallsAtPostTask)
	}
}

// Verifies that when the binary hash is unchanged, Evolve does NOT trigger a
// restart even when Evolve is enabled — the loop continues normally.
func TestLoop_EvolveNoRestartOnUnchangedBinary(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/wip-branch",
		Ship:           git.ShipResult{PRNumber: 42, Merged: true},
	})

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Improve feature X",
		NextID:    "ralph-imp",
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
		Evolve:        true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		BinaryHasher: unchangedBinaryHasher(),
	})

	l.runner = &stubRunner{
		onRun:  func() { gm.CommitAll("simulated agent commit") },
		result: claude.Result{SignalDetected: true},
	}

	_ = l.Run(context.Background())

	finalState, _ := st.Load()
	if finalState.Status == "evolve_restart" {
		t.Error("should NOT set evolve_restart when binary hash is unchanged")
	}
}

// Verifies that Evolve runs across multiple stacked iterations (merged=false,
// Stacked=true) and only triggers restart when the binary actually changes.
// Simulates 3 stacked-close iterations; the binary is swapped after
// iteration 2, so iteration 3 is the first to emit signalEvolve with
// status=evolve_restart. Iterations 1 and 2 must continue normally.
func TestLoop_EvolveStackedCloseRestartsOnBinaryChange(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 3,
			Total:     3,
			NextTask:  "Stacked task 1",
			NextID:    "ralph-stk1",
		},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/wip-branch",
		Ship:           git.ShipResult{PRNumber: 10, Merged: false, Stacked: true},
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		AutoMerge:     true,
		Evolve:        true,
	}
	logger := logging.New(nil)

	// Hash sequence: call #1 is startup (old). Calls #2 and #3 are iter 1 and
	// iter 2 maybeEvolve checks — both must return old so no restart fires.
	// Call #4 is iter 3's maybeEvolve — binary has been swapped, returns new,
	// triggering signalEvolve.
	hasher := &stubBinaryHasher{Hashes: [][]byte{
		[]byte("old"), // startup
		[]byte("old"), // iter 1
		[]byte("old"), // iter 2
		[]byte("new"), // iter 3 → change detected
	}}

	postTaskCallsSeen := []int{}
	postTaskHook := &stubPostTaskHook{fn: func(_ context.Context, _ string, _ int, _ bool) {
		postTaskCallsSeen = append(postTaskCallsSeen, hasher.Calls)
		backend.Lock()
		switch len(postTaskCallsSeen) {
		case 1:
			backend.NextID = "ralph-stk2"
			backend.NextTask = "Stacked task 2"
		case 2:
			backend.NextID = "ralph-stk3"
			backend.NextTask = "Stacked task 3"
		}
		backend.Unlock()
	}}

	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		PostTaskHook: postTaskHook,
		BinaryHasher: hasher,
	})

	l.runner = &stubRunner{
		onRun:  func() { gm.CommitAll("stacked commit") },
		result: claude.Result{SignalDetected: true},
	}

	_ = l.Run(context.Background())

	if len(postTaskCallsSeen) != 3 {
		t.Fatalf("expected 3 iterations (stacked-close × 3), got %d", len(postTaskCallsSeen))
	}
	// Ordering proof per iteration: post-task for iter N must run before that
	// iteration's maybeEvolve Hash() call. Startup=1, iter1-post-task=1,
	// iter1-evolve=2, iter2-post-task=2, iter2-evolve=3, iter3-post-task=3.
	for i, got := range postTaskCallsSeen {
		want := i + 1
		if got != want {
			t.Errorf("iteration %d: post-task must run BEFORE maybeEvolve; expected hasher.Calls=%d at post-task, got %d", i+1, want, got)
		}
	}
	finalState, _ := st.Load()
	if finalState.Status != "evolve_restart" {
		t.Errorf("expected evolve_restart after binary swap at iteration 3, got %q", finalState.Status)
	}
}

// Verifies that when Evolve is enabled and the binary changed, the loop restarts
// even when auto-merge fails (PR created, merge=false). The merge outcome no
// longer gates evolve — binary freshness is the only condition.
func TestLoop_EvolveNoRestartOnMergeFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	workDir := filepath.Join(dir, "worktree")
	// Ship returns a PR number but merged=false — simulating merge failure.
	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/wip-branch",
		Ship:           git.ShipResult{PRNumber: 77, Merged: false},
	})

	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Improve feature Y",
		NextID:    "ralph-imp2",
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
		Evolve:        true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		BinaryHasher: changedBinaryHasher(),
	})

	l.runner = &stubRunner{
		onRun:  func() { gm.CommitAll("work commit") },
		result: claude.Result{SignalDetected: true},
	}

	_ = l.Run(context.Background())

	finalState, _ := st.Load()
	if finalState.Status != "evolve_restart" {
		t.Errorf("expected evolve_restart when binary changed and merge failed, got %q", finalState.Status)
	}
}
