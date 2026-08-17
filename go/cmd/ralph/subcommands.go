package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	"github.com/brokenalarms/ralph/internal/tasks"
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

		backend := &tasks.BD{ProjectDir: sub.Dir}
		if err := backend.AppendNotes(taskID, msg); err != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "Failed to append notes: %v", err)
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

	case "resume":
		return handleResume(sub, log)

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

	case "guard-edit":
		return handleGuardEdit(sub)
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
		extractedDir, extractErr := extractEmbeddedPrompts(ralphDir)
		if extractErr != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "Failed to extract embedded prompts: %v", extractErr)
			return 1
		}
		promptsDir = extractedDir
	}

	dirs := workctx.New(cfg.ProjectDir, promptsDir)

	if cfg.UseTmux {
		return handleTmux(cfg, scriptPath, sub.Args, ralphDir, log)
	}

	return runMain(cfg, dirs, scriptPath, sub.Args, log)
}

// handleResume re-runs the loop with the flags recorded from the last run.
// The compiled binary is the source of truth for resume: state.json's
// cli_config holds only explicit deviations from registry defaults (see
// ConfigToState), and everything unrecorded takes the current binary's
// default — so a version upgrade between run and resume picks up new
// defaults automatically instead of freezing old ones. Explicit flags
// passed to `ralph resume` are appended after the recorded ones, so they
// win when config.Parse applies args in order.
func handleResume(sub config.Subcommand, log *logging.Logger) int {
	if hasHelpFlag(sub.Args) {
		printResumeUsage()
		return 0
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

	saved, err := state.NewStore(filepath.Join(repoRoot, ".ralph")).LoadCLIConfig()
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Warn}, "Could not read saved run configuration: %v", err)
	}
	if len(saved) == 0 {
		log.Emit(logging.Opts{}, "No saved run configuration — starting with defaults")
	} else {
		log.Emit(logging.Opts{}, "Resuming with recorded flags: %s", strings.Join(config.ArgsFromState(saved), " "))
	}

	return handleLoop(config.Subcommand{Name: "loop", Dir: sub.Dir, Args: resumeArgs(saved, sub.Args)}, log)
}

// resumeArgs reconstructs loop args from the recorded cli_config map and
// appends explicit overrides after them so the overrides take precedence.
func resumeArgs(saved map[string]string, overrides []string) []string {
	return append(config.ArgsFromState(saved), overrides...)
}

func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// interactiveSessionConfig captures everything that differs between the
// interactive sessions launched by runInteractiveSession — currently
// `ralph review` and `ralph task`. Everything else (help-flag check, repo
// resolution, prompts-dir fallback, worktree setup, the Interactive call,
// and the post-exit settle delay) is shared prologue that lives in
// runInteractiveSession itself.
type interactiveSessionConfig struct {
	usage func()
	model string
	// buildPrompt returns the system prompt for the session given the
	// resolved prompts/project/.ralph directories and the session's own
	// worktree (workDir). The review-session builder ignores workDir.
	buildPrompt func(promptsDir, projectDir, ralphDir, workDir string) (string, error)
	// extraArgs returns additional CLI args appended after the system
	// prompt (e.g. --session-id), given the session's git module, the
	// project's .ralph directory and the session worktree. May be nil.
	extraArgs func(gm git.Ops, ralphDir, workDir string) ([]string, error)
	// resolveWorkDir returns an existing worktree to anchor the session in,
	// bypassing worktree creation. `ralph task --resume` supplies it so a
	// resumed session runs in the worktree its original session recorded,
	// and so a missing worktree fails loudly instead of silently stranding
	// the session in a fresh one. When nil, the session creates its own
	// worktree via git.Ops.InitTask.
	resolveWorkDir func(ralphDir string) (string, error)
	// postWorktreeSetup runs after the session worktree is created (workDir
	// known) and before the Interactive launch. Task sessions use it to
	// install the main-checkout guard hook into the worktree. May be nil.
	postWorktreeSetup func(projectDir, workDir string) error
	// onExit runs once Interactive returns successfully, after the
	// terminal-settle delay.
	onExit func(gm git.Ops, projectDir, workDir string)
}

// runInteractiveSession holds the prologue shared by every interactive
// Claude session ralph launches: help-flag handling, git-repo resolution,
// prompts-dir fallback, worktree setup via git.Ops.InitTask, and the
// Interactive invocation itself. Callers supply only what differs.
func runInteractiveSession(sub config.Subcommand, log *logging.Logger, cfg interactiveSessionConfig) int {
	if hasHelpFlag(sub.Args) {
		cfg.usage()
		return 0
	}

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
		extractedDir, extractErr := extractEmbeddedPrompts(ralphDir)
		if extractErr != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "Failed to extract embedded prompts: %v", extractErr)
			return 1
		}
		promptsDir = extractedDir
	}

	// Spawn the session inside an isolated worktree — never the project
	// root. Agents must not run with cwd == projectDir.
	var workDir string
	if cfg.resolveWorkDir != nil {
		workDir, err = cfg.resolveWorkDir(ralphDir)
		if err != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "%v", err)
			return 1
		}
	}

	gm := git.New(git.Config{
		ProjectDir: projectDir,
		WorkDir:    workDir,
		RalphDir:   ralphDir,
		BaseBranch: "main",
		Logger:     log,
	})

	if cfg.resolveWorkDir == nil {
		if err := gm.InitTask(context.Background()); err != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "Worktree setup failed: %v", err)
			return 1
		}
		workDir = gm.GetWorkDir()
	}

	if cfg.postWorktreeSetup != nil {
		if err := cfg.postWorktreeSetup(projectDir, workDir); err != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "Worktree guard setup failed: %v", err)
			return 1
		}
	}

	systemPrompt, err := cfg.buildPrompt(promptsDir, projectDir, ralphDir, workDir)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "%v", err)
		return 1
	}

	var extraArgs []string
	if cfg.extraArgs != nil {
		extraArgs, err = cfg.extraArgs(gm, ralphDir, workDir)
		if err != nil {
			log.Emit(logging.Opts{Level: logging.Error}, "%v", err)
			return 1
		}
	}

	r := newInteractiveAgent(log, projectDir, cfg.model)
	exitCode, err := r.Interactive(workDir, systemPrompt, extraArgs...)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "Interactive session failed: %v", err)
		return 1
	}

	// Let the terminal finish processing any ANSI escape sequences from
	// the CLI's exit message before running exit cleanup.
	time.Sleep(100 * time.Millisecond)

	if cfg.onExit != nil {
		cfg.onExit(gm, projectDir, workDir)
	}
	return exitCode
}

func handleReview(sub config.Subcommand, log *logging.Logger) int {
	return runInteractiveSession(sub, log, interactiveSessionConfig{
		usage: printReviewUsage,
		model: agent.ModelOpus,
		buildPrompt: func(promptsDir, projectDir, ralphDir, workDir string) (string, error) {
			reflections, err := prompt.ReadReflections(ralphDir)
			if err != nil {
				log.Emit(logging.Opts{Level: logging.Warn}, "Failed to read reflections: %v", err)
			}
			systemPrompt, err := prompt.BuildReviewPrompt(promptsDir, projectDir, ralphDir, reflections)
			if err != nil {
				return "", fmt.Errorf("failed to build review prompt: %w", err)
			}
			return systemPrompt, nil
		},
		onExit: func(gm git.Ops, projectDir, workDir string) {
			postReviewCleanup(filepath.Join(projectDir, ".ralph"), log)
			if workDir != projectDir && gm.LogOneline("origin/main", "HEAD") == "" {
				gm.RemoveWorktree()
			}
		},
	})
}

// postReviewCleanup clears completed_tasks from state.json and archives reflections.
func postReviewCleanup(ralphDir string, log *logging.Logger) {
	st := state.NewStore(ralphDir)

	archived, err := prompt.ArchiveReflections(ralphDir)
	if err != nil {
		log.Emit(logging.Opts{Domain: logging.Review, Level: logging.Warn}, "Failed to archive reflections: %v", err)
	} else if len(archived) > 0 {
		log.Emit(logging.Opts{Domain: logging.Review}, "Archived %d reflections", len(archived))
	}

	if err := st.ClearCompletedTasks(); err != nil {
		log.Emit(logging.Opts{Domain: logging.Review, Level: logging.Warn}, "Failed to clear completed tasks: %v", err)
	} else {
		log.Emit(logging.Opts{Domain: logging.Review}, "Cleared completed_tasks from state")
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

// taskExtraArgs assembles the extra CLI args for a fresh task session: the
// session-id flag and its value, plus --disallowedTools EnterWorktree (see
// taskSessionArgs).
func taskExtraArgs(sessionID string) []string {
	return taskSessionArgs("--session-id", sessionID)
}

// taskResumeExtraArgs is the `ralph task --resume` counterpart of
// taskExtraArgs: --resume replaces --session-id, since the two are mutually
// exclusive — --session-id asks the CLI to create a session with that id,
// --resume asks it to reopen one.
func taskResumeExtraArgs(sessionID string) []string {
	return taskSessionArgs("--resume", sessionID)
}

// taskSessionArgs pairs the session flag with --disallowedTools
// EnterWorktree, which pins the session's working root to the ralph-assigned
// worktree (the harness's session-level worktree-switch tool must not
// re-anchor the session into the main checkout). This does NOT restrict the
// Agent tool's per-subagent worktree isolation — that is a separate,
// legitimate harness mechanism.
func taskSessionArgs(sessionFlag, sessionID string) []string {
	return []string{
		sessionFlag, sessionID,
		"--disallowedTools", "EnterWorktree",
	}
}

// handleTask launches an interactive Claude session with the task manager
// prompt. Runs standalone — no tmux required.
//
// With --resume <session-id> it reopens a previous task session in the
// worktree that session recorded, rebuilding the task-manager system prompt
// from scratch. The system prompt is per-invocation, not part of the restored
// transcript, so a session reopened with a bare `claude --resume` comes back
// as a plain Claude session with no knowledge of ralph's conventions.
func handleTask(sub config.Subcommand, log *logging.Logger) int {
	resumeID, args, err := parseTaskResumeFlag(sub.Args)
	if err != nil {
		log.Emit(logging.Opts{Level: logging.Error}, "%v", err)
		printTaskUsage()
		return 1
	}
	sub.Args = args

	// sessionID and sessionBranch describe the session the exit prompt acts
	// on: generated at launch for a fresh session, read from the recorded
	// session for a resume.
	sessionID := resumeID
	var sessionBranch string

	cfg := interactiveSessionConfig{
		usage: printTaskUsage,
		model: agent.ModelFable,
		buildPrompt: func(promptsDir, projectDir, ralphDir, workDir string) (string, error) {
			startupCtx := preloadTaskContext(&tasks.BD{ProjectDir: projectDir}, log)
			systemPrompt, err := prompt.BuildTaskManagerPrompt(promptsDir, projectDir, workDir, ralphDir, startupCtx)
			if err != nil {
				return "", fmt.Errorf("failed to build task manager prompt: %w", err)
			}
			return systemPrompt, nil
		},
		extraArgs: func(gm git.Ops, ralphDir, workDir string) ([]string, error) {
			if resumeID != "" {
				return taskResumeExtraArgs(resumeID), nil
			}
			id, err := generateSessionID()
			if err != nil {
				return nil, fmt.Errorf("failed to generate session ID: %w", err)
			}
			sessionID = id
			sessionBranch = gm.GetWorktreeBranch()
			session := taskSession{WorkDir: workDir, Branch: sessionBranch}
			if err := writeTaskSession(ralphDir, id, session); err != nil {
				log.Emit(logging.Opts{Level: logging.Warn}, "Could not record task session for resume: %v", err)
			}
			return taskExtraArgs(id), nil
		},
		postWorktreeSetup: func(projectDir, workDir string) error {
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolving ralph executable: %w", err)
			}
			return writeTaskGuardSettings(workDir, projectDir, exe)
		},
		onExit: func(gm git.Ops, projectDir, workDir string) {
			if promptKeepOrCleanupWorktree(os.Stdout, os.Stdin, gm, sessionBranch) {
				printTaskResumeHint(os.Stdout, sessionID)
				return
			}
			removeTaskSession(filepath.Join(projectDir, ".ralph"), sessionID)
		},
	}

	if resumeID != "" {
		cfg.resolveWorkDir = func(ralphDir string) (string, error) {
			session, err := readTaskSession(ralphDir, resumeID)
			if err != nil {
				return "", err
			}
			sessionBranch = session.Branch
			return session.WorkDir, nil
		}
	}

	return runInteractiveSession(sub, log, cfg)
}

// parseTaskResumeFlag extracts --resume <session-id> (or --resume=<id>) from
// the `ralph task` args, returning the session id and the remaining args.
func parseTaskResumeFlag(args []string) (string, []string, error) {
	var sessionID string
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--resume":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--resume requires a session id")
			}
			sessionID = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--resume="):
			sessionID = strings.TrimPrefix(args[i], "--resume=")
			if sessionID == "" {
				return "", nil, fmt.Errorf("--resume requires a session id")
			}
		default:
			rest = append(rest, args[i])
		}
	}
	return sessionID, rest, nil
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
  ralph resume [options]       Re-run the loop with the flags recorded from the last run
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
	fmt.Printf("%sUSAGE:%s\n  ralph task [--model <model>] [--resume <session-id>]\n\n", logging.Bold, logging.Reset)
	fmt.Printf("Launches an interactive Claude session for creating tasks, writing specs,\nand managing the project backlog.\n\n")
	fmt.Printf("%sOPTIONS:%s\n  --model <model>            Model ceiling for the session — alias (opus, sonnet, haiku) for latest, or pinned ID (e.g. claude-sonnet-4-6)\n", logging.Bold, logging.Reset)
	fmt.Printf("  --resume <session-id>      Reopen a previous task session in the worktree it ran in, with the task manager prompt rebuilt\n")
}

func printReviewUsage() {
	fmt.Printf("%sralph review%s - Post-mortem review\n\n", logging.Bold, logging.Reset)
	fmt.Printf("%sUSAGE:%s\n  ralph review [directory] [--model <model>]\n\n", logging.Bold, logging.Reset)
	fmt.Printf("Launches an interactive Claude session for reviewing reflections, auditing\ntests, and identifying refactoring opportunities.\n\n")
	fmt.Printf("%sOPTIONS:%s\n  --model <model>    Model ceiling for the session — alias (opus, sonnet, haiku) for latest, or pinned ID (e.g. claude-sonnet-4-6)\n", logging.Bold, logging.Reset)
}

func printResumeUsage() {
	fmt.Printf("%sralph resume%s - Re-run the loop with the flags recorded from the last run\n\n", logging.Bold, logging.Reset)
	fmt.Printf("%sUSAGE:%s\n  ralph resume [options]\n\n", logging.Bold, logging.Reset)
	fmt.Printf("Reads the explicit flags recorded in .ralph/state.json from the last\n`ralph loop` invocation and starts the loop with them. Flags never set\nexplicitly take the current binary's defaults. Any options given here\noverride the recorded ones.\n")
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
func preloadTaskContext(backend tasks.Backend, log *logging.Logger) string {
	var parts []string

	openList, openErr := backend.ListOpen()
	if openErr == nil && strings.TrimSpace(openList) != "" {
		parts = append(parts, fmt.Sprintf("$ bd list\n%s", strings.TrimSpace(openList)))
	}

	readyList, readyErr := backend.ListReady()
	if readyErr == nil && strings.TrimSpace(readyList) != "" {
		parts = append(parts, fmt.Sprintf("$ bd ready\n%s", strings.TrimSpace(readyList)))
	}

	if openErr != nil && readyErr != nil {
		log.Emit(logging.Opts{Level: logging.Warn}, "bd not found, skipping startup preload")
	}

	closedList, closedErr := backend.ListClosed()
	if closedErr != nil {
		log.Emit(logging.Opts{Level: logging.Warn}, "bd list closed failed, skipping audit-window: %v", closedErr)
	} else {
		unaudited := tasks.UnauditedClosures(closedList, config.LoopAssignee, time.Now(), tasks.AuditWindow)
		if len(unaudited) > 0 {
			parts = append(parts, formatAuditWindow(unaudited))
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// formatAuditWindow renders the unaudited-closure set as the "$ audit-window"
// startup context block the task-manager prompt reads to surface the
// recent-closure audit prompt without recomputing the window itself.
func formatAuditWindow(unaudited []tasks.ClosedTaskInfo) string {
	lines := make([]string, 0, len(unaudited)+1)
	lines = append(lines, fmt.Sprintf("%d unaudited loop closures (72h window):", len(unaudited)))
	for _, c := range unaudited {
		lines = append(lines, fmt.Sprintf("%s  %s  %s", c.ID, c.ClosedAt.Format(time.RFC3339), c.Title))
	}
	return "$ audit-window\n" + strings.Join(lines, "\n")
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

// taskSessionsDirName is the .ralph subdirectory holding one record per
// launched `ralph task` session, keyed by the session id given to the CLI.
const taskSessionsDirName = "task-sessions"

// taskSession is what `ralph task --resume` needs to reopen a session where
// it left off: the worktree it ran in and the branch checked out there, so
// the exit prompt can still clean both up.
type taskSession struct {
	WorkDir string `json:"work_dir"`
	Branch  string `json:"branch"`
}

// taskSessionPath rejects any session id that is not plain hex-and-dashes —
// the id reaches this code from the command line, and it is used as a file
// name.
func taskSessionPath(ralphDir, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("empty task session id")
	}
	for _, c := range sessionID {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex && c != '-' {
			return "", fmt.Errorf("invalid task session id %q — expected a session UUID", sessionID)
		}
	}
	return filepath.Join(ralphDir, taskSessionsDirName, sessionID+".json"), nil
}

func writeTaskSession(ralphDir, sessionID string, session taskSession) error {
	path, err := taskSessionPath(ralphDir, sessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating task session dir: %w", err)
	}
	data, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// readTaskSession resolves the worktree recorded for sessionID at launch.
// Both a missing record and a vanished worktree (the user answered "n" to
// "Keep this task worktree for resume?") are errors: a resumed session must
// come up in the worktree it was working in, never in a freshly created one.
func readTaskSession(ralphDir, sessionID string) (taskSession, error) {
	path, err := taskSessionPath(ralphDir, sessionID)
	if err != nil {
		return taskSession{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return taskSession{}, fmt.Errorf("no worktree recorded for task session %s — only sessions started by `ralph task` can be resumed", sessionID)
	}
	var session taskSession
	if err := json.Unmarshal(data, &session); err != nil {
		return taskSession{}, fmt.Errorf("task session record %s is unreadable: %w", path, err)
	}
	info, err := os.Stat(session.WorkDir)
	if err != nil || !info.IsDir() {
		return taskSession{}, fmt.Errorf("worktree %s for task session %s no longer exists — it was cleaned up when that session exited", session.WorkDir, sessionID)
	}
	return session, nil
}

func removeTaskSession(ralphDir, sessionID string) {
	if path, err := taskSessionPath(ralphDir, sessionID); err == nil {
		os.Remove(path)
	}
}

// printTaskResumeHint writes a visually dominant boxed resume command to w.
// It names `ralph task --resume` rather than a raw `claude --resume`: the
// system prompt is per-invocation and is not restored with the transcript,
// so only ralph can bring the session back with the task-manager prompt
// (and its own worktree) intact.
func printTaskResumeHint(w io.Writer, sessionID string) {
	cmd := fmt.Sprintf("ralph task --resume %s", sessionID)
	line := strings.Repeat("─", len(cmd)+4)
	fmt.Fprintf(w, "\n%s%s┌%s┐%s\n", logging.Cyan, logging.Bold, line, logging.Reset)
	fmt.Fprintf(w, "%s%s│  %s  │%s\n", logging.Cyan, logging.Bold, cmd, logging.Reset)
	fmt.Fprintf(w, "%s%s└%s┘%s\n\n", logging.Cyan, logging.Bold, line, logging.Reset)
}

// promptKeepOrCleanupWorktree asks the user whether to keep the task worktree
// for resume, defaulting to Keep on empty/EOF input — which also covers the
// non-TTY case, since a closed or non-interactive stdin reads as EOF. Only an
// explicit "n"/"no" answer triggers cleanup: it removes the worktree and its
// ralph/task/YYYYMMDD-NN branch via gm.RemoveWorktreeForBranch
// (registration-aware, never a raw os.RemoveAll) and returns false so the
// caller skips the resume hint. The branch is passed in rather than read from
// gm, because a resumed session adopts an existing worktree instead of
// creating one and so learns its branch from the session record.
func promptKeepOrCleanupWorktree(w io.Writer, r io.Reader, gm git.Ops, branch string) bool {
	fmt.Fprint(w, "Keep this task worktree for resume? [Y/n] ")
	line, _ := bufio.NewReader(r).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "n" && answer != "no" {
		return true
	}
	gm.RemoveWorktreeForBranch(branch)
	fmt.Fprintln(w, "Task worktree cleaned up.")
	return false
}
