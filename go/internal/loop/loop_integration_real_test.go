package loop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/testutil"
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
