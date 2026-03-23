package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Pane indices within a ralph tmux session (3-pane layout).
const (
	PaneRalph  = 0
	PaneStream = 1
	PanePlan   = 2
)

// Pane indices within a commander tmux session (4-pane layout).
// After splits: 0=loop (top-left), 1=task (bottom-left),
// 2=stream (top-right), 3=plan (bottom-right).
const (
	CmdrPaneLoop   = 0
	CmdrPaneTask   = 1
	CmdrPaneStream = 2
	CmdrPanePlan   = 3
)

// Session manages a tmux session with ralph's pane layout.
// Standard mode: 3 panes (left=loop, top-right=stream, bottom-right=plan).
// Commander mode: 4 panes (top-left=loop, bottom-left=task manager,
// top-right=stream, bottom-right=plan).
type Session struct {
	Name       string
	ProjectDir string
	RalphDir   string
	RawLogPath string

	// RalphCmd is the command line to re-exec ralph in the loop pane (without --tmux).
	RalphCmd string

	// ScriptPath is the path to the ralph binary, used to build subcommands.
	ScriptPath string

	// TaskCmd is the command to run in the task manager pane (commander mode only).
	TaskCmd string

	// Commander enables the 4-pane layout with a task manager pane.
	Commander bool

	// TaskBackend is "bd" or "checklist", controls plan pane rendering.
	TaskBackend string

	// PlanFile is the path to plan.md for checklist backend.
	PlanFile string

	paneTitle *PaneTitle
}

// Available returns true if tmux is on the PATH.
func Available() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// Setup creates the tmux session, writes helper scripts, and attaches.
// This function blocks until the session ends (tmux attach-session).
// It returns nil on normal exit or an error if setup fails.
func (s *Session) Setup() error {
	// Clear stale files from a previous run so panes don't briefly show
	// old data before the loop writes current values.
	os.Remove(filepath.Join(s.RalphDir, ".stream-task"))
	os.Remove(filepath.Join(s.RalphDir, ".completed-tasks"))
	os.Remove(filepath.Join(s.RalphDir, ".stream-filter.sh"))

	if err := s.writePlanWatcher(); err != nil {
		return fmt.Errorf("write plan watcher: %w", err)
	}

	if err := s.createSession(); err != nil {
		return fmt.Errorf("create tmux session: %w", err)
	}

	streamPane := PaneStream
	if s.Commander {
		streamPane = CmdrPaneStream
	}
	s.paneTitle = NewPaneTitle(s.Name, s.RalphDir, streamPane)

	// Signal the plan pane to render immediately instead of waiting for the first iteration.
	touchFile(filepath.Join(s.RalphDir, ".plan-refresh"))

	return nil
}

// Attach attaches to the tmux session. Blocks until detached or session ends.
func (s *Session) Attach() error {
	cmd := exec.Command("tmux", "attach-session", "-t", s.Name)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Kill destroys the tmux session. Used on interrupt/Ctrl-C.
func (s *Session) Kill() {
	exec.Command("tmux", "kill-session", "-t", s.Name).Run() //nolint:errcheck
}

// HasSession returns true if the tmux session still exists.
func (s *Session) HasSession() bool {
	return exec.Command("tmux", "has-session", "-t", s.Name).Run() == nil
}

// PaneTitle returns the PaneTitle manager for the stream pane.
// Returns nil if Setup hasn't been called.
func (s *Session) PaneTitle() *PaneTitle {
	return s.paneTitle
}

func (s *Session) createSession() error {
	if s.Commander {
		return s.createCommanderSession()
	}
	return s.createStandardSession()
}

func (s *Session) createStandardSession() error {
	planWatchPath := filepath.Join(s.RalphDir, ".plan-watch.sh")

	ralphCmd := fmt.Sprintf("export _RALPH_TMUX_SESSION=%s; %s", s.Name, s.RalphCmd)

	if err := tmuxCmd("new-session", "-d", "-s", s.Name, "-c", s.ProjectDir, ralphCmd); err != nil {
		return err
	}

	streamCmd := s.filterStreamCmd()
	if err := tmuxCmd("split-window", "-h", "-t", s.Name, streamCmd); err != nil {
		return err
	}

	planCmd := fmt.Sprintf("bash '%s'", planWatchPath)
	if err := tmuxCmd("split-window", "-v", "-t", s.Name+":.1", planCmd); err != nil {
		return err
	}

	tmuxCmd("select-pane", "-t", s.Name+":.0", "-T", "(go) ralph") //nolint:errcheck
	tmuxCmd("select-pane", "-t", s.Name+":.1", "-T", "stream") //nolint:errcheck
	tmuxCmd("select-pane", "-t", s.Name+":.2", "-T", "plan")   //nolint:errcheck

	s.applySessionOptions()

	tmuxCmd("select-pane", "-t", s.Name+":.0") //nolint:errcheck

	return nil
}

// createCommanderSession builds the 4-pane layout:
//
//	┌──────────────┬──────────────┐
//	│ pane 0: loop │ pane 2: strm │
//	├──────────────┤──────────────┤
//	│ pane 1: task │ pane 3: plan │
//	└──────────────┴──────────────┘
func (s *Session) createCommanderSession() error {
	planWatchPath := filepath.Join(s.RalphDir, ".plan-watch.sh")

	ralphCmd := fmt.Sprintf("export _RALPH_TMUX_SESSION=%s; %s", s.Name, s.RalphCmd)

	if err := tmuxCmd("new-session", "-d", "-s", s.Name, "-c", s.ProjectDir, ralphCmd); err != nil {
		return err
	}

	streamCmd := s.filterStreamCmd()
	if err := tmuxCmd("split-window", "-h", "-t", s.Name, streamCmd); err != nil {
		return err
	}

	// Split left pane (0) vertically to create task manager below loop.
	if err := tmuxCmd("split-window", "-v", "-t", s.Name+":.0", s.TaskCmd); err != nil {
		return err
	}

	planCmd := fmt.Sprintf("bash '%s'", planWatchPath)
	// After the split, stream is now pane 2. Split it vertically for plan.
	if err := tmuxCmd("split-window", "-v", "-t", s.Name+":.2", planCmd); err != nil {
		return err
	}

	tmuxCmd("select-pane", "-t", s.Name+":.0", "-T", "(go) ralph") //nolint:errcheck
	tmuxCmd("select-pane", "-t", s.Name+":.1", "-T", "task")       //nolint:errcheck
	tmuxCmd("select-pane", "-t", s.Name+":.2", "-T", "stream")     //nolint:errcheck
	tmuxCmd("select-pane", "-t", s.Name+":.3", "-T", "plan")       //nolint:errcheck

	s.applySessionOptions()

	tmuxCmd("select-pane", "-t", s.Name+":.0") //nolint:errcheck

	return nil
}

func (s *Session) filterStreamCmd() string {
	return fmt.Sprintf("%s filter-stream %s", shellQuote(s.ScriptPath), shellQuote(s.RawLogPath))
}

func (s *Session) applySessionOptions() {
	tmuxCmd("set-option", "-t", s.Name, "pane-border-status", "top")                                                                    //nolint:errcheck
	tmuxCmd("set-option", "-t", s.Name, "pane-border-format", "#{?pane_dead, #{pane_title} (dead) — press q to exit , #{pane_title} }") //nolint:errcheck
	tmuxCmd("set-option", "-t", s.Name, "remain-on-exit", "on")                                                                         //nolint:errcheck

	deadCheck := fmt.Sprintf("tmux display-message -t '%s:.0' -p '#{pane_dead}' | grep -q 1", s.Name)
	killCmd := fmt.Sprintf("kill-session -t '%s'", s.Name)
	tmuxCmd("bind-key", "-T", "root", "q", "if-shell", deadCheck, killCmd) //nolint:errcheck
}

func (s *Session) writePlanWatcher() error {
	var renderBlock string
	if s.TaskBackend == "bd" {
		renderBlock = s.bdPlanRender()
	} else {
		renderBlock = fmt.Sprintf("      cat '%s' 2>/dev/null", s.PlanFile)
	}

	script := fmt.Sprintf(`#!/usr/bin/env bash
stty -echo 2>/dev/null
BOLD=$'\033[1m'
CYAN=$'\033[0;36m'
GREEN=$'\033[0;32m'
DIM=$'\033[2m'
NC=$'\033[0m'
while true; do
  if [[ -f '%s/.plan-flash' ]]; then
    rm -f '%s/.plan-flash'
    (tmux set-option -p pane-border-style 'fg=green,bold' 2>/dev/null
     sleep 3
     tmux set-option -p -u pane-border-style 2>/dev/null) &
  fi
  if [[ -f '%s/.plan-refresh' ]]; then
    rm -f '%s/.plan-refresh'
    printf '\033[2J\033[H'
%s
  fi
  sleep 1
done
`, s.RalphDir, s.RalphDir, s.RalphDir, s.RalphDir, renderBlock)

	return writeScript(filepath.Join(s.RalphDir, ".plan-watch.sh"), script)
}

func (s *Session) bdPlanRender() string {
	return fmt.Sprintf(`    current_json=$(bd list --status in_progress --flat --json --limit 1 2>/dev/null)
    current_title=$(echo "$current_json" | jq -r '.[0].title // empty' 2>/dev/null)
    current_id=$(echo "$current_json" | jq -r '.[0].id // empty' 2>/dev/null)
    if [[ -n "$current_title" ]]; then
      printf "${BOLD}${CYAN}▶ %%s${NC} (%%s)\n" "$current_title" "$current_id"
      printf "\n"
    fi
    if [[ -n "$current_id" ]]; then
      ready_list=$(bd ready --json --limit 8 2>/dev/null | jq -r --arg cid "$current_id" '[.[] | select(.id != $cid)] | .[] | "  \(.id) · \(.title)"' 2>/dev/null || true)
    else
      ready_list=$(bd ready --json --limit 8 2>/dev/null | jq -r '.[] | "  \(.id) · \(.title)"' 2>/dev/null || true)
    fi
    if [[ -n "$ready_list" ]]; then
      printf "${BOLD}Ready:${NC}\n%%s\n\n" "$ready_list"
    fi
    if [[ -n "$current_id" ]]; then
      unblocks=$(bd show "$current_id" --json 2>/dev/null | jq -r '.[0].dependents[]? | "  → \(.id): \(.title)"' 2>/dev/null || true)
      if [[ -n "$unblocks" ]]; then
        printf "${BOLD}Unblocks:${NC}\n%%s\n\n" "$unblocks"
      fi
    fi
    closed=$(bd count --status closed 2>/dev/null || echo 0)
    total=$(bd count 2>/dev/null || echo 0)
    if [[ $total -gt 0 ]]; then
      bar_w=20
      filled=$((closed * bar_w / total))
      empty=$((bar_w - filled))
      bar=""
      for ((i=0; i<filled; i++)); do bar+="█"; done
      for ((i=0; i<empty; i++)); do bar+="░"; done
      printf "${GREEN}[%%s] %%s/%%s done${NC}\n" "$bar" "$closed" "$total"
    else
      printf "${GREEN}0/0 done${NC}\n"
    fi
    if [[ -f '%s/.completed-tasks' ]]; then
      while IFS= read -r ctask; do
        [[ -n "$ctask" ]] && printf "${DIM}  ✓ %%s${NC}\n" "$ctask"
      done < '%s/.completed-tasks'
    fi`, s.RalphDir, s.RalphDir)
}

// BuildRalphCmd constructs the ralph re-exec command from the original args,
// stripping --tmux and the commander subcommand, and adding --quiet.
func BuildRalphCmd(scriptPath string, origArgs []string) string {
	parts := []string{shellQuote(scriptPath)}
	for _, arg := range origArgs {
		if arg == "--tmux" || arg == "commander" {
			continue
		}
		parts = append(parts, shellQuote(arg))
	}
	parts = append(parts, "--quiet")
	return strings.Join(parts, " ")
}

// BuildTaskCmd constructs the command to launch the task manager pane.
// It runs `ralph task` with the project directory.
func BuildTaskCmd(scriptPath, projectDir string) string {
	return shellQuote(scriptPath) + " task " + shellQuote(projectDir)
}

func tmuxCmd(args ...string) error {
	return exec.Command("tmux", args...).Run()
}

func writeScript(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o755)
}

func touchFile(path string) {
	f, err := os.Create(path)
	if err == nil {
		f.Close()
	}
}

// SessionName builds a tmux-safe session name from a project directory path.
// The base name is "{basename}-loop". Characters invalid in tmux session names
// (dots and colons) are replaced with hyphens. If a session with that name
// already exists, a numeric suffix is appended (e.g. "ralph-loop-2").
func SessionName(projectDir string) string {
	base := sanitizeSessionName(filepath.Base(projectDir)) + "-loop"
	if !sessionExists(base) {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !sessionExists(candidate) {
			return candidate
		}
	}
}

func sanitizeSessionName(name string) string {
	name = strings.ReplaceAll(name, ".", "-")
	name = strings.ReplaceAll(name, ":", "-")
	return name
}

// sessionExists is a package-level var so tests can stub it to avoid
// depending on live tmux state.
var sessionExists = func(name string) bool {
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
