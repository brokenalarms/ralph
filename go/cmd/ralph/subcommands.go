package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenalarms/ralph/internal/agent"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/pidfile"
	"github.com/brokenalarms/ralph/internal/prompt"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/verify"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// newInteractiveAgent creates a centralized agent runner for interactive
// sessions (review, task manager). projectDir is the repository root that
// the agent must NEVER chdir into — every agent spawn validates workDir
// against it as the structural defense against "worktree leaked into main".
func newInteractiveAgent(log *logging.Logger, projectDir, model string) *agent.Runner {
	r := agent.New(log, projectDir)
	r.Model = model
	return r
}

func handleSubcommand(sub config.Subcommand, log *logging.Logger) int {
	ralphDir := fmt.Sprintf("%s/.ralph", sub.Dir)

	switch sub.Name {
	case "stop":
		if _, err := os.Stat(ralphDir); os.IsNotExist(err) {
			log.Emit(logging.Opts{Level: logging.Error}, "No .ralph directory found. Is ralph running here?")
			return 1
		}
		stopFile := fmt.Sprintf("%s/stop", ralphDir)
		if err := os.WriteFile(stopFile, nil, 0o644); err != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "Failed to create stop file: %v", err)
			return 1
		}
		log.Emit(logging.Opts{Level: logging.Warn}, "Stop requested — ralph will halt after the current iteration.")
		log.Emit(logging.Opts{Level: logging.Warn}, "Ctrl+C to kill immediately if you don't need iteration results.")
		return 0

	case "feedback":
		if _, err := os.Stat(ralphDir); os.IsNotExist(err) {
			log.Emit(logging.Opts{Level: logging.Error}, "No .ralph directory found. Is ralph running here?")
			return 1
		}
		if len(sub.Args) > 0 && sub.Args[0] == "unskip" {
			return handleUnskip(log)
		}
		if len(sub.Args) == 0 {
			log.Emit(logging.Opts{}, "Usage: ralph feedback <message>")
			return 0
		}
		msg := strings.Join(sub.Args, " ")

		st := filepath.Join(ralphDir, "state.json")
		taskID := readTaskIDFromState(st)
		if taskID == "" {
			log.Emit(logging.Opts{Level: logging.Error}, "No active task — is ralph running?")
			return 1
		}

		bdBin, err := findBD()
		if err != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "bd not found: %v", err)
			return 1
		}
		bdCmd := appendNotesBD(bdBin, sub.Dir, taskID, msg)
		if out, err := bdCmd.CombinedOutput(); err != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "Failed to append notes: %s", strings.TrimSpace(string(out)))
			return 1
		}

		feedbackSignal := filepath.Join(ralphDir, "feedback")
		if err := os.WriteFile(feedbackSignal, nil, 0o644); err != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "Failed to write feedback signal: %v", err)
			return 1
		}
		log.Emit(logging.Opts{Level: logging.Success}, "Feedback sent — agent will restart with updated bead notes.")
		return 0

	case "attach":
		return handleAttach(sub, log)

	case "loop":
		return handleLoop(sub, log)

	case "merge":
		return handleMerge(sub, log)

	case "review":
		return handleReview(sub, log)

	case "task":
		return handleTask(sub, log)

	case "filter-stream":
		if len(sub.Args) == 0 {
			log.Emit(logging.Opts{Level: logging.Error}, "Usage: ralph filter-stream <rawlog> [workdir]")
			return 1
		}
		var workDir string
		if len(sub.Args) > 1 {
			workDir = sub.Args[1]
		}
		claude.FilterStream(sub.Args[0], workDir, false)
		return 0
	}

	return 1
}

// handleLoop is the autonomous executor: picks up beads, writes code, pushes
// PRs. This is the main ralph execution path, formerly the bare `ralph` default.
func handleLoop(sub config.Subcommand, log *logging.Logger) int {
	cfg, err := config.Parse(sub.Args)
	if errors.Is(err, config.ErrHelp) {
		printLoopUsage()
		return 0
	}
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "%v", err)
		printLoopUsage()
		return 1
	}

	absDir, _ := filepath.Abs(sub.Dir)

	if !git.IsGitRepo(absDir) {
		log.Emit(logging.Opts{Level: logging.Error}, "Not a git repository: %s", absDir)
		return 1
	}

	repoRoot, err := git.RepoRoot(absDir)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "%v", err)
		return 1
	}
	cfg.ProjectDir = repoRoot

	// Load config file before Validate so base_branch set in config.toml is visible.
	// LoadConfigFile is a no-op when the file does not exist; initRalphDir creates it
	// and loads it again later (idempotent — CLI-set values are not overwritten).
	ralphDir := filepath.Join(cfg.ProjectDir, ".ralph")
	_ = cfg.LoadConfigFile(filepath.Join(ralphDir, "config.toml"))

	if err := cfg.Validate(); err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "%v", err)
		return 1
	}

	scriptPath, _ := os.Executable()

	existingPID, err := pidfile.Check(filepath.Join(ralphDir, "loop.pid"))
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "PID file check failed: %v", err)
		return 1
	}
	if existingPID != 0 {
		log.Emit(logging.Opts{Level: logging.Error}, "ralph loop is already running (PID %d)", existingPID)
		return 1
	}

	promptsDir := filepath.Join(cfg.ProjectDir, "go", "cmd", "ralph", "prompts")
	if _, err := os.Stat(promptsDir); os.IsNotExist(err) {
		tmpDir, extractErr := extractEmbeddedPrompts()
		if extractErr != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "Failed to extract embedded prompts: %v", extractErr)
			return 1
		}
		promptsDir = tmpDir
	}

	dirs := workctx.New(cfg.ProjectDir, promptsDir)

	if cfg.UseTmux {
		return handleTmux(cfg, scriptPath, sub.Args, ralphDir, dirs.LogDir, log)
	}

	return runMain(cfg, dirs, scriptPath, sub.Args, log)
}

func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

func handleReview(sub config.Subcommand, log *logging.Logger) int {
	if hasHelpFlag(sub.Args) {
		printReviewUsage()
		return 0
	}

	reviewCfg, _ := config.Parse(sub.Args)
	reviewModel := verify.CapModel(modelCap(reviewCfg), agent.ModelOpus)

	absDir, _ := filepath.Abs(sub.Dir)

	if !git.IsGitRepo(absDir) {
		log.Emit(logging.Opts{Level: logging.Error}, "Not a git repository: %s", absDir)
		return 1
	}

	projectDir, err := git.RepoRoot(absDir)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "%v", err)
		return 1
	}

	ralphDir := filepath.Join(projectDir, ".ralph")

	promptsDir := filepath.Join(projectDir, "go", "cmd", "ralph", "prompts")
	if _, err := os.Stat(promptsDir); os.IsNotExist(err) {
		tmpDir, extractErr := extractEmbeddedPrompts()
		if extractErr != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "Failed to extract embedded prompts: %v", extractErr)
			return 1
		}
		promptsDir = tmpDir
	}

	reflections, err := prompt.ReadReflections(ralphDir)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Warn}, "Failed to read reflections: %v", err)
	}

	systemPrompt, err := prompt.BuildReviewPrompt(promptsDir, projectDir, ralphDir, reflections)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Failed to build review prompt: %v", err)
		return 1
	}

	// Spawn review inside an isolated worktree — never the project root.
	// Mirrors `ralph task`: agents must not run with cwd == projectDir.
	gm := git.New(git.Config{
		ProjectDir: projectDir,
		RalphDir:   ralphDir,
		BaseBranch: "main",
		Logger:     log,
	})
	if err := gm.InitTask(context.Background()); err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Review worktree setup failed: %v", err)
		return 1
	}
	workDir := gm.GetWorkDir()
	defer func() {
		if workDir != projectDir && gm.LogOneline("origin/main", "HEAD") == "" {
			gm.RemoveWorktree()
		}
	}()

	r := newInteractiveAgent(log, projectDir, reviewModel)
	exitCode, err := r.Interactive(workDir, systemPrompt)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Review session failed: %v", err)
		return 1
	}

	// Let the terminal finish processing any ANSI escape sequences from
	// the CLI's exit message before printing cleanup log lines.
	time.Sleep(100 * time.Millisecond)

	postReviewCleanup(ralphDir, log)
	return exitCode
}

// postReviewCleanup clears completed_tasks from state.json and archives reflections.
func postReviewCleanup(ralphDir string, log *logging.Logger) {
	st := state.NewStore(ralphDir)

	archived, err := prompt.ArchiveReflections(ralphDir)
	if err != nil {
		log.Emit(logging.Opts{Domain: "review", Level: logging.Warn}, "Failed to archive reflections: %v", err)
	} else if len(archived) > 0 {
		log.Emit(logging.Opts{Domain: "review"}, "Archived %d reflections", len(archived))
	}

	if err := st.ClearCompletedTasks(); err != nil {
		log.Emit(logging.Opts{Domain: "review", Level: logging.Warn}, "Failed to clear completed tasks: %v", err)
	} else {
		log.Emit(logging.Opts{Domain: "review"}, "Cleared completed_tasks from state")
	}

	os.Remove(filepath.Join(ralphDir, ".completed-tasks"))
}

// handleAttach attaches to an existing loop's tmux session. Requires a running
// loop (detected via .ralph/loop.pid). Does not start a new loop.
func handleAttach(sub config.Subcommand, log *logging.Logger) int {
	if hasHelpFlag(sub.Args) {
		printAttachUsage()
		return 0
	}

	projectDir, _ := filepath.Abs(sub.Dir)
	ralphDir := filepath.Join(projectDir, ".ralph")

	existingPID, err := pidfile.Check(filepath.Join(ralphDir, "loop.pid"))
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "PID file check failed: %v", err)
		return 1
	}
	if existingPID == 0 {
		log.Emit(logging.Opts{Level: logging.Error}, "No ralph loop running. Start one first with: ralph loop --tmux")
		return 1
	}

	cfg, err := config.Parse(sub.Args)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "%v", err)
		return 1
	}
	cfg.ProjectDir = projectDir

	scriptPath, _ := os.Executable()

	logDir, err := logging.StableLogDir(projectDir)
	if err != nil {
		logDir = ralphDir
	}

	log.Emit(logging.Opts{}, "Attaching to ralph loop (PID %d)", existingPID)
	return handleTmuxAttach(cfg, scriptPath, ralphDir, logDir, existingPID, log)
}

// handleTask launches an interactive Claude session with the task manager prompt.
// Runs standalone — no tmux required.
func handleTask(sub config.Subcommand, log *logging.Logger) int {
	if hasHelpFlag(sub.Args) {
		printTaskUsage()
		return 0
	}

	taskCfg, _ := config.Parse(sub.Args)
	taskModel := verify.CapModel(modelCap(taskCfg), agent.ModelOpus)

	absDir, _ := filepath.Abs(sub.Dir)

	if !git.IsGitRepo(absDir) {
		log.Emit(logging.Opts{Level: logging.Error}, "Not a git repository: %s", absDir)
		return 1
	}

	projectDir, err := git.RepoRoot(absDir)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "%v", err)
		return 1
	}

	ralphDir := filepath.Join(projectDir, ".ralph")

	promptsDir := filepath.Join(projectDir, "go", "cmd", "ralph", "prompts")
	if _, err := os.Stat(promptsDir); os.IsNotExist(err) {
		tmpDir, extractErr := extractEmbeddedPrompts()
		if extractErr != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "Failed to extract embedded prompts: %v", extractErr)
			return 1
		}
		promptsDir = tmpDir
	}

	gm := git.New(git.Config{
		ProjectDir: projectDir,
		RalphDir:   ralphDir,
		BaseBranch: "main",
		Logger:     log,
	})
	ctx := context.Background()
	// Worktree invariant: never silently fall back to projectDir. If the
	// worktree can't be set up, the agent must not run — that fallback is
	// the recurring root cause of "work leaking into main".
	if err := gm.InitTask(ctx); err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Task worktree setup failed: %v", err)
		return 1
	}
	workDir := gm.GetWorkDir()
	defer func() {
		if workDir != projectDir && gm.LogOneline("origin/main", "HEAD") == "" {
			gm.RemoveWorktree()
		}
	}()

	startupCtx := preloadTaskContext(projectDir, log)

	systemPrompt, err := prompt.BuildTaskManagerPrompt(promptsDir, projectDir, ralphDir, startupCtx)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Failed to build task manager prompt: %v", err)
		return 1
	}

	sessionID, err := generateSessionID()
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Failed to generate session ID: %v", err)
		return 1
	}

	r := newInteractiveAgent(log, projectDir, taskModel)
	exitCode, err := r.Interactive(workDir, systemPrompt, "--session-id", sessionID)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Task manager failed: %v", err)
		return 1
	}

	// Let the terminal finish processing any ANSI escape sequences from
	// the CLI's exit message before printing the resume hint.
	time.Sleep(100 * time.Millisecond)

	printTaskResumeHint(os.Stdout, workDir, sessionID)
	return exitCode
}

func handleUnskip(log *logging.Logger) int {
	log.Emit(logging.Opts{}, "Skipped tasks are now deferred in bd. Use 'bd list --status=deferred' to see them and 'bd update <id> --status=open' to undefer.")
	return 0
}

func printUsage() {
	fmt.Printf("%sRalph v%s (go)%s - Autonomous Claude Code task orchestrator\n\n", logging.Bold, config.Version, logging.Reset)
	fmt.Printf(`%sUSAGE:%s
  ralph <command> [options]

%sCOMMANDS:%s
  ralph loop [options]         Autonomous executor — picks up tasks, writes code, pushes PRs
  ralph attach [directory]     Attach to a running loop's tmux session (3-pane: loop + stream + plan)
  ralph merge <top-pr>         Rebase and merge a stacked PR chain bottom-up
  ralph task                   Interactive task triage and spec session
  ralph review [directory]     Post-mortem review: reflections, test audit, refactoring

%sEXAMPLES:%s
  ralph loop --tmux --max 20
  ralph loop --auto-merge --evolve
  ralph attach

%sHOW IT WORKS:%s
  1. Triage:   ralph task — create tasks, write specs, manage backlog
  2. Execute:  ralph loop — autonomous iteration over tasks
  3. Attach:   ralph attach — monitor a running loop in tmux
  4. Review:   ralph review — post-mortem analysis of reflections, tests, and code health

Use "ralph <command> --help" for more information about a command.
`,
		logging.Bold, logging.Reset,
		logging.Bold, logging.Reset,
		logging.Bold, logging.Reset,
		logging.Bold, logging.Reset,
	)
}

func printTaskUsage() {
	fmt.Printf("%sralph task%s - Interactive task triage and spec session\n\n", logging.Bold, logging.Reset)
	fmt.Printf("%sUSAGE:%s\n  ralph task [--model <model>]\n\n", logging.Bold, logging.Reset)
	fmt.Printf("Launches an interactive Claude session for creating tasks, writing specs,\nand managing the project backlog.\n\n")
	fmt.Printf("%sOPTIONS:%s\n  --model <model>    Model ceiling for the session — alias (opus, sonnet, haiku) for latest, or pinned ID (e.g. claude-sonnet-4-6)\n", logging.Bold, logging.Reset)
}

func printReviewUsage() {
	fmt.Printf("%sralph review%s - Post-mortem review\n\n", logging.Bold, logging.Reset)
	fmt.Printf("%sUSAGE:%s\n  ralph review [directory] [--model <model>]\n\n", logging.Bold, logging.Reset)
	fmt.Printf("Launches an interactive Claude session for reviewing reflections, auditing\ntests, and identifying refactoring opportunities.\n\n")
	fmt.Printf("%sOPTIONS:%s\n  --model <model>    Model ceiling for the session — alias (opus, sonnet, haiku) for latest, or pinned ID (e.g. claude-sonnet-4-6)\n", logging.Bold, logging.Reset)
}

func printAttachUsage() {
	fmt.Printf("%sralph attach%s - Attach to a running loop's tmux session\n\n", logging.Bold, logging.Reset)
	fmt.Printf("%sUSAGE:%s\n  ralph attach [directory]\n\n", logging.Bold, logging.Reset)
	fmt.Printf("Attaches to an existing ralph loop's tmux session with 3 panes:\nloop log, filtered stream, and plan watcher.\n\nRequires a running loop (started with `ralph loop --tmux`).\nDetaching (Ctrl-B d) does not kill the running loop.\n")
}

func printLoopUsage() {
	fmt.Printf("%sralph loop%s - Autonomous task executor\n\n", logging.Bold, logging.Reset)
	fmt.Printf("%sUSAGE:%s\n  ralph loop [OPTIONS]\n\n", logging.Bold, logging.Reset)
	fmt.Printf("%sOPTIONS:%s\n%s\n", logging.Bold, logging.Reset, config.FlagUsage())
	fmt.Printf(`%sWHILE RUNNING:%s
  ralph stop                 Halt the loop after the current iteration
  ralph feedback [message]   Send feedback to the agent (appends to bead notes)
  ralph feedback unskip      Show how to undefer skipped tasks in bd

%sEXAMPLES:%s
  ralph loop -n 20
  ralph loop --auto-merge --evolve
  ralph loop -p "Fix all failing tests"
`,
		logging.Bold, logging.Reset,
		logging.Bold, logging.Reset,
	)
}

func readTaskIDFromState(statePath string) string {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return ""
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return ""
	}
	v, ok := raw["last_task_id"]
	if !ok {
		return ""
	}
	var id string
	if json.Unmarshal(v, &id) != nil {
		return ""
	}
	return id
}

// preloadTaskContext runs bd list and bd ready before launching Claude so the
// task manager can present the startup summary without making tool calls —
// preventing the response from disrupting user typing.
//
// bd prime is deliberately excluded: its canned workflow boilerplate carries a
// SESSION-CLOSE push mandate (bd dolt push / git push) and a "use bd remember"
// directive that contradict ralph's own rules, and the task-manager prompt
// already supplies all of the orchestrator's bd workflow guidance.
func preloadTaskContext(projectDir string, log *logging.Logger) string {
	bdBin, err := findBD()
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Warn}, "bd not found, skipping startup preload")
		return ""
	}

	var parts []string
	for _, subcmd := range []string{"list", "ready"} {
		cmd := exec.Command(bdBin, subcmd)
		cmd.Dir = projectDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(out))
		if text != "" {
			parts = append(parts, fmt.Sprintf("$ bd %s\n%s", subcmd, text))
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// generateSessionID returns a random v4 UUID formatted as 8-4-4-4-12 hex.
// Uses crypto/rand — no external module required.
func generateSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}

// printTaskResumeHint writes a visually dominant boxed resume command to w.
// The command includes the worktree path so it works from any directory.
func printTaskResumeHint(w io.Writer, workDir, sessionID string) {
	cmd := fmt.Sprintf("cd %s && claude --resume %s", workDir, sessionID)
	line := strings.Repeat("─", len(cmd)+4)
	fmt.Fprintf(w, "\n%s%s┌%s┐%s\n", logging.Cyan, logging.Bold, line, logging.Reset)
	fmt.Fprintf(w, "%s%s│  %s  │%s\n", logging.Cyan, logging.Bold, cmd, logging.Reset)
	fmt.Fprintf(w, "%s%s└%s┘%s\n\n", logging.Cyan, logging.Bold, line, logging.Reset)
}

func findBD() (string, error) {
	if p, err := exec.LookPath("bd"); err == nil {
		return p, nil
	}
	if home, _ := os.UserHomeDir(); home != "" {
		p := filepath.Join(home, ".local", "bin", "bd")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("bd binary not found")
}

func appendNotesBD(bdBin, projectDir, taskID, msg string) *exec.Cmd {
	cmd := exec.Command(bdBin, "update", taskID, "--append-notes", msg)
	cmd.Dir = projectDir
	return cmd
}
