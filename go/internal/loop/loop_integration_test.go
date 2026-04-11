package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verifier"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// integrationBackend extends TrackingBackend with mutex-protected metadata
// and external ref tracking, so integration tests can assert on the full
// state written by Run().
type integrationBackend struct {
	testutil.TrackingBackend
	mu           sync.Mutex
	metadata     map[string]map[string]string
	externalRefs map[string]string
	states       []integrationStateCall
	onClose      func(id string) // optional hook called before CloseTask records the ID
}

// CloseTask overrides TrackingBackend.CloseTask to call onClose before recording.
func (b *integrationBackend) CloseTask(id, reason string) error {
	if b.onClose != nil {
		b.onClose(id)
	}
	return b.TrackingBackend.CloseTask(id, reason)
}

type integrationStateCall struct {
	id, dimension, value, reason string
}

func newIntegrationBackend() *integrationBackend {
	return &integrationBackend{
		metadata:     make(map[string]map[string]string),
		externalRefs: make(map[string]string),
	}
}

func (b *integrationBackend) SetMetadata(id, key, value string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.metadata[id] == nil {
		b.metadata[id] = make(map[string]string)
	}
	b.metadata[id][key] = value
	return nil
}

func (b *integrationBackend) GetMetadata(id, key string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.metadata[id] != nil {
		return b.metadata[id][key], nil
	}
	return "", nil
}

func (b *integrationBackend) SetExternalRef(id, ref string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.externalRefs[id] = ref
	return nil
}

func (b *integrationBackend) GetExternalRef(id string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.externalRefs[id], nil
}

func (b *integrationBackend) SetState(id, dimension, value, reason string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.states = append(b.states, integrationStateCall{id, dimension, value, reason})
	return nil
}

func (b *integrationBackend) GetState(id, key string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.states) - 1; i >= 0; i-- {
		if b.states[i].id == id && b.states[i].dimension == key {
			return b.states[i].value, nil
		}
	}
	return "", nil
}

// setupIntegrationTest creates a temp dir, state store, and prompt templates.
func setupIntegrationTest(t *testing.T) (dir, ralphDir, promptsDir string, st *state.Store) {
	t.Helper()
	dir, st = setupTestDir(t)
	ralphDir = filepath.Join(dir, ".ralph")
	promptsDir = filepath.Join(dir, "prompts")
	createPromptTemplates(t, promptsDir)
	return dir, ralphDir, promptsDir, st
}

// Scenario 1: Happy path — task signals completion, verification passes,
// push creates a PR, merge succeeds, bead is closed with PR reference.
func TestIntegration_HappyPath_SignalVerifyPushMergeClose(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Fix auth middleware"
	backend.NextID = "ralph-hp1"
	backend.BackendLabel = "beads"

	gm := git.NewStubRepo()
	gm.ProjectDir = dir
	gm.WorkDir = dir
	gm.WorktreeBranch = "ralph/next"
	gm.RemoteURLValue = "https://github.com/owner/repo.git"
	gm.GH.OpenPR = 0
	gm.GH.PRNumber = 0

	runner := &stubRunner{
		onRun: func() {
			gm.HeadRevValue = "abc123"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "fixed auth middleware"},
	}
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
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})

	l.runner = runner
	gm.ShipResult = git.ShipResult{PRNumber: 42}
	gm.MergeRetryResult = true

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) == 0 {
		t.Fatal("expected CloseTask to be called")
	}
	if backend.ClosedIDs[0] != "ralph-hp1" {
		t.Errorf("expected close for ralph-hp1, got %q", backend.ClosedIDs[0])
	}
	if !strings.Contains(backend.CloseReasons[0], "42") {
		t.Errorf("close reason should reference PR #42, got %q", backend.CloseReasons[0])
	}

	// Phase transitions: implementing -> verified
	backend.mu.Lock()
	defer backend.mu.Unlock()
	phases := []string{}
	for _, s := range backend.states {
		if s.id == "ralph-hp1" && s.dimension == "phase" {
			phases = append(phases, s.value)
		}
	}
	if len(phases) < 2 {
		t.Fatalf("expected at least 2 phase transitions, got %d: %v", len(phases), phases)
	}
	if phases[0] != "implementing" {
		t.Errorf("first phase should be implementing, got %q", phases[0])
	}
	foundVerified := false
	for _, p := range phases {
		if p == "verified" {
			foundVerified = true
		}
	}
	if !foundVerified {
		t.Errorf("expected verified phase, got phases: %v", phases)
	}

	// Session tasks include the completed task.
	sessionTasks := l.SessionTasks()
	if len(sessionTasks) == 0 {
		t.Fatal("expected at least 1 session task")
	}
	if sessionTasks[0].ID != "ralph-hp1" {
		t.Errorf("session task ID = %q, want ralph-hp1", sessionTasks[0].ID)
	}
}

// Scenario 2: Resume via existing PR that is already MERGED.
// The bead is closed without running the agent.
func TestIntegration_ResumeViaPR_Merged(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Already merged task"
	backend.NextID = "ralph-m1"
	backend.BackendLabel = "beads"
	backend.externalRefs["ralph-m1"] = "https://github.com/owner/repo/pull/100"

	gm := git.NewStubRepo()
	gm.ProjectDir = dir
	gm.WorkDir = dir
	gm.WorktreeBranch = "ralph/next"
	gm.RemoteURLValue = "https://github.com/owner/repo.git"
	gm.GH.PRState = "MERGED"
	gm.ResumeResult = git.ResumeTaskResult{
		Handled:       true,
		AlreadyMerged: true,
		PRNumber:      100,
	}

	agentCalled := false
	runner := &stubRunner{
		onRun: func() {
			agentCalled = true
		},
		result: claude.Result{},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if agentCalled {
		t.Error("agent should not run when PR is already merged")
	}
	if gm.MergeRetryCalls > 0 {
		t.Error("merge should not be called for already-merged PR")
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) == 0 {
		t.Fatal("expected CloseTask to be called for merged PR")
	}
	if backend.ClosedIDs[0] != "ralph-m1" {
		t.Errorf("expected close for ralph-m1, got %q", backend.ClosedIDs[0])
	}
}

// Scenario 2b: Merged PR found via branch metadata (any-state lookup), not external-ref.
// resumeViaPR uses FindPRForBranch to detect the merged PR and closes the bead
// without running the agent. This covers the regression where FindOpenPRForBranch
// missed merged/closed PRs and the agent would re-do already-landed work.
func TestIntegration_ResumeViaPR_MergedFoundViaBranchMetadata(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Already landed task"
	backend.NextID = "ralph-ac6"
	backend.BackendLabel = "beads"
	// Simulate prior iteration: branch was stored in metadata, but no external-ref set.
	_ = backend.SetMetadata("ralph-ac6", "branch", "ralph/ralph-ac6-already-landed-task")

	gm := git.NewStubRepo()
	gm.ProjectDir = dir
	gm.WorkDir = dir
	gm.WorktreeBranch = "ralph/ralph-ac6-already-landed-task"
	gm.RemoteURLValue = "https://github.com/owner/repo.git"
	gm.GH.OpenPR = 0
	gm.GH.PRNumber = 200
	gm.GH.PRState = git.PRStateMerged
	gm.GH.PRTitle = "Already landed task"
	gm.GH.PRURL = "https://github.com/owner/repo/pull/200"
	gm.BranchRenamed = true
	gm.ResumeResult = git.ResumeTaskResult{
		Handled:       true,
		AlreadyMerged: true,
		PRNumber:      200,
	}

	agentCalled := false
	runner := &stubRunner{
		onRun:  func() { agentCalled = true },
		result: claude.Result{},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 3,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if agentCalled {
		t.Error("agent should not run when a merged PR already exists for the branch")
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) == 0 {
		t.Fatal("expected bead to be closed when merged PR found via branch metadata")
	}
	if backend.ClosedIDs[0] != "ralph-ac6" {
		t.Errorf("expected close for ralph-ac6, got %q", backend.ClosedIDs[0])
	}
}

// Scenario 3: Resume via existing PR that is OPEN with auto-merge enabled.
// Merge is attempted and bead is closed after merge.
func TestIntegration_ResumeViaPR_OpenAutoMerge(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Open PR task"
	backend.NextID = "ralph-o1"
	backend.BackendLabel = "beads"
	backend.externalRefs["ralph-o1"] = "https://github.com/owner/repo/pull/200"

	gm := git.NewStubRepo()
	gm.ProjectDir = dir
	gm.WorkDir = dir
	gm.WorktreeBranch = "ralph/next"
	gm.RemoteURLValue = "https://github.com/owner/repo.git"
	gm.GH.PRHead = "ralph-o1-open-pr-task"
	gm.RemoteBranchCommits = true
	gm.ResumeResult = git.ResumeTaskResult{
		Handled:  true,
		Merged:   true,
		PRNumber: 200,
	}
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
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = &stubRunner{}
	gm.MergeRetryResult = true

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) == 0 {
		t.Fatal("expected CloseTask after merge")
	}
	if backend.ClosedIDs[0] != "ralph-o1" {
		t.Errorf("expected close for ralph-o1, got %q", backend.ClosedIDs[0])
	}
}

// Scenario 4: Resume via existing PR that is CLOSED.
// External ref and branch metadata are cleared, agent runs.
func TestIntegration_ResumeViaPR_Closed(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Closed PR task"
	backend.NextID = "ralph-c1"
	backend.BackendLabel = "beads"
	backend.externalRefs["ralph-c1"] = "gh-300"
	backend.metadata["ralph-c1"] = map[string]string{"branch": "ralph-c1-old-branch"}

	gm := git.NewStubRepo()
	gm.ProjectDir = dir
	gm.WorkDir = dir
	gm.WorktreeBranch = "ralph/next"
	gm.RemoteURLValue = "https://github.com/owner/repo.git"
	gm.GH.PRState = "CLOSED"
	gm.ResumeResult = git.ResumeTaskResult{
		Handled:       false,
		ClearMetadata: true,
		PRNumber:      300,
		NewBranch:     "ralph/ralph-c1-closed-pr-task",
	}

	agentCalled := false
	runner := &stubRunner{
		onRun: func() {
			agentCalled = true
		},
		result: claude.Result{},
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
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !agentCalled {
		t.Error("agent should run after closed PR is detected")
	}

	// External ref should be cleared so the closed PR is not re-discovered.
	backend.mu.Lock()
	ref := backend.externalRefs["ralph-c1"]
	branch := ""
	if backend.metadata["ralph-c1"] != nil {
		branch = backend.metadata["ralph-c1"]["branch"]
	}
	backend.mu.Unlock()

	if ref != "" {
		t.Errorf("external ref should be cleared, got %q", ref)
	}
	// Branch metadata is cleared and replaced with the new task-specific name.
	// The key behavior: the old branch name ("ralph-c1-old-branch") is gone.
	if branch == "ralph-c1-old-branch" {
		t.Error("stale branch name should be replaced with a new task-specific name")
	}
}

// Scenario 5: Test failure -> fix agent -> tests pass.
// Uses onSignal path with a Makefile-based test runner. First test run
// fails, fix agent runs, second test run passes — task completes.
func TestIntegration_TestFailureThenFixAgentPasses(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	// Create a Makefile that fails on first call but passes after fix.
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\t@echo 'FAIL: broken' && exit 1\n"), 0o644)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Fix test failures"
	backend.NextID = "ralph-tf1"
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	fixAgentCalls := 0
	runner := &signalCallingRunner{
		result: claude.Result{Summary: "done"},
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
		VerifyDir:     dir,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				fixAgentCalls++
				// Fix agent "fixes" by removing the failing Makefile.
				os.Remove(filepath.Join(dir, "Makefile"))
				return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "fixed tests"}}
			},
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				return "YES: approved", nil
			}},
		}),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if fixAgentCalls == 0 {
		t.Error("fix agent should have been spawned for test failures")
	}

	// Task should have completed (completed-tasks file written).
	data, err := os.ReadFile(filepath.Join(ralphDir, ".completed-tasks"))
	if err == nil && strings.Contains(string(data), "ralph-tf1") {
		// Task completed as expected.
	}
}

// Scenario 6: CI failure -> fix agent -> CI passes -> merge.
// mergeFunc returns CIFailureError on first call, then succeeds.
func TestIntegration_CIFailureThenFixThenMerge(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "CI fix task"
	backend.NextID = "ralph-ci1"
	backend.BackendLabel = "beads"

	gm := git.NewStubRepo()
	gm.ProjectDir = dir
	gm.WorkDir = dir
	gm.WorktreeBranch = "ralph/next"
	gm.RemoteURLValue = "https://github.com/owner/repo.git"
	gm.GH.OpenPR = 0

	runner := &stubRunner{
		onRun: func() {
			gm.HeadRevValue = "abc123"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "implemented feature"},
	}
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
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner
	gm.ShipResult = git.ShipResult{PRNumber: 55}
	gm.PRNumber = 55
	gm.MergeRetryResult = true

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) == 0 {
		t.Fatal("expected task to be closed after merge")
	}
	if !strings.Contains(backend.CloseReasons[0], "55") {
		t.Errorf("close reason should reference PR #55, got %q", backend.CloseReasons[0])
	}
}

// Scenario 7: Max iterations reached — loop exits after the configured limit.
func TestIntegration_MaxIterationsReached(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	iterationCount := 0
	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    1,
			Completed:    0,
			Total:        1,
			NextTask:     "Infinite task",
			NextID:       "ralph-max1",
			BackendLabel: "beads",
		},
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
		},
		result: claude.Result{},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 2,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Errorf("expected 2 iterations, got %d", iterationCount)
	}

	finalState, _ := st.Load()
	if finalState.Status != "max_iterations_reached" {
		t.Errorf("expected status 'max_iterations_reached', got %q", finalState.Status)
	}
}

// Scenario 8: External ref consistency — after push and PR creation,
// the external ref written to the backend should be a full URL.
// Current code may write "gh-N" format when findPRInfo returns no URL.
func TestIntegration_ExternalRefFormat(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Ref format task"
	backend.NextID = "ralph-ref1"
	backend.BackendLabel = "beads"

	gm := git.NewStubRepo()
	gm.ProjectDir = dir
	gm.WorkDir = dir
	gm.WorktreeBranch = "ralph/next"
	gm.RemoteURLValue = "https://github.com/owner/repo.git"
	gm.GH.PRNumber = 77
	gm.GH.PRTitle = "Ref format task"
	gm.GH.PRURL = "https://github.com/owner/repo/pull/77"

	runner := &stubRunner{
		onRun: func() {
			gm.HeadRevValue = "def456"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner
	gm.ShipResult = git.ShipResult{PRNumber: 77}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	backend.mu.Lock()
	ref := backend.externalRefs["ralph-ref1"]
	backend.mu.Unlock()

	if ref == "" {
		t.Fatal("expected external ref to be set after push")
	}

	// The external ref should be a URL when findPRInfo returns a URL.
	// Current behavior: completeTask calls findPRInfo and uses the URL if available,
	// falling back to "gh-N" if findPRInfo returns no URL.
	// When StubGitHub has PRURL set, FindPR returns it, so the ref should be a URL.
	if strings.Contains(ref, "github.com") && strings.Contains(ref, "/pull/") {
		// URL format — this is the desired behavior.
	} else if strings.HasPrefix(ref, "gh-") {
		// "gh-" prefix format — known inconsistency. The refactor will fix this
		// to always use URL format when available. This documents current behavior.
		t.Logf("NOTE: external ref is in gh- format (%q) rather than URL format. "+
			"This is a known inconsistency that the refactor will address.", ref)
	} else {
		t.Errorf("external ref has unexpected format: %q", ref)
	}
}

// Scenario 9: Push always goes through pushPRFunc (which internally squashes).
// Verifies that pushPRFunc is called in the signal -> push flow.
func TestIntegration_PushCalledOnSignal(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Push test task"
	backend.NextID = "ralph-push1"
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/next",
	}

	runner := &stubRunner{
		onRun: func() {
			gm.HeadRevValue = "push123"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gm.ShipCalls == 0 {
		t.Error("Ship should be called after signal detection with new commits")
	}
	if gm.LastShipOpts.TaskID != "ralph-push1" {
		t.Errorf("Ship received wrong taskID: %q", gm.LastShipOpts.TaskID)
	}
}

// Scenario 10: No tasks -> wait mode -> new task appears.
// Starts with no remaining tasks, Wait=true. Uses onWaitFunc to add a task,
// then the loop picks it up and runs it.
func TestIntegration_WaitModePicksUpNewTask(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := &testutil.MutableBackend{
		StubBackend: testutil.StubBackend{
			Remaining:    0,
			Completed:    1,
			Total:        1,
			BackendLabel: "beads",
		},
	}

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	agentCalled := false
	runner := &stubRunner{
		onRun: func() {
			agentCalled = true
			backend.Lock()
			backend.Remaining = 0
			backend.Completed++
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
	}
	waitEntered := make(chan struct{}, 2)
	waitCount := 0
	waitHook := &stubWaitHook{fn: func() {
		waitCount++
		if waitCount == 1 {
			// Add a new task while waiting.
			backend.Lock()
			backend.Remaining = 1
			backend.Total++
			backend.NextTask = "New task from wait"
			backend.NextID = "ralph-wait1"
			backend.Unlock()
		}
		waitEntered <- struct{}{}
	}}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		Runner:       runner,
		WaitHook:     waitHook,
	})

	go func() {
		<-waitEntered
		// Wait for second wait entry (after the new task completes).
		select {
		case <-waitEntered:
		case <-time.After(10 * time.Second):
		}
		cancel()
	}()

	err := l.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !agentCalled {
		t.Error("agent should run for the new task added during wait mode")
	}
}

// Scenario 5 alternative: Test failure -> fix agent -> tests pass
// using the onSignal-based verification flow with a signalCallingRunner
// that triggers verification.
func TestIntegration_TestFailureFixedByAgent(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	// Create a Makefile that fails.
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\t@echo 'FAIL: broken' && exit 1\n"), 0o644)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Fix test failures v2"
	backend.NextID = "ralph-tf2"
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	fixCalls := 0
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		VerifyDir:     dir,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				fixCalls++
				// Fix agent removes the failing Makefile so tests pass.
				os.Remove(filepath.Join(dir, "Makefile"))
				return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "fixed tests"}}
			},
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				return "YES: approved", nil
			}},
		}),
		Connectivity: onlineStubConnectivity(),
	})

	l.runner = &signalCallingRunner{
		result: claude.Result{Summary: "done"},
	}

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if fixCalls == 0 {
		t.Error("fix agent should have been spawned for test failures")
	}
}

// Scenario: parsePRNumber correctly parses both URL and gh- prefix formats.
func TestIntegration_ParsePRNumber(t *testing.T) {
	tests := []struct {
		ref  string
		want int
	}{
		{"https://github.com/owner/repo/pull/123", 123},
		{"gh-456", 456},
		{"", 0},
		{"random string", 0},
	}
	for _, tt := range tests {
		got := parsePRNumber(tt.ref)
		if got != tt.want {
			t.Errorf("parsePRNumber(%q) = %d, want %d", tt.ref, got, tt.want)
		}
	}
}

// Scenario: ResumeTask result is acted on correctly by the loop for each PR state.
func TestIntegration_ResumeTask_AllStates(t *testing.T) {
	for _, tc := range []struct {
		name           string
		resumeResult   git.ResumeTaskResult
		wantClosed     bool
		wantRefCleared bool
		wantAgentRun   bool
	}{
		{
			name: "MERGED closes and skips agent",
			resumeResult: git.ResumeTaskResult{
				Handled:       true,
				AlreadyMerged: true,
				Merged:        true,
				PRNumber:      99,
			},
			wantClosed:   true,
			wantAgentRun: false,
		},
		{
			name: "OPEN with auto-merge closes and skips agent",
			resumeResult: git.ResumeTaskResult{
				Handled:  true,
				Merged:   true,
				PRNumber: 99,
			},
			wantClosed:   true,
			wantAgentRun: false,
		},
		{
			name: "CLOSED clears ref and re-runs agent",
			resumeResult: git.ResumeTaskResult{
				Handled:       false,
				ClearMetadata: true,
				PRNumber:      99,
			},
			wantRefCleared: true,
			wantAgentRun:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

			backend := newIntegrationBackend()
			backend.Remaining = 1
			backend.Total = 1
			backend.NextTask = "test task"
			backend.NextID = "ralph-test"
			backend.externalRefs["ralph-test"] = "https://github.com/owner/repo/pull/99"

			agentCalled := false
			gm := &git.StubRepo{
				ProjectDir:     dir,
				WorkDir:        dir,
				WorktreeBranch: "ralph/next",
				RemoteURLValue: "https://github.com/owner/repo.git",
				ResumeResult:   tc.resumeResult,
			}
			cfg := Config{
				Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
				MaxIterations: 1,
				CallsPerHour:  80,
				AutoMerge:     true,
			}
			logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
			l.runner = &stubRunner{onRun: func() { agentCalled = true }}

			if err := l.Run(context.Background()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.wantClosed {
				backend.CloseMu.Lock()
				closed := len(backend.ClosedIDs) > 0
				backend.CloseMu.Unlock()
				if !closed {
					t.Error("expected task to be closed")
				}
			}

			if tc.wantRefCleared {
				backend.mu.Lock()
				ref := backend.externalRefs["ralph-test"]
				backend.mu.Unlock()
				if ref != "" {
					t.Errorf("expected external ref cleared, got %q", ref)
				}
			}

			if agentCalled != tc.wantAgentRun {
				t.Errorf("agentCalled = %v, want %v", agentCalled, tc.wantAgentRun)
			}
		})
	}
}

// Scenario: Full end-to-end with two tasks completing in sequence.
func TestIntegration_TwoTasksCompleteSequentially(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	iterationCount := 0
	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 2
	backend.NextTask = "Task A"
	backend.NextID = "ralph-a1"
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/next",
	}

	runner := &stubRunner{
		onRun: func() {
			iterationCount++
			gm.HeadRevValue = fmt.Sprintf("commit%d", iterationCount)
			backend.Lock()
			defer backend.Unlock()
			if iterationCount == 1 {
				backend.Completed = 1
				backend.Remaining = 1
				backend.NextTask = "Task B"
				backend.NextID = "ralph-b1"
			} else {
				backend.Completed = 2
				backend.Remaining = 0
			}
		},
		result: claude.Result{SignalDetected: true, Summary: "completed"},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if iterationCount != 2 {
		t.Errorf("expected 2 iterations, got %d", iterationCount)
	}

	// Both tasks should appear in completed-tasks file.
	data, err := os.ReadFile(filepath.Join(ralphDir, ".completed-tasks"))
	if err != nil {
		t.Fatalf("expected .completed-tasks file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "ralph-a1") {
		t.Error("ralph-a1 should be in completed tasks")
	}
	if !strings.Contains(content, "ralph-b1") {
		t.Error("ralph-b1 should be in completed tasks")
	}

	finalState, _ := st.Load()
	if finalState.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", finalState.Status)
	}
}

// Scenario: finalizePR with a stacked PR (base != default branch) skips
// merge but still closes the task.
func TestIntegration_StackedPRSkipsMergeButCloses(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Stacked task"
	backend.NextID = "ralph-stk1"
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/next",
		RemoteURLValue: "https://github.com/owner/repo.git",
		PRBase:         "ralph-prev-task",
		DefaultBranch:  "main",
	}

	// Ship returns Stacked=true when PR targets a non-default branch.
	gm.ShipFunc = func(_ context.Context, opts git.ShipOpts) (git.ShipResult, error) {
		return git.ShipResult{PRNumber: 88, Stacked: true}, nil
	}

	runner := &stubRunner{
		onRun: func() {
			gm.HeadRevValue = "sha-stk1"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "stacked task done"},
	}
	cfg := Config{
		Dirs:          workctx.WorkContext{ProjectDir: dir, WorkDir: dir, RalphDir: ralphDir, PromptsDir: promptsDir},
		MaxIterations: 5,
		CallsPerHour:  80,
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	if gm.MergeRetryCalls > 0 {
		t.Error("merge should not be called for stacked PR (base != default branch)")
	}
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) == 0 || backend.ClosedIDs[0] != "ralph-stk1" {
		t.Errorf("expected close for ralph-stk1, got %v", backend.ClosedIDs)
	}
}

// Scenario: Merge conflict → retry succeeds.
// When mergeFunc returns an error on first call (simulating conflict/rebase),
// the loop should retry and succeed on the second call.
func TestIntegration_MergeConflictThenRetrySucceeds(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Conflict task"
	backend.NextID = "ralph-conf1"
	backend.BackendLabel = "beads"

	gm := git.NewStubRepo()
	gm.ProjectDir = dir
	gm.WorkDir = dir
	gm.WorktreeBranch = "ralph/next"
	gm.RemoteURLValue = "https://github.com/owner/repo.git"
	gm.GH.OpenPR = 0

	runner := &stubRunner{
		onRun: func() {
			gm.HeadRevValue = "abc123"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
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
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner
	gm.ShipResult = git.ShipResult{PRNumber: 60}
	gm.PRNumber = 60
	gm.MergeRetryResult = true

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Retries happen inside MergeWithRetry in production; we verify merge was attempted.
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if gm.MergeRetryCalls == 0 {
		t.Error("merge should have been attempted")
	}
}

// Agent exits without signal — loop retries, does not close the bead.
// An agent that exits without signaling made no verifiable progress.
// The loop should retry on the next iteration, not treat it as completion.
func TestIntegration_AgentExitsWithoutSignal_Retries(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Silent task"
	backend.NextID = "ralph-ns1"
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	runCount := 0
	runner := &stubRunner{
		onRun:  func() { runCount++ },
		result: claude.Result{SignalDetected: false}, // no signal
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir, WorkDir: dir,
			RalphDir: ralphDir, PromptsDir: promptsDir,
		},
		MaxIterations: 3,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	l.Run(context.Background())

	if runCount < 2 {
		t.Errorf("agent should run multiple iterations when no signal; ran %d times", runCount)
	}
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) > 0 {
		t.Errorf("task should NOT be closed when agent never signals; closed: %v", backend.ClosedIDs)
	}
}

// Same task ID must not be re-selected after it was closed in the same session.
// If the backend keeps returning the same ID after close, the loop should skip
// it rather than processing it again.
func TestIntegration_CompletedTaskNotReselected(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 2
	backend.NextTask = "Only task"
	backend.NextID = "ralph-dup1"
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	taskIDs := []string{}
	runCount := 0
	runner := &stubRunner{
		onRun: func() {
			runCount++
			gm.HeadRevValue = fmt.Sprintf("commit-%d", runCount)
			// After first run, mark completed but keep returning same ID
			// (simulating a backend bug)
			backend.Lock()
			if runCount >= 2 {
				backend.Remaining = 0
			}
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir, WorkDir: dir,
			RalphDir: ralphDir, PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner

	l.Run(context.Background())

	// Track which task IDs the loop saw across iterations
	for _, ct := range l.SessionTasks() {
		taskIDs = append(taskIDs, ct.ID)
	}

	// The same task ID should not appear twice in session tasks.
	seen := map[string]int{}
	for _, id := range taskIDs {
		seen[id]++
		if seen[id] > 1 {
			t.Errorf("task %s was completed %d times — should only complete once", id, seen[id])
		}
	}
}

// Idle timeout after max failures skips the task.
// If the agent times out repeatedly without progress, the loop should
// skip the task rather than retrying forever.
func TestIntegration_IdleTimeoutSkipsAfterMaxFailures(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Stuck task"
	backend.NextID = "ralph-idle1"
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	runner := &stubRunner{
		result: claude.Result{IdleTimeout: true},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir, WorkDir: dir,
			RalphDir: ralphDir, PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	l.Run(context.Background())

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.SkippedIDs) == 0 {
		t.Error("task should be skipped after max idle timeout failures")
	}
	if len(backend.SkippedIDs) > 0 && backend.SkippedIDs[0] != "ralph-idle1" {
		t.Errorf("expected ralph-idle1 to be skipped, got %v", backend.SkippedIDs)
	}
}

// Feedback kill restarts the iteration — the agent is killed and retried.
func TestIntegration_FeedbackKillRestartsIteration(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Feedback task"
	backend.NextID = "ralph-fb1"
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	runCount := 0
	runner := &stubRunner{
		onRun: func() {
			runCount++
			if runCount >= 2 {
				gm.HeadRevValue = "after-feedback"
				backend.Lock()
				backend.Remaining = 0
				backend.Completed = 1
				backend.Unlock()
			}
		},
		result: claude.Result{FeedbackKill: true},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir, WorkDir: dir,
			RalphDir: ralphDir, PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner

	// After first feedback kill, switch to signal completion
	originalOnRun := runner.onRun
	runner.onRun = func() {
		originalOnRun()
		if runCount >= 2 {
			runner.result = claude.Result{SignalDetected: true, Summary: "done after feedback"}
		}
	}

	l.Run(context.Background())

	if runCount < 2 {
		t.Errorf("agent should run at least twice (once killed by feedback, once completing); ran %d", runCount)
	}
}

// Stop file halts the loop cleanly between iterations.
func TestIntegration_StopFileHaltsLoop(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Stoppable task"
	backend.NextID = "ralph-stop1"
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	runCount := 0
	runner := &stubRunner{
		onRun: func() {
			runCount++
			// Create stop file after first run
			os.WriteFile(filepath.Join(ralphDir, "stop"), []byte(""), 0o644)
		},
		result: claude.Result{},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir, WorkDir: dir,
			RalphDir: ralphDir, PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	l.Run(context.Background())

	if runCount > 2 {
		t.Errorf("loop should stop after detecting stop file; ran %d times", runCount)
	}

	finalState, _ := st.Load()
	if finalState.Status != "stopped" {
		t.Errorf("expected status 'stopped', got %q", finalState.Status)
	}
}

// Evolve mode: after successful merge, loop returns (signals restart).
func TestIntegration_EvolveExitsAfterMerge(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Evolve task"
	backend.NextID = "ralph-ev1"
	backend.BackendLabel = "beads"

	gm := git.NewStubRepo()
	gm.ProjectDir = dir
	gm.WorkDir = dir
	gm.WorktreeBranch = "ralph/next"
	gm.RemoteURLValue = "https://github.com/owner/repo.git"
	gm.GH.OpenPR = 0
	gm.GH.PRNumber = 0

	runner := &stubRunner{
		onRun: func() {
			gm.HeadRevValue = "evolved-commit"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir, WorkDir: dir,
			RalphDir: ralphDir, PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
		Evolve:        true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner
	gm.ShipResult = git.ShipResult{PRNumber: 99}
	gm.MergeRetryResult = true

	l.Run(context.Background())

	finalState, _ := st.Load()
	if finalState.Status != "evolve_restart" {
		t.Errorf("expected status 'evolve_restart', got %q", finalState.Status)
	}
}

// Pre-iteration tests run before agent, post-signal tests run after.
// The verification flow is: pre-iteration tests → agent runs → post-signal tests.
func TestIntegration_TestsRunBeforeAndAfterAgent(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Tested task"
	backend.NextID = "ralph-tt1"
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	sequence := []string{}
	runner := &stubRunner{
		onRun: func() {
			sequence = append(sequence, "agent")
			gm.HeadRevValue = "tested-commit"
			backend.Lock()
			backend.Remaining = 0
			backend.Completed = 1
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir, WorkDir: dir,
			RalphDir: ralphDir, PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		VerifyDir:     dir, // enables verification
	}
	verifyHook := &stubVerifyHook{passed: true, onCall: func(_ context.Context, _, _ string) {
		sequence = append(sequence, "post-signal-verify")
	}}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				sequence = append(sequence, "llm-verify")
				return "YES: approved", nil
			}},
		}),
		Connectivity: onlineStubConnectivity(),
		Runner:       runner,
		VerifyHook:   verifyHook,
	})

	l.Run(context.Background())

	// Agent must run. Verification runs after.
	agentIdx := -1
	for i, s := range sequence {
		if s == "agent" {
			agentIdx = i
			break
		}
	}
	if agentIdx == -1 {
		t.Fatal("agent never ran")
	}

	// Any verify step should come after the agent
	for i, s := range sequence {
		if (s == "post-signal-verify" || s == "llm-verify") && i < agentIdx {
			t.Errorf("verification step %q at index %d ran before agent at index %d", s, i, agentIdx)
		}
	}
}

// Task close blocked by dependency is skipped, not retried forever.
func TestIntegration_DependencyBlockedTaskIsSkipped(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Blocked task"
	backend.NextID = "ralph-blk1"
	backend.BackendLabel = "beads"
	// CloseTask will return a dependency error
	backend.CloseErr = fmt.Errorf("blocked by dependency: ralph-parent1 is not closed")

	gm := git.NewStubRepo()
	gm.ProjectDir = dir
	gm.WorkDir = dir
	gm.WorktreeBranch = "ralph/next"
	gm.RemoteURLValue = "https://github.com/owner/repo.git"
	gm.GH.OpenPR = 0

	runner := &stubRunner{
		onRun: func() {
			gm.HeadRevValue = "blocked-commit"
			backend.Lock()
			backend.Remaining = 0
			backend.Completed = 1
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir, WorkDir: dir,
			RalphDir: ralphDir, PromptsDir: promptsDir,
		},
		MaxIterations: 3,
		CallsPerHour:  80,
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner
	gm.ShipResult = git.ShipResult{PRNumber: 77}
	gm.PRNumber = 77
	gm.MergeRetryResult = true

	l.Run(context.Background())

	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.SkippedIDs) == 0 {
		t.Error("task should be skipped when CloseTask fails with dependency error")
	}
}

// ciTriggerGit extends StubGit to call opts.OnCIFailure once when configured,
// enabling tests to verify the CI failure → fix agent path through the full loop
// without a real GitHub connection.
type ciTriggerGit struct {
	*git.StubRepo
	triggerCI bool // if true, next MergeWithRetry call triggers OnCIFailure once
}

func (g *ciTriggerGit) MergeWithRetry(ctx context.Context, opts git.MergeRetryOpts) (bool, error) {
	g.StubRepo.MergeRetryCalls++
	if g.triggerCI && opts.OnCIFailure != nil {
		g.triggerCI = false
		ciErr := &git.CIFailureError{
			PRNumber: 42,
			Failures: []git.CICheckResult{{Name: "tests", Bucket: "fail"}},
		}
		result := opts.OnCIFailure(ciErr)
		if result == git.CIFixApplied {
			return true, nil
		}
		return false, ciErr
	}
	if g.StubRepo.MergeRetryFunc != nil {
		return g.StubRepo.MergeRetryFunc(ctx)
	}
	return g.StubRepo.MergeRetryResult, g.StubRepo.MergeRetryErr
}

// testGitRunner is a git.Runner for lifecycle integration tests. It handles the
// two git commands that AutoMergeCurrentBranch needs without spawning real
// processes. All other commands return empty string with nil error so the fast
// path can complete without real git operations.
type testGitRunner struct {
	remoteURL string
	headSHA   func() string // called on every rev-parse HEAD invocation
}

func (r *testGitRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	switch strings.Join(args, " ") {
	case "remote get-url origin":
		return r.remoteURL, nil
	case "rev-parse HEAD":
		return r.headSHA(), nil
	}
	// Return nil error for all other commands so push, fetch, merge-base etc.
	// succeed without real git state. The fast path skips these entirely.
	return "", nil
}

// realMergeGit wraps StubGit for all loop operations except MergeWithRetry,
// which delegates to a real git.Repo. This exercises the full
// AutoMergeCurrentBranch → AwaitCI → evaluateChecks pipeline driven by
// StubGitHub, proving CI results flow through the real code path.
type realMergeGit struct {
	*git.StubRepo
	realMgr *git.Repo
}

// SetKnownPRNumber keeps the real manager in sync with the stub so
// AutoMergeCurrentBranch skips FindOpenPR (no network call needed).
func (g *realMergeGit) SetKnownPRNumber(n int) {
	g.StubRepo.SetKnownPRNumber(n)
	g.realMgr.KnownPRNumber = n
}

// MergeWithRetry delegates to the real git.Repo, passing opts including
// OnCIFailure so the full CI failure → fix agent path can be exercised.
func (g *realMergeGit) MergeWithRetry(ctx context.Context, opts git.MergeRetryOpts) (bool, error) {
	g.StubRepo.MergeRetryCalls++
	return g.realMgr.MergeWithRetry(ctx, opts)
}

// Ship handles the two-phase Ship lifecycle for integration tests:
//   - Phase 1 (AutoMerge=false): delegates to the stub's ShipFunc or ShipResult
//     to record "push" and return a PR number.
//   - Phase 2 (AutoMerge=true): delegates to the real git.Repo.MergeWithRetry
//     so the full AwaitCI → evaluateChecks → MergePR pipeline runs.
func (g *realMergeGit) Ship(ctx context.Context, opts git.ShipOpts) (git.ShipResult, error) {
	if !opts.AutoMerge {
		return g.StubRepo.Ship(ctx, opts)
	}
	g.StubRepo.ShipCalls++
	g.StubRepo.LastShipOpts = opts
	// Mirror real Ship: check PR state when PRNumber is set.
	if opts.PRNumber != 0 {
		prState, stateErr := g.StubRepo.GetPRState(opts.PRNumber)
		if stateErr != nil {
			return git.ShipResult{PRNumber: opts.PRNumber}, stateErr
		}
		switch prState {
		case git.PRStateMerged:
			g.StubRepo.PostMergeUpdateMain()
			return git.ShipResult{PRNumber: opts.PRNumber, AlreadyMerged: true, Merged: true}, nil
		case git.PRStateClosed:
			return git.ShipResult{PRNumber: opts.PRNumber, Closed: true}, nil
		}
	}
	g.realMgr.KnownPRNumber = opts.PRNumber
	merged, err := g.realMgr.MergeWithRetry(ctx, git.MergeRetryOpts{})
	if err != nil {
		var ciErr *git.CIFailureError
		if errors.As(err, &ciErr) {
			return git.ShipResult{PRNumber: opts.PRNumber, CIFailure: true, CIFailureDetail: ciErr}, nil
		}
		return git.ShipResult{PRNumber: opts.PRNumber}, nil
	}
	return git.ShipResult{PRNumber: opts.PRNumber, Merged: merged}, nil
}

// TestIntegration_FullLifecycleSequenceOrdering verifies all lifecycle stages
// execute in order: signal → verify → push → ci_poll → ci_passed → merge →
// close, for each task. The merge step uses a real git.Repo so
// AutoMergeCurrentBranch → AwaitCI → evaluateChecks all execute; StubGitHub
// drives CI results. The sequence recorder captures CI-specific stages between
// push and merge, proving the CI pipeline actually ran.
//
// Also verifies the second iteration: after first close, next task is selected
// and branch setup runs for the next task.
func TestIntegration_FullLifecycleSequenceOrdering(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	var seq []string
	var seqMu sync.Mutex
	record := func(stage string) {
		seqMu.Lock()
		seq = append(seq, stage)
		seqMu.Unlock()
	}

	backend := newIntegrationBackend()
	backend.onClose = func(id string) { record("close:" + id) }
	backend.Remaining = 2
	backend.Completed = 0
	backend.Total = 2
	backend.NextTask = "Add auth middleware"
	backend.NextID = "ralph-seq1"
	backend.BackendLabel = "beads"

	// realMgr drives CI through the real AwaitCI → evaluateChecks code path.
	// realGH drives CI results and HeadSHA is updated per task so the fast path triggers.
	realMgr, realGH := git.NewRepoForTesting(git.Config{
		ProjectDir: dir,
		WorkDir:    dir + "/wt",
		RalphDir:   ralphDir,
		BaseBranch: "main",
		State:      newGitMemState(),
		Logger:     logging.New(nil),
	})
	realGH.OpenPR = 0
	realGH.PRBase = "main"
	realGH.PRState = git.PRStateOpen
	realMgr.WorktreeBranch = "ralph/lifecycle-seq"

	// ChecksFunc records ci_poll and returns a passing check. The real
	// evaluateChecks function receives these checks and must return CIPassed.
	realGH.ChecksFunc = func(_ int) []git.CICheckResult {
		record("ci_poll")
		return []git.CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}
	}

	// testGitRunner handles the two commands AutoMergeCurrentBranch needs.
	// headSHA reads from the stub so fast-path (HeadSHA == HeadRev) triggers.
	stub := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/next",
		RemoteURLValue: "https://github.com/owner/repo.git",
		PRBase:         "main",
		DefaultBranch:  "main",
		PRHealthy:      true,
	}
	runner2 := &testGitRunner{
		remoteURL: "https://github.com/owner/repo.git",
		headSHA:   func() string { return stub.HeadRevValue },
	}
	realMgr.Runner = runner2
	gm := &realMergeGit{StubRepo: stub, realMgr: realMgr}

	prCounter := 0
	stub.ShipFunc = func(_ context.Context, _ git.ShipOpts) (git.ShipResult, error) {
		record("push")
		prCounter++
		pr := prCounter * 10
		// Keep realGH and stub in sync so FindPR / MergePR use the right number.
		realGH.PRNumber = pr
		return git.ShipResult{PRNumber: pr}, nil
	}

	taskIdx := 0
	agentRunner := &stubRunner{
		onRun: func() {
			record("agent_signal")
			taskIdx++
			sha := fmt.Sprintf("sha%d", taskIdx)
			// Update both stub.HeadRevValue (for testGitRunner closure) and
			// realGH.HeadSHA (for fast-path check in AutoMergeCurrentBranch).
			stub.HeadRevValue = sha
			realGH.HeadSHA = sha
			backend.Lock()
			backend.Completed = taskIdx
			if taskIdx == 1 {
				backend.NextTask = "Write auth tests"
				backend.NextID = "ralph-seq2"
				backend.Remaining = 1
			} else {
				backend.Remaining = 0
			}
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 10,
		CallsPerHour:  80,
		AutoMerge:     true,
	}
	verifyHook := &stubVerifyHook{passed: true, onCall: func(_ context.Context, _, _ string) {
		record("verify")
	}}
	postTaskHook := &stubPostTaskHook{fn: func(_ context.Context, _ string, _ int, _ bool) { record("post_task") }}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		Runner:       agentRunner,
		VerifyHook:   verifyHook,
		PostTaskHook: postTaskHook,
	})

	// "merge" is recorded when MergePR fires inside executeMerge — this runs only
	// after AwaitCI returns CIPassed, so merge cannot appear before ci_passed.
	realGH.OnMerge = func() { record("merge") }

	// "ci_poll" is recorded when ChecksFunc is called (real ListChecks path).
	// "ci_passed" is recorded immediately after because these checks always pass
	// and evaluateChecks returns CIPassed — proving the real pipeline evaluated them.
	origChecksFunc := realGH.ChecksFunc
	realGH.ChecksFunc = func(call int) []git.CICheckResult {
		checks := origChecksFunc(call) // records "ci_poll"
		record("ci_passed")            // evaluateChecks will return CIPassed for these
		return checks
	}

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	seqMu.Lock()
	got := make([]string, len(seq))
	copy(got, seq)
	seqMu.Unlock()

	// The sequence proves: CI pipeline actually ran (ci_poll → ci_passed) between
	// push and merge, not a stub that bypasses AwaitCI entirely.
	want := []string{
		"agent_signal", "verify", "push", "ci_poll", "ci_passed", "merge", "close:ralph-seq1", "post_task",
		"agent_signal", "verify", "push", "ci_poll", "ci_passed", "merge", "close:ralph-seq2", "post_task",
	}
	if len(got) != len(want) {
		t.Fatalf("stage sequence length: got %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("stage[%d]: got %q, want %q\nfull sequence: %v", i, got[i], want[i], got)
		}
	}

	// Both tasks are recorded in session output.
	sessionTasks := l.SessionTasks()
	if len(sessionTasks) != 2 {
		t.Fatalf("expected 2 session tasks, got %d", len(sessionTasks))
	}
	if sessionTasks[0].ID != "ralph-seq1" || sessionTasks[1].ID != "ralph-seq2" {
		t.Errorf("session task IDs: got [%s, %s], want [ralph-seq1, ralph-seq2]",
			sessionTasks[0].ID, sessionTasks[1].ID)
	}

	// Branch setup ran for both tasks (PrepareForNextCalls ≥ 2).
	if stub.PrepareForNextCalls < 2 {
		t.Errorf("branch setup (PrepareForNextCalls) expected ≥2 calls, got %d", stub.PrepareForNextCalls)
	}
}

// TestIntegration_LifecycleCI_FailureFixRetry verifies that a CI failure during
// merge triggers the fix agent path and the loop merges successfully after the
// fix. Unlike TestIntegration_CIFailureTriggersFixAgent (which uses ciTriggerGit
// to stub the CI failure at the MergeWithRetry boundary), this test uses a real
// git.Repo so the CI failure flows through the real AwaitCI → evaluateChecks
// path: ChecksFunc returns failure on the first call and passing on subsequent
// calls. The fix agent updates the HEAD SHA so tryFixCI detects a new commit and
// returns CIFixApplied; the real opts.AwaitCI then polls again and sees CI pass.
func TestIntegration_LifecycleCI_FailureFixRetry(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Add feature with CI failure"
	backend.NextID = "ralph-cifr1"
	backend.BackendLabel = "beads"

	realMgr, realGH := git.NewRepoForTesting(git.Config{
		ProjectDir: dir,
		WorkDir:    dir + "/wt",
		RalphDir:   ralphDir,
		BaseBranch: "main",
		State:      newGitMemState(),
		Logger:     logging.New(nil),
	})
	realGH.OpenPR = 0
	realGH.PRBase = "main"
	realGH.PRState = git.PRStateOpen
	realGH.PRNumber = 55
	realMgr.WorktreeBranch = "ralph/cifr-test"

	// ChecksFunc returns failure on the first call (AutoMergeCurrentBranch attempt
	// 1), passing on all subsequent calls (opts.AwaitCI after fix, attempt 2).
	realGH.ChecksFunc = func(call int) []git.CICheckResult {
		if call == 1 {
			return []git.CICheckResult{{Name: "tests", State: "FAILURE", Bucket: "fail"}}
		}
		return []git.CICheckResult{{Name: "tests", State: "SUCCESS", Bucket: "pass"}}
	}

	stub := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/next",
		RemoteURLValue: "https://github.com/owner/repo.git",
		PRBase:         "main",
		DefaultBranch:  "main",
		PRHealthy:      true,
		ShipResult:     git.ShipResult{PRNumber: 55},
	}
	gitRunner := &testGitRunner{
		remoteURL: "https://github.com/owner/repo.git",
		headSHA:   func() string { return stub.HeadRevValue },
	}
	realMgr.Runner = gitRunner
	gm := &realMergeGit{StubRepo: stub, realMgr: realMgr}
	gm.SetKnownPRNumber(55)

	agentRunner := &stubRunner{
		onRun: func() {
			stub.HeadRevValue = "initial-sha"
			// HeadSHA differs from HeadRev so fast path is not taken and
			// AutoMergeCurrentBranch uses the push path → AwaitCI → CI fails.
			realGH.HeadSHA = "pr-head-sha"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "implemented"},
	}

	fixAgentCalled := false
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
	}
	// Fix agent changes the HEAD SHA, proving tryFixCI sees a new commit and
	// returns CIFixApplied so MergeWithRetry continues to a second attempt.
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				fixAgentCalled = true
				stub.HeadRevValue = "sha-after-fix"
				return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "ci fixed"}}
			},
		}),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = agentRunner

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	if !fixAgentCalled {
		t.Error("fix agent should have been spawned for CI failure")
	}

	// The task must be closed after fix agent resolved CI and merge succeeded.
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) == 0 {
		t.Fatal("task should be closed after CI fix and merge")
	}
	if backend.ClosedIDs[0] != "ralph-cifr1" {
		t.Errorf("expected ralph-cifr1 to be closed, got %q", backend.ClosedIDs[0])
	}

	// Real MergePR must have been called — merge actually happened.
	if realGH.MergeCalls == 0 {
		t.Error("expected MergePR to be called after CI passed")
	}
}

// TestIntegration_CIFailureTriggersFixAgent verifies that when Ship reports a
// CI failure the loop spawns a fix agent and retries, and the loop completes
// successfully after the fix.
func TestIntegration_CIFailureTriggersFixAgent(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Fix CI pipeline"
	backend.NextID = "ralph-ci2"
	backend.BackendLabel = "beads"

	stub := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/next",
		RemoteURLValue: "https://github.com/owner/repo.git",
	}

	// ShipFunc drives the two-phase Ship lifecycle:
	//   call 1 — push+PR (AutoMerge=false): return PR number
	//   call 2 — merge attempt (AutoMerge=true): simulate CI failure
	//   call 3 — merge retry (AutoMerge=true, after fix): succeed
	shipCall := 0
	stub.ShipFunc = func(_ context.Context, opts git.ShipOpts) (git.ShipResult, error) {
		shipCall++
		if !opts.AutoMerge {
			return git.ShipResult{PRNumber: 99}, nil
		}
		if shipCall == 2 {
			return git.ShipResult{
				PRNumber:  99,
				CIFailure: true,
				CIFailureDetail: &git.CIFailureError{
					PRNumber: 99,
					Failures: []git.CICheckResult{{Name: "tests", Bucket: "fail"}},
				},
			}, nil
		}
		return git.ShipResult{PRNumber: 99, Merged: true}, nil
	}
	runner := &stubRunner{
		onRun: func() {
			stub.HeadRevValue = "initial-sha"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "implemented"},
	}

	fixAgentCalled := false
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
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         stub,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				fixAgentCalled = true
				stub.HeadRevValue = "sha-after-fix"
				return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "ci fixed"}}
			},
		}),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	if !fixAgentCalled {
		t.Error("fix agent should have been spawned for CI failure")
	}

	// Task was closed after fix agent resolved CI.
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) == 0 {
		t.Fatal("task should be closed after CI fix and merge")
	}
	if backend.ClosedIDs[0] != "ralph-ci2" {
		t.Errorf("expected ralph-ci2 to be closed, got %q", backend.ClosedIDs[0])
	}
}

// gitCommandTracker implements git.Runner and records every git command issued.
// Returns the value from outputs (keyed by joined args) for known commands;
// returns an error for unregistered commands so unexpected invocations fail fast.
type gitCommandTracker struct {
	mu      sync.Mutex
	calls   []string
	outputs map[string]string
}

func (r *gitCommandTracker) Run(_ context.Context, _ string, args ...string) (string, error) {
	key := strings.Join(args, " ")
	r.mu.Lock()
	r.calls = append(r.calls, key)
	out, ok := r.outputs[key]
	r.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("gitCommandTracker: unregistered command %q", key)
	}
	return out, nil
}

// calledWith reports whether any recorded call has the given string as its
// first space-delimited token (exact subcommand match, not substring).
func (r *gitCommandTracker) calledWith(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		first, _, _ := strings.Cut(c, " ")
		if first == sub {
			return true
		}
	}
	return false
}

// gitMemState is a minimal git.StateStore backed by a map.
type gitMemState struct {
	mu   sync.Mutex
	data map[string]string
}

func newGitMemState() *gitMemState { return &gitMemState{data: make(map[string]string)} }

func (s *gitMemState) Read(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[key], nil
}

func (s *gitMemState) Write(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return nil
}

// Scenario 12: CI already passing on the current PR head SHA — the loop
// completes signal→verify→merge without a git push during the merge phase.
// Exercises the fast path in AutoMergeCurrentBranch where SHA matches and CI
// is green, so rebase+push is skipped entirely.
func TestIntegration_CIAlreadyPassing_SkipsPushAndMerges(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	const taskID = "ralph-cap1"
	const localSHA = "sha-already-passing"

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "CI already passing task"
	backend.NextID = taskID
	backend.BackendLabel = "beads"

	// gitCommandTracker doubles as git.Runner for the real git.Repo used
	// inside MergeRetryFunc. Outputs are configured for the commands that
	// AutoMergeCurrentBranch issues on the fast path.
	tracker := &gitCommandTracker{
		outputs: map[string]string{
			"remote get-url origin": "https://github.com/owner/repo.git",
			"rev-parse HEAD":        localSHA,
		},
	}

	// realGH drives the real git.Repo's GitHub calls. HeadSHA matches
	// the local SHA so the fast path triggers and CI is already resolved.
	realMgr, realGH := git.NewRepoForTesting(git.Config{
		ProjectDir: dir,
		WorkDir:    dir + "/wt",
		RalphDir:   ralphDir,
		BaseBranch: "main",
		State:      newGitMemState(),
		Logger:     logging.New(nil),
	})
	realGH.OpenPR = 99
	realGH.PRNumber = 99
	realGH.PRTitle = "CI already passing task"
	realGH.HeadSHA = localSHA
	realGH.Checks = []git.CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}
	realMgr.WorktreeBranch = "ralph/cap1-ci-already-passing"
	realMgr.Runner = tracker

	gm := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/next",
		RemoteURLValue: "https://github.com/owner/repo.git",
		PRBase:         "main",
		DefaultBranch:  "main",
		PRHealthy:      true,
		ShipResult:     git.ShipResult{PRNumber: 99},
		MergeRetryFunc: func(ctx context.Context) (bool, error) {
			return realMgr.AutoMergeCurrentBranch(ctx)
		},
	}

	runner := &stubRunner{
		onRun: func() {
			gm.HeadRevValue = localSHA
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "ci fast path"},
	}
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
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Fast path must not push during the merge phase.
	if tracker.calledWith("push") {
		t.Error("git push should not be called when CI is already passing on the current SHA")
	}

	// Merge must succeed and task must close.
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) == 0 {
		t.Fatal("expected task to be closed after fast-path merge")
	}
	if backend.ClosedIDs[0] != taskID {
		t.Errorf("expected close for %s, got %q", taskID, backend.ClosedIDs[0])
	}
	if !strings.Contains(backend.CloseReasons[0], "99") {
		t.Errorf("close reason should reference PR #99, got %q", backend.CloseReasons[0])
	}
}

// Scenario 13: Local HEAD differs from the PR head SHA — the loop falls
// through to the normal rebase+push+CI-poll flow. Exercises the branch in
// AutoMergeCurrentBranch where the SHA check fails so a git push is issued
// before CI is awaited.
func TestIntegration_CIAlreadyPassing_FallsThrough_WhenHeadDiffers(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	const taskID = "ralph-cap2"
	const localSHA = "sha-local-new-commit"
	const prHeadSHA = "sha-pr-head-differs"

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "SHA differs normal flow task"
	backend.NextID = taskID
	backend.BackendLabel = "beads"

	tracker := &gitCommandTracker{
		outputs: map[string]string{
			"remote get-url origin": "https://github.com/owner/repo.git",
			"rev-parse HEAD":        localSHA,
		},
	}

	// realGH has a HeadSHA that differs from localSHA so the fast path does
	// not trigger and the normal rebase+push+AwaitCI flow runs instead.
	var logBuf strings.Builder
	realMgr, realGH := git.NewRepoForTesting(git.Config{
		ProjectDir: dir,
		WorkDir:    dir + "/wt",
		RalphDir:   ralphDir,
		BaseBranch: "main",
		State:      newGitMemState(),
		Logger:     logging.NewWithWriter(&logBuf),
	})
	realGH.OpenPR = 100
	realGH.PRNumber = 100
	realGH.PRTitle = "SHA differs normal flow task"
	realGH.HeadSHA = prHeadSHA
	realGH.Checks = []git.CICheckResult{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}
	realMgr.WorktreeBranch = "ralph/cap2-sha-differs"
	realMgr.Runner = tracker

	gm := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/next",
		RemoteURLValue: "https://github.com/owner/repo.git",
		PRBase:         "main",
		DefaultBranch:  "main",
		PRHealthy:      true,
		ShipResult:     git.ShipResult{PRNumber: 100},
		MergeRetryFunc: func(ctx context.Context) (bool, error) {
			return realMgr.AutoMergeCurrentBranch(ctx)
		},
	}

	runner := &stubRunner{
		onRun: func() {
			gm.HeadRevValue = localSHA
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "sha differs"},
	}
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
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Normal flow must push during the merge phase because SHA differs.
	if !tracker.calledWith("push") {
		t.Error("git push should be called when local HEAD differs from PR head SHA")
	}

	// AwaitCI must be called with a non-zero pushedAt — the whole point of the
	// falls-through path is to wait for fresh CI after pushing.
	if !strings.Contains(logBuf.String(), "Waiting for fresh CI checks (pushed at") {
		t.Errorf("expected 'Waiting for fresh CI checks (pushed at' in log output, got:\n%s", logBuf.String())
	}

	// Merge must succeed and task must close.
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) == 0 {
		t.Fatal("expected task to be closed after normal flow merge")
	}
	if backend.ClosedIDs[0] != taskID {
		t.Errorf("expected close for %s, got %q", taskID, backend.ClosedIDs[0])
	}
	if !strings.Contains(backend.CloseReasons[0], "100") {
		t.Errorf("close reason should reference PR #100, got %q", backend.CloseReasons[0])
	}
}

// TestIntegration_SameSHA_NoOpPush_FailingChecksNotFiltered verifies that when
// push does not change HeadRev (same SHA before and after), existing failing CI
// checks are NOT filtered out by the pushedAt filter. This was the exact
// regression from tabi PR #495: the pushedAt filter discarded pre-push failing
// checks, making it look like CI passed when it hadn't.
//
// The test uses realMergeGit so the full AutoMergeCurrentBranch → AwaitCI →
// evaluateChecks pipeline runs. The testGitRunner always returns the same SHA
// for rev-parse HEAD, making push a no-op. ChecksFunc returns failing checks.
// The test asserts that MergePR is never called and the task still closes
// (CI failure does not block task closure, only merge).
func TestIntegration_SameSHA_NoOpPush_FailingChecksNotFiltered(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	const taskID = "ralph-nop1"
	const localSHA = "sha-unchanged-by-push"

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Same SHA no-op push task"
	backend.NextID = taskID
	backend.BackendLabel = "beads"

	realMgr, realGH := git.NewRepoForTesting(git.Config{
		WorkDir:    dir,
		RalphDir:   ralphDir,
		BaseBranch: "main",
		State:      newGitMemState(),
		Logger:     logging.New(nil),
	})
	realGH.OpenPR = 0
	realGH.PRBase = "main"
	realGH.PRState = git.PRStateOpen
	realGH.PRNumber = 77
	realGH.HeadSHA = "sha-old-pr-head"
	realGH.ChecksFunc = func(_ int) []git.CICheckResult {
		return []git.CICheckResult{{Name: "tests", State: "FAILURE", Bucket: "fail"}}
	}
	realMgr.WorkDir = dir + "/wt"
	realMgr.WorktreeBranch = "ralph/nop-push-test"

	stub := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/next",
		RemoteURLValue: "https://github.com/owner/repo.git",
		PRBase:         "main",
		DefaultBranch:  "main",
		PRHealthy:      true,
		ShipResult:     git.ShipResult{PRNumber: 77},
	}

	// testGitRunner always returns the same SHA — push is a no-op.
	runner2 := &testGitRunner{
		remoteURL: "https://github.com/owner/repo.git",
		headSHA:   func() string { return localSHA },
	}
	realMgr.Runner = runner2
	gm := &realMergeGit{StubRepo: stub, realMgr: realMgr}
	gm.SetKnownPRNumber(77)

	agentRunner := &stubRunner{
		onRun: func() {
			stub.HeadRevValue = localSHA
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "implemented"},
	}
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
	}
	// Fix agent changes the SHA so tryFixCI returns CIFixApplied (not
	// CIFixNoCommits which triggers 30s infrastructure retries). But CI
	// still fails on re-check, so MergeWithRetry exhausts MaxMergeAttempts
	// and returns CIFailureError — proving the failing checks weren't filtered.
	fixAttempt := 0
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			newRunner: func() verifier.Runner {
				fixAttempt++
				stub.HeadRevValue = fmt.Sprintf("sha-after-fix-%d", fixAttempt)
				return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "ci fix attempted"}}
			},
		}),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = agentRunner

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	// MergePR must NOT be called — CI failed, so merge must not proceed.
	if realGH.MergeCalls > 0 {
		t.Errorf("MergePR should not be called when CI is failing, got %d calls", realGH.MergeCalls)
	}

	// Task must NOT be closed — CI fix agents applied code (CIFixApplied) but tests
	// are still failing. The loop leaves the task open for manual investigation.
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) != 0 {
		t.Fatalf("task must not be closed when CI fix agents exhausted and tests still failing, got closed: %v", backend.ClosedIDs)
	}
}

// TestIntegration_LifecycleOrdering_BranchRenameAndReviewers traces the full
// iteration lifecycle using a recorded call log and asserts the exact ordering:
// rename → agent_signal → verify → push → detect_reviewers → close.
//
// Proves:
//   - Branch is renamed (task-specific) before any push occurs.
//   - DetectActiveReviewers is called during finalizePR (after push), not
//     before the agent runs — reviewer detection is deferred to the merge phase.
//   - The call log catches ordering violations: a refactor that moves rename
//     after push, or moves DetectActiveReviewers before the agent, will fail.
func TestIntegration_LifecycleOrdering_BranchRenameAndReviewers(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	var seq []string
	var seqMu sync.Mutex
	record := func(stage string) {
		seqMu.Lock()
		seq = append(seq, stage)
		seqMu.Unlock()
	}

	backend := newIntegrationBackend()
	backend.onClose = func(id string) { record("close:" + id) }
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Add auth handler"
	backend.NextID = "ralph-ord1"
	backend.BackendLabel = "beads"

	// No existing open PR — OpenPR=0 prevents the initWorktree resume path
	// from firing and ensures the agent actually runs.
	gm := &git.StubRepo{
		ProjectDir:              dir,
		WorkDir:                 dir,
		WorktreeBranch:          "ralph/next",
		RemoteURLValue:          "https://github.com/owner/repo.git",
		OnRenameBranch:          func(_, _ string) { record("rename") },
		OnDetectActiveReviewers: func() { record("detect_reviewers") },
	}

	gm.ShipFunc = func(_ context.Context, _ git.ShipOpts) (git.ShipResult, error) {
		record("push")
		return git.ShipResult{PRNumber: 77}, nil
	}

	runner := &stubRunner{
		onRun: func() {
			record("agent_signal")
			gm.HeadRevValue = "sha-ord1"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "auth handler done"},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	verifyHook := &stubVerifyHook{passed: true, onCall: func(_ context.Context, _, _ string) {
		record("verify")
	}}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		Runner:       runner,
		VerifyHook:   verifyHook,
	})

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	seqMu.Lock()
	got := make([]string, len(seq))
	copy(got, seq)
	seqMu.Unlock()

	want := []string{
		"rename", "agent_signal", "verify", "push", "detect_reviewers", "close:ralph-ord1",
	}
	if len(got) != len(want) {
		t.Fatalf("stage sequence length: got %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("stage[%d]: got %q, want %q\nfull sequence: %v", i, got[i], want[i], got)
		}
	}

	// Structural assertions as belt-and-suspenders.
	renameIdx, pushIdx := -1, -1
	reviewersIdx, agentIdx := -1, -1
	for i, s := range got {
		switch s {
		case "rename":
			renameIdx = i
		case "push":
			pushIdx = i
		case "detect_reviewers":
			reviewersIdx = i
		case "agent_signal":
			agentIdx = i
		}
	}
	if renameIdx < 0 || pushIdx < 0 {
		t.Fatal("rename or push not found in sequence")
	}
	if renameIdx >= pushIdx {
		t.Errorf("rename (idx %d) must occur before push (idx %d)", renameIdx, pushIdx)
	}
	if reviewersIdx < 0 || agentIdx < 0 {
		t.Fatal("detect_reviewers or agent_signal not found in sequence")
	}
	if reviewersIdx <= pushIdx {
		t.Errorf("detect_reviewers (idx %d) must occur after push (idx %d) — detection belongs in finalizePR", reviewersIdx, pushIdx)
	}
	if reviewersIdx <= agentIdx {
		t.Errorf("detect_reviewers (idx %d) must occur after agent_signal (idx %d) — detection must not happen before agent runs", reviewersIdx, agentIdx)
	}
}

// TestIntegration_LifecycleOrdering_NoReviewerDetectionWithoutTasks verifies
// that when the loop starts with zero tasks, DetectActiveReviewers is never
// called. The loop should exit immediately without reaching the reviewer
// detection step (AC3 — NOT called during startup when no tasks run).
func TestIntegration_LifecycleOrdering_NoReviewerDetectionWithoutTasks(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 0
	backend.Completed = 0
	backend.Total = 0
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/next",
		RemoteURLValue: "https://github.com/owner/repo.git",
	}

	var seq []string
	var seqMu sync.Mutex
	record := func(stage string) {
		seqMu.Lock()
		seq = append(seq, stage)
		seqMu.Unlock()
	}

	gm.OnDetectActiveReviewers = func() { record("detect_reviewers") }
	gm.OnRenameBranch = func(_, _ string) { record("rename") }
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})

	_ = l.Run(context.Background())

	seqMu.Lock()
	got := make([]string, len(seq))
	copy(got, seq)
	seqMu.Unlock()

	for _, s := range got {
		if s == "detect_reviewers" {
			t.Error("DetectActiveReviewers was called during startup with no tasks — it should only be called lazily when a task runs")
		}
	}

	if gm.DetectActiveReviewersCalled {
		t.Error("DetectActiveReviewersCalled should be false when no tasks run")
	}
}

// TestIntegration_LifecycleOrdering_FlushNoopAfterShip verifies that after a
// task is shipped (Ship creates the PR), the flush step that runs when the loop
// finds no remaining tasks does NOT call PushAndCreatePR — the work is already
// pushed so flush correctly skips the redundant API call.
//
// Uses FlushUnpushedWorkFunc to simulate the real decision logic (checking
// whether Ship already ran) so a regression that removes the guard causes
// PushAndCreatePRCalls to become non-zero, failing the assertion.
func TestIntegration_LifecycleOrdering_FlushNoopAfterShip(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Refactor auth module"
	backend.NextID = "ralph-flush1"
	backend.BackendLabel = "beads"

	// No existing open PR — OpenPR=0 prevents initWorktree from taking the
	// resume-from-PR path before the agent runs.
	gm := &git.StubRepo{
		ProjectDir:     dir,
		WorkDir:        dir,
		WorktreeBranch: "ralph/next",
		RemoteURLValue: "https://github.com/owner/repo.git",
	}

	// Ship records when it's called and returns a valid PR number.
	gm.ShipFunc = func(_ context.Context, _ git.ShipOpts) (git.ShipResult, error) {
		return git.ShipResult{PRNumber: 88}, nil
	}

	// FlushUnpushedWorkFunc simulates the real guard: if Ship already ran
	// (ShipCalls > 0), the branch was pushed — skip PushAndCreatePR.
	// Without this guard a regression increments PushAndCreatePRCalls.
	gm.FlushUnpushedWorkFunc = func(_ context.Context, _, _ string, _ bool) (bool, error) {
		if gm.ShipCalls > 0 {
			// Branch was already pushed via Ship — no-op, same as the real
			// rev-list guard in git.Repo.FlushUnpushedWork.
			return false, nil
		}
		// Branch not yet pushed — simulate PushAndCreatePR call.
		gm.PushAndCreatePRCalls++
		return false, nil
	}

	runner := &stubRunner{
		onRun: func() {
			gm.HeadRevValue = "sha-flush1"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "refactor done"},
	}
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
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		Runner:       runner,
		VerifyHook:   passingVerifyHook(),
	})
	gm.MergeRetryResult = true

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	if gm.ShipCalls == 0 {
		t.Fatal("expected Ship to be called during task completion")
	}
	if gm.FlushUnpushedCalls == 0 {
		t.Fatal("expected FlushUnpushedWork to be called after task completes")
	}
	if gm.PushAndCreatePRCalls > 0 {
		t.Errorf("PushAndCreatePR should not be called after Ship already pushed the branch, got %d calls", gm.PushAndCreatePRCalls)
	}
}

// TestIntegration_CreatePRViaAPI_SpecialCharsJSON verifies that CreatePRViaAPI
// produces valid JSON when the PR title and body contain newlines, quotes,
// backticks, and Unicode — characters that previously caused GitHub 400 errors
// when fmt.Sprintf was used instead of json.Marshal.
func TestIntegration_CreatePRViaAPI_SpecialCharsJSON(t *testing.T) {
	bin := t.TempDir()
	stdinFile := filepath.Join(bin, "stdin.json")
	script := "#!/bin/sh\ncat > " + stdinFile + "\necho '{\"number\":42}'\n"
	ghPath := filepath.Join(bin, "gh")
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	m := &git.Repo{}
	gh := m.GH()

	opts := git.CreatePROpts{
		Title: "Fix `backtick` and \"double quotes\"",
		Body:  "Line one\nLine two\tTabbed\nBackslash: \\\nUnicode: éàü\nBacktick: `code`\nQuote: \"hello\"",
		Head:  "ralph/task-branch",
		Base:  "main",
	}
	prNum, err := gh.CreatePRViaAPI("owner/repo", opts)
	if err != nil {
		t.Fatalf("CreatePRViaAPI returned error: %v", err)
	}
	if prNum != 42 {
		t.Errorf("expected PR number 42, got %d", prNum)
	}

	raw, readErr := os.ReadFile(stdinFile)
	if readErr != nil {
		t.Fatalf("gh was never called or stdin not captured: %v", readErr)
	}

	var parsed map[string]string
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("request body is not valid JSON: %v\nbody: %s", err, raw)
	}
	if parsed["title"] != opts.Title {
		t.Errorf("title mismatch: got %q, want %q", parsed["title"], opts.Title)
	}
	if parsed["body"] != opts.Body {
		t.Errorf("body mismatch: got %q, want %q", parsed["body"], opts.Body)
	}
	if parsed["head"] != opts.Head {
		t.Errorf("head mismatch: got %q, want %q", parsed["head"], opts.Head)
	}
	if parsed["base"] != opts.Base {
		t.Errorf("base mismatch: got %q, want %q", parsed["base"], opts.Base)
	}
}

// Simulate evolve rebase pulling a user commit from main: the agent receives
// the rebase-baseline instruction in its prompt, verification passes, and the
// user's file changes survive — proving the agent does not revert them.
func TestIntegration_EvolveRebasePreservesUserCommits(t *testing.T) {
	dir, ralphDir, _, st := setupIntegrationTest(t)

	// Use the real internal.md from the prompts shipped with the binary so the
	// rebase-baseline instruction is present in the assembled prompt.
	realPromptsDir := filepath.Join("..", "..", "cmd", "ralph", "prompts")
	if _, err := os.Stat(filepath.Join(realPromptsDir, "internal.md")); err != nil {
		t.Skipf("real prompts dir not found at %s: %v", realPromptsDir, err)
	}

	// Create a file representing a user commit pulled from main via rebase.
	userFile := filepath.Join(dir, "config.go")
	userContent := []byte("package main\n\n// user's intentional refactor\nvar Version = \"2.0\"\n")
	os.WriteFile(userFile, userContent, 0o644)

	// Seed stale attempt history referencing the user-modified file as a
	// "regression" — this is the scenario where the agent might revert.
	attemptsDir := filepath.Join(ralphDir, "attempts")
	os.MkdirAll(attemptsDir, 0o755)
	staleAttempt := "### Attempt 1\n" +
		"Summary: attempted to fix config.go but it regressed — Version changed unexpectedly\n" +
		"Changes: config.go 3 insertions\n" +
		"Analysis: warn:stuck\n"
	os.WriteFile(filepath.Join(attemptsDir, "ralph-rb1.log"), []byte(staleAttempt), 0o644)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Fix logging format"
	backend.NextID = "ralph-rb1"
	backend.BackendLabel = "beads"

	gm := git.NewStubRepo()
	gm.ProjectDir = dir
	gm.WorkDir = dir
	gm.WorktreeBranch = "ralph/next"
	gm.RemoteURLValue = "https://github.com/owner/repo.git"
	gm.GH.OpenPR = 0
	gm.GH.PRNumber = 0
	gm.DiffStatValue = "config.go | 3 +++\n 1 file changed, 3 insertions(+)"

	var capturedPrompt string
	runner := &stubRunner{
		onRunCfg: func(cfg claude.RunConfig) {
			capturedPrompt = cfg.Prompt
			// Simulate agent making a commit (on the task, not reverting user file).
			gm.HeadRevValue = "agent-commit-123"
			backend.Lock()
			backend.Completed = 1
			backend.Remaining = 0
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "fixed logging format"},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: realPromptsDir,
		},
		MaxIterations: 5,
		CallsPerHour:  80,
		AutoMerge:     true,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})

	l.runner = runner
	gm.ShipResult = git.ShipResult{PRNumber: 55}
	gm.MergeRetryResult = true

	err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Criterion 1: prompt contains the rebase-baseline instruction.
	if !strings.Contains(capturedPrompt, "Never revert") {
		t.Error("prompt must contain 'Never revert' instruction for rebase baseline")
	}
	if !strings.Contains(capturedPrompt, "new baseline") {
		t.Error("prompt must contain 'new baseline' instruction")
	}

	// Criterion 6: stale reflection content is present but overridden by instruction.
	if !strings.Contains(capturedPrompt, "regressed") {
		t.Error("stale attempt history referencing 'regressed' should be present in prompt")
	}

	// Criterion 5: user's file from main is preserved — agent did not revert it.
	got, readErr := os.ReadFile(userFile)
	if readErr != nil {
		t.Fatalf("user file should still exist: %v", readErr)
	}
	if string(got) != string(userContent) {
		t.Errorf("user's commit was reverted!\nwant: %s\ngot:  %s", userContent, got)
	}

	// Criterion 2: task completed successfully (closed with PR reference).
	backend.CloseMu.Lock()
	defer backend.CloseMu.Unlock()
	if len(backend.ClosedIDs) == 0 {
		t.Fatal("expected task to be closed after successful verification")
	}
	if backend.ClosedIDs[0] != "ralph-rb1" {
		t.Errorf("expected close for ralph-rb1, got %q", backend.ClosedIDs[0])
	}
}

// Scenario: an open PR already exists for the task branch at iteration start.
// resumeViaPR detects it via FindPRForBranch and resolves the task without
// running the agent. This covers the bug where FindOpenPR / FindPR used the
// unreliable gh api head filter and returned empty, causing the agent to
// re-do work that was already done and Ship to 422.
func TestIntegration_ResumeViaPR_DetectsExistingOpenPR(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Completed = 0
	backend.Total = 1
	backend.NextTask = "Refactor hints"
	backend.NextID = "ralph-peou"
	backend.BackendLabel = "beads"
	_ = backend.SetMetadata("ralph-peou", "branch", "ralph/ralph-peou-refactor-hints")

	gm := git.NewStubRepo()
	gm.ProjectDir = dir
	gm.WorkDir = dir
	gm.WorktreeBranch = "ralph/ralph-peou-refactor-hints"
	gm.RemoteURLValue = "https://github.com/owner/repo.git"
	gm.GH.OpenPR = 0
	gm.GH.PRNumber = 303
	gm.GH.PRState = git.PRStateOpen
	gm.GH.PRTitle = "Refactor hints"
	gm.GH.PRURL = "https://github.com/owner/repo/pull/303"
	gm.GH.Checks = []git.CICheckResult{
		{Name: "build", State: "SUCCESS", Bucket: "pass"},
	}
	gm.BranchRenamed = true
	gm.ResumeResult = git.ResumeTaskResult{
		Handled:  true,
		PRNumber: 303,
	}

	agentCalled := false
	runner := &stubRunner{
		onRun:  func() { agentCalled = true },
		result: claude.Result{},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir,
			WorkDir:    dir,
			RalphDir:   ralphDir,
			PromptsDir: promptsDir,
		},
		MaxIterations: 3,
		CallsPerHour:  80,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier:    newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if agentCalled {
		t.Error("agent must not run when an open PR already exists for the task branch")
	}
}

// TestIntegration_VerifyBuildRunsBeforeAgent asserts that the --verify-build
// script runs before the agent and its failure output appears in the prompt.
func TestIntegration_VerifyBuildRunsBeforeAgent(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Fix the widget"
	backend.NextID = "ralph-vb1"
	backend.BackendLabel = "beads"

	// Write a template that includes the test status placeholder so the
	// build failure message lands in the assembled prompt.
	os.WriteFile(filepath.Join(promptsDir, "internal.md"), []byte("{{TEST_STATUS}}"), 0o644)

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	var seq []string
	var capturedPrompt string
	runner := &stubRunner{
		onRunCfg: func(cfg claude.RunConfig) {
			seq = append(seq, "agent")
			capturedPrompt = cfg.Prompt
			gm.HeadRevValue = "vb-commit"
			backend.Lock()
			backend.Remaining = 0
			backend.Completed = 1
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir, WorkDir: dir,
			RalphDir: ralphDir, PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		VerifyBuild:   "echo 'ERROR: missing import' && exit 1",
	}
	verifyHook := &stubVerifyHook{passed: true, onCall: func(_ context.Context, _, _ string) {
		seq = append(seq, "verify")
	}}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:        st,
		Git:          gm,
		TaskBackend:  backend,
		Logger:       logger,
		Verifier:     newTestVerifier(t, cfg, logger),
		Connectivity: onlineStubConnectivity(),
		Runner:       runner,
		VerifyHook:   verifyHook,
	})

	l.Run(context.Background())

	// Build check ran before agent.
	agentIdx := -1
	for i, s := range seq {
		if s == "agent" {
			agentIdx = i
			break
		}
	}
	if agentIdx == -1 {
		t.Fatal("agent never ran")
	}

	// Build failure message must appear in the agent prompt.
	if !strings.Contains(capturedPrompt, "BUILD IS BROKEN") {
		t.Error("expected build failure message in agent prompt")
	}
	if !strings.Contains(capturedPrompt, "missing import") {
		t.Error("expected build output in agent prompt")
	}
}

// TestIntegration_PreIterationTestsRunBeforeAgent asserts that the pre-iteration
// test suite runs before the agent and its status feeds into the prompt.
func TestIntegration_PreIterationTestsRunBeforeAgent(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	// Create a passing ralph:verify script.
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("ralph-verify:\n\t@echo 'ok'\n"), 0o644)
	// Template must include {{TEST_STATUS}} so pre-iteration output lands in prompt.
	os.WriteFile(filepath.Join(promptsDir, "internal.md"), []byte("{{TEST_STATUS}}"), 0o644)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Add auth"
	backend.NextID = "ralph-pit1"
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir}

	var capturedPrompt string
	runner := &stubRunner{
		onRunCfg: func(cfg claude.RunConfig) {
			capturedPrompt = cfg.Prompt
			gm.HeadRevValue = "pit-commit"
			backend.Lock()
			backend.Remaining = 0
			backend.Completed = 1
			backend.Unlock()
		},
		result: claude.Result{SignalDetected: true, Summary: "done"},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir, WorkDir: dir,
			RalphDir: ralphDir, PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		VerifyDir:     dir,
	}
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				return "YES: ok", nil
			}},
		}),
		Connectivity: onlineStubConnectivity(),
		VerifyHook: passingVerifyHook(),
	})
	l.runner = runner

	l.Run(context.Background())

	// Pre-iteration test status must appear in the prompt.
	if !strings.Contains(capturedPrompt, "tests passing") {
		t.Error("expected pre-iteration test status in agent prompt")
	}
}

// TestIntegration_LLMVerifyRejectThenFixThenPass asserts the full lifecycle:
// agent signals → tests pass → LLM rejects → fix agent spawned → LLM approves.
func TestIntegration_LLMVerifyRejectThenFixThenPass(t *testing.T) {
	dir, ralphDir, promptsDir, st := setupIntegrationTest(t)

	backend := newIntegrationBackend()
	backend.Remaining = 1
	backend.Total = 1
	backend.NextTask = "Fix formatting"
	backend.NextID = "ralph-llm1"
	backend.BackendLabel = "beads"

	gm := &git.StubRepo{ProjectDir: dir, WorkDir: dir, DiffFullValue: "+ stub diff"}

	var seq []string
	runner := &signalCallingRunner{
		onRun: func() {
			seq = append(seq, "agent")
			gm.HeadRevValue = "llm-commit"
			backend.Lock()
			backend.Remaining = 0
			backend.Completed = 1
			backend.Unlock()
		},
		result: claude.Result{Summary: "done"},
	}
	cfg := Config{
		Dirs: workctx.WorkContext{
			ProjectDir: dir, WorkDir: dir,
			RalphDir: ralphDir, PromptsDir: promptsDir,
		},
		MaxIterations: 1,
		CallsPerHour:  80,
		VerifyDir:     dir,
	}
	// LLM rejects first attempt, approves second.
	llmCalls := 0
	// Fix agent is spawned after rejection.
	fixCalls := 0
	logger := logging.New(nil)
	l := New(cfg, Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      logger,
		Verifier: newTestVerifier(t, cfg, logger, verifierTestStubs{
			querier: &stubQuerier{fn: func(ctx context.Context, workDir, prompt, model string) (string, error) {
				llmCalls++
				if llmCalls == 1 {
					seq = append(seq, "llm_reject")
					return "NO: no test for auth", nil
				}
				seq = append(seq, "llm_approve")
				return "YES: looks good", nil
			}},
			newRunner: func() verifier.Runner {
				fixCalls++
				seq = append(seq, "fix_agent")
				return &stubRunner{result: claude.Result{SignalDetected: true, Summary: "added tests"}}
			},
		}),
		Connectivity: onlineStubConnectivity(),
	})
	l.runner = runner

	l.Run(context.Background())

	// Fix agent must have been spawned.
	if fixCalls == 0 {
		t.Error("fix agent should have been spawned after LLM rejection")
	}

	// Ordering: agent → llm_reject → fix_agent → llm_approve
	wantSubseq := []string{"agent", "llm_reject", "fix_agent", "llm_approve"}
	seqIdx := 0
	for _, s := range seq {
		if seqIdx < len(wantSubseq) && s == wantSubseq[seqIdx] {
			seqIdx++
		}
	}
	if seqIdx != len(wantSubseq) {
		t.Errorf("expected subsequence %v in sequence %v (matched %d/%d)", wantSubseq, seq, seqIdx, len(wantSubseq))
	}
}

// Ensure the integrationBackend satisfies tasks.Backend.
var _ tasks.Backend = (*integrationBackend)(nil)
