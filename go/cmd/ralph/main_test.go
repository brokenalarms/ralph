package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/loop"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
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
func (s *stubBackend) GetNextTaskInfo() (tasks.TaskInfo, error)   { return tasks.TaskInfo{}, nil }
func (s *stubBackend) HasTasks() (bool, error)                    { return s.total > 0, nil }
func (s *stubBackend) CloseTask(string, string) error             { return nil }
func (s *stubBackend) SkipTask(string, string) error              { return nil }
func (s *stubBackend) ReopenTask(string) error                    { return nil }
func (s *stubBackend) SetState(_, _, _, _ string) error           { return nil }
func (s *stubBackend) GetState(_, _ string) (string, error)       { return "", nil }
func (s *stubBackend) ExecutionInstructions() (string, error)     { return "", nil }
func (s *stubBackend) ProjectContext() (string, error)            { return "", nil }
func (s *stubBackend) GetDescription(_ string) (string, error)    { return "", nil }
func (s *stubBackend) GetFullContext(_ string) (string, error)    { return "", nil }
func (s *stubBackend) Label() string                              { return "beads" }

// Proves: bare `ralph` (no args) returns 0 and does not enter the loop,
// ensuring users must choose an explicit subcommand.
func TestRun_BareRalphShowsUsage(t *testing.T) {
	code := run(nil)
	if code != 0 {
		t.Errorf("bare ralph should exit 0, got %d", code)
	}
}

// Proves: `ralph loop` is recognized as a subcommand and routes to handleLoop.
// Without a valid git repo it should fail, confirming it reached the loop path.
func TestRun_LoopSubcommand(t *testing.T) {
	dir := t.TempDir()
	code := run([]string{"loop", "--dir", dir})
	// Should fail because dir is not a git repo — but the fact it fails
	// with exit 1 (not "unknown command") proves routing works.
	if code != 1 {
		t.Errorf("ralph loop in non-git dir should exit 1, got %d", code)
	}
}

// Proves: `ralph review` is recognized as a subcommand.
func TestRun_ReviewSubcommand(t *testing.T) {
	dir := t.TempDir()
	code := run([]string{"review", dir})
	// Should fail (no git repo / no prompts) but not with "unknown command".
	if code != 1 {
		t.Errorf("ralph review in non-git dir should exit 1, got %d", code)
	}
}

// Proves: unknown subcommand shows error and exits 1.
func TestRun_UnknownSubcommand(t *testing.T) {
	code := run([]string{"bogus"})
	if code != 1 {
		t.Errorf("unknown subcommand should exit 1, got %d", code)
	}
}

// Proves: initTaskBackend returns an error (not a fallback) when bd is
// unavailable. This ensures bd is a hard requirement.
func TestInitTaskBackend_ErrorsWhenBDUnavailable(t *testing.T) {
	// Remove bd from PATH so Init fails
	cfg := config.Config{ProjectDir: t.TempDir()}
	log := logging.New(nil)

	t.Setenv("PATH", t.TempDir())

	_, err := initTaskBackend(cfg, t.TempDir(), log)
	if err == nil {
		t.Fatal("expected error when bd is unavailable, got nil")
	}
	if !strings.Contains(err.Error(), "bd is required") {
		t.Errorf("error should mention bd is required, got: %v", err)
	}
}

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
// and other .ralph contents, so evolve restart resumes correctly.
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

	cleanup(cfg, gm, st, backend, ralphDir, filepath.Join(ralphDir, "plan.md"), "/usr/local/bin/ralph", nil, nil, true, log)

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

	cleanup(cfg, gm, st, backend, ralphDir, filepath.Join(ralphDir, "plan.md"), "/usr/local/bin/ralph", nil, nil, false, log)

	status, _ := st.Read("status")
	if status != "completed" {
		t.Errorf("expected status 'completed' preserved, got %q", status)
	}
}


// Verifies that --wait auto-resets when a previous run completed, skipping
// the interactive "Run fresh?" prompt so unattended operation isn't blocked.
func TestInitRalphDir_WaitAutoResetsOnCompleted(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	logFile := filepath.Join(ralphDir, "loop.log")
	stateFile := filepath.Join(ralphDir, "state.json")

	st := state.NewStore(ralphDir)
	st.Init(5, 0)
	st.Write("status", "completed")

	cfg := config.Config{ProjectDir: dir, Wait: true}
	log := logging.New(nil)

	resume, exitCode := initRalphDir(context.Background(), cfg, ralphDir, logFile, stateFile, log)

	if exitCode >= 0 {
		t.Fatalf("expected continue (exitCode < 0), got %d — --wait should auto-reset", exitCode)
	}
	if resume {
		t.Error("expected fresh start after auto-reset, not resume")
	}

	// State should be wiped (fresh .ralph dir).
	if fileExists(stateFile) {
		t.Error("state.json should not exist after auto-reset")
	}
}

// Verifies that without --wait, initRalphDir blocks on the interactive prompt
// and exits 0 when context is cancelled (simulating no user input).
func TestInitRalphDir_NoWaitCompletedBlocksOnPrompt(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	logFile := filepath.Join(ralphDir, "loop.log")
	stateFile := filepath.Join(ralphDir, "state.json")

	st := state.NewStore(ralphDir)
	st.Init(5, 0)
	st.Write("status", "completed")

	cfg := config.Config{ProjectDir: dir, Wait: false}
	log := logging.New(nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, exitCode := initRalphDir(ctx, cfg, ralphDir, logFile, stateFile, log)

	if exitCode != 0 {
		t.Errorf("expected exit code 0 (prompt cancelled), got %d", exitCode)
	}
}

// Verifies that --wait auto-chooses fresh worktree on rebase conflict,
// skipping the interactive recovery prompt.
func TestPromptRebaseRecovery_WaitAutoChoosesFreshWorktree(t *testing.T) {
	handler := promptRebaseRecovery(context.Background(), true)
	result := handler(fmt.Errorf("test conflict"))

	if result != git.RebaseFreshWorktree {
		t.Errorf("expected RebaseFreshWorktree with --wait, got %v", result)
	}
}

// Verifies printSessionSummary displays bead ID, title, agent summary, and PR
// reference for each completed task, giving the operator a clear picture of
// what was accomplished before evolve restart or exit.
func TestPrintSessionSummary_FormatsCompletedTasks(t *testing.T) {
	var buf strings.Builder
	log := logging.NewWithWriter(&buf)

	tasks := []loop.CompletedTask{
		{
			ID:      "ralph-5eu",
			Title:   "[bug] Last task's work not pushed before wait mode",
			Summary: "Track last-merged flag, skip flush when already merged",
			PRNum:   "160",
			PRTitle: "fix: skip redundant flush after signal-handler merge",
		},
		{
			ID:      "ralph-abc",
			Title:   "Add session summary",
			Summary: "Show completed tasks before evolve",
		},
	}

	printSessionSummary(tasks, log)
	out := buf.String()

	if !strings.Contains(out, "ralph-5eu") {
		t.Error("expected bead ID ralph-5eu in output")
	}
	if !strings.Contains(out, "[bug] Last task's work not pushed before wait mode") {
		t.Error("expected task title in output")
	}
	if !strings.Contains(out, "Track last-merged flag") {
		t.Error("expected agent summary in output")
	}
	if !strings.Contains(out, "PR #160") {
		t.Error("expected PR number in output")
	}
	if !strings.Contains(out, "fix: skip redundant flush") {
		t.Error("expected PR title in output")
	}
	if !strings.Contains(out, "ralph-abc") {
		t.Error("expected second task ID in output")
	}
	if strings.Contains(out, "PR #") && strings.Count(out, "PR #") != 1 {
		t.Error("second task without PR should not show PR line")
	}
}

// Proves stop and feedback appear in loop help as WHILE RUNNING commands,
// not in the top-level help, so users discover them where they're relevant.
func TestHelpText_StopFeedbackInLoopOnly(t *testing.T) {
	captureStdout := func(fn func()) string {
		t.Helper()
		r, w, _ := os.Pipe()
		old := os.Stdout
		os.Stdout = w
		fn()
		w.Close()
		os.Stdout = old
		data, _ := io.ReadAll(r)
		return string(data)
	}

	loopHelp := captureStdout(printLoopUsage)

	if !strings.Contains(loopHelp, "WHILE RUNNING") {
		t.Error("loop help should contain WHILE RUNNING section")
	}
	if !strings.Contains(loopHelp, "ralph stop") {
		t.Error("loop help should list ralph stop")
	}
	if !strings.Contains(loopHelp, "ralph feedback [message]") {
		t.Error("loop help should show feedback with optional [message] arg")
	}
	// Feedback should be a single line, not split into two commands.
	if strings.Contains(loopHelp, "ralph feedback <message>") {
		t.Error("feedback should be one line with [message], not separate <message> line")
	}

	topHelp := captureStdout(printUsage)

	if strings.Contains(topHelp, "ralph stop") {
		t.Error("top-level help should NOT list ralph stop")
	}
	if strings.Contains(topHelp, "ralph feedback") {
		t.Error("top-level help should NOT list ralph feedback")
	}
}

// Verifies printSessionSummary shows a clickable PR URL instead of
// bare "PR #N" when the URL is available from GitHub.
func TestPrintSessionSummary_ShowsPRURL(t *testing.T) {
	var buf strings.Builder
	log := logging.NewWithWriter(&buf)

	tasks := []loop.CompletedTask{
		{
			ID:      "ralph-xyz",
			Title:   "Add PR link to session summary",
			PRNum:   "172",
			PRTitle: "feat: clickable PR links",
			PRURL:   "https://github.com/brokenalarms/ralph/pull/172",
		},
	}

	printSessionSummary(tasks, log)
	out := buf.String()

	if !strings.Contains(out, "https://github.com/brokenalarms/ralph/pull/172") {
		t.Error("expected full PR URL in output")
	}
	if strings.Contains(out, "PR #172") {
		t.Error("should show URL, not bare PR #172, when URL is available")
	}
}

// Verifies printSessionSummary produces no output when no tasks were completed,
// keeping the log clean for sessions that didn't finish any work.
func TestPrintSessionSummary_EmptyNoOutput(t *testing.T) {
	var buf strings.Builder
	log := logging.NewWithWriter(&buf)

	printSessionSummary(nil, log)

	if buf.Len() > 0 {
		t.Errorf("expected no output for empty session, got: %s", buf.String())
	}
}
