package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
)

func runCmd(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}

// stubBackend implements tasks.Backend for main integration tests.
type stubBackend struct {
	remaining int
	completed int
	total     int
}

func (s *stubBackend) Init() error                                { return nil }
func (s *stubBackend) HasRemaining() (bool, error)                { return s.remaining > 0, nil }
func (s *stubBackend) CountCompleted() (int, error)               { return s.completed, nil }
func (s *stubBackend) CountRemaining() (int, error)               { return s.remaining, nil }
func (s *stubBackend) CountTotal() (int, error)                   { return s.total, nil }
func (s *stubBackend) GetNextTask() (string, error)               { return "", nil }
func (s *stubBackend) GetNextTaskID() (string, error)             { return "", nil }
func (s *stubBackend) GetNextTaskInfo() (string, string, error)   { return "", "", nil }
func (s *stubBackend) HasTasks() (bool, error)                    { return s.total > 0, nil }
func (s *stubBackend) NeedsPlanning() (bool, error)               { return false, nil }
func (s *stubBackend) PlanningSucceeded() (bool, error)           { return true, nil }
func (s *stubBackend) CloseTask(string, string) error             { return nil }
func (s *stubBackend) SkipTask(string, string) error              { return nil }
func (s *stubBackend) ReopenTask(string) error                    { return nil }
func (s *stubBackend) SetState(_, _, _, _ string) error           { return nil }
func (s *stubBackend) GetState(_, _ string) (string, error)       { return "", nil }
func (s *stubBackend) ExecutionInstructions() (string, error)     { return "", nil }
func (s *stubBackend) PlanningInstructions() string               { return "" }
func (s *stubBackend) GetDescription(_ string) (string, error)    { return "", nil }
func (s *stubBackend) Label() string                              { return "checklist" }

// Verifies the resume script contains the correct flags from the config,
// proving that interrupted sessions can be resumed with the same parameters.
func TestGenerateResumeScript(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	cfg := config.Config{
		ProjectDir:    dir,
		MaxIterations: 20,
		Quiet:         true,
		UseWorktree:   true,
		AutoMerge:     true,
		CallsPerHour:  40,
	}

	log := logging.New(nil)
	generateResumeScript(cfg, ralphDir, "/usr/local/bin/ralph", nil, log)

	resumePath := filepath.Join(ralphDir, "resume.sh")
	data, err := os.ReadFile(resumePath)
	if err != nil {
		t.Fatalf("resume script should exist: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "--max 20") {
		t.Error("resume script should contain --max 20")
	}
	if !strings.Contains(content, "--quiet") {
		t.Error("resume script should contain --quiet")
	}
	if !strings.Contains(content, "--calls-per-hour 40") {
		t.Error("resume script should contain --calls-per-hour 40")
	}
	if !strings.Contains(content, "--auto-merge") {
		t.Error("resume script should contain --auto-merge")
	}
	if strings.Contains(content, "--no-worktree") {
		t.Error("resume script should NOT contain --no-worktree when worktree is enabled")
	}
}

// Verifies the resume script includes --evolve when enabled.
func TestGenerateResumeScript_Evolve(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	cfg := config.Config{
		ProjectDir:    dir,
		MaxIterations: 50,
		UseWorktree:   true,
		AutoMerge:     true,
		Evolve:        true,
		CallsPerHour:  80,
	}

	log := logging.New(nil)
	generateResumeScript(cfg, ralphDir, "/usr/local/bin/ralph", nil, log)

	data, _ := os.ReadFile(filepath.Join(ralphDir, "resume.sh"))
	content := string(data)
	if !strings.Contains(content, "--evolve") {
		t.Error("resume script should contain --evolve")
	}
	if !strings.Contains(content, "--auto-merge") {
		t.Error("resume script should contain --auto-merge")
	}
}

// Verifies the resume script includes --no-worktree when worktree is disabled.
func TestGenerateResumeScript_NoWorktree(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	cfg := config.Config{
		ProjectDir:    dir,
		MaxIterations: 50,
		UseWorktree:   false,
		CallsPerHour:  80,
	}

	log := logging.New(nil)
	generateResumeScript(cfg, ralphDir, "/usr/local/bin/ralph", nil, log)

	data, _ := os.ReadFile(filepath.Join(ralphDir, "resume.sh"))
	if !strings.Contains(string(data), "--no-worktree") {
		t.Error("resume script should contain --no-worktree")
	}
}

// Verifies the summary prints correct task counts from the backend.
func TestPrintSummary_TaskCounts(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	st := state.NewStore(ralphDir)
	st.Init(5, 0)
	st.Write("iteration", "3")
	st.Write("status", "completed")

	backend := &stubBackend{
		completed: 3,
		remaining: 0,
		total:     3,
	}

	gm := &git.Manager{
		ProjectDir: dir,
		WorkDir:    dir,
	}

	log := logging.New(nil)
	planFile := filepath.Join(ralphDir, "plan.md")

	// Should not panic or error.
	printSummary(config.Config{ProjectDir: dir}, gm, st, backend, ralphDir, planFile, log)
}

// Verifies initRalphDir creates the .ralph directory and log files.
func TestInitRalphDir_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	logFile := filepath.Join(ralphDir, "loop.log")
	stateFile := filepath.Join(ralphDir, "state.json")

	cfg := config.Config{ProjectDir: dir}

	log := logging.New(nil)
	resume, exitCode := initRalphDir(context.Background(), cfg, ralphDir, logFile, stateFile, log)

	if exitCode >= 0 {
		t.Fatalf("expected continue (exitCode < 0), got %d", exitCode)
	}
	if resume {
		t.Error("expected fresh start, not resume")
	}

	if _, err := os.Stat(ralphDir); os.IsNotExist(err) {
		t.Error(".ralph directory should exist")
	}
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		t.Error("log file should exist")
	}
}

// Verifies clearSignalFiles removes signal files but preserves state.json
// and other .ralph contents, so evolve restart resumes instead of replanning.
func TestClearSignalFiles_PreservesState(t *testing.T) {
	ralphDir := t.TempDir()

	// Create state and signal files
	os.WriteFile(filepath.Join(ralphDir, "state.json"), []byte(`{"status":"running"}`), 0o644)
	os.WriteFile(filepath.Join(ralphDir, "loop.log"), []byte("log data"), 0o644)
	os.WriteFile(filepath.Join(ralphDir, ".signal_complete"), []byte("done"), 0o644)
	os.WriteFile(filepath.Join(ralphDir, ".signal_current_task"), []byte("task"), 0o644)
	os.WriteFile(filepath.Join(ralphDir, ".signal_all_complete"), []byte("all done"), 0o644)
	os.WriteFile(filepath.Join(ralphDir, ".stream-task"), []byte("stream"), 0o644)

	clearSignalFiles(ralphDir)

	// Signal files should be gone
	for _, f := range []string{".signal_complete", ".signal_current_task", ".signal_all_complete", ".stream-task"} {
		if _, err := os.Stat(filepath.Join(ralphDir, f)); !os.IsNotExist(err) {
			t.Errorf("signal file %s should be removed", f)
		}
	}

	// State files should be preserved
	for _, f := range []string{"state.json", "loop.log"} {
		if _, err := os.Stat(filepath.Join(ralphDir, f)); os.IsNotExist(err) {
			t.Errorf("state file %s should be preserved", f)
		}
	}
}

// Verifies initRalphDir exits with error when working tree has uncommitted changes,
// preventing the .gitignore commit from sweeping in unrelated staged work.
func TestInitRalphDir_DirtyWorkingTreeExitsWithError(t *testing.T) {
	dir := t.TempDir()

	// Init a git repo so isGitRepo returns true
	runCmd(t, "git", "init", "-b", "main", dir)
	runCmd(t, "git", "-C", dir, "commit", "--allow-empty", "-m", "init")

	// Create uncommitted changes
	os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("uncommitted"), 0o644)
	runCmd(t, "git", "-C", dir, "add", "dirty.txt")

	ralphDir := filepath.Join(dir, ".ralph")
	logFile := filepath.Join(ralphDir, "loop.log")
	stateFile := filepath.Join(ralphDir, "state.json")

	cfg := config.Config{ProjectDir: dir}
	log := logging.New(nil)

	_, exitCode := initRalphDir(context.Background(), cfg, ralphDir, logFile, stateFile, log)

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
}

// Verifies initRalphDir detects an existing state file and enables resume.
func TestInitRalphDir_DetectsResume(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	logFile := filepath.Join(ralphDir, "loop.log")
	stateFile := filepath.Join(ralphDir, "state.json")

	st := state.NewStore(ralphDir)
	st.Init(5, 0)
	st.Write("status", "running")

	cfg := config.Config{ProjectDir: dir}
	log := logging.New(nil)

	resume, exitCode := initRalphDir(context.Background(), cfg, ralphDir, logFile, stateFile, log)

	if exitCode >= 0 {
		t.Fatalf("expected continue, got exit code %d", exitCode)
	}
	if !resume {
		t.Error("expected resume=true when state.json exists with running status")
	}
}

// Verifies that safeRemoveRalphDir refuses to delete .beads or .dolt,
// which contain permanent task history that must survive resets.
func TestSafeRemoveRalphDir_ProtectsBeads(t *testing.T) {
	tmp := t.TempDir()

	beadsDir := filepath.Join(tmp, ".beads")
	os.MkdirAll(beadsDir, 0o755)
	os.WriteFile(filepath.Join(beadsDir, "data.db"), []byte("tasks"), 0o644)

	if err := safeRemoveRalphDir(beadsDir); err == nil {
		t.Fatal("expected error when trying to remove .beads")
	}
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		t.Fatal(".beads should still exist after refused removal")
	}

	doltDir := filepath.Join(tmp, ".dolt")
	os.MkdirAll(doltDir, 0o755)

	if err := safeRemoveRalphDir(doltDir); err == nil {
		t.Fatal("expected error when trying to remove .dolt")
	}

	ralphDir := filepath.Join(tmp, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	if err := safeRemoveRalphDir(ralphDir); err != nil {
		t.Fatalf("removing .ralph should succeed: %v", err)
	}
	if _, err := os.Stat(ralphDir); !os.IsNotExist(err) {
		t.Fatal(".ralph should be removed")
	}
}

// Verifies that readLineCtx returns immediately with context.Canceled when
// the context is already cancelled, instead of blocking on stdin. This
// prevents Ctrl-C from leaving the user trapped at interactive prompts.
func TestReadLineCtx_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		_, err := readLineCtx(ctx)
		if err != context.Canceled {
			t.Errorf("expected context.Canceled, got %v", err)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readLineCtx should return immediately on cancelled context")
	}
}

// Verifies embedded prompts (go/cmd/ralph/prompts/) match the source prompts
// (prompts/) so the fallback for non-self-hosted projects stays in sync.
func TestEmbeddedPrompts_MatchSourcePrompts(t *testing.T) {
	// go/cmd/ralph/ → project root
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve project root: %v", err)
	}
	sourceDir := filepath.Join(root, "prompts")
	embeddedDir := filepath.Join(root, "go", "cmd", "ralph", "prompts")

	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		t.Fatalf("read source prompts dir: %v", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		src, err := os.ReadFile(filepath.Join(sourceDir, name))
		if err != nil {
			t.Fatalf("read source %s: %v", name, err)
		}
		emb, err := os.ReadFile(filepath.Join(embeddedDir, name))
		if err != nil {
			t.Errorf("embedded prompt %s missing: %v", name, err)
			continue
		}
		if string(src) != string(emb) {
			t.Errorf("embedded prompt %s differs from source — run: cp prompts/%s go/cmd/ralph/prompts/%s", name, name, name)
		}
	}
}

// Verifies evolveRestart skips the pull/rebuild/exec sequence when a stop
// file exists, so that "ralph stop" issued during an iteration is honored
// before restarting with a new binary.
func TestEvolveRestart_StopFileSkipsRestart(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	stopFile := filepath.Join(ralphDir, "stop")
	os.WriteFile(stopFile, []byte{}, 0o644)

	log := logging.New(nil)

	// evolveRestart would fail on git fetch in a non-git temp dir,
	// so if it returns nil, the stop file short-circuited before any git ops.
	err := evolveRestart(dir, "/nonexistent/ralph", "develop", nil, log)
	if err != nil {
		t.Fatalf("expected nil error when stop file present, got: %v", err)
	}

	if _, statErr := os.Stat(stopFile); !os.IsNotExist(statErr) {
		t.Error("stop file should be removed after being honored")
	}
}

// Verifies evolveRestart proceeds (and fails on git) when no stop file exists,
// confirming the stop-file check only fires when the file is present.
func TestEvolveRestart_NoStopFileProceeds(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	log := logging.New(nil)

	// No stop file → should attempt git fetch, which fails in a temp dir.
	err := evolveRestart(dir, "/nonexistent/ralph", "develop", nil, log)
	if err == nil {
		t.Fatal("expected error from git fetch in temp dir, got nil")
	}
}

// Verifies that cleanup writes status="stopped" when interrupted, overriding
// any stale status (e.g., "halted_stagnation") from a previous analyzer run.
func TestCleanup_InterruptedWritesStopped(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	st := state.NewStore(ralphDir)
	st.Init(5, 0)
	st.Write("status", "halted_stagnation")

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	backend := &stubBackend{total: 1, remaining: 1}
	log := logging.New(nil)
	cfg := config.Config{ProjectDir: dir, MaxIterations: 5, UseWorktree: true, CallsPerHour: 80}

	cleanup(cfg, gm, st, backend, ralphDir, filepath.Join(ralphDir, "plan.md"), "/usr/local/bin/ralph", nil, true, log)

	status, _ := st.Read("status")
	if status != "stopped" {
		t.Errorf("expected status 'stopped' after interrupted cleanup, got %q", status)
	}
}

// Verifies that cleanup preserves the existing status when NOT interrupted,
// so normal exits don't overwrite meaningful statuses like "completed".
func TestCleanup_NotInterruptedPreservesStatus(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	st := state.NewStore(ralphDir)
	st.Init(5, 0)
	st.Write("status", "completed")

	gm := &git.Manager{ProjectDir: dir, WorkDir: dir}
	backend := &stubBackend{total: 3, completed: 3}
	log := logging.New(nil)
	cfg := config.Config{ProjectDir: dir, MaxIterations: 5, UseWorktree: true, CallsPerHour: 80}

	cleanup(cfg, gm, st, backend, ralphDir, filepath.Join(ralphDir, "plan.md"), "/usr/local/bin/ralph", nil, false, log)

	status, _ := st.Read("status")
	if status != "completed" {
		t.Errorf("expected status 'completed' preserved, got %q", status)
	}
}

// Verifies that validatePlanFile rejects a nonexistent file with "not found".
func TestValidatePlanFile_NonexistentExitsWithError(t *testing.T) {
	err := validatePlanFile("/nonexistent/plan.md")
	if err == nil {
		t.Fatal("expected error for nonexistent plan file")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %q", err)
	}
}

// Verifies that validatePlanFile rejects a file without checkboxes.
func TestValidatePlanFile_NoCheckboxesRejected(t *testing.T) {
	badPlan := filepath.Join(t.TempDir(), "bad-plan.md")
	os.WriteFile(badPlan, []byte("Just some text without checkboxes"), 0o644)

	err := validatePlanFile(badPlan)
	if err == nil {
		t.Fatal("expected error for plan without checkboxes")
	}
	if !strings.Contains(err.Error(), "Ralph format") {
		t.Errorf("expected 'Ralph format' in error, got %q", err)
	}
}

// Verifies that validatePlanFile accepts a valid plan with checkboxes.
func TestValidatePlanFile_ValidPlanAccepted(t *testing.T) {
	plan := filepath.Join(t.TempDir(), "plan.md")
	os.WriteFile(plan, []byte("- [ ] Test task\n- [ ] Another task"), 0o644)

	err := validatePlanFile(plan)
	if err != nil {
		t.Errorf("expected no error for valid plan, got %v", err)
	}
}
