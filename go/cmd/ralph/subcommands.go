package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/agent"
	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/git"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/prompt"
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
		feedbackFile := fmt.Sprintf("%s/feedback", ralphDir)
		if len(sub.Args) == 0 {
			data, err := os.ReadFile(feedbackFile)
			if err == nil && len(data) > 0 {
				log.Log("", "Queued feedback:")
				fmt.Print(string(data))
			} else {
				log.Log("", "No feedback queued.")
			}
			return 0
		}
		msg := strings.Join(sub.Args, " ") + "\n"
		f, err := os.OpenFile(feedbackFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Error("", "Failed to write feedback: %v", err)
			return 1
		}
		defer f.Close()
		if _, err := f.WriteString(msg); err != nil {
			log.Error("", "Failed to write feedback: %v", err)
			return 1
		}
		log.Success("", "Feedback sent — agent will pick it up on next tool call.")
		return 0

	case "command":
		return handleCommander(sub, log)

	case "loop":
		return handleLoop(sub, log)

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

	projectDir, _ := filepath.Abs(sub.Dir)
	if sub.Dir != "." {
		cfg.ProjectDir = projectDir
	} else {
		cfg.ProjectDir, _ = filepath.Abs(cfg.ProjectDir)
	}

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
	return exitCode
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
  ralph task [directory]       Interactive task triage and spec session
  ralph review [directory]     Post-mortem review: reflections, test audit, refactoring
  ralph command [directory]    Full 4-pane tmux layout (loop + task manager + stream + plan)

%sEXAMPLES:%s
  ralph loop --dir ~/myproject --max 20
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
  ralph feedback             Show queued feedback for the loop
  ralph feedback [message]   Queue a message to the loop in progress
  ralph feedback unskip      Show how to undefer skipped tasks in bd

%sEXAMPLES:%s
  ralph loop --dir ~/myproject -n 20
  ralph loop --auto-merge --evolve
  ralph loop -p "Fix all failing tests"
`,
		logging.Bold, logging.Reset,
		logging.Bold, logging.Reset,
	)
}
