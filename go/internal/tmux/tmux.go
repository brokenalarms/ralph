package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Pane indices within a ralph tmux session.
const (
	PaneRalph  = 0
	PaneStream = 1
	PanePlan   = 2
)

// Session manages a tmux session with ralph's 3-pane layout:
// left=ralph loop, top-right=claude stream, bottom-right=plan progress.
type Session struct {
	Name       string
	ProjectDir string
	RalphDir   string
	RawLogPath string

	// RalphCmd is the command line to re-exec ralph in the left pane (without --tmux).
	RalphCmd string

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

	if err := s.writeStreamFilter(); err != nil {
		return fmt.Errorf("write stream filter: %w", err)
	}
	if err := s.writePlanWatcher(); err != nil {
		return fmt.Errorf("write plan watcher: %w", err)
	}

	if err := s.createSession(); err != nil {
		return fmt.Errorf("create tmux session: %w", err)
	}

	s.paneTitle = NewPaneTitle(s.Name, s.RalphDir)

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
	streamFilterPath := filepath.Join(s.RalphDir, ".stream-filter.sh")
	planWatchPath := filepath.Join(s.RalphDir, ".plan-watch.sh")

	ralphCmd := fmt.Sprintf("export _RALPH_TMUX_SESSION=%s; %s", s.Name, s.RalphCmd)

	if err := tmuxCmd("new-session", "-d", "-s", s.Name, "-c", s.ProjectDir, ralphCmd); err != nil {
		return err
	}

	streamCmd := fmt.Sprintf("bash '%s' '%s'", streamFilterPath, s.RawLogPath)
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

	tmuxCmd("set-option", "-t", s.Name, "pane-border-status", "top")                                                          //nolint:errcheck
	tmuxCmd("set-option", "-t", s.Name, "pane-border-format", "#{?pane_dead, #{pane_title} (dead) — press q to exit , #{pane_title} }") //nolint:errcheck
	tmuxCmd("set-option", "-t", s.Name, "remain-on-exit", "on")                                                            //nolint:errcheck

	// Bind q to kill the session when the main ralph pane is dead.
	deadCheck := fmt.Sprintf("tmux display-message -t '%s:.0' -p '#{pane_dead}' | grep -q 1", s.Name)
	killCmd := fmt.Sprintf("kill-session -t '%s'", s.Name)
	tmuxCmd("bind-key", "-T", "root", "q", "if-shell", deadCheck, killCmd) //nolint:errcheck

	tmuxCmd("select-pane", "-t", s.Name+":.0") //nolint:errcheck

	return nil
}

func (s *Session) writeStreamFilter() error {
	script := `#!/usr/bin/env bash
set +m
stty -echo 2>/dev/null
exec 2>"$(dirname "$0")/.stream-filter.err"
tail -f -n 0 "$1" | jq --raw-input --join-output --unbuffered '
  fromjson? // empty |
  if .type == "assistant" then
    .message.content[0]? //empty |
    if .type == "text" then "\n[claude] " + .text + "\n"
    elif .type == "tool_use" then
      if .name == "TodoWrite" then
        ([.input.todos[]? | .content] | if length == 0 then "[]"
          else join(", ") end) as $items |
        "\n[TodoWrite] " + $items + "\n"
      else
        (.input.file_path // .input.command // .input.pattern //
          .input.query // .input.url // .input.description //
          .input.task_id // .input.skill // .input.prompt //
          null) as $target |
        if $target then "\n[" + .name + "] " + $target + "\n"
        else "\n[" + .name + "]\n"
        end
      end
    else empty end
  elif .type == "result" then
    "\n[done]\n"
  else empty end
' | perl -ne '
  use POSIX; $|=1;
  chomp;
  next if $_ eq "";
  print strftime("%H:%M:%S", localtime()) . " " . $_ . "\n";
' | sed -u -E \
  -e $'s/\\[done\\]/\033[0;32m[done]\033[0m/g' \
  -e $'s/\\[claude\\]/\033[0;36m[claude]\033[0m/g' \
  -e $'s/\\[([A-Z][A-Za-z]*)\\]/\033[0;34m[\\1]\033[0m/g'
`
	return writeScript(filepath.Join(s.RalphDir, ".stream-filter.sh"), script)
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
NC=$'\033[0m'
while true; do
  if [[ -f '%s/.plan-refresh' ]]; then
    rm -f '%s/.plan-refresh'
    printf '\033[2J\033[H'
%s
  fi
  sleep 1
done
`, s.RalphDir, s.RalphDir, renderBlock)

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
    printf "${GREEN}%%s/%%s done${NC}\n" "$closed" "$total"
    if [[ -f '%s/.completed-tasks' ]]; then
      completed_list=$(paste -sd', ' '%s/.completed-tasks')
      if [[ -n "$completed_list" ]]; then
        printf "${GREEN}%%s${NC}\n" "$completed_list"
      fi
    fi`, s.RalphDir, s.RalphDir)
}

// BuildRalphCmd constructs the ralph re-exec command from the original args,
// stripping --tmux and adding --quiet.
func BuildRalphCmd(scriptPath string, origArgs []string) string {
	parts := []string{shellQuote(scriptPath)}
	for _, arg := range origArgs {
		if arg == "--tmux" {
			continue
		}
		parts = append(parts, shellQuote(arg))
	}
	parts = append(parts, "--quiet")
	return strings.Join(parts, " ")
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

func sessionExists(name string) bool {
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
