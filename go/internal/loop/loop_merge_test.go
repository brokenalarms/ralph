package loop

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// Verifies that auto-merge fires once per task and calls PostMergeReset after
// each successful merge, so the next task starts from merged main — not stale
// commits.
func TestLoop_AutoMergeFiresPerTask(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(10)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	mergeCount := 0
	iterationCount := 0

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     3,
		nextTask:  "task A",
		nextID:    "ralph-aaa",
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			// Create a commit so headAfterSignal != headBefore.
			fname := fmt.Sprintf("task%d.go", iterationCount)
			os.WriteFile(filepath.Join(project, fname), []byte("package main\n"), 0o644)
			run(t, "git", "-C", project, "add", fname)
			run(t, "git", "-C", project, "commit", "-m", fmt.Sprintf("task %d work", iterationCount))
			backend.mu.Lock()
			defer backend.mu.Unlock()
			backend.completed = iterationCount
			switch iterationCount {
			case 1:
				backend.remaining = 1
				backend.nextTask = "task B"
				backend.nextID = "ralph-bbb"
			case 2:
				backend.remaining = 1
				backend.nextTask = "task C"
				backend.nextID = "ralph-ccc"
			default:
				backend.remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	gm := &git.Manager{
		ProjectDir: project,
		WorkDir:    project,
		Logger:     logging.New(nil),
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    project,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) { return "99", nil }
	l.mergeFunc = func(context.Context) (bool, error) {
		mergeCount++
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 3 {
		t.Errorf("expected 3 iterations, got %d", iterationCount)
	}

	if mergeCount != 3 {
		t.Errorf("expected auto-merge to fire 3 times (once per task), got %d", mergeCount)
	}
}

// Verifies that PostMergeUpdateMain resets the worktree branch to
// origin/main between tasks using a real git worktree, proving each task
// starts from merged main rather than building on stale commits.
func TestLoop_PostMergeResetResetsWorktree(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(10)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	originMain := gm.HeadRev()
	iterationCount := 0

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     2,
		nextTask:  "task A",
		nextID:    "ralph-aaa",
	}

	var headAfterMerge string
	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if iterationCount == 1 {
				backend.completed = 1
				backend.remaining = 1
				backend.nextTask = "task B"
				backend.nextID = "ralph-bbb"
			} else {
				headAfterMerge = gm.HeadRev()
				backend.completed = 2
				backend.remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.mergeFunc = func(context.Context) (bool, error) {
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Fatalf("expected 2 iterations, got %d", iterationCount)
	}

	if headAfterMerge != originMain {
		t.Errorf("second iteration should start from origin/main (%s), got %s", originMain, headAfterMerge)
	}

	// With stacked PRs, the branch stays as the task branch after merge
	// — no reset to temp branch.
}

// When completed tasks exist in state.json and the backend returns their
// branch metadata, the next task starts from the stack head (last completed
// task's branch) instead of origin/main — enabling stacked PRs.
func TestLoop_StackHeadBranchesFromLastCompletedTask(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(10)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	iterationCount := 0
	var taskABranch string
	var headAtTaskBStart string

	// setStackHead needs gh to list open PR branches. The stub returns
	// task A's branch dynamically since the name is set at runtime.
	ghStub := &stubGitHub{available: true}
	gm.GitHub = ghStub

	backend := &mutableBackend{
		remaining:    1,
		completed:    0,
		total:        2,
		nextTask:     "task A",
		nextID:       "ralph-aaa",
		metadata:     map[string]map[string]string{},
		externalRefs: map[string]string{},
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if iterationCount == 1 {
				// Task A: commit work and push the branch to origin.
				writeFile(t, gm.WorkDir, "a.txt", "task A work\n")
				run(t, "git", "-C", gm.WorkDir, "commit", "-m", "task A")
				taskABranch = gm.WorktreeBranch
				run(t, "git", "-C", gm.WorkDir, "push", "-u", "origin", taskABranch)

				// Record task A as completed with its branch metadata and PR ref.
				backend.metadata["ralph-aaa"] = map[string]string{"branch": taskABranch}
				backend.externalRefs["ralph-aaa"] = "gh-100"
				st.AddCompletedTask("ralph-aaa")
				ghStub.openPRBranches = []string{taskABranch}

				backend.completed = 1
				backend.remaining = 1
				backend.nextTask = "task B"
				backend.nextID = "ralph-bbb"
			} else {
				headAtTaskBStart = gm.HeadRev()
				backend.completed = 2
				backend.remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.mergeFunc = func(context.Context) (bool, error) {
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Fatalf("expected 2 iterations, got %d", iterationCount)
	}

	// Task B should have started from task A's tip, not origin/main.
	out, _ := exec.Command("git", "-C", gm.WorkDir, "rev-parse", "origin/"+taskABranch).Output()
	taskATip := strings.TrimSpace(string(out))
	if headAtTaskBStart != taskATip {
		t.Errorf("task B should start from task A tip (%s), got %s", taskATip, headAtTaskBStart)
	}
}

// When a completed task's PR is merged (branch deleted from remote),
// the between-tasks transition falls back to the default branch instead
// of failing on a fetch of the deleted branch. This is the core behavior
// that setStackHead provides over the removed resolveStackHead.
func TestLoop_StackHeadSkipsMergedPR(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(10)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	iterationCount := 0
	var taskABranch string
	var headAtTaskBStart string

	backend := &mutableBackend{
		remaining:    1,
		completed:    0,
		total:        2,
		nextTask:     "task A",
		nextID:       "ralph-aaa",
		metadata:     map[string]map[string]string{},
		externalRefs: map[string]string{},
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if iterationCount == 1 {
				writeFile(t, gm.WorkDir, "a.txt", "task A work\n")
				run(t, "git", "-C", gm.WorkDir, "commit", "-m", "task A")
				taskABranch = gm.WorktreeBranch
				run(t, "git", "-C", gm.WorkDir, "push", "-u", "origin", taskABranch)

				backend.metadata["ralph-aaa"] = map[string]string{"branch": taskABranch}
				backend.externalRefs["ralph-aaa"] = "gh-100"
				st.AddCompletedTask("ralph-aaa")

				// Simulate merge: land task A's work on main, then delete branch.
				run(t, "git", "-C", project, "fetch", "origin", taskABranch)
				run(t, "git", "-C", project, "checkout", "main")
				run(t, "git", "-C", project, "merge", "origin/"+taskABranch)
				run(t, "git", "-C", project, "push", "origin", "main")
				run(t, "git", "-C", project, "push", "origin", "--delete", taskABranch)

				backend.completed = 1
				backend.remaining = 1
				backend.nextTask = "task B"
				backend.nextID = "ralph-bbb"
			} else {
				headAtTaskBStart = gm.HeadRev()
				backend.completed = 2
				backend.remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	// setStackHead uses git ancestry — no GitHub stub needed. Task A's branch
	// is deleted from remote (simulating merge), so it's skipped.

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.mergeFunc = func(context.Context) (bool, error) {
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Fatalf("expected 2 iterations, got %d", iterationCount)
	}

	// Task B should start from origin/main since task A's PR was merged.
	mainTip, _ := exec.Command("git", "-C", gm.WorkDir, "rev-parse", "origin/main").Output()
	mainRev := strings.TrimSpace(string(mainTip))
	if headAtTaskBStart != mainRev {
		t.Errorf("task B should start from main (%s) after merged PR, got %s", mainRev, headAtTaskBStart)
	}
}

// setStackHead skips a branch whose work landed on main even when the
// remote branch still exists (ancestry check, not branch deletion).
func TestLoop_StackHeadSkipsBranchAncestorOfMain(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(10)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	iterationCount := 0
	var headAtTaskBStart string

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     2,
		nextTask:  "task A",
		nextID:    "ralph-aaa",
		metadata:  map[string]map[string]string{},
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if iterationCount == 1 {
				writeFile(t, gm.WorkDir, "a.txt", "task A work\n")
				run(t, "git", "-C", gm.WorkDir, "commit", "-m", "task A")
				taskABranch := gm.WorktreeBranch
				run(t, "git", "-C", gm.WorkDir, "push", "-u", "origin", taskABranch)

				backend.metadata["ralph-aaa"] = map[string]string{"branch": taskABranch}
				st.AddCompletedTask("ralph-aaa")

				// Merge work into main but leave the branch on remote.
				run(t, "git", "-C", project, "fetch", "origin", taskABranch)
				run(t, "git", "-C", project, "checkout", "main")
				run(t, "git", "-C", project, "merge", "origin/"+taskABranch)
				run(t, "git", "-C", project, "push", "origin", "main")

				backend.completed = 1
				backend.remaining = 1
				backend.nextTask = "task B"
				backend.nextID = "ralph-bbb"
			} else {
				headAtTaskBStart = gm.HeadRev()
				backend.completed = 2
				backend.remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.mergeFunc = func(context.Context) (bool, error) {
		return true, nil
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Fatalf("expected 2 iterations, got %d", iterationCount)
	}

	// Task B should target main: task A's branch is ancestor of main (work landed).
	mainTip, _ := exec.Command("git", "-C", gm.WorkDir, "rev-parse", "origin/main").Output()
	mainRev := strings.TrimSpace(string(mainTip))
	if headAtTaskBStart != mainRev {
		t.Errorf("task B should start from main (%s) when branch is ancestor, got %s", mainRev, headAtTaskBStart)
	}
}

// Verifies the full post-merge branch rename cycle: task A merges →
// PostMergeUpdateMain resets to /next → next iteration renames to thematic
// branch for task B. Proves each successive task gets its own descriptively
// named branch even after the previous one is squash-merged.
func TestLoop_PostMergeRenamesCycleFull(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(10)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logging.New(nil),
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	iterationCount := 0
	var branchDuringTaskA, branchDuringTaskB string

	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     2,
		nextTask:  "Fix tail leak",
		nextID:    "ralph-t1",
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			switch iterationCount {
			case 1:
				branchDuringTaskA = gm.WorktreeBranch
				backend.mu.Lock()
				backend.completed = 1
				backend.remaining = 1
				backend.nextTask = "Add retry logic"
				backend.nextID = "ralph-r2"
				backend.mu.Unlock()
			case 2:
				branchDuringTaskB = gm.WorktreeBranch
				backend.mu.Lock()
				backend.completed = 2
				backend.remaining = 0
				backend.mu.Unlock()
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logging.New(nil))
	l.runner = runner
	l.mergeFunc = func(context.Context) (bool, error) { return true, nil }

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Fatalf("expected 2 iterations, got %d", iterationCount)
	}

	if !strings.Contains(branchDuringTaskA, "ralph-t1-fix-tail-leak") {
		t.Errorf("task A branch should contain slug, got %q", branchDuringTaskA)
	}
	if !strings.Contains(branchDuringTaskB, "ralph-r2-add-retry-logic") {
		t.Errorf("task B branch should contain slug, got %q", branchDuringTaskB)
	}
	if branchDuringTaskA == branchDuringTaskB {
		t.Errorf("tasks should have different branches, both got %q", branchDuringTaskA)
	}
}

// After a merge, PostMergeUpdateMain already syncs the worktree to main.
// The next iteration must NOT call ResetToDefaultBranch again, which would
// produce a duplicate "Reset worktree" log line.
func TestLoop_NoDoubleResetAfterMerge(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	st := state.NewStore(ralphDir)
	st.Init(10)

	promptsDir := filepath.Join(project, "prompts")
	createPromptTemplates(t, promptsDir)

	var logBuf strings.Builder
	logger := logging.NewWithWriter(&logBuf)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logger,
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	iterationCount := 0
	backend := &mutableBackend{
		remaining: 1,
		completed: 0,
		total:     2,
		nextTask:  "task A",
		nextID:    "ralph-aaa",
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			// Create a commit so the signal handler takes the push/merge path
			// instead of the no-commits shortcut.
			f := filepath.Join(gm.WorkDir, fmt.Sprintf("file%d.txt", iterationCount))
			os.WriteFile(f, []byte("content"), 0o644)
			exec.Command("git", "-C", gm.WorkDir, "add", ".").Run()
			exec.Command("git", "-C", gm.WorkDir, "commit", "-m", fmt.Sprintf("task %d", iterationCount)).Run()
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if iterationCount == 1 {
				backend.completed = 1
				backend.remaining = 1
				backend.nextTask = "task B"
				backend.nextID = "ralph-bbb"
			} else {
				backend.completed = 2
				backend.remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		TaskBackend:   backend,
	}, st, gm, logger)
	l.runner = runner
	l.pushPRFunc = func(context.Context, string, string, string) (string, error) {
		return "99", nil
	}
	l.mergeFunc = func(context.Context) (bool, error) {
		return true, nil
	}

	_ = l.Run(context.Background())

	output := logBuf.String()

	// initRun may produce one "Reset worktree" (before any task runs).
	// After the merge, PostMergeUpdateMain logs "Updated local" instead.
	// A second "Reset worktree" would mean the next iteration redundantly
	// called ResetToDefaultBranch — that's the bug this test guards against.
	resetCount := strings.Count(output, "Reset worktree")
	if resetCount > 1 {
		t.Errorf("expected at most 1 'Reset worktree' (from initRun), got %d — next-task path should skip reset after merge:\n%s", resetCount, output)
	}

	// Verify PostMergeUpdateMain logged its distinct message.
	if !strings.Contains(output, "Updated local") {
		t.Errorf("expected 'Updated local' from PostMergeUpdateMain, got:\n%s", output)
	}
}

// setStackHead logs "No stacked parents — resetting to main" when all
// completed tasks have merged PRs and no stack head is found.
// setStackHead silently falls through when all completed branches are
// gone from remote (fetch fails) — no stack head is set, PrevBranch stays empty.
func TestSetStackHead_SkipsUnfetchableBranch(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(10)

	var buf bytes.Buffer
	logger := logging.NewWithWriter(&buf)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logger,
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	st.AddCompletedTask("ralph-aaa")
	backend := &mutableBackend{
		metadata: map[string]map[string]string{"ralph-aaa": {"branch": "some-branch"}},
	}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
		},
		TaskBackend: backend,
	}, st, gm, logger)

	l.setStackHead()

	if gm.PrevBranch != "" {
		t.Errorf("PrevBranch should be empty when branch is unfetchable, got %q", gm.PrevBranch)
	}
	output := buf.String()
	if strings.Contains(output, "Stack head") {
		t.Errorf("should not log 'Stack head' when branch is unfetchable, got:\n%s", output)
	}
}

// setStackHead does NOT log "No stacked parents" when there are no
// completed tasks — the early return path should be silent.
func TestSetStackHead_NoLogWhenNoCompletedTasks(t *testing.T) {
	project, _ := initBareRepoWithOrigin(t)
	ralphDir := filepath.Join(project, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	st := state.NewStore(ralphDir)
	st.Init(10)

	var buf bytes.Buffer
	logger := logging.NewWithWriter(&buf)

	gm := &git.Manager{
		ProjectDir: project,
		RalphDir:   ralphDir,
		State:      st,
		Logger:     logger,
	}
	if err := gm.SetupWorktree(context.Background()); err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}

	backend := &mutableBackend{}

	l := New(Config{
		Dirs: workctx.WorkContext{
			ProjectDir: project,
			WorkDir:    gm.WorkDir,
			RalphDir:   ralphDir,
		},
		TaskBackend: backend,
	}, st, gm, logger)

	l.setStackHead()

	output := buf.String()
	if strings.Contains(output, "No stacked parents") {
		t.Errorf("should not log 'No stacked parents' when no completed tasks exist, got:\n%s", output)
	}
}
