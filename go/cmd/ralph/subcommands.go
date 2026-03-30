package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/agent"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/attempts"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/prompt"
	"github.com/brokenalarms/ralph/internal/state"
	"github.com/brokenalarms/ralph/internal/workctx"
)

// newInteractiveAgent creates a centralized agent runner for interactive
// sessions (review, task manager).
func newInteractiveAgent(log *logging.Logger) *agent.Runner {
	return agent.New(log)
}

func handleSubcommand(sub config.Subcommand, log *logging.Logger) int {
	ralphDir := fmt.Sprintf("%s/.ralph", sub.Dir)

	switch sub.Name {
	case "stop":
		if _, err := os.Stat(ralphDir); os.IsNotExist(err) {
			log.Error("", "No .ralph directory found. Is ralph running here?")
			return 1
		}
		stopFile := fmt.Sprintf("%s/stop", ralphDir)
		if err := os.WriteFile(stopFile, nil, 0o644); err != nil {
			log.Error("", "Failed to create stop file: %v", err)
			return 1
		}
		log.Warn("", "Stop requested — ralph will halt after the current iteration.")
		log.Warn("", "Ctrl+C to kill immediately if you don't need iteration results.")
		return 0

	case "feedback":
		if _, err := os.Stat(ralphDir); os.IsNotExist(err) {
			log.Error("", "No .ralph directory found. Is ralph running here?")
			return 1
		}
		if len(sub.Args) > 0 && sub.Args[0] == "unskip" {
			return handleUnskip(log)
		}
		if len(sub.Args) == 0 {
			log.Log("", "Usage: ralph feedback <message>")
			return 0
		}
		msg := strings.Join(sub.Args, " ")

		st := filepath.Join(ralphDir, "state.json")
		taskID := readTaskIDFromState(st)
		if taskID == "" {
			log.Error("", "No active task — is ralph running?")
			return 1
		}

		bdBin, err := findBD()
		if err != nil {
			log.Error("", "bd not found: %v", err)
			return 1
		}
		bdCmd := appendNotesBD(bdBin, sub.Dir, taskID, msg)
		if out, err := bdCmd.CombinedOutput(); err != nil {
			log.Error("", "Failed to append notes: %s", strings.TrimSpace(string(out)))
			return 1
		}

		feedbackSignal := filepath.Join(ralphDir, "feedback")
		if err := os.WriteFile(feedbackSignal, nil, 0o644); err != nil {
			log.Error("", "Failed to write feedback signal: %v", err)
			return 1
		}
		log.Success("", "Feedback sent — agent will restart with updated bead notes.")
		return 0

	case "command":
		return handleCommander(sub, log)

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
			log.Error("", "Usage: ralph filter-stream <rawlog> [workdir]")
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
		log.Error("", "%v", err)
		printLoopUsage()
		return 1
	}

	cfg.ProjectDir, _ = filepath.Abs(sub.Dir)

	if err := cfg.Validate(); err != nil {
		log.Error("", "%v", err)
		return 1
	}

	if !git.IsGitRepo(cfg.ProjectDir) {
		log.Error("", "Not a git repository: %s", cfg.ProjectDir)
		return 1
	}

	scriptPath, _ := os.Executable()
	ralphDir := filepath.Join(cfg.ProjectDir, ".ralph")

	promptsDir := filepath.Join(cfg.ProjectDir, "go", "cmd", "ralph", "prompts")
	if _, err := os.Stat(promptsDir); os.IsNotExist(err) {
		tmpDir, extractErr := extractEmbeddedPrompts()
		if extractErr != nil {
			log.Error("", "Failed to extract embedded prompts: %v", extractErr)
			return 1
		}
		promptsDir = tmpDir
	}

	if cfg.UseTmux {
		if err := os.MkdirAll(ralphDir, 0o755); err != nil {
			log.Error("", "Failed to create .ralph dir: %v", err)
			return 1
		}
		return handleTmux(cfg, scriptPath, sub.Args, ralphDir, false, log)
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

	projectDir, _ := filepath.Abs(sub.Dir)

	if !git.IsGitRepo(projectDir) {
		log.Error("", "Not a git repository: %s", projectDir)
		return 1
	}

	ralphDir := filepath.Join(projectDir, ".ralph")

	promptsDir := filepath.Join(projectDir, "go", "cmd", "ralph", "prompts")
	if _, err := os.Stat(promptsDir); os.IsNotExist(err) {
		tmpDir, extractErr := extractEmbeddedPrompts()
		if extractErr != nil {
			log.Error("", "Failed to extract embedded prompts: %v", extractErr)
			return 1
		}
		promptsDir = tmpDir
	}

	reflections, err := prompt.ReadReflections(ralphDir)
	if err != nil {
		log.Warn("", "Failed to read reflections: %v", err)
	}

	systemPrompt, err := prompt.BuildReviewPrompt(promptsDir, projectDir, ralphDir, reflections)
	if err != nil {
		log.Error("", "Failed to build review prompt: %v", err)
		return 1
	}

	r := newInteractiveAgent(log)
	exitCode, err := r.Interactive(projectDir, systemPrompt, prompt.ReviewBootstrapPrompt)
	if err != nil {
		log.Error("", "Review session failed: %v", err)
		return 1
	}

	postReviewCleanup(ralphDir, log)
	return exitCode
}

// postReviewCleanup clears completed_tasks from state.json, archives
// reflections, and removes attempt data for completed tasks.
func postReviewCleanup(ralphDir string, log *logging.Logger) {
	st := state.NewStore(ralphDir)
	tasks, err := st.GetCompletedTasks()
	if err != nil {
		log.Warn("review", "Failed to read completed tasks: %v", err)
	}

	archived, err := prompt.ArchiveReflections(ralphDir)
	if err != nil {
		log.Warn("review", "Failed to archive reflections: %v", err)
	} else if len(archived) > 0 {
		log.Log("review", "Archived %d reflections", len(archived))
	}

	if len(tasks) > 0 {
		tracker := attempts.New(ralphDir)
		tracker.ClearForTasks(tasks)
		log.Log("review", "Cleared attempt data for %d tasks", len(tasks))
	}

	if err := st.ClearCompletedTasks(); err != nil {
		log.Warn("review", "Failed to clear completed tasks: %v", err)
	} else {
		log.Log("review", "Cleared completed_tasks from state")
	}

	os.Remove(filepath.Join(ralphDir, ".completed-tasks"))
}

// handleCommander launches the 4-pane tmux layout with both the ralph loop
// and an interactive task manager. Remaining args are passed through to the loop.
func handleCommander(sub config.Subcommand, log *logging.Logger) int {
	if hasHelpFlag(sub.Args) {
		printCommanderUsage()
		return 0
	}

	projectDir, _ := filepath.Abs(sub.Dir)
	ralphDir := filepath.Join(projectDir, ".ralph")

	if err := os.MkdirAll(ralphDir, 0o755); err != nil {
		log.Error("", "Failed to create .ralph dir: %v", err)
		return 1
	}

	cfg, err := config.Parse(sub.Args)
	if err != nil {
		log.Error("", "%v", err)
		return 1
	}
	cfg.ProjectDir = projectDir
	cfg.UseTmux = true

	scriptPath, _ := os.Executable()

	allArgs := append([]string{"command"}, sub.Args...)
	if sub.Dir != "." {
		allArgs = append([]string{"command", sub.Dir}, sub.Args...)
	}

	return handleTmux(cfg, scriptPath, allArgs, ralphDir, true, log)
}

// handleTask launches an interactive Claude session with the task manager prompt.
// Runs standalone — no tmux required.
func handleTask(sub config.Subcommand, log *logging.Logger) int {
	if hasHelpFlag(sub.Args) {
		printTaskUsage()
		return 0
	}

	projectDir, _ := filepath.Abs(sub.Dir)
	ralphDir := filepath.Join(projectDir, ".ralph")

	promptsDir := filepath.Join(projectDir, "go", "cmd", "ralph", "prompts")
	if _, err := os.Stat(promptsDir); os.IsNotExist(err) {
		tmpDir, extractErr := extractEmbeddedPrompts()
		if extractErr != nil {
			log.Error("", "Failed to extract embedded prompts: %v", extractErr)
			return 1
		}
		promptsDir = tmpDir
	}

	systemPrompt, err := prompt.BuildTaskManagerPrompt(promptsDir, projectDir, ralphDir)
	if err != nil {
		log.Error("", "Failed to build task manager prompt: %v", err)
		return 1
	}

	r := newInteractiveAgent(log)
	exitCode, err := r.Interactive(projectDir, systemPrompt, prompt.TaskManagerBootstrapPrompt)
	if err != nil {
		log.Error("", "Task manager failed: %v", err)
		return 1
	}
	return exitCode
}

func handleUnskip(log *logging.Logger) int {
	log.Log("", "Skipped tasks are now deferred in bd. Use 'bd list --status=deferred' to see them and 'bd update <id> --status=open' to undefer.")
	return 0
}

func printUsage() {
	fmt.Printf("%sRalph v%s (go)%s - Autonomous Claude Code task orchestrator\n\n", logging.Bold, config.Version, logging.Reset)
	fmt.Printf(`%sUSAGE:%s
  ralph <command> [options]

%sCOMMANDS:%s
  ralph loop [options]         Autonomous executor — picks up tasks, writes code, pushes PRs
  ralph merge <top-pr>         Rebase and merge a stacked PR chain bottom-up
  ralph task [directory]       Interactive task triage and spec session
  ralph review [directory]     Post-mortem review: reflections, test audit, refactoring
  ralph command [directory]    Full 4-pane tmux layout (loop + task manager + stream + plan)

%sEXAMPLES:%s
  ralph loop --max 20
  ralph loop --auto-merge --evolve
  ralph task ~/myproject
  ralph review

%sHOW IT WORKS:%s
  1. Triage:   ralph task — create tasks, write specs, manage backlog
  2. Execute:  ralph loop — autonomous iteration over tasks
  3. Review:   ralph review — post-mortem analysis of reflections, tests, and code health
  Run task and loop in parallel: task in one window, loop in another.

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
	fmt.Printf("%sUSAGE:%s\n  ralph task [directory]\n\n", logging.Bold, logging.Reset)
	fmt.Printf("Launches an interactive Claude session for creating tasks, writing specs,\nand managing the project backlog.\n")
}

func printReviewUsage() {
	fmt.Printf("%sralph review%s - Post-mortem review\n\n", logging.Bold, logging.Reset)
	fmt.Printf("%sUSAGE:%s\n  ralph review [directory]\n\n", logging.Bold, logging.Reset)
	fmt.Printf("Launches an interactive Claude session for reviewing reflections, auditing\ntests, and identifying refactoring opportunities.\n")
}

func printCommanderUsage() {
	fmt.Printf("%sralph command%s - Full 4-pane tmux layout\n\n", logging.Bold, logging.Reset)
	fmt.Printf("%sUSAGE:%s\n  ralph command [directory] [loop-options...]\n\n", logging.Bold, logging.Reset)
	fmt.Printf("Starts a tmux session with the autonomous loop, task manager, stream log,\nand plan watcher in a 4-pane layout.\n\n")
	fmt.Printf("%sLOOP OPTIONS:%s\n%s\n", logging.Bold, logging.Reset, config.FlagUsage())
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
  ralph loop --auto-merge --evolve --post-task 'scripts/build-go.sh'
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
