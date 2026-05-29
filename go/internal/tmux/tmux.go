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

// Session manages a tmux session with ralph's 3-pane layout:
// left=loop, top-right=stream, bottom-right=plan.
type Session struct {
	Name       string
	ProjectDir string
	RalphDir   string
	RawLogPath string

	// RalphCmd is the command line to run in the loop pane.
	RalphCmd string

	// ScriptPath is the path to the ralph binary, used to build subcommands.
	ScriptPath string

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
	// Note: .stream-task is NOT removed — it contains the current task label
	// written by the loop, and PaneTitle.Run() needs it to show the task ID
	// in the stream pane title when attaching to a running session.
	os.Remove(filepath.Join(s.RalphDir, ".completed-tasks"))
	os.Remove(filepath.Join(s.RalphDir, ".stream-filter.sh"))

	if err := s.writePlanWatcher(); err != nil {
		return fmt.Errorf("write plan watcher: %w", err)
	}

	if err := s.createSession(); err != nil {
		return fmt.Errorf("create tmux session: %w", err)
	}

	s.paneTitle = NewPaneTitle(s.Name, s.RalphDir, PaneStream)

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
	return sessionExists(s.Name)
}

// PaneTitle returns the PaneTitle manager for the stream pane.
// Returns nil if Setup hasn't been called.
func (s *Session) PaneTitle() *PaneTitle {
	return s.paneTitle
}

func (s *Session) createSession() error {
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

func (s *Session) filterStreamCmd() string {
	return fmt.Sprintf("%s filter-stream %s %s", shellQuote(s.ScriptPath), shellQuote(s.RawLogPath), shellQuote(s.ProjectDir))
}

func (s *Session) applySessionOptions() {
	tmuxCmd("set-option", "-t", s.Name, "pane-border-status", "top")                                                  //nolint:errcheck
	tmuxCmd("set-option", "-t", s.Name, "pane-border-format", "#{?pane_dead, #{pane_title} (dead) , #{pane_title} }") //nolint:errcheck
	tmuxCmd("set-option", "-t", s.Name, "remain-on-exit", "on")                                                       //nolint:errcheck
	tmuxCmd("set-option", "-t", s.Name, "set-titles", "off")                                                           //nolint:errcheck

	// Auto-kill the session when the ralph loop pane (pane 0) dies.
	// This replaces the old root-level q binding that stole keypresses globally.
	hookCmd := fmt.Sprintf(
		"if-shell \"tmux display-message -t '%s:.0' -p '#{pane_dead}' | grep -q 1\" \"kill-session -t '%s'\"",
		s.Name, s.Name,
	)
	tmuxCmd("set-hook", "-t", s.Name, "pane-died", hookCmd) //nolint:errcheck
}

func (s *Session) writePlanWatcher() error {
	renderBlock := s.bdPlanRender()

	script := fmt.Sprintf(`#!/usr/bin/env bash
stty -echo 2>/dev/null
BOLD=$'\033[1m'
CYAN=$'\033[0;36m'
GREEN=$'\033[0;32m'
DIM=$'\033[2m'
NC=$'\033[0m'
poll_counter=0
poll_interval=45
while true; do
  if [[ -f '%s/.plan-flash' ]]; then
    rm -f '%s/.plan-flash'
    (tmux set-option -p pane-border-style 'fg=green,bold' 2>/dev/null
     sleep 3
     tmux set-option -p -u pane-border-style 2>/dev/null) &
  fi
  needs_render=0
  if [[ -f '%s/.plan-refresh' ]]; then
    rm -f '%s/.plan-refresh'
    needs_render=1
    poll_counter=0
  fi
  if (( poll_counter >= poll_interval )); then
    needs_render=1
    poll_counter=0
  fi
  if (( needs_render )); then
    printf '\033[2J\033[H'
%s
  fi
  poll_counter=$((poll_counter + 1))
  sleep 1
done
`, s.RalphDir, s.RalphDir, s.RalphDir, s.RalphDir, renderBlock)

	return writeScript(filepath.Join(s.RalphDir, ".plan-watch.sh"), script)
}

func (s *Session) bdPlanRender() string {
	stateFile := filepath.Join(s.RalphDir, "state.json")
	return fmt.Sprintf(`    current_id=$(jq -r '.current_task_id // .last_task_id // empty' '%s' 2>/dev/null)
    current_title=$(jq -r '.last_task // empty' '%s' 2>/dev/null)
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
    open=$(bd count --status open 2>/dev/null || echo 0)
    inp=$(bd count --status in_progress 2>/dev/null || echo 0)
    total=$(( closed + open + inp ))
    if (( total > 0 )); then
      printf "\n${GREEN}%%s/%%s done${NC}\n" "$closed" "$total"
    fi
    if [[ -f '%s/.completed-tasks' ]]; then
      completed=$(grep -v '^$' '%s/.completed-tasks' | paste -sd ',' - | sed 's/,/, /g')
      [[ -n "$completed" ]] && printf "${DIM}Done: %%s${NC}\n" "$completed"
    fi`, stateFile, stateFile, s.RalphDir, s.RalphDir)
}

// BuildRalphCmd constructs the ralph re-exec command from the original args,
// stripping --tmux and adding --quiet.
// Always emits the "loop" subcommand so the re-exec uses the explicit path.
func BuildRalphCmd(scriptPath string, origArgs []string) string {
	parts := []string{shellQuote(scriptPath), "loop"}
	for _, arg := range origArgs {
		if arg == "--tmux" || arg == "loop" {
			continue
		}
		parts = append(parts, shellQuote(arg))
	}
	parts = append(parts, "--quiet")
	return strings.Join(parts, " ")
}

var tmuxCmd = func(args ...string) error {
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

// BaseSessionName returns the canonical session name for a project directory:
// "{basename}-loop" with dots and colons replaced by hyphens. This is the
// name used by `ralph loop --tmux` and what `ralph attach` should look for
// when deciding whether to reuse an existing session.
func BaseSessionName(projectDir string) string {
	return sanitizeSessionName(filepath.Base(projectDir)) + "-loop"
}

// SessionName builds a tmux-safe session name from a project directory path.
// The base name is "{basename}-loop". Characters invalid in tmux session names
// (dots and colons) are replaced with hyphens. If a session with that name
// already exists, a numeric suffix is appended (e.g. "ralph-loop-2").
func SessionName(projectDir string) string {
	base := BaseSessionName(projectDir)
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
