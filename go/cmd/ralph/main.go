package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/loop"
	"github.com/brokenalarms/ralph/internal/planning"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/tasks"
	"github.com/brokenalarms/ralph/internal/tmux"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	log := logging.New(nil)

	if sub, ok := config.ParseSubcommand(args); ok {
		return handleSubcommand(sub, log)
	}

	cfg, err := config.Parse(args)
	if errors.Is(err, config.ErrHelp) {
		printUsage()
		return 0
	}
	if err != nil {
		log.Error("%v", err)
		printUsage()
		return 1
	}

	// Resolve project directory to absolute path.
	cfg.ProjectDir, _ = filepath.Abs(cfg.ProjectDir)

	scriptPath, _ := os.Executable()
	ralphDir := filepath.Join(cfg.ProjectDir, ".ralph")
	promptsDir := filepath.Join(cfg.ProjectDir, "prompts")

	// Tmux outer wrapper: set up tmux session, then re-exec ralph inside pane 0.
	if cfg.UseTmux {
		return handleTmux(cfg, scriptPath, args, ralphDir, log)
	}

	return runMain(cfg, ralphDir, promptsDir, scriptPath, args, log)
}

func runMain(cfg config.Config, ralphDir, promptsDir, scriptPath string, args []string, log *logging.Logger) int {
	// Set up signal handling for cleanup.
	ctx, cancel := context.WithCancel(context.Background())
	interrupted := false
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		interrupted = true
		cancel()
	}()

	planFile := filepath.Join(ralphDir, "plan.md")
	stateFile := filepath.Join(ralphDir, "state.json")
	logFile := filepath.Join(ralphDir, "loop.log")

	// Initialize .ralph directory and check for resume.
	resume, exitCode := initRalphDir(cfg, ralphDir, logFile, stateFile, log)
	if exitCode >= 0 {
		return exitCode
	}

	st := state.NewStore(ralphDir)
	if err := st.Init(cfg.MaxIterations, cfg.RefactorEvery); err != nil {
		log.Error("Failed to initialize state: %v", err)
		return 1
	}

	// Set up log file writer.
	logFileWriter, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Error("Failed to open log file: %v", err)
		return 1
	}
	defer logFileWriter.Close()
	log = logging.New(logFileWriter)

	// Initialize task backend.
	backend, err := initTaskBackend(cfg, resume, st, ralphDir, planFile, promptsDir, log)
	if err != nil {
		log.Error("Task backend init failed: %v", err)
		return 1
	}
	st.Write("task_backend", backend.Label())
	log.TaskLabel = func() string { return backend.Label() }

	// Set up git manager.
	gm := &git.Manager{
		ProjectDir:  cfg.ProjectDir,
		RalphDir:    ralphDir,
		UseWorktree: cfg.UseWorktree,
		Resume:      resume,
		State:       st,
		Logger:      log,
	}
	if err := gm.SetupWorktree(); err != nil {
		log.Error("Worktree setup failed: %v", err)
		return 1
	}

	log.Phase("Ralph Loop v%s (go)", config.Version)
	log.Log("Project: %s", cfg.ProjectDir)
	if gm.WorkDir != cfg.ProjectDir {
		log.Log("Worktree: %s", gm.WorkDir)
	}
	log.Log("Task backend: %s", backend.Label())
	log.Log("Max iterations: %d", cfg.MaxIterations)

	st.Write("started_at", time.Now().UTC().Format("2006-01-02T15:04:05Z"))

	// Planning phase.
	planDeps := planning.Deps{
		Backend:      backend,
		StateStore:   st,
		Logger:       log,
		PromptsDir:   promptsDir,
		WorkDir:      gm.WorkDir,
		RalphDir:     ralphDir,
		PlanFile:     planFile,
		Prompt:       cfg.Prompt,
		SkipPlanning: cfg.SkipPlanning,
		RenameWorktree: func(theme string) error {
			gm.RenameWorktreeForTheme(theme)
			return nil
		},
	}

	if err := planning.Run(planDeps); err != nil {
		log.Error("Planning failed: %v", err)
		cleanup(cfg, gm, st, backend, ralphDir, planFile, scriptPath, args, interrupted, log)
		return 1
	}

	if cfg.PlanOnly {
		log.Log("Plan-only mode, exiting")
		cleanup(cfg, gm, st, backend, ralphDir, planFile, scriptPath, args, interrupted, log)
		return 0
	}

	// Execution phase.
	execLoop := loop.New(loop.Config{
		ProjectDir:          cfg.ProjectDir,
		WorkDir:             gm.WorkDir,
		RalphDir:            ralphDir,
		PromptsDir:          promptsDir,
		PlanFile:            planFile,
		MaxIterations:       cfg.MaxIterations,
		RefactorEvery:       cfg.RefactorEvery,
		Quiet:               cfg.Quiet,
		CallsPerHour:        cfg.CallsPerHour,
		TaskBackend:         backend,
		IdleTimeout:         cfg.IdleTimeout,
		IdleTimeoutProgress: cfg.IdleTimeoutProgress,
	}, st, gm, log)

	if err := execLoop.Run(ctx); err != nil {
		log.Error("Execution failed: %v", err)
	}

	cleanup(cfg, gm, st, backend, ralphDir, planFile, scriptPath, args, interrupted, log)
	return 0
}

// initRalphDir creates .ralph, checks for dirty working tree, handles resume
// detection. Returns (resume, exitCode). exitCode < 0 means continue.
func initRalphDir(cfg config.Config, ralphDir, logFile, stateFile string, log *logging.Logger) (bool, int) {
	if err := os.MkdirAll(ralphDir, 0o755); err != nil {
		log.Error("Failed to create .ralph dir: %v", err)
		return false, 1
	}

	// Touch log files.
	touchFile(logFile)
	touchFile(filepath.Join(ralphDir, "raw.log"))

	// Ensure reflections directory exists for post-task reflections.
	os.MkdirAll(filepath.Join(ralphDir, "reflections"), 0o755)

	// Check for uncommitted changes (skip on resume).
	if !fileExists(stateFile) {
		if isGitRepo(cfg.ProjectDir) && hasUncommittedChanges(cfg.ProjectDir) {
			log.Error("uncommitted changes in %s — please commit or stash before running ralph.", cfg.ProjectDir)
			return false, 1
		}
	}

	// Ensure .ralph is gitignored.
	ensureGitignored(cfg.ProjectDir, ".ralph")

	// Check for existing state (resume detection).
	if fileExists(stateFile) {
		st := state.NewStore(ralphDir)
		status, _ := st.Read("status")
		if status == "completed" {
			log.Task("All tasks completed from previous run.")
			fmt.Printf("%s[ralph v%s (go)]%s Run fresh? (y/n) ", logging.Yellow, config.Version, logging.Reset)
			var answer string
			fmt.Scanln(&answer)
			if answer == "y" || answer == "Y" {
				os.RemoveAll(ralphDir)
				os.MkdirAll(ralphDir, 0o755)
				touchFile(logFile)
				touchFile(filepath.Join(ralphDir, "raw.log"))
				return false, -1
			}
			return false, 0
		}
		log.Log("Resuming from previous state (status: %s)", status)
		return true, -1
	}

	return false, -1
}

// initTaskBackend selects and initializes the task backend. On resume,
// restores from state. BD falls back to checklist if unavailable.
func initTaskBackend(cfg config.Config, resume bool, st *state.Store, ralphDir, planFile, promptsDir string, log *logging.Logger) (tasks.Backend, error) {
	backendLabel := "bd"

	if resume {
		stored, _ := st.Read("task_backend")
		if stored == "bd" || stored == "checklist" {
			backendLabel = stored
		} else if fileExists(planFile) {
			data, _ := os.ReadFile(planFile)
			if strings.Contains(string(data), "- [") {
				backendLabel = "checklist"
			}
		}
	}

	if backendLabel == "bd" {
		bd := &tasks.BD{
			ProjectDir: cfg.ProjectDir,
			PromptsDir: promptsDir,
		}
		if err := bd.Init(); err != nil {
			if errors.Is(err, tasks.ErrNeedsFallback) {
				log.Warn("bd unavailable, falling back to checklist")
				backendLabel = "checklist"
			} else {
				return nil, err
			}
		} else {
			return bd, nil
		}
	}

	return &tasks.Checklist{
		PlanFile:   planFile,
		PromptsDir: promptsDir,
	}, nil
}

// cleanup generates resume script, prints summary, and removes unused worktrees.
func cleanup(cfg config.Config, gm *git.Manager, st *state.Store, backend tasks.Backend, ralphDir, planFile, scriptPath string, args []string, interrupted bool, log *logging.Logger) {
	// Remove unused worktree (branch still named /next = no work committed).
	if gm.WorktreeBranch != "" &&
		strings.HasSuffix(gm.WorktreeBranch, "/next") &&
		gm.WorkDir != cfg.ProjectDir {
		if interrupted {
			removeWorktree(cfg.ProjectDir, gm.WorkDir, gm.WorktreeBranch)
		}
	}

	generateResumeScript(cfg, ralphDir, scriptPath, args, log)
	printSummary(cfg, gm, st, backend, ralphDir, planFile, log)
}

// generateResumeScript writes a shell script that re-runs ralph with the same
// flags, allowing the user to easily resume after interruption.
func generateResumeScript(cfg config.Config, ralphDir, scriptPath string, args []string, log *logging.Logger) {
	resumePath := filepath.Join(ralphDir, "resume.sh")

	var extraArgs []string
	if cfg.Quiet {
		extraArgs = append(extraArgs, "--quiet")
	}
	if !cfg.UseWorktree {
		extraArgs = append(extraArgs, "--no-worktree")
	}
	if cfg.CallsPerHour != 80 {
		extraArgs = append(extraArgs, fmt.Sprintf("--calls-per-hour %d", cfg.CallsPerHour))
	}
	if cfg.UseTmux || os.Getenv("_RALPH_TMUX_SESSION") != "" {
		extraArgs = append(extraArgs, "--tmux")
	}

	extra := ""
	if len(extraArgs) > 0 {
		extra = " " + strings.Join(extraArgs, " ")
	}

	content := fmt.Sprintf(`#!/usr/bin/env bash
# Ralph Loop - Resume Script
# Generated at: %s
exec "%s" --dir "%s" --max %d%s
`, time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		scriptPath, cfg.ProjectDir, cfg.MaxIterations, extra)

	os.WriteFile(resumePath, []byte(content), 0o755)
	log.Log("Resume script: %s", resumePath)
}

// printSummary displays the end-of-run summary.
func printSummary(cfg config.Config, gm *git.Manager, st *state.Store, backend tasks.Backend, ralphDir, planFile string, log *logging.Logger) {
	fmt.Println()
	log.Phase("=== SUMMARY ===")

	iteration, _ := st.Read("iteration")
	status, _ := st.Read("status")
	log.Log("Status:     %s", status)
	log.Log("Iterations: %s total", iteration)

	completed, _ := backend.CountCompleted()
	remaining, _ := backend.CountRemaining()
	total, _ := backend.CountTotal()
	log.Task("Tasks: %d/%d completed, %d remaining", completed, total, remaining)

	log.Log("Log:        %s", filepath.Join(ralphDir, "loop.log"))
	if backend.Label() == "checklist" {
		log.Log("Plan:       %s", planFile)
	}

	if gm.WorktreeBranch != "" && gm.ProjectName != "" {
		log.Log("Worktree:   %s", gm.WorkDir)

		branches := listProjectBranches(cfg.ProjectDir, gm.ProjectName)
		if len(branches) > 1 {
			log.Log("Branches:")
			for _, b := range branches {
				log.Log("  %s", b)
			}
		} else {
			log.Log("Branch:     %s", gm.WorktreeBranch)
		}
		log.Log("To merge:   git merge %s", gm.WorktreeBranch)
	}

	hasRemaining, _ := backend.HasRemaining()
	if hasRemaining {
		log.Log("Resume:     %s", filepath.Join(ralphDir, "resume.sh"))
	}
}

func handleTmux(cfg config.Config, scriptPath string, args []string, ralphDir string, log *logging.Logger) int {
	if !tmux.Available() {
		log.Error("tmux not found on PATH")
		return 1
	}

	ralphCmd := tmux.BuildRalphCmd(scriptPath, args)
	planFile := filepath.Join(ralphDir, "plan.md")

	backendLabel := "bd"
	// Quick check: if checklist plan exists, use checklist rendering.
	if fileExists(planFile) {
		backendLabel = "checklist"
	}

	sess := &tmux.Session{
		Name:        fmt.Sprintf("ralph-%d", os.Getpid()),
		ProjectDir:  cfg.ProjectDir,
		RalphDir:    ralphDir,
		RawLogPath:  filepath.Join(ralphDir, "raw.log"),
		RalphCmd:    ralphCmd,
		TaskBackend: backendLabel,
		PlanFile:    planFile,
	}

	if err := sess.Setup(); err != nil {
		log.Error("Tmux setup failed: %v", err)
		return 1
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		sess.Kill()
	}()

	if err := sess.Attach(); err != nil {
		return 1
	}
	return 0
}

// handleSubcommand processes stop/feedback subcommands.
func handleSubcommand(sub config.Subcommand, log *logging.Logger) int {
	ralphDir := filepath.Join(sub.Dir, ".ralph")

	switch sub.Name {
	case "stop":
		if _, err := os.Stat(ralphDir); os.IsNotExist(err) {
			log.Error("No .ralph directory found. Is ralph running here?")
			return 1
		}
		stopFile := filepath.Join(ralphDir, "stop")
		if err := os.WriteFile(stopFile, nil, 0o644); err != nil {
			log.Error("Failed to create stop file: %v", err)
			return 1
		}
		log.Warn("Stop requested — ralph will halt after the current iteration.")
		log.Warn("Ctrl+C to kill immediately if you don't need iteration results.")
		return 0

	case "feedback":
		if _, err := os.Stat(ralphDir); os.IsNotExist(err) {
			log.Error("No .ralph directory found. Is ralph running here?")
			return 1
		}
		feedbackFile := filepath.Join(ralphDir, "feedback")
		if len(sub.Args) == 0 {
			data, err := os.ReadFile(feedbackFile)
			if err == nil && len(data) > 0 {
				log.Log("Queued feedback:")
				fmt.Print(string(data))
			} else {
				log.Log("No feedback queued.")
			}
			return 0
		}
		msg := strings.Join(sub.Args, " ") + "\n"
		f, err := os.OpenFile(feedbackFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Error("Failed to write feedback: %v", err)
			return 1
		}
		defer f.Close()
		if _, err := f.WriteString(msg); err != nil {
			log.Error("Failed to write feedback: %v", err)
			return 1
		}
		log.Success("Feedback queued for next iteration.")
		return 0
	}

	return 1
}

func printUsage() {
	fmt.Printf(`%sRalph Loop v%s (go)%s - Autonomous Claude Code task iteration

%sUSAGE:%s
  ralph [OPTIONS] [directory]

%sOPTIONS:%s
  -d, --dir <path>       Project directory (default: cwd)
  -n, --max <N>          Max iterations (default: 50, env RALPH_MAX_ITERATIONS)
  -p, --prompt <text>    Prompt override (otherwise Claude reads repo context)
  --plan-file <path>     Pre-made plan in Ralph format (markdown checkboxes). Skips planning phase.
  --plan                 Run planning phase only
  --skip-planning        Skip interactive planning, go straight to autonomous execution
  -q, --quiet            Suppress Claude output streaming (log only)
  --no-worktree          Run directly in project dir (no git worktree isolation)
  --calls-per-hour <N>   Max Claude calls per hour (default: 80)
  --refactor-every <N>   Inject a refactor iteration every N iterations (default: 0/disabled, env RALPH_REFACTOR_EVERY)
  --idle-timeout <dur>   Kill session after this idle duration (default: 10m, env RALPH_IDLE_TIMEOUT)
  --idle-timeout-progress <dur>  Shorter idle timeout when progress detected (default: 30s, env RALPH_IDLE_TIMEOUT_PROGRESS)
  --tmux                 Run in tmux 3-pane layout (status / output / plan)
  --auto-merge           Squash-merge each PR into main after task completion
  -h, --help             Show this help

%sEXAMPLES:%s
  ralph ~/myproject -n 20
  ralph -p "Fix all failing tests"
  ralph . --plan-file plan.md

%sSUBCOMMANDS:%s
  ralph stop [directory]       Halt after the current iteration
  ralph feedback [message]     Show queued feedback, or queue a new message

%sHOW IT WORKS:%s
  1. Planning: Claude reads the repo and creates .ralph/plan.md with atomic tasks
  2. Execution: Each task runs in a fresh Claude context (~200k tokens)
  3. Completion: Claude echoes a signal token when each task is done
  4. Repeat: Loop continues until all tasks complete or iteration cap is hit
`,
		logging.Bold, config.Version, logging.Reset,
		logging.Bold, logging.Reset,
		logging.Bold, logging.Reset,
		logging.Bold, logging.Reset,
		logging.Bold, logging.Reset,
		logging.Bold, logging.Reset,
	)
}

// --- Helpers ---


func touchFile(path string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		f.Close()
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isGitRepo(dir string) bool {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--git-dir")
	return cmd.Run() == nil
}

func hasUncommittedChanges(dir string) bool {
	cmd1 := exec.Command("git", "-C", dir, "diff", "--quiet")
	cmd2 := exec.Command("git", "-C", dir, "diff", "--cached", "--quiet")
	return cmd1.Run() != nil || cmd2.Run() != nil
}

func ensureGitignored(projectDir, entry string) {
	gitignorePath := filepath.Join(projectDir, ".gitignore")
	existing := ""
	if data, err := os.ReadFile(gitignorePath); err == nil {
		existing = string(data)
	}

	found := false
	for _, line := range strings.Split(existing, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == entry || trimmed == entry+"/" || trimmed == entry+"/*" {
			found = true
			break
		}
	}
	if found {
		return
	}

	existing += entry + "\n"
	os.WriteFile(gitignorePath, []byte(existing), 0o644)

	if isGitRepo(projectDir) {
		exec.Command("git", "-C", projectDir, "add", ".gitignore").Run()
		exec.Command("git", "-C", projectDir, "commit", "-m", "Add "+entry+" to .gitignore").Run()
	}
}

func removeWorktree(projectDir, worktreeDir, branch string) {
	exec.Command("git", "-C", projectDir, "worktree", "remove", "--force", worktreeDir).Run()
	exec.Command("git", "-C", projectDir, "branch", "-D", branch).Run()
}

func listProjectBranches(dir, projectName string) []string {
	cmd := exec.Command("git", "-C", dir, "branch", "--list", "ralph/"+projectName+"/*", "--sort=refname")
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "* ")
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches
}
