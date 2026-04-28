package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// sessions (review, task manager).
func newInteractiveAgent(log *logging.Logger, model string) *agent.Runner {
	r := agent.New(log)
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

	if err := cfg.Validate(); err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "%v", err)
		return 1
	}

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

	scriptPath, _ := os.Executable()
	ralphDir := filepath.Join(cfg.ProjectDir, ".ralph")

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

	if cfg.UseTmux {
		return handleTmux(cfg, scriptPath, sub.Args, ralphDir, log)
	}

	dirs := workctx.New(cfg.ProjectDir, promptsDir)
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

	r := newInteractiveAgent(log, reviewModel)
	exitCode, err := r.Interactive(projectDir, systemPrompt)
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

	log.Emit(logging.Opts{}, "Attaching to ralph loop (PID %d)", existingPID)
	return handleTmuxAttach(cfg, scriptPath, ralphDir, existingPID, log)
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
	if err := gm.InitTask(ctx); err != nil {
		log.Emit(logging.Opts{Level: logging.Warn}, "Task worktree setup failed, falling back to project dir: %v", err)
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

	r := newInteractiveAgent(log, taskModel)
	exitCode, err := r.Interactive(workDir, systemPrompt)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Task manager failed: %v", err)
		return 1
	}
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

// preloadTaskContext runs bd prime, bd list, and bd ready before launching
// Claude so the task manager can present the startup summary without making
// tool calls — preventing the response from disrupting user typing.
func preloadTaskContext(projectDir string, log *logging.Logger) string {
	bdBin, err := findBD()
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Warn}, "bd not found, skipping startup preload")
		return ""
	}

	var parts []string
	for _, subcmd := range []string{"prime", "list", "ready"} {
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
