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

// Verifies that when Evolve is enabled and the binary changes between iterations,
// the loop exits with "evolve_restart" — whether the previous iteration ended with
// a merged PR or a stacked (not-yet-merged) PR. The evolve check fires at the
// START of iteration N+1, before any branch setup. Proven by asserting that
// BranchForTask is called exactly once (for iter1 only, not for the iter2 that
// triggers evolve).
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

			// Two tasks: iter1 runs normally with binary=old; iter2 triggers evolve.
			backend := &testutil.MutableBackend{
				StubBackend: testutil.StubBackend{
					Remaining: 2,
					Total:     2,
					NextTask:  "Task 1",
					NextID:    "ralph-t1",
				},
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

			// Binary unchanged for startup and iter1-start; changed for iter2-start.
			hasher := &stubBinaryHasher{Hashes: [][]byte{
				[]byte("old"), // startup
				[]byte("old"), // iter1-start: no restart
				[]byte("new"), // iter2-start: restart
			}}

			postTaskFired := 0
			postTaskHook := &stubPostTaskHook{fn: func(_ context.Context, _ string, _ int, _ bool) {
				postTaskFired++
				// Serve the second task so the loop dequeues it on the next iteration.
				backend.Lock()
				backend.NextID = "ralph-t2"
				backend.NextTask = "Task 2"
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
				onRun:  func() { gm.CommitAll("simulated agent commit") },
				result: claude.Result{SignalDetected: true},
			}

			err := l.Run(context.Background())
			if err != nil {
				t.Fatalf("[%s] expected no error, got %v", tc.name, err)
			}

			finalState, _ := st.Load()
			if finalState.Status != "evolve_restart" {
				t.Errorf("[%s] expected status 'evolve_restart', got %q", tc.name, finalState.Status)
			}
			if postTaskFired != 1 {
				t.Errorf("[%s] expected post-task to fire once (iter1 only), got %d", tc.name, postTaskFired)
			}
			// Ordering proof: evolve fired at start of iter2 before BranchForTask.
			if got := gm.(git.StubInspector).GetBranchForTaskCalls(); got != 1 {
				t.Errorf("[%s] BranchForTask must be called once (iter1 only, not iter2): got %d — evolve must fire before BranchForTask", tc.name, got)
			}
		})
	}
}

// Verifies that when Evolve is enabled and the binary has already changed when
// the first iteration begins, the loop exits with "evolve_restart" before any
// iteration work (no agent run, no post-task, no BranchForTask call). This is
// the degenerate case of the start-of-iteration evolve check.
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

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/wip-branch",
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

	postTaskFired := 0
	postTaskHook := &stubPostTaskHook{fn: func(_ context.Context, _ string, _ int, _ bool) {
		postTaskFired++
	}}

	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		PostTaskHook: postTaskHook,
		BinaryHasher: changedBinaryHasher(),
	})

	// Runner never commits — but the agent won't even run since evolve fires first.
	l.runner = &stubRunner{result: claude.Result{SignalDetected: true}}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "evolve_restart" {
		t.Errorf("expected status 'evolve_restart', got %q", finalState.Status)
	}
	// Evolve fires before the agent runs, so post-task must never fire.
	if postTaskFired != 0 {
		t.Errorf("post-task must not fire when evolve triggers before agent: fired %d times", postTaskFired)
	}
	// Evolve fires before BranchForTask.
	if got := gm.(git.StubInspector).GetBranchForTaskCalls(); got != 0 {
		t.Errorf("BranchForTask must not be called when evolve triggers at start of iter1: got %d", got)
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
// Simulates 3 tasks with stacked-close; the binary is swapped before
// iteration 3 starts, so iteration 3's start-of-iteration evolve check
// detects the change and triggers restart without running the agent.
// Iterations 1 and 2 must complete normally.
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

	// Calls #1-4: startup=old, iter1-start=old, iter2-start=old, iter3-start=new.
	// iter1 and iter2 complete; iter3's start-of-iteration check triggers restart.
	hasher := &stubBinaryHasher{Hashes: [][]byte{
		[]byte("old"), // startup
		[]byte("old"), // iter1-start
		[]byte("old"), // iter2-start
		[]byte("new"), // iter3-start → change detected
	}}

	// postTaskCallsSeen records hasher.Calls at the moment each post-task fires.
	// With start-of-iteration evolve, each post-task fires after the iteration's
	// start-of-iter hash check: iter1-post-task sees Calls=2, iter2-post-task sees Calls=3.
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

	// iter3 restarts at start-of-iteration — its post-task never fires.
	if len(postTaskCallsSeen) != 2 {
		t.Fatalf("expected 2 completed iterations (iter3 restarts at start), got %d", len(postTaskCallsSeen))
	}
	// Ordering proof per iteration: post-task for iter N fires after iter N's
	// start-of-iter hash check. Startup=call#1; iter1-start=call#2 (before post-task),
	// iter2-start=call#3 (before post-task), so post-task[i] sees Calls == i+2.
	for i, got := range postTaskCallsSeen {
		want := i + 2
		if got != want {
			t.Errorf("iteration %d: post-task must run AFTER start-of-iteration evolve check; expected hasher.Calls=%d at post-task, got %d", i+1, want, got)
		}
	}
	finalState, _ := st.Load()
	if finalState.Status != "evolve_restart" {
		t.Errorf("expected evolve_restart after binary swap at iteration 3, got %q", finalState.Status)
	}
}

// Verifies that when Evolve is enabled and the binary changes between iterations,
// the loop restarts at the start of the next iteration even when the previous
// iteration's merge attempt failed (PR created but not merged). Binary freshness
// is the only condition — prior merge outcome does not gate evolve.
func TestLoop_EvolveRestartsAfterMergeFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	workDir := filepath.Join(dir, "worktree")

	// iter1: binary=old, agent runs, PR created but not merged.
	// iter2: binary=new → evolve fires at start → restart.
	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 2,
			Total:     2,
			NextTask:  "Improve feature Y",
			NextID:    "ralph-imp",
		},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        workDir,
		WorktreeBranch: "ralph/wip-branch",
		Ship:           git.ShipResult{PRNumber: 77, Merged: false},
	})
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    workDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		AutoMerge:     true,
		Evolve:        true,
	}
	logger := logging.New(nil)

	hasher := &stubBinaryHasher{Hashes: [][]byte{
		[]byte("old"), // startup
		[]byte("old"), // iter1-start: no restart
		[]byte("new"), // iter2-start: restart
	}}

	postTaskFired := 0
	postTaskHook := &stubPostTaskHook{fn: func(_ context.Context, _ string, _ int, _ bool) {
		postTaskFired++
		backend.Lock()
		backend.NextID = "ralph-imp2"
		backend.NextTask = "Task 2"
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
		onRun:  func() { gm.CommitAll("work commit") },
		result: claude.Result{SignalDetected: true},
	}

	_ = l.Run(context.Background())

	finalState, _ := st.Load()
	if finalState.Status != "evolve_restart" {
		t.Errorf("expected evolve_restart when binary changed and prior merge failed, got %q", finalState.Status)
	}
	if postTaskFired != 1 {
		t.Errorf("expected post-task to fire once (iter1), got %d", postTaskFired)
	}
}

// Verifies the AC#4 ordering invariant: after --wait returns a task, maybeEvolve
// fires before BranchForTask or any other pre-iteration network operation. Uses
// a two-task backend simulating a wait gap where the binary changes between
// iterations. Proven by asserting BranchForTask was called exactly once (iter1
// only) and IterationHook.OnIterationStart fired exactly once (iter1 only) —
// since both fire after the evolve check in the iteration entry point.
func TestLoop_EvolveAtStartBeforeBranchForTask(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining: 2,
			Total:     2,
			NextTask:  "Task 1",
			NextID:    "ralph-ord1",
		},
	}

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/wip-branch",
		Ship:           git.ShipResult{PRNumber: 1, Merged: true},
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

	// Binary unchanged for startup and iter1-start; changed for iter2-start
	// (simulates a fix landing during the wait period between iterations).
	hasher := &stubBinaryHasher{Hashes: [][]byte{
		[]byte("old"), // startup
		[]byte("old"), // iter1-start: no restart
		[]byte("new"), // iter2-start: fix landed during wait → restart
	}}

	iterationHookCalls := 0
	postTaskFired := 0
	postTaskHook := &stubPostTaskHook{fn: func(_ context.Context, _ string, _ int, _ bool) {
		postTaskFired++
		backend.Lock()
		backend.NextID = "ralph-ord2"
		backend.NextTask = "Task 2"
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
		IterationHook: &stubIterationHook{fn: func() { iterationHookCalls++ }},
	})

	l.runner = &stubRunner{
		onRun:  func() { gm.CommitAll("agent commit") },
		result: claude.Result{SignalDetected: true},
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "evolve_restart" {
		t.Errorf("expected 'evolve_restart', got %q", finalState.Status)
	}

	// iter1 completes, iter2 triggers evolve before BranchForTask.
	// BranchForTask must be called exactly once (iter1 only).
	if got := gm.(git.StubInspector).GetBranchForTaskCalls(); got != 1 {
		t.Errorf("BranchForTask called %d times, want 1: evolve must fire before BranchForTask on iter2", got)
	}

	// IterationHook fires after BranchForTask. Since evolve breaks the loop
	// before reaching IterationHook for iter2, it must fire exactly once (iter1).
	if iterationHookCalls != 1 {
		t.Errorf("IterationHook fired %d times, want 1: must not fire when evolve exits before BranchForTask", iterationHookCalls)
	}

	if postTaskFired != 1 {
		t.Errorf("post-task fired %d times, want 1 (iter1 only)", postTaskFired)
	}
}
