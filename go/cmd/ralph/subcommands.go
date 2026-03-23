package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/claude"
	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/logging"
	"github.com/brokenalarms/ralph/internal/prompt"
)

func handleSubcommand(sub config.Subcommand, log *logging.Logger) int {
	ralphDir := fmt.Sprintf("%s/.ralph", sub.Dir)

	switch sub.Name {
	case "stop":
		if _, err := os.Stat(ralphDir); os.IsNotExist(err) {
			log.Error("No .ralph directory found. Is ralph running here?")
			return 1
		}
		stopFile := fmt.Sprintf("%s/stop", ralphDir)
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
		feedbackFile := fmt.Sprintf("%s/feedback", ralphDir)
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
		log.Success("Feedback sent — agent will pick it up on next tool call.")
		return 0

	case "commander":
		return handleCommander(sub, log)

	case "task":
		return handleTask(sub, log)

	case "filter-stream":
		if len(sub.Args) == 0 {
			log.Error("Usage: ralph filter-stream <rawlog>")
			return 1
		}
		claude.FilterStream(sub.Args[0])
		return 0
	}

	return 1
}

// handleCommander launches the 4-pane tmux layout with both the ralph loop
// and an interactive task manager. Remaining args are passed through to the loop.
func handleCommander(sub config.Subcommand, log *logging.Logger) int {
	projectDir, _ := filepath.Abs(sub.Dir)
	ralphDir := filepath.Join(projectDir, ".ralph")

	if err := os.MkdirAll(ralphDir, 0o755); err != nil {
		log.Error("Failed to create .ralph dir: %v", err)
		return 1
	}

	// Re-parse remaining args as loop flags (everything after "commander [dir]").
	cfg, err := config.Parse(sub.Args)
	if err != nil {
		log.Error("%v", err)
		return 1
	}
	cfg.ProjectDir = projectDir
	cfg.UseTmux = true

	scriptPath, _ := os.Executable()

	// Build the two commands for the tmux panes.
	allArgs := append([]string{"commander"}, sub.Args...)
	if sub.Dir != "." {
		allArgs = append([]string{"commander", sub.Dir}, sub.Args...)
	}

	return handleTmuxCommander(cfg, scriptPath, allArgs, ralphDir, log)
}

// handleTask launches an interactive Claude session with the task manager prompt.
// Runs standalone — no tmux required.
func handleTask(sub config.Subcommand, log *logging.Logger) int {
	projectDir, _ := filepath.Abs(sub.Dir)
	ralphDir := filepath.Join(projectDir, ".ralph")

	promptsDir := filepath.Join(projectDir, "prompts")
	if _, err := os.Stat(promptsDir); os.IsNotExist(err) {
		tmpDir, extractErr := extractEmbeddedPrompts()
		if extractErr != nil {
			log.Error("Failed to extract embedded prompts: %v", extractErr)
			return 1
		}
		promptsDir = tmpDir
	}

	systemPrompt, err := prompt.BuildTaskManagerPrompt(promptsDir, projectDir, ralphDir)
	if err != nil {
		log.Error("Failed to build task manager prompt: %v", err)
		return 1
	}

	cmd := exec.Command("claude", "--system-prompt", systemPrompt)
	cmd.Dir = projectDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		log.Error("Task manager failed: %v", err)
		return 1
	}
	return 0
}

func printUsage() {
	fmt.Printf("%sRalph Loop v%s (go)%s - Autonomous Claude Code task iteration\n\n", logging.Bold, config.Version, logging.Reset)
	fmt.Printf("%sUSAGE:%s\n  ralph [OPTIONS]\n\n", logging.Bold, logging.Reset)
	fmt.Printf("%sOPTIONS:%s\n%s\n", logging.Bold, logging.Reset, config.FlagUsage())
	fmt.Printf(`%sEXAMPLES:%s
  ralph --dir ~/myproject -n 20
  ralph -p "Fix all failing tests"

%sSUBCOMMANDS:%s
  ralph commander [directory]  Full 4-pane tmux layout (loop + task manager + stream + plan)
  ralph task [directory]       Interactive task manager (standalone, no tmux)
  ralph stop [directory]       Halt after the current iteration
  ralph feedback [message]     Show queued feedback, or queue a new message
  ralph filter-stream <rawlog> Stream filter for tmux panes (colored, timestamped)

%sHOW IT WORKS:%s
  1. Tasks: Create tasks with ralph task (or bd directly)
  2. Execution: Each task runs in a fresh Claude context (~200k tokens)
  3. Completion: Claude signals when each task is done
  4. Repeat: Loop continues until all tasks complete or iteration cap is hit
`,
		logging.Bold, logging.Reset,
		logging.Bold, logging.Reset,
		logging.Bold, logging.Reset,
	)
}
