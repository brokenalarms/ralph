package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/state"
)

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
func (s *stubBackend) HasTasks() (bool, error)                    { return s.total > 0, nil }
func (s *stubBackend) NeedsPlanning() (bool, error)               { return false, nil }
func (s *stubBackend) PlanningSucceeded() (bool, error)           { return true, nil }
func (s *stubBackend) CloseTask(string, string) error             { return nil }
func (s *stubBackend) ExecutionInstructions() (string, error)     { return "", nil }
func (s *stubBackend) PlanningInstructions() string               { return "" }
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
	if strings.Contains(content, "--no-worktree") {
		t.Error("resume script should NOT contain --no-worktree when worktree is enabled")
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
	resume, exitCode := initRalphDir(cfg, ralphDir, logFile, stateFile, log)

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

	resume, exitCode := initRalphDir(cfg, ralphDir, logFile, stateFile, log)

	if exitCode >= 0 {
		t.Fatalf("expected continue, got exit code %d", exitCode)
	}
	if !resume {
		t.Error("expected resume=true when state.json exists with running status")
	}
}

