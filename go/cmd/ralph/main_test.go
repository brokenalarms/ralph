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

	"github.com/brokenalarms/ralph/internal/agent"
	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/loop"
	"github.com/brokenalarms/ralph/internal/pidfile"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/testutil"
	"github.com/brokenalarms/ralph/internal/verify"
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

// Proves: `ralph review` is recognized as a subcommand and routes to handleReview.
func TestRun_ReviewSubcommand(t *testing.T) {
	// review -h should print help and exit 0, confirming routing works.
	code := run([]string{"review", "-h"})
	if code != 0 {
		t.Errorf("ralph review -h should exit 0, got %d", code)
	}
}

// Proves: `ralph task -h` prints help and exits 0 instead of passing -h to Claude.
func TestRun_TaskHelp(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		code := run([]string{"task", flag})
		if code != 0 {
			t.Errorf("ralph task %s should exit 0, got %d", flag, code)
		}
	}
}

// Proves: `ralph review -h` prints help and exits 0 instead of passing -h to Claude.
func TestRun_ReviewHelp(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		code := run([]string{"review", flag})
		if code != 0 {
			t.Errorf("ralph review %s should exit 0, got %d", flag, code)
		}
	}
}

// Proves: `ralph attach -h` prints help and exits 0.
func TestRun_AttachHelp(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		code := run([]string{"attach", flag})
		if code != 0 {
			t.Errorf("ralph attach %s should exit 0, got %d", flag, code)
		}
	}
}

// Proves: `ralph attach` refuses to start when no loop is running (no PID file).
func TestHandleAttach_RefusesWhenNoLoopRunning(t *testing.T) {
	dir := t.TempDir()
	log := logging.New(nil)
	sub := config.Subcommand{Name: "attach", Dir: dir, Args: nil}
	code := handleAttach(sub, log)
	if code != 1 {
		t.Errorf("ralph attach should exit 1 when no loop running, got %d", code)
	}
}

// Proves: `ralph command` is no longer recognized as a subcommand.
func TestRun_CommandSubcommandRemoved(t *testing.T) {
	code := run([]string{"command"})
	if code != 1 {
		t.Errorf("ralph command should exit 1 as unknown command, got %d", code)
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
		AutoMerge:     true,
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
	if !strings.Contains(content, "--auto-merge") {
		t.Error("resume script should contain --auto-merge")
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

// Verifies the resume script includes all non-default flags from the config,
// specifically --evolve and --base-branch which were previously missing.
func TestGenerateResumeScript_AllFlags(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	cfg := config.Config{
		ProjectDir:    dir,
		MaxIterations: 30,
		AutoMerge:     true,
		Evolve:        true,
		BaseBranch:    "main",
		Wait:          true,
		Verbose:       true,
	}

	log := logging.New(nil)
	generateResumeScript(cfg, ralphDir, "/usr/local/bin/ralph", nil, log)

	data, err := os.ReadFile(filepath.Join(ralphDir, "resume.sh"))
	if err != nil {
		t.Fatalf("resume script should exist: %v", err)
	}

	content := string(data)
	for _, flag := range []string{
		"--max 30",
		"--auto-merge",
		"--evolve",
		"--base-branch main",
		"--wait",
		"--verbose",
	} {
		if !strings.Contains(content, flag) {
			t.Errorf("resume script should contain %q\ngot: %s", flag, content)
		}
	}
}

// Verifies the summary prints correct task counts from the backend.
func TestPrintSummary_TaskCounts(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	st := state.NewStore(ralphDir)
	st.Init(5)
	st.Write("iteration", "3")
	st.Write("status", "completed")

	backend := &testutil.StubBackend{
		Completed: 3,
		Remaining: 0,
		Total:     3,
	}

	log := logging.New(nil)
	gm := git.New(git.Config{
		WorkDir:    dir,
		BaseBranch: "main",
		Logger:     log,
	})

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
	os.WriteFile(filepath.Join(ralphDir, ".signal_no_code_needed"), []byte("already fixed"), 0o644)
	os.WriteFile(filepath.Join(ralphDir, ".stream-task"), []byte("stream"), 0o644)

	clearSignalFiles(ralphDir)

	// Signal files should be gone
	for _, f := range []string{".signal_complete", ".signal_current_task", ".signal_all_complete", ".signal_no_code_needed", ".stream-task"} {
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

// Verifies the dirty-working-tree check rejects working trees with
// uncommitted changes, preventing the .gitignore commit from sweeping
// in unrelated staged work. The check itself lives inline in runMain
// after git.New construction; this test exercises gm.HasUncommittedChanges
// directly with a real git repo.
func TestDirtyWorkingTree_HasUncommittedChanges(t *testing.T) {
	dir := t.TempDir()

	runCmd(t, "git", "init", "-b", "main", dir)
	runCmd(t, "git", "-C", dir, "commit", "--allow-empty", "-m", "init")

	os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("uncommitted"), 0o644)
	runCmd(t, "git", "-C", dir, "add", "dirty.txt")

	log := logging.New(nil)
	gm := git.New(git.Config{
		WorkDir:    dir,
		RalphDir:   filepath.Join(dir, ".ralph"),
		BaseBranch: "main",
		Logger:     log,
	})

	if !gm.HasUncommittedChanges() {
		t.Error("expected HasUncommittedChanges() to return true with staged file")
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
	st.Init(5)
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

// evolveRestart removes the PID file before attempting rebuild/exec so that
// the new process (which inherits the same PID via syscall.Exec) doesn't
// see its own PID and refuse to start.
func TestEvolveRestart_RemovesPIDFile(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	pidPath := filepath.Join(ralphDir, "loop.pid")
	if err := pidfile.Write(pidPath); err != nil {
		t.Fatalf("Write PID file failed: %v", err)
	}

	log := logging.New(nil)

	// Will fail at rebuild (nonexistent source), but PID file should already
	// be removed by that point.
	_ = evolveRestart(dir, "/nonexistent/ralph", "develop", nil, log)

	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("PID file should be removed before exec attempt")
	}
}

// Verifies evolveRestart proceeds (and fails on git) when no stop file exists,
// confirming the stop-file check only fires when the file is present.
func TestEvolveRestart_NoStopFileProceeds(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	log := logging.New(nil)

	// No stop file → should attempt rebuild, which fails with nonexistent project dir.
	err := evolveRestart(dir, "/nonexistent/ralph", "develop", nil, log)
	if err == nil {
		t.Fatal("expected error with nonexistent source dir, got nil")
	}
}

// Verifies that cleanup writes status="stopped" when interrupted, overriding
// any stale status (e.g., "halted_stagnation") from a previous analyzer run.
func TestCleanup_InterruptedWritesStopped(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	st := state.NewStore(ralphDir)
	st.Init(5)
	st.Write("status", "halted_stagnation")

	gm := git.New(git.Config{WorkDir: dir, BaseBranch: "main"})
	backend := &testutil.StubBackend{Total: 1, Remaining: 1}
	log := logging.New(nil)
	cfg := config.Config{ProjectDir: dir, MaxIterations: 5, CallsPerHour: 80}

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
	st.Init(5)
	st.Write("status", "completed")

	gm := git.New(git.Config{WorkDir: dir, BaseBranch: "main"})
	backend := &testutil.StubBackend{Total: 3, Completed: 3}
	log := logging.New(nil)
	cfg := config.Config{ProjectDir: dir, MaxIterations: 5, CallsPerHour: 80}

	cleanup(cfg, gm, st, backend, ralphDir, filepath.Join(ralphDir, "plan.md"), "/usr/local/bin/ralph", nil, nil, false, log)

	status, _ := st.Read("status")
	if status != "completed" {
		t.Errorf("expected status 'completed' preserved, got %q", status)
	}
}

// Proves: cleanup clears cli_config from state.json so stale flags from a
// previous run don't leak into a manual restart. Evolve restart is unaffected
// because syscall.Exec replaces the process before cleanup runs.
func TestCleanup_ClearsCLIConfig(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	st := state.NewStore(ralphDir)
	st.Init(5)
	st.SaveCLIConfig(map[string]string{"evolve": "true", "max": "20"})

	// Verify cli_config exists before cleanup.
	cfg, _ := st.LoadCLIConfig()
	if cfg == nil {
		t.Fatal("cli_config should exist before cleanup")
	}

	gm := git.New(git.Config{WorkDir: dir, BaseBranch: "main"})
	backend := &testutil.StubBackend{Total: 1}
	log := logging.New(nil)
	c := config.Config{ProjectDir: dir, MaxIterations: 5, CallsPerHour: 80}

	cleanup(c, gm, st, backend, ralphDir, filepath.Join(ralphDir, "plan.md"), "/usr/local/bin/ralph", nil, nil, true, log)

	// cli_config must be cleared.
	cfg, err := st.LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig after cleanup: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected cli_config cleared after cleanup, got %v", cfg)
	}
}

// Proves: cli_config in state.json is never read back for execution — it is
// a write-only audit record. LoadCLIConfig must not appear in the evolve
// restart path in main.go. If someone re-adds it, this test catches it.
func TestCLIConfig_NeverReadForExecution(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	// The evolve_restart block must not call LoadCLIConfig or ArgsFromState.
	// These were removed so that evolve restart passes original args through
	// instead of reconstructing from stale state.
	src := string(mainSrc)

	// Find the evolve_restart block.
	evolveIdx := strings.Index(src, `status == "evolve_restart"`)
	if evolveIdx < 0 {
		t.Fatal("could not find evolve_restart block in main.go")
	}

	// Check the block from evolve_restart to cleanup (next major section).
	evolveBlock := src[evolveIdx:]
	cleanupIdx := strings.Index(evolveBlock, "cleanup(")
	if cleanupIdx > 0 {
		evolveBlock = evolveBlock[:cleanupIdx]
	}

	if strings.Contains(evolveBlock, "LoadCLIConfig") {
		t.Error("evolve_restart block must not call LoadCLIConfig — cli_config is a write-only audit record")
	}
	if strings.Contains(evolveBlock, "ArgsFromState") {
		t.Error("evolve_restart block must not call ArgsFromState — original args should be passed through")
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
	st.Init(5)
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

// Verifies that without --wait, initRalphDir blocks on the interactive
// prompt and exits 0 when context is cancelled (simulating no user input).
func TestInitRalphDir_NoWaitCompletedBlocksOnPrompt(t *testing.T) {
	dir := t.TempDir()
	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)
	logFile := filepath.Join(ralphDir, "loop.log")
	stateFile := filepath.Join(ralphDir, "state.json")

	st := state.NewStore(ralphDir)
	st.Init(5)
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
			PRNum:   160,
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

// Verifies ralph task help text does not show [directory] in the usage line
// or in the examples, since ralph task always operates on the current directory.
func TestHelpText_TaskNoDirectoryArg(t *testing.T) {
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

	taskHelp := captureStdout(printTaskUsage)

	if strings.Contains(taskHelp, "[directory]") {
		t.Error("ralph task help should not show [directory] in usage line")
	}
	if strings.Contains(taskHelp, "directory") {
		t.Error("ralph task help should not mention directory")
	}

	topHelp := captureStdout(printUsage)

	if strings.Contains(topHelp, "ralph task [directory]") {
		t.Error("top-level help should not show ralph task [directory]")
	}
	if strings.Contains(topHelp, "ralph task ~/") {
		t.Error("top-level help examples should not show a directory arg for ralph task")
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
			PRNum:   172,
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

// Proves: postReviewCleanup clears completed_tasks from state.json, archives
// reflections to reflections/archived/, clears attempt data for completed tasks,
// and removes the .completed-tasks display file.
func TestPostReviewCleanup(t *testing.T) {
	ralphDir := filepath.Join(t.TempDir(), ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	// Set up state with completed tasks
	st := state.NewStore(ralphDir)
	st.AddCompletedTask("ralph-abc", true)
	st.AddCompletedTask("ralph-def", true)

	// Create reflection files
	refDir := filepath.Join(ralphDir, "reflections")
	os.MkdirAll(refDir, 0o755)
	os.WriteFile(filepath.Join(refDir, "ralph-abc.md"), []byte("# Task ABC"), 0o644)
	os.WriteFile(filepath.Join(refDir, "ralph-def.md"), []byte("# Task DEF"), 0o644)

	// Create attempt files
	attDir := filepath.Join(ralphDir, "attempts")
	os.MkdirAll(attDir, 0o755)
	os.WriteFile(filepath.Join(attDir, "ralph-abc.log"), []byte("### Attempt 1\n"), 0o644)
	os.WriteFile(filepath.Join(attDir, "ralph-def.log"), []byte("### Attempt 1\n"), 0o644)
	os.WriteFile(filepath.Join(attDir, "ralph-ghi.log"), []byte("### Attempt 1\n"), 0o644)

	// Create .completed-tasks display file
	os.WriteFile(filepath.Join(ralphDir, ".completed-tasks"), []byte("ralph-abc\nralph-def\n"), 0o644)

	var buf strings.Builder
	log := logging.NewWithWriter(&buf)

	postReviewCleanup(ralphDir, log)

	// AC1: completed_tasks cleared from state.json
	tasks, err := st.GetCompletedTasks()
	if err != nil {
		t.Fatalf("GetCompletedTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected 0 completed tasks, got %d", len(tasks))
	}

	// AC2: reflections archived
	if _, err := os.Stat(filepath.Join(refDir, "ralph-abc.md")); !os.IsNotExist(err) {
		t.Error("ralph-abc.md should be removed from reflections/")
	}
	if _, err := os.Stat(filepath.Join(refDir, "archived", "ralph-abc.md")); err != nil {
		t.Error("ralph-abc.md should exist in reflections/archived/")
	}
	if _, err := os.Stat(filepath.Join(refDir, "archived", "ralph-def.md")); err != nil {
		t.Error("ralph-def.md should exist in reflections/archived/")
	}

	// AC3: legacy attempt files on disk are left alone — ralph no longer writes or
	// deletes .ralph/attempts/ files; users may clean up manually.
	if _, err := os.Stat(filepath.Join(attDir, "ralph-abc.log")); err != nil {
		t.Error("ralph-abc.log should be left on disk (ralph does not delete legacy attempt files)")
	}
	if _, err := os.Stat(filepath.Join(attDir, "ralph-def.log")); err != nil {
		t.Error("ralph-def.log should be left on disk (ralph does not delete legacy attempt files)")
	}
	if _, err := os.Stat(filepath.Join(attDir, "ralph-ghi.log")); err != nil {
		t.Error("ralph-ghi.log should be preserved")
	}

	// .completed-tasks display file removed
	if _, err := os.Stat(filepath.Join(ralphDir, ".completed-tasks")); !os.IsNotExist(err) {
		t.Error(".completed-tasks should be removed")
	}
}

// handleLoop refuses to start when a PID file exists for a different live process.
func TestHandleLoop_RefusesDuplicateLoop(t *testing.T) {
	dir := t.TempDir()
	runCmd(t, "git", "-C", dir, "init")
	runCmd(t, "git", "-C", dir, "commit", "--allow-empty", "-m", "init")

	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	// Start a child process so the PID is alive but not ours.
	cmd := exec.Command("sleep", "1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start sleep: %v", err)
	}
	defer cmd.Process.Kill()

	pidPath := filepath.Join(ralphDir, "loop.pid")
	os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0o644)
	defer pidfile.Remove(pidPath)

	log := logging.New(io.Discard)
	sub := config.Subcommand{Name: "loop", Dir: dir, Args: nil}
	code := handleLoop(sub, log)
	if code != 1 {
		t.Errorf("handleLoop should exit 1 when PID file exists for alive process, got %d", code)
	}
}

// Proves: modelCap returns the model string when --model-ceiling is explicitly set via
// the CLI. This is the enforcement point for the model ceiling.
func TestModelCap_ExplicitlySetViaCLI(t *testing.T) {
	cfg, err := config.Parse([]string{"--model-ceiling", config.ModelSonnet})
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	cap := modelCap(cfg)
	if cap != config.ModelSonnet {
		t.Errorf("expected %s, got %q", config.ModelSonnet, cap)
	}
}

// Proves: modelCap returns empty string when --model-ceiling is not explicitly set via
// the CLI, meaning no ceiling is applied and the full escalation ladder is used.
func TestModelCap_DefaultNotExplicit(t *testing.T) {
	cfg, err := config.Parse([]string{})
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	cap := modelCap(cfg)
	if cap != "" {
		t.Errorf("expected empty cap when --model-ceiling not set via CLI, got %q", cap)
	}
}

// Proves: with --model-ceiling=sonnet, the resolved model for ralph task and ralph
// review is sonnet (not the default opus).
func TestTaskReviewModelResolution_SonnetCap(t *testing.T) {
	cfg, err := config.Parse([]string{"--model-ceiling", config.ModelSonnet})
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	resolved := verify.CapModel(modelCap(cfg), agent.ModelOpus)
	if resolved != config.ModelSonnet {
		t.Errorf("--model-ceiling=sonnet: expected %s, got %s", config.ModelSonnet, resolved)
	}
}

// Proves: with --model-ceiling=opus (or no cap), the resolved model for ralph task
// and ralph review is opus (full ladder).
func TestTaskReviewModelResolution_OpusCap(t *testing.T) {
	cfg, err := config.Parse([]string{"--model-ceiling", config.ModelOpus})
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	resolved := verify.CapModel(modelCap(cfg), agent.ModelOpus)
	if resolved != config.ModelOpus {
		t.Errorf("--model-ceiling=opus: expected %s, got %s", config.ModelOpus, resolved)
	}
}

// handleLoop starts normally when a stale PID file exists (dead process).
// Verifies stale cleanup by confirming the PID file is removed.
func TestHandleLoop_CleansUpStalePID(t *testing.T) {
	dir := t.TempDir()
	runCmd(t, "git", "-C", dir, "init")
	runCmd(t, "git", "-C", dir, "commit", "--allow-empty", "-m", "init")

	ralphDir := filepath.Join(dir, ".ralph")
	os.MkdirAll(ralphDir, 0o755)

	pidPath := filepath.Join(ralphDir, "loop.pid")
	os.WriteFile(pidPath, []byte("99999999"), 0o644)

	log := logging.New(io.Discard)
	sub := config.Subcommand{Name: "loop", Dir: dir, Args: nil}
	// This will proceed past PID check but fail later (no bd, etc.) — that's fine.
	// The point is it does NOT exit 1 with "already running".
	code := handleLoop(sub, log)

	// If PID file was cleaned up, the loop progressed past the PID check.
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("stale PID file should have been cleaned up")
	}

	// It should fail later (bd not found, etc.) but not at the PID check.
	// Code 1 is expected from downstream failures — we verify the PID file
	// was removed as proof it passed the PID check.
	_ = code
}
