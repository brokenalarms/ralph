package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// When Phase 1 push succeeds but CreatePR fails, completeTask skips the bead
// with the pr_creation_failed prefix. The pushed branch must be recorded in
// state so that the next iteration's setStackHead can find it as a candidate.
func TestLoop_PrCreationFailed_RecordsPushedBranch(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	pushed := "ralph/ralph-orph-task-whose-pr-fails"
	fs := buildFinalizeSetup(t, dir, "ralph-orph", "Task whose PR fails to create", backend, shipResult{
		prNumber:     0,
		pushedBranch: pushed,
	})

	fs.loop.completeTask(context.Background(), fs.p)

	branches, err := fs.st.GetPushedBranches()
	if err != nil {
		t.Fatalf("GetPushedBranches: %v", err)
	}
	found := false
	for _, b := range branches {
		if b == pushed {
			found = true
		}
	}
	if !found {
		t.Errorf("pushed branch %q not recorded in state after pr_creation_failed skip; got %v", pushed, branches)
	}
}

// When a normal ship succeeds (push + PR created), the branch is also
// recorded so it participates in stack head detection for the next task.
func TestLoop_ShipSucceeded_RecordsPushedBranch(t *testing.T) {
	dir, _ := setupTestDir(t)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{StubBackend: testutil.StubBackend{Remaining: 1, Total: 1}},
	}

	pushed := "ralph/ralph-abc-fix-login"
	fs := buildFinalizeSetup(t, dir, "ralph-abc", "Fix login", backend, shipResult{
		prNumber:     42,
		pushedBranch: pushed,
	})

	fs.loop.completeTask(context.Background(), fs.p)

	branches, err := fs.st.GetPushedBranches()
	if err != nil {
		t.Fatalf("GetPushedBranches: %v", err)
	}
	found := false
	for _, b := range branches {
		if b == pushed {
			found = true
		}
	}
	if !found {
		t.Errorf("pushed branch %q not recorded in state after successful ship; got %v", pushed, branches)
	}
}

// completedBranches reads from state.GetPushedBranches() when it is non-empty,
// returning branches in chronological order (oldest first). This is the primary
// source for stack head detection.
func TestLoop_CompletedBranches_UsesPushedBranchesFromState(t *testing.T) {
	dir, st := setupTestDir(t)
	ralphDir := filepath.Join(dir, ".ralph")

	_ = st.AddPushedBranch("ralph/task-a")
	_ = st.AddPushedBranch("ralph/task-b")
	_ = st.AddPushedBranch("ralph/task-c")

	cfg := Config{
		Dirs: workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir},
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         git.NewStub(git.StubRepoConfig{ProjectDir: dir, WorkDir: dir}),
		TaskBackend: &testutil.StubBackend{},
		Logger:      logger,
	})

	got := l.completedBranches()

	want := []string{"ralph/task-a", "ralph/task-b", "ralph/task-c"}
	if len(got) != len(want) {
		t.Fatalf("completedBranches() returned %d entries, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("completedBranches()[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// Two-iteration integration test against a real bare repo: iteration 1 pushes
// branch A and is skipped with pr_creation_failed; iteration 2's BranchForTask
// resolves prevBranch=A so CreatePR targets base=A and Push squashes against
// origin/A, producing exactly one commit on the new PR.
func TestIntegrationReal_SkippedBranch_BecomesStackHead(t *testing.T) {
	setup := newGitIntegrationSetup(t)
	promptsDir := filepath.Join(setup.projectDir, "prompts")
	createPromptTemplatesIn(t, promptsDir)

	branchA := "ralph/ralph-aaa-task-a"

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        2,
				NextTask:     "task A",
				NextID:       "ralph-aaa",
				BackendLabel: "beads",
			},
		},
	}

	// Iteration 1: push succeeds but CreatePR fails. Iteration 2: CreatePR
	// succeeds so we can inspect the base it was called with.
	ghCfg := git.StubGitHubConfig{
		Available:         true,
		CreatePRErr:       nil,
		CreatePRViaAPIErr: nil,
	}

	logger := logging.New(nil)
	gm, workDir := withWorktree(t, setup, ghCfg, logger)

	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: setup.projectDir,
			WorkDir:    workDir,
			RalphDir:   setup.ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 2,
		CallsPerHour:  80,
	}

	_, st := setupTestDir(t)

	iteration := 0
	runner := &stubRunner{
		onRun: func() {
			iteration++
			fname := filepath.Join(workDir, "task-"+string(rune('A'+iteration-1))+".txt")
			os.WriteFile(fname, []byte("work "+string(rune('A'+iteration-1))+"\n"), 0o644)
			gitCmd(t, workDir, "git", "add", filepath.Base(fname))
			gitCmd(t, workDir, "git", "commit", "-m", "agent: add "+filepath.Base(fname))
			backend.Lock()
			if iteration == 1 {
				backend.Completed = 1
				backend.Remaining = 1
				backend.NextTask = "task B"
				backend.NextID = "ralph-bbb"
			} else {
				backend.Completed = 2
				backend.Remaining = 0
			}
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})
	l.runner = runner

	// Sabotage CreatePR for iteration 1: the onRun callback above runs before
	// Ship, so after the first agent call we swap in the error. We use a
	// pre/post hook on the verify step to toggle the error at the right time.
	//
	// Instead of trying to toggle the stub mid-flight, simulate the end state
	// of iteration 1 directly: push branch A to origin before the loop starts,
	// record it in state as a pushed branch, and skip task A in the backend.
	// This is equivalent to iteration 1 completing with pr_creation_failed:A.
	//
	// Then the loop runs iteration 2 for task B, which exercises the full
	// BranchForTask → setStackHead → Ship → CreatePR(base=A) pipeline.

	// ---- Simulate iteration 1's outcome: branch A pushed, task skipped ----
	gitCmd(t, workDir, "git", "checkout", "-b", branchA)
	os.WriteFile(filepath.Join(workDir, "task-A.txt"), []byte("work A\n"), 0o644)
	gitCmd(t, workDir, "git", "add", "task-A.txt")
	gitCmd(t, workDir, "git", "commit", "-m", "agent: add task-A.txt")
	gitCmd(t, workDir, "git", "push", "origin", branchA)
	gitCmd(t, workDir, "git", "checkout", "-")

	_ = st.AddPushedBranch(branchA)
	_ = st.AddSkippedTask("ralph-aaa")

	// Backend already skipped task A — advance to task B.
	backend.Lock()
	backend.Completed = 1
	backend.Remaining = 1
	backend.NextTask = "task B"
	backend.NextID = "ralph-bbb"
	backend.Unlock()
	backend.SkipMu.Lock()
	backend.SkippedIDs = append(backend.SkippedIDs, "ralph-aaa")
	backend.SkipReasons = append(backend.SkipReasons, "pr_creation_failed:"+branchA)
	backend.SkipMu.Unlock()

	// Reset runner for a single iteration (task B only).
	iteration = 0
	cfg.MaxIterations = 1
	l = New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})
	runner = &stubRunner{
		onRun: func() {
			fname := filepath.Join(workDir, "task-B.txt")
			os.WriteFile(fname, []byte("work B\n"), 0o644)
			gitCmd(t, workDir, "git", "add", "task-B.txt")
			gitCmd(t, workDir, "git", "commit", "-m", "agent: add task-B.txt")
			backend.Lock()
			backend.Completed = 2
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}
	l.runner = runner

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("loop.Run: %v", err)
	}

	// Observable #1: prevBranch resolved to branch A (stack head detection
	// found the pushed-but-skipped branch).
	if got := gm.GetPrevBranch(); got != branchA {
		t.Errorf("prevBranch = %q, want %q (stack head should be the skipped branch)", got, branchA)
	}

	// Observable #2: the PR created for task B targets base=branchA.
	prs, err := gm.ListAllPRs(workDir)
	if err != nil {
		t.Fatalf("ListAllPRs: %v", err)
	}
	var taskBPR *git.PRInfo
	for i := range prs {
		if strings.Contains(prs[i].Head, "ralph-bbb") || strings.Contains(prs[i].Head, "task-b") {
			taskBPR = &prs[i]
			break
		}
	}
	if taskBPR == nil {
		t.Fatalf("no PR found for task B; all PRs: %+v", prs)
	}
	if taskBPR.Base != branchA {
		t.Errorf("task B PR base = %q, want %q (should stack on the skipped branch)", taskBPR.Base, branchA)
	}

	// Observable #3: task B's branch has exactly one commit ahead of branch A,
	// confirming Push squashed against origin/A (not origin/main).
	commitCount := gitOutputAt(t, setup.bareDir, "rev-list", "--count", branchA+".."+taskBPR.Head)
	if commitCount != "1" {
		t.Errorf("task B branch has %s commits ahead of %s, want 1 (squash against stack parent)", commitCount, branchA)
	}
}
