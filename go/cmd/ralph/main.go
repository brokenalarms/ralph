package main

import (
	"context"
	"embed"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/loop"
	"github.com/brokenalarms/ralph/internal/pidfile"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/tmux"
	"github.com/brokenalarms/ralph/internal/verifier"
	"github.com/brokenalarms/ralph/internal/workctx"
)

//go:embed prompts/*
var embeddedPrompts embed.FS

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	log := logging.New(nil)

	if sub, ok := config.ParseSubcommand(args); ok {
		return handleSubcommand(sub, log)
	}

	// No subcommand: check for help flag, otherwise show usage.
	for _, a := range args {
		if a == "-h" || a == "--help" {
			printUsage()
			return 0
		}
	}

	if len(args) > 0 {
		log.Emit(logging.Opts{Level: logging.Error}, "unknown command: %s", args[0])
		fmt.Println()
	}

	printUsage()
	if len(args) > 0 {
		return 1
	}
	return 0
}

// modelCap returns the model ceiling for all LLM interactions when --model-ceiling
// was explicitly set via the CLI. An empty string means no cap (full
// escalation ladder applies).
func modelCap(cfg config.Config) string {
	if cfg.CLISet("model_ceiling") {
		return cfg.Model
	}
	return ""
}

func runMain(cfg config.Config, dirs workctx.WorkContext, scriptPath string, args []string, log *logging.Logger) int {
	ralphDir := dirs.RalphDir
	promptsDir := dirs.PromptsDir
	// Set up signal handling for cleanup.
	ctx, cancel := context.WithCancel(context.Background())
	interrupted := false
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Emit(logging.Opts{Level: logging.Warn}, "Ctrl-C received — exiting gracefully")
		interrupted = true
		cancel()
	}()

	stateFile := filepath.Join(ralphDir, "state.json")
	logFile := filepath.Join(ralphDir, "loop.log")

	// Phase 1 — initialize the .ralph state directory and detect resume
	// status. Pure local-state setup; no git operations.
	resume, exitCode := initRalphDir(ctx, &cfg, ralphDir, logFile, stateFile, log)
	if exitCode >= 0 {
		return exitCode
	}

	// Write PID file so other ralph instances can detect a running loop.
	pidPath := filepath.Join(ralphDir, "loop.pid")
	if err := pidfile.Write(pidPath); err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Failed to write PID file: %v", err)
		return 1
	}
	defer pidfile.Remove(pidPath)

	// Clear stale stop file from a previous run so we don't halt immediately.
	os.Remove(filepath.Join(ralphDir, "stop"))

	planFile := filepath.Join(ralphDir, "plan.md")

	// Phase 2 — state and backend init. Pure data-store setup, no git.
	st := state.NewStore(ralphDir)
	if err := st.Init(cfg.MaxIterations); err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Failed to initialize state: %v", err)
		return 1
	}
	if err := st.SaveCLIConfig(config.ConfigToState(&cfg)); err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Failed to save CLI config to state: %v", err)
		return 1
	}

	// Open the log file and switch the logger over to file-backed writes.
	logFileWriter, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Failed to open log file: %v", err)
		return 1
	}
	defer logFileWriter.Close()
	log = logging.New(logFileWriter)

	backend, err := initTaskBackend(cfg, promptsDir, log)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Task backend init failed: %v", err)
		return 1
	}
	st.Write("task_backend", backend.Label())

	// Phase 3 — construct git module. All data inputs are ready. Git
	// constructs its own sub-modules (GitHub CLI, state store) internally.
	gm := git.New(git.Config{
		WorkDir:                     cfg.ProjectDir,
		RalphDir:                    ralphDir,
		BaseBranch:                  cfg.BaseBranch,
		Resume:                      resume,
		Logger:                      log,
		CompileCheckTimeout:         cfg.CompileCheckTimeout,
		CIPollTimeout:               cfg.CIPollTimeout,
		NoCIGracePeriod:             cfg.NoCIGracePeriod,
		CopilotGatedTimeout:         cfg.ReviewerGatedTimeout,
		CopilotOpportunisticTimeout: cfg.ReviewerOpportunisticTimeout,
		CodeRabbitTimeout:           cfg.CodeRabbitReviewTimeout,
		AdminMergeOnCIInfraFailure:  cfg.AdminMergeOnCIInfraFailure,
	})

	// Phase 4 — git pre-flight checks and worktree setup. gm.Init bundles
	// ValidateRemoteBranch + dirty-tree check + EnsureGitignored +
	// PruneOrphanedWorktrees + SetupWorktree so callers can't forget the
	// sequence and a future caller gets one method to call.
	if err := gm.Init(ctx); err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "%v", err)
		return 1
	}
	dirs.WorkDir = gm.GetWorkDir()

	// Write initial branch label for the pane title updater. On resume,
	// the old task branch is still checked out until the loop renames it,
	// so show a transitional label instead of the stale branch name.
	runBranchFile := filepath.Join(ralphDir, ".run-branch")
	if resume && gm.GetWorktreeBranch() != "" && !strings.HasSuffix(gm.GetWorktreeBranch(), "/next") {
		os.WriteFile(runBranchFile, []byte("resuming…"), 0o644)
	} else {
		branch := gm.GetWorktreeBranch()
		if branch == "" {
			branch = "ralph"
		}
		os.WriteFile(runBranchFile, []byte(branch), 0o644)
	}

	log.Phase("Ralph Loop v%s (go)", config.Version)
	log.Emit(logging.Opts{}, "Project: %s", dirs.ProjectDir)
	if dirs.WorkDir != dirs.ProjectDir {
		log.Emit(logging.Opts{}, "Worktree: %s", dirs.WorkDir)
	}
	log.Emit(logging.Opts{}, "Task backend: %s", backend.Label())
	log.Emit(logging.Opts{}, "Max iterations: %d", cfg.MaxIterations)

	st.Write("started_at", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	st.ClearPushedBranches()

	// Construct the verifier module. Verifier holds no module references —
	// it exposes stateless operations that Loop orchestrates. See
	// internal/verifier/verifier.go for the rationale.
	vrf := verifier.New(verifier.Config{
		VerifyDir:             dirs.WorkDir,
		ProjectDir:            dirs.ProjectDir,
		ConfigVerify:          cfg.Verify,
		VerifyModel:           cfg.VerifyModel,
		VerifyEscalationModel: cfg.VerifyEscalationModel,
		FixModel:              cfg.FixModel,
		FixEscalationModel:    cfg.FixEscalationModel,
		ModelCap:              modelCap(cfg),
		PromptsDir:            dirs.PromptsDir,
		RalphDir:              ralphDir,
		IdleTimeout:           cfg.IdleTimeout,
		FixMaxRunDuration:     cfg.FixMaxRunDuration,
		TestTimeout:           cfg.TestTimeout,
		CompileCheckTimeout:   cfg.CompileCheckTimeout,
		Signals:               claude.DefaultSignalPaths(ralphDir),
	}, log, nil, nil)

	// Execution phase.
	execLoop := loop.New(loop.Config{
		Dirs:                     dirs,
		PlanFile:                 planFile,
		MaxIterations:            cfg.MaxIterations,
		Verbose:                  cfg.Verbose,
		AutoMerge:                cfg.AutoMerge,
		Evolve:                   cfg.Evolve,
		CallsPerHour:             cfg.CallsPerHour,
		IdleTimeout:              cfg.IdleTimeout,
		IdleTimeoutProgress:      cfg.IdleTimeoutProgress,
		MaxRunDuration:           cfg.MaxRunDuration,
		PostSignalTimeout:        cfg.PostSignalTimeout,
		PostTask:                 cfg.PostTask,
		VerifyBuild:              cfg.VerifyBuild,
		Verify:                   cfg.Verify,
		Notify:                   cfg.Notify,
		Wait:                     cfg.Wait,
		Model:                    cfg.Model,
		AgentEscalationModel:     cfg.AgentEscalationModel,
		ModelCap:                 modelCap(cfg),
		Version:                  config.Version,
		VerifyDir:                dirs.WorkDir,
		VerifyModel:              cfg.VerifyModel,
		VerifyEscalationModel:    cfg.VerifyEscalationModel,
		FixModel:                 cfg.FixModel,
		FixEscalationModel:       cfg.FixEscalationModel,
		MaxPromptAttempts:        cfg.MaxPromptAttempts,
		MaxIdleTimeoutFailures:   cfg.MaxIdleTimeoutFailures,
		MaxLLMVerifyAttempts:     cfg.MaxLLMVerifyAttempts,
		MaxTestFixAttempts:       cfg.MaxTestFixAttempts,
		TestTimeout:              cfg.TestTimeout,
		CompileCheckTimeout:      cfg.CompileCheckTimeout,
		ConnectivityCheckTimeout: cfg.ConnectivityCheckTimeout,
		InternetRestoreInterval:  cfg.InternetRestoreInterval,
	}, loop.Modules{
		State:       st,
		Git:         gm,
		TaskBackend: backend,
		Logger:      log,
		Verifier:    vrf,
		IterationHook: &resumeScriptHook{
			cfg: cfg, ralphDir: ralphDir, scriptPath: scriptPath, args: args, log: log,
		},
	})

	if err := execLoop.Run(ctx); err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Execution failed: %v", err)
	}

	sessionTasks := execLoop.SessionTasks()

	if status, _ := st.Read("status"); status == "evolve_restart" {
		printSessionSummary(sessionTasks, log)
		if err := evolveRestart(cfg.ProjectDir, scriptPath, cfg.BaseBranch, args, log); err != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "Evolve restart failed: %v", err)
		}
	}

	cleanup(cfg, gm, st, backend, ralphDir, planFile, scriptPath, args, sessionTasks, interrupted, log)
	return 0
}

// initRalphDir creates .ralph, checks for dirty working tree, handles resume
// detection. Returns (resume, exitCode). exitCode < 0 means continue.
// initRalphDir creates the .ralph state directory (mkdir + touched log
// files + reflections subdirectory) and detects resume status from
// state.json. When the previous run completed, it prompts the user to
// run fresh (or auto-resets under --wait).
//
// This is pure local-state setup — it does not touch git, GitHub, or
// the task backend. The git pre-flight checks (uncommitted changes,
// gitignore, prune worktrees) live separately in runMain after git.New.
//
// Returns:
//   - resume=true, exitCode=-1: resume from previous run
//   - resume=false, exitCode=-1: fresh run
//   - exitCode=0: clean exit (user declined to rerun completed task)
//   - exitCode=1: hard error
func initRalphDir(ctx context.Context, cfg *config.Config, ralphDir, logFile, stateFile string, log *logging.Logger) (bool, int) {
	if err := os.MkdirAll(ralphDir, 0o755); err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Failed to create .ralph dir: %v", err)
		return false, 1
	}

	configPath := filepath.Join(ralphDir, "config.toml")
	if _, statErr := os.Stat(configPath); os.IsNotExist(statErr) {
		if initErr := config.InitConfig(configPath); initErr != nil {
			log.Emit(logging.Opts{Level: logging.Warn}, "Failed to create config.toml: %v", initErr)
		}
	}
	if loadErr := cfg.LoadConfigFile(configPath); loadErr != nil {
		log.Emit(logging.Opts{Level: logging.Warn}, "Failed to load config.toml: %v", loadErr)
	}

	touchFile(logFile)
	touchFile(filepath.Join(ralphDir, "raw.log"))
	os.MkdirAll(filepath.Join(ralphDir, "reflections"), 0o755)

	// Resume detection — pure state-file check, no git involved.
	if fileExists(stateFile) {
		st := state.NewStore(ralphDir)
		status, _ := st.Read("status")
		if status == "completed" {
			log.Emit(logging.Opts{}, "All tasks completed from previous run.")
			runFresh := false
			if cfg.Wait {
				log.Emit(logging.Opts{}, "--wait: auto-resetting for new tasks")
				runFresh = true
			} else {
				fmt.Printf("%s[ralph v%s (go)]%s Run fresh? (y/n) ", logging.Yellow, config.Version, logging.Reset)
				answer, err := readLineCtx(ctx)
				if err != nil {
					return false, 0
				}
				runFresh = answer == "y" || answer == "Y"
			}
			if runFresh {
				os.RemoveAll(ralphDir)
				os.MkdirAll(ralphDir, 0o755)
				touchFile(logFile)
				touchFile(filepath.Join(ralphDir, "raw.log"))
				return false, -1
			}
			return false, 0
		}
		log.Emit(logging.Opts{}, "Resuming from previous state (status: %s)", status)
		return true, -1
	}

	return false, -1
}


// resumeScriptHook implements loop.IterationHook by regenerating the
// resume script at the start of each loop iteration. The script captures
// the current task / branch / state so the user can resume from the most
// recent point.
type resumeScriptHook struct {
	cfg        config.Config
	ralphDir   string
	scriptPath string
	args       []string
	log        *logging.Logger
}

// OnIterationStart implements loop.IterationHook.
func (h *resumeScriptHook) OnIterationStart() {
	generateResumeScript(h.cfg, h.ralphDir, h.scriptPath, h.args, h.log)
}

// initTaskBackend initializes the bd task backend. BD is required — if
// unavailable, ralph exits with an error.
func initTaskBackend(cfg config.Config, promptsDir string, log *logging.Logger) (tasks.Backend, error) {
	bd := &tasks.BD{
		ProjectDir: cfg.ProjectDir,
		PromptsDir: promptsDir,
	}
	if err := bd.Init(); err != nil {
		return nil, fmt.Errorf("bd is required but unavailable: %w", err)
	}
	return bd, nil
}

// cleanup generates resume script, prints summary, and removes unused worktrees.
func cleanup(cfg config.Config, gm git.Ops, st *state.Store, backend tasks.Backend, ralphDir, planFile, scriptPath string, args []string, sessionTasks []loop.CompletedTask, interrupted bool, log *logging.Logger) {
	clearSignalFiles(ralphDir)

	// Clear cli_config so stale flags don't persist across manual restarts.
	// Evolve restart preserves cli_config because syscall.Exec replaces the
	// process before cleanup runs.
	st.ClearCLIConfig()

	if interrupted {
		log.Emit(logging.Opts{Level: logging.Warn}, "Session interrupted — cleaning up")
		log.Emit(logging.Opts{}, "Writing interrupted state to bead...")
		st.Write("status", "stopped")
	}

	// Remove unused worktree (branch still named /next = no work committed).
	if gm.GetWorktreeBranch() != "" &&
		strings.HasSuffix(gm.GetWorktreeBranch(), "/next") &&
		gm.GetWorkDir() != cfg.ProjectDir {
		if interrupted {
			log.Emit(logging.Opts{}, "Cleaning up worktree %s...", gm.GetWorktreeBranch())
			gm.RemoveWorktree()
		}
	}

	generateResumeScript(cfg, ralphDir, scriptPath, args, log)
	printSessionSummary(sessionTasks, log)
	printSummary(cfg, gm, st, backend, ralphDir, planFile, log)
}

// generateResumeScript writes a shell script that re-runs ralph with the same
// flags, allowing the user to easily resume after interruption. Flags are
// derived from the config registry so new flags are included automatically.
func generateResumeScript(cfg config.Config, ralphDir, scriptPath string, args []string, log *logging.Logger) {
	resumePath := filepath.Join(ralphDir, "resume.sh")

	stateMap := config.ConfigToState(&cfg)
	if cfg.UseTmux || os.Getenv("_RALPH_TMUX_SESSION") != "" {
		stateMap["tmux"] = "true"
	}
	extraArgs := config.ArgsFromState(stateMap)

	extra := ""
	if len(extraArgs) > 0 {
		extra = " " + strings.Join(extraArgs, " ")
	}

	content := fmt.Sprintf(`#!/usr/bin/env bash
# Ralph Loop - Resume Script
# Generated at: %s
cd "%s"
exec "%s" loop%s
`, time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		cfg.ProjectDir, scriptPath, extra)

	os.WriteFile(resumePath, []byte(content), 0o755)
	log.Emit(logging.Opts{}, "Resume script: %s", resumePath)
}

// printSessionSummary shows what was accomplished this session: each completed
// task with its bead ID, title, agent summary, and PR reference.
func printSessionSummary(tasks []loop.CompletedTask, log *logging.Logger) {
	if len(tasks) == 0 {
		return
	}
	fmt.Println()
	log.Phase("=== SESSION WORK ===")
	for _, t := range tasks {
		label := t.ID
		if label == "" {
			label = t.Title
		}
		if t.Title != "" && t.ID != "" {
			log.Emit(logging.Opts{}, "%s: %s", t.ID, t.Title)
		} else {
			log.Emit(logging.Opts{}, "%s", label)
		}
		if t.Summary != "" {
			log.Emit(logging.Opts{}, "  Fix: %s", t.Summary)
		}
		if t.PRNum != 0 {
			pr := fmt.Sprintf("PR #%d", t.PRNum)
			if t.PRURL != "" {
				pr = t.PRURL
			}
			if t.PRTitle != "" {
				log.Emit(logging.Opts{}, "  %s: %s", pr, t.PRTitle)
			} else {
				log.Emit(logging.Opts{}, "  %s", pr)
			}
		}
	}
}

// printSummary displays the end-of-run summary.
func printSummary(cfg config.Config, gm git.Ops, st *state.Store, backend tasks.Backend, ralphDir, planFile string, log *logging.Logger) {
	fmt.Println()
	log.Phase("=== SUMMARY ===")

	iteration, _ := st.Read("iteration")
	status, _ := st.Read("status")
	log.Emit(logging.Opts{}, "Status:     %s", status)
	log.Emit(logging.Opts{}, "Iterations: %s lifetime", iteration)

	completed, _ := backend.CountCompleted()
	remaining, _ := backend.CountRemaining()
	total, _ := backend.CountTotal()
	log.Emit(logging.Opts{}, "Tasks: %d/%d completed, %d remaining", completed, total, remaining)

	log.Emit(logging.Opts{}, "Log:        %s", filepath.Join(ralphDir, "loop.log"))

	if gm.GetWorktreeBranch() != "" && gm.GetProjectDir() != "" {
		log.Emit(logging.Opts{}, "Worktree:   %s", gm.GetWorkDir())

		pushed, _ := st.GetPushedBranches()
		if len(pushed) > 0 {
			log.Emit(logging.Opts{}, "Pushed branches:")
			for _, b := range pushed {
				log.Emit(logging.Opts{}, "  %s", b)
			}
		} else {
			log.Emit(logging.Opts{}, "Branch:     %s", gm.GetWorktreeBranch())
		}
		log.Emit(logging.Opts{}, "To merge:   git merge %s", gm.GetWorktreeBranch())
	}

	hasRemaining, _ := backend.HasRemaining()
	if hasRemaining {
		log.Emit(logging.Opts{}, "Resume:     %s", filepath.Join(ralphDir, "resume.sh"))
	}
}

// handleTmuxAttach attaches to an already-running loop's tmux session.
// If the loop was started with --tmux the session already exists; attach to it
// directly without creating a new one. Otherwise create a new session whose
// loop pane tails the log.
func handleTmuxAttach(cfg config.Config, scriptPath string, ralphDir string, existingPID int, log *logging.Logger) int {
	if !tmux.Available() {
		log.Emit(logging.Opts{Level: logging.Error}, "tmux not found on PATH")
		return 1
	}

	sess := &tmux.Session{
		Name:       tmux.BaseSessionName(cfg.ProjectDir),
		ProjectDir: cfg.ProjectDir,
		RalphDir:   ralphDir,
		RawLogPath: filepath.Join(ralphDir, "raw.log"),
		ScriptPath: scriptPath,
	}

	if sess.HasSession() {
		// Loop was started with --tmux; reuse the existing session.
		if err := sess.Attach(); err != nil {
			return 1
		}
		return 0
	}

	// Loop is running without --tmux; create a new session that tails the log.
	sess.RalphCmd = fmt.Sprintf("echo 'Attached to ralph loop (PID %d)'; tail -f '%s'",
		existingPID, filepath.Join(ralphDir, "loop.log"))

	if err := sess.Setup(); err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Tmux setup failed: %v", err)
		return 1
	}

	stopTitle := make(chan struct{})
	var stopOnce sync.Once
	closeTitle := func() { stopOnce.Do(func() { close(stopTitle) }) }
	go sess.PaneTitle().Run(stopTitle)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		closeTitle()
		sess.Kill()
	}()

	if err := sess.Attach(); err != nil {
		closeTitle()
		return 1
	}
	closeTitle()
	return 0
}

func handleTmux(cfg config.Config, scriptPath string, args []string, ralphDir string, log *logging.Logger) int {
	if !tmux.Available() {
		log.Emit(logging.Opts{Level: logging.Error}, "tmux not found on PATH")
		return 1
	}

	sess := &tmux.Session{
		Name:       tmux.SessionName(cfg.ProjectDir),
		ProjectDir: cfg.ProjectDir,
		RalphDir:   ralphDir,
		RawLogPath: filepath.Join(ralphDir, "raw.log"),
		ScriptPath: scriptPath,
		RalphCmd:   tmux.BuildRalphCmd(scriptPath, args),
	}

	if err := sess.Setup(); err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Tmux setup failed: %v", err)
		return 1
	}

	stopTitle := make(chan struct{})
	var stopOnce sync.Once
	closeTitle := func() { stopOnce.Do(func() { close(stopTitle) }) }
	go sess.PaneTitle().Run(stopTitle)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		closeTitle()
		sess.Kill()
	}()

	if err := sess.Attach(); err != nil {
		closeTitle()
		return 1
	}
	closeTitle()
	return 0
}
