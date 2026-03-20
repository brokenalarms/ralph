package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brokenalarms/ralph/internal/config"
	"github.com/brokenalarms/ralph/internal/logging"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	log := logging.New(nil)

	// Check subcommands before flag parsing (matches ralph.sh behavior).
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

	// Placeholder: print parsed config to confirm CLI works.
	log.Phase("Ralph Loop v%s", config.Version)
	log.Log("project=%s iterations=%d worktree=%v", cfg.ProjectDir, cfg.MaxIterations, cfg.UseWorktree)

	return 0
}

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
	fmt.Printf(`%sRalph Loop v%s%s - Autonomous Claude Code task iteration

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
