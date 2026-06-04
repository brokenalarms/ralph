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

// Verifies that when Evolve is enabled and the binary changes at the end of an
// iteration (after sync-and-build), the loop exits with "evolve_restart" — whether
// the iteration ended with a merged PR or a stacked (not-yet-merged) PR. The evolve
// check fires at the END of the iteration, after post-task, before the next
// BranchForTask. Proven by asserting BranchForTask is called exactly once (iter1
// only) and postTask fires exactly once (iter1 end-of-task only).
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

			// Two tasks: iter1 runs normally; binary changes after iter1 ends.
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

			// Binary unchanged at startup; changes after iter1 completes (end-of-task
			// evolve check detects the rebuild and restarts before iter2 begins).
			hasher := &stubBinaryHasher{Hashes: [][]byte{
				[]byte("old"), // startup
				[]byte("new"), // iter1-end: rebuild detected → restart before iter2
			}}

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
				t.Errorf("[%s] expected post-task to fire once (iter1 end-of-task), got %d", tc.name, postTaskFired)
			}
			// Ordering proof: evolve fires at end of iter1, before iter2's BranchForTask.
			if got := gm.(git.StubInspector).GetBranchForTaskCalls(); got != 1 {
				t.Errorf("[%s] BranchForTask must be called once (iter1 only): got %d — evolve must fire before iter2 BranchForTask", tc.name, got)
			}
		})
	}
}

// Verifies that when Evolve is enabled and the binary changes after an iteration
// that produced no new commits, the loop exits with "evolve_restart" at the
// end-of-task evolve check. Post-task fires once (before maybeEvolve in the
// helper) and BranchForTask fires once (the iteration ran to completion).
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

	// Agent signals completion but makes no commits.
	l.runner = &stubRunner{result: claude.Result{SignalDetected: true}}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	finalState, _ := st.Load()
	if finalState.Status != "evolve_restart" {
		t.Errorf("expected status 'evolve_restart', got %q", finalState.Status)
	}
	// End-of-task evolve: post-task fires once before the hash check triggers restart.
	if postTaskFired != 1 {
		t.Errorf("post-task must fire once at end-of-task before evolve restarts: fired %d times", postTaskFired)
	}
	// The iteration ran to completion, so BranchForTask was called once.
	if got := gm.(git.StubInspector).GetBranchForTaskCalls(); got != 1 {
		t.Errorf("BranchForTask must be called once (iter1 ran fully): got %d", got)
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
// Post-task fires at the END of each iteration before its evolve check. All
// three iterations fire post-task; the binary changes after iter3's sync step,
// so iter3's end-of-task evolve check detects the change and triggers restart.
// Ordering proof: postTask[i] sees hasher.Calls == i+1 (fires before Hash call i+2).
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

	// Calls: startup=old, iter1-end=old, iter2-end=old, iter3-end=new.
	// iter1 and iter2 complete; iter3's end-of-task check detects the change.
	hasher := &stubBinaryHasher{Hashes: [][]byte{
		[]byte("old"), // startup
		[]byte("old"), // iter1-end: no restart
		[]byte("old"), // iter2-end: no restart
		[]byte("new"), // iter3-end: change detected → restart
	}}

	// postTaskCallsSeen records hasher.Calls at the moment each post-task fires.
	// With end-of-task evolve, post-task fires BEFORE the evolve hash call:
	// iter1-post-task sees Calls=1 (startup only), iter2-post-task sees Calls=2,
	// iter3-post-task sees Calls=3. So postTask[i] sees Calls == i+1.
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

	// All 3 iterations fire post-task at end-of-task before maybeEvolve.
	if len(postTaskCallsSeen) != 3 {
		t.Fatalf("expected 3 post-task calls (all iterations fire post-task before evolve check), got %d", len(postTaskCallsSeen))
	}
	// Ordering proof: post-task fires BEFORE the evolve hash call each iteration.
	// startup=call#1; iter1-post fires (Calls=1), then iter1-evolve Hash #2 (Calls=2), etc.
	for i, got := range postTaskCallsSeen {
		want := i + 1
		if got != want {
			t.Errorf("iteration %d: post-task must run BEFORE evolve hash check; expected hasher.Calls=%d at post-task, got %d", i+1, want, got)
		}
	}
	finalState, _ := st.Load()
	if finalState.Status != "evolve_restart" {
		t.Errorf("expected evolve_restart after binary swap at iteration 3, got %q", finalState.Status)
	}
}

// Verifies that when Evolve is enabled and the binary changes after an iteration
// that created a PR but did not merge it, the loop exits with "evolve_restart"
// at the end-of-task evolve check. Binary freshness is the only condition —
// prior merge outcome does not gate evolve.
func TestLoop_EvolveRestartsAfterMergeFailure(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	workDir := filepath.Join(dir, "worktree")

	// iter1: binary=old, agent runs, PR created but not merged.
	// End of iter1: post-task fires, evolve checks hash → new → restart.
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

	// Binary unchanged at startup; changes after iter1 completes.
	hasher := &stubBinaryHasher{Hashes: [][]byte{
		[]byte("old"), // startup
		[]byte("new"), // iter1-end: restart before iter2
	}}

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
		VerifyHook:   passingVerifyHook(),
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
		t.Errorf("expected post-task to fire once (iter1 end-of-task), got %d", postTaskFired)
	}
}

// AC6: Verifies end-of-task ordering: after a successful iteration,
// execRunPostTask runs and then maybeEvolve runs (binary check), both at the
// iterLoop level after completeTask returns. Proven by recording hasher.Calls
// at post-task time: post-task fires BEFORE the end-of-task hash call, so
// Calls==1 (startup only) at post-task time; after the loop Calls==2
// (startup + end-of-task check).
func TestLoop_EvolveEndOfTaskOrdering(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")
	promptsDir := filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := git.NewStub(git.StubRepoConfig{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/wip-branch",
		Ship:           git.ShipResult{PRNumber: 10, Merged: true},
	})
	backend := &testutil.StubBackend{
		Remaining: 1,
		Total:     1,
		NextTask:  "Fix bug",
		NextID:    "ralph-ord",
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

	// Binary unchanged throughout — loop completes normally without restarting.
	hasher := unchangedBinaryHasher()

	var postTaskHasherCallsAtFire int
	postTaskHook := &stubPostTaskHook{fn: func(_ context.Context, _ string, _ int, _ bool) {
		// Snapshot Calls at the moment post-task fires: must be BEFORE the
		// end-of-task hash call (which increments Calls inside maybeEvolve).
		postTaskHasherCallsAtFire = hasher.Calls
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
		onRun:  func() { gm.CommitAll("agent commit") },
		result: claude.Result{SignalDetected: true},
	}

	_ = l.Run(context.Background())

	// post-task must fire (exactly once for this 1-iteration run).
	if postTaskHasherCallsAtFire == 0 {
		t.Fatal("post-task hook was never called — postTaskAndMaybeEvolve did not run")
	}
	// post-task fires BEFORE the end-of-task hash call: startup is the only
	// Hash() call before post-task fires, so Calls==1 at post-task time.
	if postTaskHasherCallsAtFire != 1 {
		t.Errorf("post-task must fire before end-of-task maybeEvolve: expected hasher.Calls=1 at post-task, got %d", postTaskHasherCallsAtFire)
	}
	// After the loop: startup (1) + end-of-task hash check (1) = 2 calls total.
	if hasher.Calls != 2 {
		t.Errorf("expected 2 total hash calls (startup + end-of-task), got %d", hasher.Calls)
	}
	// Loop completes normally — binary unchanged, no evolve restart.
	finalState, _ := st.Load()
	if finalState.Status == "evolve_restart" {
		t.Error("should NOT restart when binary hash is unchanged")
	}
}

// AC7: Verifies end-of-wait ordering: with --wait enabled and an empty initial
// backlog, after a stub injects a task and waitForTasks returns, postTaskAndMaybeEvolve
// fires (execRunPostTask then maybeEvolve) before BranchForTask runs.
// Also verifies that with a non-empty initial backlog (no wait), the end-of-wait
// helper does NOT fire — only the end-of-task helper fires after the iteration.
func TestLoop_EvolveEndOfWaitOrdering(t *testing.T) {
	t.Run("empty backlog: end-of-wait helper fires before BranchForTask", func(t *testing.T) {
		dir, st := setupTestDir(t)
		ralphDir := filepath.Join(dir, ".ralph")
		promptsDir := filepath.Join(dir, "prompts")
		createPromptTemplates(t, promptsDir)

		backend := &testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining: 0,
				Total:     0,
				NextTask:  "",
				NextID:    "",
			},
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
			Wait:          true,
			Evolve:        true,
		}
		logger := logging.New(nil)

		// Binary unchanged at startup; changes after waitForTasks exits (simulating
		// a rebuild that happened during the wait period).
		hasher := &stubBinaryHasher{Hashes: [][]byte{
			[]byte("old"), // startup
			[]byte("new"), // end-of-wait check: rebuild detected → restart
		}}

		postTaskFired := 0
		postTaskHook := &stubPostTaskHook{fn: func(_ context.Context, _ string, _ int, _ bool) {
			postTaskFired++
		}}

		// waitHook injects the task so waitForTasks returns with waited=true.
		waitHook := &stubWaitHook{fn: func() {
			backend.Lock()
			backend.Remaining = 1
			backend.Total = 1
			backend.NextID = "ralph-w1"
			backend.NextTask = "Waited task"
			backend.Unlock()
		}}

		iterationHookCalls := 0
		l := New(cfg, Modules{
			State:         st,
			Git:           gm,
			TaskBackend:   backend,
			Logger:        logger,
			Verifier:      newTestVerifier(t, cfg, logger),
			Connectivity:  onlineStubConnectivity(),
			PostTaskHook:  postTaskHook,
			WaitHook:      waitHook,
			BinaryHasher:  hasher,
			IterationHook: &stubIterationHook{fn: func() { iterationHookCalls++ }},
		})

		l.runner = &stubRunner{result: claude.Result{}}

		err := l.Run(context.Background())
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		finalState, _ := st.Load()
		if finalState.Status != "evolve_restart" {
			t.Errorf("expected 'evolve_restart' after end-of-wait binary change, got %q", finalState.Status)
		}
		// end-of-wait: post-task fires once before BranchForTask.
		if postTaskFired != 1 {
			t.Errorf("post-task must fire once (end-of-wait helper), got %d", postTaskFired)
		}
		// evolve fires before BranchForTask — BranchForTask must not be called.
		if got := gm.(git.StubInspector).GetBranchForTaskCalls(); got != 0 {
			t.Errorf("BranchForTask must not be called when end-of-wait evolve fires: got %d", got)
		}
		// IterationHook fires after BranchForTask — must not be called either.
		if iterationHookCalls != 0 {
			t.Errorf("IterationHook must not fire when end-of-wait evolve exits: got %d", iterationHookCalls)
		}
	})

	t.Run("non-empty backlog: end-of-wait helper does not fire extra call", func(t *testing.T) {
		dir, st := setupTestDir(t)
		ralphDir := filepath.Join(dir, ".ralph")
		promptsDir := filepath.Join(dir, "prompts")
		createPromptTemplates(t, promptsDir)

		gm := git.NewStub(git.StubRepoConfig{
			ProjectDir:     dir,
			WorkDir:        dir,
			WorktreeBranch: "ralph/wip-branch",
			Ship:           git.ShipResult{PRNumber: 5, Merged: true},
		})
		backend := &testutil.StubBackend{
			Remaining: 1,
			Total:     1,
			NextTask:  "Immediate task",
			NextID:    "ralph-imm",
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
			Evolve:        true,
			AutoMerge:     true,
		}
		logger := logging.New(nil)

		postTaskCalls := 0
		postTaskHook := &stubPostTaskHook{fn: func(_ context.Context, _ string, _ int, _ bool) {
			postTaskCalls++
		}}

		l := New(cfg, Modules{
			State:        st,
			Git:          gm,
			TaskBackend:  backend,
			Logger:       logger,
			Verifier:     newTestVerifier(t, cfg, logger),
			Connectivity: onlineStubConnectivity(),
			PostTaskHook: postTaskHook,
			BinaryHasher: unchangedBinaryHasher(),
		})

		l.runner = &stubRunner{
			onRun:  func() { gm.CommitAll("immediate agent commit") },
			result: claude.Result{SignalDetected: true},
		}

		_ = l.Run(context.Background())

		// No wait occurred, so only the end-of-task helper fires — exactly once.
		if postTaskCalls != 1 {
			t.Errorf("expected exactly 1 post-task call (end-of-task only, no end-of-wait): got %d", postTaskCalls)
		}
	})
}
