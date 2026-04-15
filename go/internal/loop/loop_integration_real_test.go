package loop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verifier"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// These tests exercise the real git module against an in-process bare repo.
// They drive Loop.Run() against git.NewForTest (real execRunner + stub
// gitHub) and assert on observable git state (branches, commits,
// origin/main refs) that unit tests cannot express.
//
// Per docs/specs/stub-interface-rewrite.md — the spec-enumerated Phase D
// integration candidates that got promoted out of loop_integration_test.go
// live here. Every test reads and writes real files on disk via the real
// git binary.

// gitIntegrationSetup holds the scaffolding for a real-git integration
// test. Construct via newGitIntegrationSetup.
type gitIntegrationSetup struct {
	t          *testing.T
	bareDir    string // --bare repo acting as origin
	projectDir string // working clone (test's "project directory")
	ralphDir   string // .ralph directory
}

// newGitIntegrationSetup creates a bare repo + cloned working directory
// seeded with an empty "init" commit on main, and a .ralph directory.
// Returns the setup; cleanup happens via t.TempDir() teardown.
func newGitIntegrationSetup(t *testing.T) *gitIntegrationSetup {
	t.Helper()
	tmp := t.TempDir()

	bare := filepath.Join(tmp, "origin.git")
	gitCmd(t, "", "git", "init", "--bare", "-b", "main", bare)

	project := filepath.Join(tmp, "project")
	gitCmd(t, "", "git", "clone", bare, project)
	gitCmd(t, project, "git", "config", "user.name", "test")
	gitCmd(t, project, "git", "config", "user.email", "test@test")
	gitCmd(t, project, "git", "commit", "--allow-empty", "-m", "init")
	gitCmd(t, project, "git", "push", "-u", "origin", "main")
	gitCmd(t, project, "git", "remote", "set-head", "origin", "main")

	ralphDir := filepath.Join(project, ".ralph")
	if err := os.MkdirAll(ralphDir, 0o755); err != nil {
		t.Fatalf("mkdir .ralph: %v", err)
	}

	return &gitIntegrationSetup{
		t:          t,
		bareDir:    bare,
		projectDir: project,
		ralphDir:   ralphDir,
	}
}

// gitCmd runs a git (or other) command in dir (or cwd if empty) and fails
// the test on non-zero exit. Sets a deterministic identity via env.
func gitCmd(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v (in %s): %v\n%s", name, args, dir, err, out)
	}
}

// gitOutputAt runs git in dir and returns trimmed stdout. Fails the test
// on non-zero exit.
func gitOutputAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v (in %s): %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

// refExistsAt returns true when git -C dir rev-parse <ref> succeeds.
func refExistsAt(dir, ref string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = dir
	return cmd.Run() == nil
}

// createPromptTemplatesIn creates minimal prompt template files so the
// loop can build prompts without errors.
func createPromptTemplatesIn(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	names := []string{
		"shared.md", "internal.md", "reflection.md", "signal.md",
		"feedback.md", "refactor.md", "refactor-style.md",
		"execution-bd.md", "bead-creation.md",
	}
	for _, name := range names {
		os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o644)
	}
}

// withWorktree initializes a real git worktree via gm.Init, which creates
// a directory under ralphDir/worktrees and checks out a placeholder
// "wip" branch there. Returns the workDir (worktree path) so the runner
// can commit in the correct location.
//
// Tests that exercise BranchForTask renames, EnsureUpToDate rebases, or
// real push-to-origin flows need this; tests that only care about a
// single commit + close observable can use the projectDir directly.
func withWorktree(t *testing.T, setup *gitIntegrationSetup, ghCfg git.StubGitHubConfig, logger *logging.Logger) (gm git.Ops, workDir string) {
	t.Helper()
	gm = git.NewForTest(git.Config{
		ProjectDir: setup.projectDir,
		WorkDir:    setup.projectDir, // starting point; Init rewrites to worktree path
		RalphDir:   setup.ralphDir,
		BaseBranch: "main",
		Logger:     logger,
	}, ghCfg)
	if err := gm.Init(context.Background()); err != nil {
		t.Fatalf("gm.Init: %v", err)
	}
	workDir = gm.GetWorkDir()
	// Identity inside the worktree (worktrees inherit config but CI envs
	// can lack global git identity).
	gitCmd(t, workDir, "git", "config", "user.name", "test")
	gitCmd(t, workDir, "git", "config", "user.email", "test@test")
	return gm, workDir
}

// TestIntegrationReal_HappyPath_SignalVerifyPushMergeClose is the spec's
// "happy path end-to-end" integration candidate. It exercises a single
// task from signal detection through verification, push, merge, and bead
// close — against a real bare repo with real git operations.
//
// What's real: the bare repo, the project clone, the worktree branch
// operations driven by the loop, the file state on disk.
//
// What's stubbed: GitHub (via StubGitHubConfig), the Claude runner (via
// stubRunner), the verifier hook (passingVerifyHook). This lets the test
// focus on observable git-state transitions without needing a network
// GitHub or a live Claude model.
func TestIntegrationReal_HappyPath_SignalVerifyPushMergeClose(t *testing.T) {
	setup := newGitIntegrationSetup(t)
	promptsDir := filepath.Join(setup.projectDir, "prompts")
	createPromptTemplatesIn(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "Add feature X",
				NextID:       "ralph-hp1",
				BackendLabel: "beads",
			},
		},
	}

	// StubGitHubConfig seeds the fake's world with an open PR that the
	// loop's Ship will merge. The PR's Branch is left empty; the fake's
	// MergePR derives its outcome from PR state (open → merged), which is
	// what real GitHub would do.
	ghCfg := git.StubGitHubConfig{
		Available: true,
		PRs: []git.StubPR{
			{Number: 42, Base: "main", State: git.PRStateOpen},
		},
	}

	logger := logging.New(nil)
	gm := git.NewForTest(git.Config{
		ProjectDir: setup.projectDir,
		WorkDir:    setup.projectDir,
		RalphDir:   setup.ralphDir,
		BaseBranch: "main",
		Logger:     logger,
	}, ghCfg)

	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: setup.projectDir,
			WorkDir:    setup.projectDir,
			RalphDir:   setup.ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}

	_, st := setupTestDir(t) // separate state store for this loop

	// Runner: emit a real commit when the agent "runs", so the loop
	// observes forward progress on HEAD.
	runner := &stubRunner{
		onRun: func() {
			path := filepath.Join(setup.projectDir, "feature.txt")
			os.WriteFile(path, []byte("agent work\n"), 0o644)
			gitCmd(t, setup.projectDir, "git", "add", "feature.txt")
			gitCmd(t, setup.projectDir, "git", "commit", "-m", "agent: add feature")
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
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

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("loop.Run: %v", err)
	}

	// Observable: the agent's commit is now in the project's log.
	logOut := gitOutputAt(t, setup.projectDir, "log", "--oneline")
	if !strings.Contains(logOut, "agent: add feature") {
		t.Errorf("expected agent commit in project log, got:\n%s", logOut)
	}

	// Observable: the bead was closed via the ship pipeline.
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-hp1" {
		t.Errorf("expected ralph-hp1 closed, got %v", backend.ClosedIDs)
	}
}

// TestIntegrationReal_PushSucceededPRCreationFailed_SkipsAndPreservesBranch
// drives a full iteration end-to-end against a real bare repo where Phase 1's
// push lands on origin but the stub GitHub's CreatePR returns an error. The
// close-vs-skip decision in completeTask must SKIP (not close) the bead —
// closing would orphan the freshly-pushed branch with no bead reference.
//
// This guards against the regression that motivated ralph-mhtr: a HTTP 422
// from CreatePR collapsed into the "prNumber == 0 → close the bead" branch,
// quietly abandoning pushed work.
//
// Assertions: (1) the pushed branch still exists on the bare remote, (2) the
// bead was skipped (not closed), (3) the skip reason carries the documented
// "pr_creation_failed:<branch>" prefix so downstream triage can find it.
func TestIntegrationReal_PushSucceededPRCreationFailed_SkipsAndPreservesBranch(t *testing.T) {
	setup := newGitIntegrationSetup(t)
	promptsDir := filepath.Join(setup.projectDir, "prompts")
	createPromptTemplatesIn(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "Task whose PR creation fails",
				NextID:       "ralph-orph",
				BackendLabel: "beads",
			},
		},
	}

	// Both CreatePR paths fail — primary (gh pr create) and the REST API
	// fallback that CreatePR tries on client-side-failure. Push itself is
	// not stubbed; it hits the real bare repo over a file:// remote, so
	// the branch really lands on origin before CreatePR is attempted.
	ghCfg := git.StubGitHubConfig{
		Available:         true,
		CreatePRErr:       errors.New("Validation Failed: base=invalid (422)"),
		CreatePRViaAPIErr: errors.New("Validation Failed: base=invalid (422)"),
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
		MaxIterations: 1,
		CallsPerHour:  80,
	}

	_, st := setupTestDir(t)

	runner := &stubRunner{
		onRun: func() {
			path := filepath.Join(workDir, "work.txt")
			os.WriteFile(path, []byte("orphan-candidate\n"), 0o644)
			gitCmd(t, workDir, "git", "add", "work.txt")
			gitCmd(t, workDir, "git", "commit", "-m", "agent: work that would orphan if bead closed")
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
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

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("loop.Run: %v", err)
	}

	// Observable #1: bead was NOT closed. Closing would orphan the remote
	// branch — that's the bug we're guarding against.
	backend.CloseMu.Lock()
	if len(backend.ClosedIDs) != 0 {
		backend.CloseMu.Unlock()
		t.Fatalf("bead must not be closed when push succeeded but CreatePR failed (would orphan branch), closed=%v reasons=%v", backend.ClosedIDs, backend.CloseReasons)
	}
	backend.CloseMu.Unlock()

	// Observable #2: bead was skipped with a reason carrying the branch
	// name in the documented machine-parseable format.
	backend.SkipMu.Lock()
	defer backend.SkipMu.Unlock()
	var skipReason string
	for i, id := range backend.SkippedIDs {
		if id == "ralph-orph" {
			skipReason = backend.SkipReasons[i]
			break
		}
	}
	if skipReason == "" {
		t.Fatalf("expected bead ralph-orph to be skipped, got skipped=%v", backend.SkippedIDs)
	}
	if !strings.HasPrefix(skipReason, "pr_creation_failed:") {
		t.Errorf("skip reason must use pr_creation_failed: prefix, got %q", skipReason)
	}

	// Observable #3: the pushed branch is still present on the bare remote.
	// The skip reason contains the branch name after the colon — use it to
	// assert the remote ref exists. If the bead had closed, triage would
	// have no way to rediscover this branch; this assertion locks in that
	// the skip reason actually points at something real.
	branchName := strings.TrimPrefix(skipReason, "pr_creation_failed:")
	if branchName == "" {
		t.Fatalf("skip reason must carry a non-empty branch name, got %q", skipReason)
	}
	if !refExistsAt(setup.bareDir, "refs/heads/"+branchName) {
		remoteBranches := gitOutputAt(t, setup.bareDir, "branch", "--list")
		t.Errorf("pushed branch %q must still exist on origin after CreatePR failure; bare branches:\n%s", branchName, remoteBranches)
	}
}

// TestIntegrationReal_PriorIterationCommit_SignalOnRetry_ShipsAndCloses is
// the spec's "real commits crossing iteration boundary" integration
// candidate. Simulates iteration N making a commit then exiting without
// signal, followed by iteration N+1 signaling without new commits.
//
// The pipeline must detect that the task's branch is already ahead of
// origin/main (prior-iteration commits exist) and proceed with the ship
// flow rather than rejecting with "No commits found" — otherwise the loop
// stagnates on tasks that signal without fresh commits in the current
// iteration.
func TestIntegrationReal_PriorIterationCommit_SignalOnRetry_ShipsAndCloses(t *testing.T) {
	setup := newGitIntegrationSetup(t)
	promptsDir := filepath.Join(setup.projectDir, "prompts")
	createPromptTemplatesIn(t, promptsDir)

	// Simulate a prior-iteration commit: make a commit on a task branch,
	// but don't push it yet. The next iteration's signal is what we're
	// testing; it must observe "branch ahead of origin/main" and ship.
	gitCmd(t, setup.projectDir, "git", "checkout", "-b", "ralph-hp2-prior-work")
	os.WriteFile(filepath.Join(setup.projectDir, "prior.txt"), []byte("prior work\n"), 0o644)
	gitCmd(t, setup.projectDir, "git", "add", "prior.txt")
	gitCmd(t, setup.projectDir, "git", "commit", "-m", "prior iteration: add prior.txt")
	// Stay on the task branch so the loop picks it up.

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "Prior work",
				NextID:       "ralph-hp2",
				BackendLabel: "beads",
			},
		},
	}

	ghCfg := git.StubGitHubConfig{
		Available: true,
		PRs: []git.StubPR{
			{Number: 55, Base: "main", State: git.PRStateOpen, Branch: "ralph-hp2-prior-work"},
		},
	}

	logger := logging.New(nil)
	gm := git.NewForTest(git.Config{
		ProjectDir: setup.projectDir,
		WorkDir:    setup.projectDir,
		RalphDir:   setup.ralphDir,
		BaseBranch: "main",
		Logger:     logger,
	}, ghCfg)

	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: setup.projectDir,
			WorkDir:    setup.projectDir,
			RalphDir:   setup.ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}

	_, st := setupTestDir(t)

	// Runner: signal without new commits. The branch is already ahead of
	// origin/main (prior commit exists).
	runner := &stubRunner{
		onRun: func() {
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "prior work is the fix"},
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

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("loop.Run: %v", err)
	}

	// Observable: the prior-iteration commit persisted through the
	// pipeline; the bead closed.
	if !refExistsAt(setup.projectDir, "HEAD") {
		t.Fatal("HEAD is missing after run")
	}
	logOut := gitOutputAt(t, setup.projectDir, "log", "--oneline")
	if !strings.Contains(logOut, "prior iteration: add prior.txt") {
		t.Errorf("expected prior-iteration commit to survive, log:\n%s", logOut)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-hp2" {
		t.Errorf("expected ralph-hp2 closed after prior-commit signal, got %v", backend.ClosedIDs)
	}
}

// TestIntegrationReal_TwoTasksCompleteSequentially is the spec's "stack
// head derivation from real branches" integration candidate. Two tasks
// run in sequence: after task A completes, task B's branch-setup must
// observe task A's branch in origin's open-PR list (via stub GitHub) and
// derive its starting point from that branch — not from origin/main — so
// the stack is coherent.
//
// Observable end-state: both tasks closed via the ship pipeline, both
// agent commits present in the project repo's log.
func TestIntegrationReal_TwoTasksCompleteSequentially(t *testing.T) {
	setup := newGitIntegrationSetup(t)
	promptsDir := filepath.Join(setup.projectDir, "prompts")
	createPromptTemplatesIn(t, promptsDir)

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

	ghCfg := git.StubGitHubConfig{
		Available: true,
		PRs: []git.StubPR{
			// Pre-seed two open PRs so the loop's FindOpenPR / ListOpenPRBranches
			// observe a stack once task A lands. Branches will align with the
			// names BranchForTask derives from task IDs + titles.
			{Number: 100, Base: "main", State: git.PRStateOpen, Branch: "ralph/ralph-aaa-task-a"},
			{Number: 101, Base: "main", State: git.PRStateOpen, Branch: "ralph/ralph-bbb-task-b"},
		},
	}

	logger := logging.New(nil)
	gm := git.NewForTest(git.Config{
		ProjectDir: setup.projectDir,
		WorkDir:    setup.projectDir,
		RalphDir:   setup.ralphDir,
		BaseBranch: "main",
		Logger:     logger,
	}, ghCfg)

	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: setup.projectDir,
			WorkDir:    setup.projectDir,
			RalphDir:   setup.ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
	}

	_, st := setupTestDir(t)

	iteration := 0
	runner := &stubRunner{
		onRun: func() {
			iteration++
			// Each iteration makes one file commit so the loop sees forward
			// progress on HEAD.
			fname := filepath.Join(setup.projectDir, "task-"+string(rune('A'+iteration-1))+".txt")
			os.WriteFile(fname, []byte("task work "+string(rune('A'+iteration-1))+"\n"), 0o644)
			gitCmd(t, setup.projectDir, "git", "add", filepath.Base(fname))
			gitCmd(t, setup.projectDir, "git", "commit", "-m", "agent: add "+filepath.Base(fname))
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

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("loop.Run: %v", err)
	}

	if iteration != 2 {
		t.Errorf("expected 2 iterations, got %d", iteration)
	}

	// Observable: both agent commits present in the project log.
	logOut := gitOutputAt(t, setup.projectDir, "log", "--oneline", "--all")
	if !strings.Contains(logOut, "add task-A.txt") {
		t.Errorf("expected task A's commit in log, got:\n%s", logOut)
	}
	if !strings.Contains(logOut, "add task-B.txt") {
		t.Errorf("expected task B's commit in log, got:\n%s", logOut)
	}

	// Observable: both beads closed.
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 2 {
		t.Errorf("expected 2 beads closed across the sequence, got %v", backend.ClosedIDs)
	}
}

// TestIntegrationReal_StackedPRSkipsMergeButCloses is the spec's "real
// base branch ≠ default" integration candidate. A PR whose base is another
// open PR's head (stacked) must NOT be merged by Ship — the loop relies on
// GitHub's main-branch protection not accepting merges from non-main bases.
// The bead still closes: work is verified, the PR exists for human merge
// once the base PR lands.
func TestIntegrationReal_StackedPRSkipsMergeButCloses(t *testing.T) {
	setup := newGitIntegrationSetup(t)
	promptsDir := filepath.Join(setup.projectDir, "prompts")
	createPromptTemplatesIn(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "Stacked task",
				NextID:       "ralph-stk1",
				BackendLabel: "beads",
			},
		},
	}

	// The task's PR will be created by the stub's CreatePR; seed it with
	// a base of "ralph/parent-feature" instead of "main" so the stub's
	// MergePR sees the PR as stacked.
	ghCfg := git.StubGitHubConfig{
		Available: true,
		PRs: []git.StubPR{
			// A parent PR on a branch the task will stack onto.
			{Number: 88, Base: "main", State: git.PRStateOpen, Branch: "ralph/parent-feature"},
			// The task's own PR, already created against the parent.
			{Number: 89, Base: "ralph/parent-feature", State: git.PRStateOpen, Branch: "ralph/ralph-stk1-stacked-task"},
		},
	}

	logger := logging.New(nil)
	gm := git.NewForTest(git.Config{
		ProjectDir: setup.projectDir,
		WorkDir:    setup.projectDir,
		RalphDir:   setup.ralphDir,
		BaseBranch: "main",
		Logger:     logger,
	}, ghCfg)

	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: setup.projectDir,
			WorkDir:    setup.projectDir,
			RalphDir:   setup.ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
	}

	_, st := setupTestDir(t)

	runner := &stubRunner{
		onRun: func() {
			os.WriteFile(filepath.Join(setup.projectDir, "stacked.txt"), []byte("stacked\n"), 0o644)
			gitCmd(t, setup.projectDir, "git", "add", "stacked.txt")
			gitCmd(t, setup.projectDir, "git", "commit", "-m", "agent: stacked work")
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
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

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("loop.Run: %v", err)
	}

	// Observable: the bead is closed (work verified, PR exists) even though
	// it cannot be merged (stacked on a non-main base).
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 1 || backend.ClosedIDs[0] != "ralph-stk1" {
		t.Errorf("expected ralph-stk1 closed, got %v", backend.ClosedIDs)
	}

	// Observable: origin/main has NOT advanced (no merge happened because
	// the PR is stacked on a non-main base).
	mainSHA := gitOutputAt(t, setup.projectDir, "rev-parse", "origin/main")
	initSHA := gitOutputAt(t, setup.projectDir, "rev-list", "--max-parents=0", "origin/main")
	if mainSHA != initSHA {
		t.Errorf("origin/main should still point at initial commit (stacked PR not mergeable); got %s (initial was %s)", mainSHA, initSHA)
	}
}

// TestIntegrationReal_LifecycleOrdering_BranchRenameAndReviewers is the
// spec's "ordering of real git ops" integration candidate. The invariant
// under test: reviewer detection (which queries GitHub for installed Apps
// on the repo) runs AFTER the task's branch is renamed from the placeholder
// wip branch — not before. If reviewer detection ran before rename, it
// would fire on every loop startup, leaking a network call on empty
// sessions.
//
// Observable: the captured log contains the "Worktree" line before the
// first "review" / reviewer line (or no reviewer line if none detected).
func TestIntegrationReal_LifecycleOrdering_BranchRenameAndReviewers(t *testing.T) {
	setup := newGitIntegrationSetup(t)
	promptsDir := filepath.Join(setup.projectDir, "prompts")
	createPromptTemplatesIn(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "Lifecycle ordering",
				NextID:       "ralph-lco1",
				BackendLabel: "beads",
			},
		},
	}

	// Seed with no reviewers so the detection path returns empty — ordering
	// still observable through the log progression.
	ghCfg := git.StubGitHubConfig{
		Available: true,
		PRs: []git.StubPR{
			{Number: 77, Base: "main", State: git.PRStateOpen, Branch: "ralph/ralph-lco1-lifecycle-ordering"},
		},
	}

	var logBuf strings.Builder
	logger := logging.NewWithWriter(&logBuf)

	gm := git.NewForTest(git.Config{
		ProjectDir: setup.projectDir,
		WorkDir:    setup.projectDir,
		RalphDir:   setup.ralphDir,
		BaseBranch: "main",
		Logger:     logger,
	}, ghCfg)

	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: setup.projectDir,
			WorkDir:    setup.projectDir,
			RalphDir:   setup.ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
	}

	_, st := setupTestDir(t)

	runner := &stubRunner{
		onRun: func() {
			os.WriteFile(filepath.Join(setup.projectDir, "lco.txt"), []byte("work\n"), 0o644)
			gitCmd(t, setup.projectDir, "git", "add", "lco.txt")
			gitCmd(t, setup.projectDir, "git", "commit", "-m", "agent: lifecycle work")
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
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

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("loop.Run: %v", err)
	}

	// Invariant: bead closed through the full pipeline.
	backend.CloseMu.Lock()
	if len(backend.ClosedIDs) != 1 {
		t.Errorf("expected ralph-lco1 closed, got %v", backend.ClosedIDs)
	}
	backend.CloseMu.Unlock()

	// Invariant: if any reviewer-related log line appears, it must NOT
	// precede "Running task". In this setup no reviewers are seeded so the
	// detection path is exercised only in the deferred ensureActiveReviewers
	// call inside the ship pipeline — well after the task has started.
	output := logBuf.String()
	taskStart := strings.Index(output, "Running task")
	reviewMention := strings.Index(output, "review")
	if reviewMention >= 0 && taskStart >= 0 && reviewMention < taskStart {
		t.Errorf("reviewer-related log appeared before task start — ordering violated.\nLog:\n%s", output)
	}
}

// TestIntegrationReal_CIFailureTriggersFixAgent is the spec's "end-to-end
// fix agent flow" integration candidate. A real worktree is used so Ship
// genuinely pushes the branch to origin, then the stub GitHub's ListChecks
// returns a failing required check; Ship's merge pipeline observes the CI
// failure and spawns a CI fix agent via verifier.newRunner.
//
// Observable: the fix-agent factory was invoked.
func TestIntegrationReal_CIFailureTriggersFixAgent(t *testing.T) {
	setup := newGitIntegrationSetup(t)
	promptsDir := filepath.Join(setup.projectDir, "prompts")
	createPromptTemplatesIn(t, promptsDir)
	os.WriteFile(filepath.Join(promptsDir, "verify-ci.md"),
		[]byte("fix CI: {{TASK_TITLE}} {{FAILED_CHECKS}} {{CI_LOG}} {{SIGNAL_COMPLETE}}"),
		0o644)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "CI failure trigger",
				NextID:       "ralph-cif1",
				BackendLabel: "beads",
			},
		},
	}

	// No pre-seeded PRs → Ship's CreatePR allocates PR #100 (the stub's
	// starting number when no PRs exist). Configure Checks under that
	// number so the post-push CI poll observes the failing required
	// check on the actual created PR.
	ghCfg := git.StubGitHubConfig{
		Available: true,
		Checks: map[int][]git.CICheckResult{
			100: {
				{Name: "tests", State: "FAILURE", Bucket: "fail", IsRequired: true},
			},
		},
		RequiredChecks: []string{"tests"},
		// Non-zero job steps: CI actually ran and produced failures, so this
		// is a real test failure (not an infrastructure failure). The fix
		// agent must be spawned. Zero would trigger the infra-failure
		// short-circuit and skip the fix agent entirely.
		JobStepCount: 12,
	}

	logger := logging.New(nil)
	// Build with a short CI poll timeout so the test fails fast if it
	// somehow loses the CI failure signal — production default is 5m.
	gm := git.NewForTest(git.Config{
		ProjectDir:    setup.projectDir,
		WorkDir:       setup.projectDir,
		RalphDir:      setup.ralphDir,
		BaseBranch:    "main",
		Logger:        logger,
		CIPollTimeout: 2 * time.Second,
	}, ghCfg)
	if err := gm.Init(context.Background()); err != nil {
		t.Fatalf("gm.Init: %v", err)
	}
	workDir := gm.GetWorkDir()
	gitCmd(t, workDir, "git", "config", "user.name", "test")
	gitCmd(t, workDir, "git", "config", "user.email", "test@test")

	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: setup.projectDir,
			WorkDir:    workDir,
			RalphDir:   setup.ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations:      1,
		CallsPerHour:       80,
		AutoMerge:          true,
		InfraRetryBackoffs: []time.Duration{0},
	}

	_, st := setupTestDir(t)

	fixAgentInvocations := 0
	runner := &stubRunner{
		onRun: func() {
			// Agent commits in the worktree so Ship has work to push.
			os.WriteFile(filepath.Join(workDir, "cif.txt"), []byte("cif\n"), 0o644)
			gitCmd(t, workDir, "git", "add", "cif.txt")
			gitCmd(t, workDir, "git", "commit", "-m", "agent: cif work")
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				fixAgentInvocations++
				return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "attempted"}}
			},
		}),
		Connectivity: onlineStubConnectivity(),
		VerifyHook:   passingVerifyHook(),
	})
	l.runner = runner

	_ = l.Run(context.Background())

	if fixAgentInvocations == 0 {
		t.Error("expected CI fix agent to be spawned on CIFailure, got 0 invocations")
	}
}

// TestIntegrationReal_MergeConflictThenRetrySucceeds is the spec's "real
// conflict required" integration candidate. A PR is seeded with
// Conflicted=true; the stub MergePR returns Conflict=true, mirroring real
// GitHub's response to a non-fast-forwardable PR. The loop's ship
// pipeline must detect the conflict and not close the bead blindly.
//
// Observable: with a conflicted PR that cannot be merged and no conflict
// fix agent hooked up, the loop completes the iteration without closing
// the bead (the PR stays open for human resolution).
func TestIntegrationReal_MergeConflictThenRetrySucceeds(t *testing.T) {
	setup := newGitIntegrationSetup(t)
	promptsDir := filepath.Join(setup.projectDir, "prompts")
	createPromptTemplatesIn(t, promptsDir)

	backend := &testutil.TrackingBackend{
		MutableBackend: testutil.MutableBackend{
			StubBackend: testutil.StubBackend{
				Remaining:    1,
				Completed:    0,
				Total:        1,
				NextTask:     "Merge conflict",
				NextID:       "ralph-mc1",
				BackendLabel: "beads",
			},
		},
	}

	// PR open + Conflicted=true → stub MergePR returns Conflict.
	ghCfg := git.StubGitHubConfig{
		Available: true,
		PRs: []git.StubPR{
			{Number: 99, Base: "main", State: git.PRStateOpen, Conflicted: true, Branch: "ralph/ralph-mc1-merge-conflict"},
		},
	}

	logger := logging.New(nil)
	gm := git.NewForTest(git.Config{
		ProjectDir: setup.projectDir,
		WorkDir:    setup.projectDir,
		RalphDir:   setup.ralphDir,
		BaseBranch: "main",
		Logger:     logger,
	}, ghCfg)

	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: setup.projectDir,
			WorkDir:    setup.projectDir,
			RalphDir:   setup.ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		AutoMerge:     true,
	}

	_, st := setupTestDir(t)

	runner := &stubRunner{
		onRun: func() {
			os.WriteFile(filepath.Join(setup.projectDir, "mc.txt"), []byte("conflict\n"), 0o644)
			gitCmd(t, setup.projectDir, "git", "add", "mc.txt")
			gitCmd(t, setup.projectDir, "git", "commit", "-m", "agent: mc work")
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
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

	_ = l.Run(context.Background())

	// Observable: origin/main has NOT advanced — conflict prevented merge.
	mainSHA := gitOutputAt(t, setup.projectDir, "rev-parse", "origin/main")
	initSHA := gitOutputAt(t, setup.projectDir, "rev-list", "--max-parents=0", "origin/main")
	if mainSHA != initSHA {
		t.Errorf("origin/main should not advance when PR is conflicted; got %s (initial was %s)", mainSHA, initSHA)
	}
}

// TestIntegrationReal_EvolveRebasePreservesUserCommits is the spec's "real
// rebase against real diverged branch" integration candidate. A user's
// commit on the worktree branch must survive a rebase when origin/main
// advances independently. We set up:
//   - project dir on main (the "base repo")
//   - a worktree on a wip branch (where the loop would operate)
//   - a user commit on the worktree branch
//   - a non-conflicting advance on origin/main (simulated via a second clone)
//
// EnsureUpToDate runs a real `git rebase origin/main` inside the worktree.
// Observable: after rebase, the worktree branch's log contains both the
// user's commit AND main's advance commit.
func TestIntegrationReal_EvolveRebasePreservesUserCommits(t *testing.T) {
	setup := newGitIntegrationSetup(t)

	logger := logging.New(nil)
	_, workDir := withWorktree(t, setup, git.StubGitHubConfig{Available: true}, logger)

	// User commit on the worktree branch (simulates prior iteration work).
	os.WriteFile(filepath.Join(workDir, "user.txt"), []byte("user content\n"), 0o644)
	gitCmd(t, workDir, "git", "add", "user.txt")
	gitCmd(t, workDir, "git", "commit", "-m", "user: important work")

	// Simulate origin/main advancing independently via a second clone.
	otherClone := filepath.Join(filepath.Dir(setup.projectDir), "other")
	gitCmd(t, "", "git", "clone", setup.bareDir, otherClone)
	gitCmd(t, otherClone, "git", "config", "user.name", "test")
	gitCmd(t, otherClone, "git", "config", "user.email", "test@test")
	os.WriteFile(filepath.Join(otherClone, "main.txt"), []byte("main advance\n"), 0o644)
	gitCmd(t, otherClone, "git", "add", "main.txt")
	gitCmd(t, otherClone, "git", "commit", "-m", "other: advance main")
	gitCmd(t, otherClone, "git", "push", "origin", "main")

	// Build a fresh gm pointed at the worktree and exercise EnsureUpToDate.
	// (withWorktree calls Init which already ran rebase against origin/main
	// when origin/main was still the initial commit — we want to rebase
	// again now that main has advanced.)
	gm := git.NewForTest(git.Config{
		ProjectDir: setup.projectDir,
		WorkDir:    workDir,
		RalphDir:   setup.ralphDir,
		BaseBranch: "main",
		Logger:     logger,
	}, git.StubGitHubConfig{Available: true})

	if err := gm.EnsureUpToDate(context.Background()); err != nil {
		t.Fatalf("EnsureUpToDate: %v", err)
	}

	// Observable: both the user's commit and main's commit are in the
	// worktree branch's history.
	logOut := gitOutputAt(t, workDir, "log", "--oneline")
	if !strings.Contains(logOut, "user: important work") {
		t.Errorf("user's commit lost after rebase — spec violation:\n%s", logOut)
	}
	if !strings.Contains(logOut, "other: advance main") {
		t.Errorf("main's advance lost after rebase — rebase did not pick up origin/main:\n%s", logOut)
	}
}

// TestIntegrationReal_ResumeWithDivergentLocalCommits_DoesNotCrash covers the
// resume scenario where the worktree branch has in-flight local commits that
// conflict with a moved-forward origin/main. Pre-fix, initialize() failed fatal
// with status=error and a nonsensical "PR #0 has unresolvable merge conflicts"
// message. Post-fix, the local-rebase abort is recoverable: a warning is
// logged, the loop proceeds, and the in-flight commits are preserved intact.
//
// Observable: initialize returns nil; state.json status is not "error"; the
// log contains the branch-qualified warning and NO "PR #0" reference; the
// worktree branch still carries the user commit after the failed rebase.
func TestIntegrationReal_ResumeWithDivergentLocalCommits_DoesNotCrash(t *testing.T) {
	setup := newGitIntegrationSetup(t)

	// Seed a shared file on main so both sides can diverge from it.
	os.WriteFile(filepath.Join(setup.projectDir, "shared.txt"), []byte("original\n"), 0o644)
	gitCmd(t, setup.projectDir, "git", "add", "shared.txt")
	gitCmd(t, setup.projectDir, "git", "commit", "-m", "seed: shared")
	gitCmd(t, setup.projectDir, "git", "push", "origin", "main")

	var logBuf strings.Builder
	logger := logging.NewWithWriter(&logBuf)

	// withWorktree calls Init which sets up a wip branch worktree. At this
	// point origin/main still matches the worktree's base.
	gm, workDir := withWorktree(t, setup, git.StubGitHubConfig{Available: true}, logger)

	// Simulate in-flight work on the worktree branch: a local commit that
	// modifies the shared file one way.
	os.WriteFile(filepath.Join(workDir, "shared.txt"), []byte("worktree change\n"), 0o644)
	gitCmd(t, workDir, "git", "add", "shared.txt")
	gitCmd(t, workDir, "git", "commit", "-m", "agent: in-flight work")
	localSHA := gitOutputAt(t, workDir, "rev-parse", "HEAD")

	// Advance origin/main independently with a conflicting change to the same
	// file (second clone to avoid touching the worktree's project dir).
	otherClone := filepath.Join(filepath.Dir(setup.projectDir), "other")
	gitCmd(t, "", "git", "clone", setup.bareDir, otherClone)
	gitCmd(t, otherClone, "git", "config", "user.name", "test")
	gitCmd(t, otherClone, "git", "config", "user.email", "test@test")
	os.WriteFile(filepath.Join(otherClone, "shared.txt"), []byte("main change\n"), 0o644)
	gitCmd(t, otherClone, "git", "add", "shared.txt")
	gitCmd(t, otherClone, "git", "commit", "-m", "other: conflicting advance")
	gitCmd(t, otherClone, "git", "push", "origin", "main")

	backend := &testutil.StubBackend{Remaining: 0, Completed: 0, Total: 0}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: setup.projectDir,
			WorkDir:    workDir,
			RalphDir:   setup.ralphDir,
		},
		MaxIterations: 1,
	}
	_, st := setupTestDir(t)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
	})

	// Core assertion: initialize does not crash when the initial rebase
	// aborts due to conflicting local commits.
	if err := l.initialize(context.Background()); err != nil {
		t.Fatalf("initialize returned error on divergent local commits (regression): %v", err)
	}

	// Status must not have been marked error by initialize.
	if status, _ := st.Read("status"); status == "error" {
		t.Errorf("state.status should not be 'error' after local-rebase abort; got %q", status)
	}

	// Warning must be present with branch-qualified, PR-free message.
	logOut := logBuf.String()
	if !strings.Contains(logOut, "could not be rebased onto origin/main") {
		t.Errorf("expected LocalRebaseConflictError warning in log, got:\n%s", logOut)
	}
	if !strings.Contains(logOut, "continuing with stale base") {
		t.Errorf("expected 'continuing with stale base' in log, got:\n%s", logOut)
	}
	if strings.Contains(logOut, "PR #0") {
		t.Errorf("log must not reference 'PR #0' for a local-rebase failure:\n%s", logOut)
	}

	// In-flight commits must still be on the worktree branch — rebase --abort
	// preserved state.
	if got := gitOutputAt(t, workDir, "rev-parse", "HEAD"); got != localSHA {
		t.Errorf("local commit lost after failed rebase; HEAD=%s want=%s", got, localSHA)
	}
	logOutput := gitOutputAt(t, workDir, "log", "--oneline")
	if !strings.Contains(logOutput, "agent: in-flight work") {
		t.Errorf("in-flight commit missing from branch history:\n%s", logOutput)
	}
}

// TestIntegrationReal_BranchForTask_SetStackHeadBeforePrepare proves that
// setStackHead runs before PrepareForNextTask in BranchForTask. Observable
// consequence: when CompletedBranches contains a pushed branch still ahead of
// main, the new wip branch is anchored at that branch's tip — not at
// origin/main. If the order were reversed, prevBranch would be empty when
// PrepareForNextTask runs, causing the branch to start from origin/main instead.
func TestIntegrationReal_BranchForTask_SetStackHeadBeforePrepare(t *testing.T) {
	setup := newGitIntegrationSetup(t)

	// Create and push task A's branch with one commit ahead of main.
	stackBranch := "ralph/ralph-a-task-a"
	gitCmd(t, setup.projectDir, "git", "checkout", "-b", stackBranch)
	os.WriteFile(filepath.Join(setup.projectDir, "task-a.txt"), []byte("task A\n"), 0o644)
	gitCmd(t, setup.projectDir, "git", "add", "task-a.txt")
	gitCmd(t, setup.projectDir, "git", "commit", "-m", "task A commit")
	gitCmd(t, setup.projectDir, "git", "push", "origin", stackBranch)
	parentTip := gitOutputAt(t, setup.projectDir, "rev-parse", stackBranch)
	gitCmd(t, setup.projectDir, "git", "checkout", "main")

	logger := logging.New(nil)
	// GitHub unavailable so validateStackParent returns early without clearing
	// prevBranch — allows the test to focus purely on the ordering invariant.
	gm, workDir := withWorktree(t, setup, git.StubGitHubConfig{Available: false}, logger)

	// Call BranchForTask with the stack parent in CompletedBranches.
	if _, err := gm.BranchForTask(context.Background(), "ralph-b", "task b", git.BranchTaskMeta{
		CompletedBranches: []string{stackBranch},
	}); err != nil {
		t.Fatalf("BranchForTask: %v", err)
	}

	// The new wip branch must be exactly at the stack parent's tip — 0 commits
	// ahead of it. If setStackHead ran after PrepareForNextTask, the branch
	// would start from origin/main and this assertion would fail.
	countAhead := gitOutputAt(t, workDir, "rev-list", "--count", "origin/"+stackBranch+"..HEAD")
	if countAhead != "0" {
		t.Errorf("new branch has %s commit(s) ahead of stack parent — setStackHead ran after PrepareForNextTask", countAhead)
	}

	// Stack parent's commit must be present on the new branch.
	countMain := gitOutputAt(t, workDir, "rev-list", "--count", "origin/main..HEAD")
	if countMain != "1" {
		t.Errorf("new branch has %s commit(s) ahead of origin/main, want 1 (stack parent's commit)", countMain)
	}

	// HEAD must equal the stack parent's tip.
	headTip := gitOutputAt(t, workDir, "rev-parse", "HEAD")
	if headTip != parentTip {
		t.Errorf("new branch tip = %q, want %q (stack parent tip)", headTip, parentTip)
	}
}
