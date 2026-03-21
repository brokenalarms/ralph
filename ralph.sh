#!/usr/bin/env bash
set -euo pipefail

# Ralph Loop - Autonomous Claude Code task iteration
# Runs Claude Code CLI in fresh-context iterations against a project repo.
# Prompts live in ./prompts/ — edit them to change Claude's behavior.

VERSION="0.1.0"
_source="${BASH_SOURCE[0]}"
while [[ -L "$_source" ]]; do _source="$(readlink "$_source")"; done
SCRIPT_DIR="$(cd "$(dirname "$_source")" && pwd)"
PROMPTS_DIR="$SCRIPT_DIR/prompts"
source "$SCRIPT_DIR/lib/tasks.sh"
source "$SCRIPT_DIR/lib/git.sh"
source "$SCRIPT_DIR/lib/quality.sh"

# --- Defaults ---
PROJECT_DIR="$(pwd)"
MAX_ITERATIONS=50
PROMPT_OVERRIDE=""
RESUME=false
PLAN_ONLY=false
SKIP_PLANNING=false
WATCHER_INTERVAL=2
PLAN_FILE_ARG=""
QUIET=false
USE_WORKTREE=true
CALLS_PER_HOUR=80
REFACTOR_THRESHOLD="${RALPH_REFACTOR_THRESHOLD:-20}"
MAX_TASK_ATTEMPTS="${RALPH_MAX_TASK_ATTEMPTS:-5}"
USE_TMUX=false
AUTO_MERGE=false
STUCK_THRESHOLD=5
STUCK_CONFIRMATION_THRESHOLD=2
STAGNATION_THRESHOLD=3
TEST_SATURATION_THRESHOLD=3
PERMISSION_DENIAL_THRESHOLD=3

# Track which settings were explicitly set via CLI (bash 3 compatible)
_CLI_MAX_ITERATIONS=""
_CLI_CALLS_PER_HOUR=""

# --- Config file loader (simple TOML subset: key = value) ---
load_config() {
  local config_file="$1"
  [[ -f "$config_file" ]] || return 0
  local line key value
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%%#*}"
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    [[ -z "$line" || "$line" == \[* ]] && continue
    key="${line%%=*}"
    value="${line#*=}"
    key="${key#"${key%%[![:space:]]*}"}"
    key="${key%"${key##*[![:space:]]}"}"
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    value="${value#\"}"
    value="${value%\"}"
    case "$key" in
      max_iterations)              [[ -z "$_CLI_MAX_ITERATIONS" ]] && MAX_ITERATIONS="$value" ;;
      calls_per_hour)              [[ -z "$_CLI_CALLS_PER_HOUR" ]] && CALLS_PER_HOUR="$value" ;;
      watcher_interval)            WATCHER_INTERVAL="$value" ;;
      stuck_threshold)             STUCK_THRESHOLD="$value" ;;
      stuck_confirmation_threshold) STUCK_CONFIRMATION_THRESHOLD="$value" ;;
      stagnation_threshold)        STAGNATION_THRESHOLD="$value" ;;
      test_saturation_threshold)   TEST_SATURATION_THRESHOLD="$value" ;;
      permission_denial_threshold) PERMISSION_DENIAL_THRESHOLD="$value" ;;
    esac
  done < "$config_file"
}
TMUX_SESSION=""
_TMUX_OUTER=false
WORK_DIR=""
WORKTREE_BRANCH=""
PROJECT_NAME=""
_TASK_SEQ=0
LOG_FILE="/dev/null"  # real path set after dir resolution

# --- Colors ---
RED=$'\033[0;31m'
GREEN=$'\033[0;32m'
YELLOW=$'\033[0;33m'
BLUE=$'\033[0;34m'
CYAN=$'\033[0;36m'
BOLD=$'\033[1m'
NC=$'\033[0m'

# --- Logging ---
_ts() { date +%H:%M:%S; }
log()         { echo -e "$(_ts) ${CYAN}[ralph]${NC} $*" | tee -a "$LOG_FILE"; }
log_success() { echo -e "$(_ts) ${GREEN}[ralph]${NC} $*" | tee -a "$LOG_FILE"; }
log_warn()    { echo -e "$(_ts) ${YELLOW}[ralph]${NC} $*" | tee -a "$LOG_FILE"; }
log_error()   { echo -e "$(_ts) ${RED}[ralph]${NC} $*" | tee -a "$LOG_FILE"; }
log_phase()   { echo -e "$(_ts) ${BOLD}${BLUE}[ralph]${NC} ${BOLD}$*${NC}" | tee -a "$LOG_FILE"; }

log_task()         { echo -e "$(_ts) ${CYAN}[$(task_label)]${NC} $*" | tee -a "$LOG_FILE"; }
log_task_success() { echo -e "$(_ts) ${GREEN}[$(task_label)]${NC} $*" | tee -a "$LOG_FILE"; }
log_task_error()   { echo -e "$(_ts) ${RED}[$(task_label)]${NC} $*" | tee -a "$LOG_FILE"; }

# --- Helpers ---
slugify() {
  echo "$1" | tr '[:upper:]' '[:lower:]' | \
    sed 's/[^a-z0-9]/-/g; s/--*/-/g; s/^-//; s/-$//' | cut -c1-50
}

_BRANCH_RENAMED=false

# --- Usage ---
usage() {
  cat <<EOF
${BOLD}Ralph Loop v${VERSION} (sh)${NC} - Autonomous Claude Code task iteration

${BOLD}USAGE:${NC}
  ralph.sh [OPTIONS] [directory]

${BOLD}OPTIONS:${NC}
  -d, --dir <path>       Project directory (default: cwd)
  -n, --max <N>          Max iterations (default: 50)
  -p, --prompt <text>    Prompt override (otherwise Claude reads repo context)
  --plan-file <path>     Pre-made plan in Ralph format (markdown checkboxes). Skips planning phase.
  --plan                 Run planning phase only
  --skip-planning        Skip interactive planning, go straight to autonomous execution
  -q, --quiet            Suppress Claude output streaming (log only)
  --no-worktree          Run directly in project dir (no git worktree isolation)
  --calls-per-hour <N>   Max Claude calls per hour (default: 80)
  --refactor-threshold <N> Quality pain score that triggers a refactor iteration (default: 20, 0 to disable)
  --max-task-attempts <N> Max attempts per task before skipping (default: 5)
  --tmux                 Run in tmux 3-pane layout (status / output / plan)
  --auto-merge           Squash-merge each PR into main after task completion
  -h, --help             Show this help

${BOLD}EXAMPLES:${NC}
  ralph.sh ~/myproject -n 20
  ralph.sh -p "Fix all failing tests"
  ralph.sh . --plan-file plan.md

${BOLD}CONFIG FILE:${NC}
  Place a ralph.toml in your project root to set defaults. CLI args override config values.
  Run 'ralph.sh --init-config' to generate a starter config with all available settings.

${BOLD}HOW IT WORKS:${NC}
  1. Planning: Claude reads the repo and creates .ralph/plan.md with atomic tasks
  2. Execution: Each task runs in a fresh Claude context (~200k tokens)
  3. Completion: Claude echoes a signal token when each task is done
  4. Repeat: Loop continues until all tasks complete or iteration cap is hit

${BOLD}SUBCOMMANDS:${NC}
  ralph stop [directory]       Halt after the current iteration
  ralph feedback [message]     Show queued feedback, or queue a new message
EOF
}

# --- Subcommands (before flag parsing) ---
if [[ "${1:-}" == "stop" ]]; then
  shift
  local_dir="."
  if [[ -n "${1:-}" && "${1:0:1}" != "-" && -d "$1" ]]; then
    local_dir="$1"
    shift
  fi
  ralph_dir="$local_dir/.ralph"
  if [[ ! -d "$ralph_dir" ]]; then
    echo -e "${RED}[ralph]${NC} No .ralph directory found. Is ralph running here?"
    exit 1
  fi
  touch "$ralph_dir/stop"
  echo -e "${YELLOW}[ralph]${NC} Stop requested — ralph will halt after the current iteration."
  echo -e "${YELLOW}[ralph]${NC} Ctrl+C to kill immediately if you don't need iteration results."
  exit 0
fi

if [[ "${1:-}" == "feedback" ]]; then
  shift
  local_dir="."
  if [[ -n "${1:-}" && "${1:0:1}" != "-" && -d "$1" ]]; then
    local_dir="$1"
    shift
  fi
  ralph_dir="$local_dir/.ralph"
  if [[ ! -d "$ralph_dir" ]]; then
    echo -e "${RED}[ralph]${NC} No .ralph directory found. Is ralph running here?"
    exit 1
  fi
  if [[ -z "$*" ]]; then
    feedback_file="$ralph_dir/feedback"
    if [[ -f "$feedback_file" && -s "$feedback_file" ]]; then
      echo -e "${CYAN}[ralph]${NC} Queued feedback:"
      cat "$feedback_file"
    else
      echo -e "${CYAN}[ralph]${NC} No feedback queued."
    fi
    exit 0
  fi
  echo "$*" >> "$ralph_dir/feedback"
  echo -e "${GREEN}[ralph]${NC} Feedback queued for next iteration."
  exit 0
fi

# --- Generate starter config ---
if [[ "${1:-}" == "--init-config" ]]; then
  shift
  local_dir="${1:-.}"
  config_path="$local_dir/ralph.toml"
  if [[ -f "$config_path" ]]; then
    echo -e "${YELLOW}[ralph]${NC} Config already exists: $config_path"
    exit 1
  fi
  cat > "$config_path" <<'TOML'
# Ralph Loop configuration
# CLI args override these values. Remove or comment out lines to use defaults.

max_iterations = 50
calls_per_hour = 80
watcher_interval = 2

stuck_threshold = 5
stuck_confirmation_threshold = 2
stagnation_threshold = 3
test_saturation_threshold = 3
permission_denial_threshold = 3
TOML
  echo -e "${GREEN}[ralph]${NC} Config written to $config_path"
  exit 0
fi

# --- Save original args (for tmux re-exec) ---
RALPH_ORIG_ARGS=("$@")

# --- Parse args ---
while [[ $# -gt 0 ]]; do
  case "$1" in
    -d|--dir)       PROJECT_DIR="$2"; shift 2 ;;
    -n|--max)       MAX_ITERATIONS="$2"; _CLI_MAX_ITERATIONS="$2"; shift 2 ;;
    -p|--prompt)    PROMPT_OVERRIDE="$2"; shift 2 ;;
    --plan-file)    PLAN_FILE_ARG="$2"; shift 2 ;;
    --plan)         PLAN_ONLY=true; shift ;;
    --skip-planning) SKIP_PLANNING=true; shift ;;
    -q|--quiet)     QUIET=true; shift ;;
    --no-worktree)  USE_WORKTREE=false; shift ;;
    --calls-per-hour) CALLS_PER_HOUR="$2"; _CLI_CALLS_PER_HOUR="$2"; shift 2 ;;
    --refactor-threshold) REFACTOR_THRESHOLD="$2"; shift 2 ;;
    --max-task-attempts) MAX_TASK_ATTEMPTS="$2"; shift 2 ;;
    --tmux)         USE_TMUX=true; shift ;;
    --auto-merge)   AUTO_MERGE=true; shift ;;
    -h|--help)      usage; exit 0 ;;
    -*)             log_error "Unknown option: $1"; usage; exit 1 ;;
    *)              PROJECT_DIR="$1"; shift ;;
  esac
done

# --- Detect task backend ---
if command -v bd &>/dev/null; then
  TASK_BACKEND="bd"
else
  TASK_BACKEND="checklist"
fi
# NOTE: bd health is verified later in bd_init() which falls back to checklist
# if the Dolt server is unreachable or misconfigured.

# --- Resolve paths ---
PROJECT_DIR="$(cd "$PROJECT_DIR" && pwd)"
RALPH_DIR="$PROJECT_DIR/.ralph"

# --- Load config file (CLI args take precedence) ---
load_config "$PROJECT_DIR/ralph.toml"

# --- Apply env var overrides (between config and CLI in precedence) ---
if [[ -z "$_CLI_MAX_ITERATIONS" && -n "${RALPH_MAX_ITERATIONS:-}" ]]; then
  MAX_ITERATIONS="$RALPH_MAX_ITERATIONS"
fi
PLAN_FILE="$RALPH_DIR/plan.md"
if [[ -n "$PLAN_FILE_ARG" ]]; then
  # Resolve plan-file to absolute path
  if [[ "$PLAN_FILE_ARG" != /* ]]; then
    PLAN_FILE_ARG="$(cd "$(dirname "$PROJECT_DIR/$PLAN_FILE_ARG")" && pwd)/$(basename "$PLAN_FILE_ARG")"
  fi
  if [[ ! -f "$PLAN_FILE_ARG" ]]; then
    log_error "Plan file not found: $PLAN_FILE_ARG"
    exit 1
  fi
  if ! grep -qE '^\s*- \[ \]' "$PLAN_FILE_ARG"; then
    log_error "Plan file is not in Ralph format (must contain '- [ ]' checkboxes): $PLAN_FILE_ARG"
    exit 1
  fi
  if [[ "$TASK_BACKEND" == "bd" ]]; then
    log "Note: --plan-file forces checklist backend (bd available but not used)"
  fi
  TASK_BACKEND="checklist"
fi
STATE_FILE="$RALPH_DIR/state.json"
STOP_FILE="$RALPH_DIR/stop"
LOG_FILE="$RALPH_DIR/loop.log"
RAW_LOG="$RALPH_DIR/raw.log"
RESUME_SCRIPT="$RALPH_DIR/resume.sh"

# Signal file paths (must be set after RALPH_DIR is resolved)
SIGNAL_COMPLETE_FILE="$RALPH_DIR/.signal_complete"
SIGNAL_TASK_FILE="$RALPH_DIR/.signal_current_task"
SIGNAL_ALL_COMPLETE_FILE="$RALPH_DIR/.signal_all_complete"

# --- Init .ralph directory ---
init_ralph_dir() {
  mkdir -p "$RALPH_DIR"
  mkdir -p "$RALPH_DIR/reflections"
  touch "$LOG_FILE" "$RAW_LOG"

  # Bail if there are staged or unstaged changes — we commit .gitignore below
  # and must not sweep unrelated work into that commit.
  # Skip this check on resume: the worktree isolates ralph's work from the
  # main tree, so local changes here shouldn't block continuing.
  if [[ ! -f "$STATE_FILE" ]] && git -C "$PROJECT_DIR" rev-parse --git-dir &>/dev/null; then
    if ! git -C "$PROJECT_DIR" diff --quiet 2>/dev/null || ! git -C "$PROJECT_DIR" diff --cached --quiet 2>/dev/null; then
      log_error "uncommitted changes in $PROJECT_DIR — please commit or stash before running ralph."
      exit 1
    fi
  fi

  # Ensure .ralph and .beads are gitignored and committed
  local gitignore="$PROJECT_DIR/.gitignore"
  local needs_commit=false
  if [[ ! -f "$gitignore" ]] || ! grep -qE '^\.ralph(/\*?)?$' "$gitignore"; then
    echo '.ralph' >> "$gitignore"
    needs_commit=true
  fi
  if [[ ! -f "$gitignore" ]] || ! grep -qE '^\.beads(/\*?)?$' "$gitignore"; then
    echo '.beads' >> "$gitignore"
    needs_commit=true
  fi
  if [[ "$needs_commit" == true ]] && git -C "$PROJECT_DIR" rev-parse --git-dir &>/dev/null; then
    git -C "$PROJECT_DIR" add .gitignore 2>/dev/null || true
    git -C "$PROJECT_DIR" commit -m "Add .ralph and .beads to .gitignore" 2>/dev/null || true
  fi

  if [[ -f "$STATE_FILE" ]]; then
    local status
    status=$(read_state "status")
    if [[ "$status" == "completed" ]]; then
      log_task "All tasks completed from previous run."
      printf "${YELLOW}[ralph]${NC} Run fresh? (y/n) "
      read -r answer
      if [[ "$answer" == "y" || "$answer" == "Y" ]]; then
        # Only remove .ralph state — .beads and .dolt are permanent and must never be deleted.
        rm -rf "$RALPH_DIR"
        mkdir -p "$RALPH_DIR"
        touch "$LOG_FILE" "$RAW_LOG"
      else
        exit 0
      fi
    else
      RESUME=true
      log "Resuming from previous state (status: $status)"
    fi
  fi

  # Clear stale signal files from previous run
  clear_signal

  # Check for leftover stop file before starting/resuming
  if [[ -f "$STOP_FILE" ]]; then
    printf "${YELLOW}[ralph]${NC} Stop file found from a previous run. Delete it to continue? (y/n) "
    read -r answer
    if [[ "$answer" == "y" || "$answer" == "Y" ]]; then
      rm -f "$STOP_FILE"
    else
      log_warn "Stop file still present — exiting"
      exit 1
    fi
  fi

  if [[ ! -f "$STATE_FILE" ]]; then
    cat > "$STATE_FILE" <<'STATE'
{
  "iteration": 0,
  "max_iterations": null,
  "refactor_threshold": null,
  "quality_score": 0,
  "status": "initialized",
  "started_at": null,
  "last_task": null,
  "worktree_dir": null,
  "worktree_branch": null,
  "task_seq": 0
}
STATE
  fi
}

# --- Stream filter helper ---
write_stream_filter() {
  cat > "$RALPH_DIR/.stream-filter.sh" <<'STREAM'
#!/usr/bin/env bash
set +m
# stream-json: each event has 1 content block. Filter and format.
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
' | perl -e '
  use POSIX; $|=1;
  my ($prev, $count, $prev_ts);
  sub flush_prev {
    return unless defined $prev;
    if ($count > 1) {
      print "$prev_ts $prev x$count\n";
    } else {
      print "$prev_ts $prev\n";
    }
  }
  while(<STDIN>) {
    chomp;
    next if $_ eq "";
    my $ts = strftime("%H:%M:%S", localtime());
    if (defined $prev && $_ eq $prev) {
      $count++;
      $prev_ts = $ts;
    } else {
      flush_prev();
      $prev = $_;
      $count = 1;
      $prev_ts = $ts;
    }
  }
  flush_prev();
' | sed -u -E \
  -e $'s/\\[done\\]/\033[0;32m[done]\033[0m/g' \
  -e $'s/\\[claude\\]/\033[0;36m[claude]\033[0m/g' \
  -e $'s/\\[([A-Z][A-Za-z]*)\\]/\033[0;34m[\\1]\033[0m/g'
STREAM
  chmod +x "$RALPH_DIR/.stream-filter.sh"
}

# --- Tmux mode ---
setup_tmux() {
  if ! command -v tmux &>/dev/null; then
    log_error "tmux not found, falling back to inline mode"
    USE_TMUX=false
    return
  fi

  TMUX_SESSION="ralph-sh-$$"

  write_stream_filter

  # Write plan watcher script — waits for signal file, then clears and re-renders
  cat > "$RALPH_DIR/.plan-watch.sh" <<PLAN_SCRIPT
#!/usr/bin/env bash
BOLD=$'\033[1m'
DIM=$'\033[2m'
CYAN=$'\033[0;36m'
GREEN=$'\033[0;32m'
YELLOW=$'\033[0;33m'
NC=$'\033[0m'

progress_bar() {
  local closed=\$1 total=\$2 width=\${3:-30}
  if (( total == 0 )); then return; fi
  local filled=\$(( closed * width / total ))
  local empty=\$(( width - filled ))
  local pct=\$(( closed * 100 / total ))
  printf "\${GREEN}"
  printf '█%.0s' \$(seq 1 \$filled 2>/dev/null) || true
  printf "\${NC}"
  printf '░%.0s' \$(seq 1 \$empty 2>/dev/null) || true
  printf " %d%%" "\$pct"
}

while true; do
  if [[ -f '$RALPH_DIR/.plan-refresh' ]]; then
    rm -f '$RALPH_DIR/.plan-refresh'
    printf '\033[2J\033[H'
    if [[ '$TASK_BACKEND' == 'bd' ]]; then
      current_json=\$(bd list --status in_progress --flat --json --limit 1 2>/dev/null)
      current_title=\$(echo "\$current_json" | jq -r '.[0].title // empty' 2>/dev/null)
      current_id=\$(echo "\$current_json" | jq -r '.[0].id // empty' 2>/dev/null)
      if [[ -n "\$current_title" ]]; then
        printf "\${BOLD}\${CYAN}▶ %s\${NC} (%s)\n" "\$current_title" "\$current_id"
        printf "\n"
      fi
      ready_list=\$(bd ready --limit 8 2>/dev/null || true)
      if [[ -n "\$ready_list" && "\$ready_list" != *"No ready work"* ]]; then
        if [[ -n "\$current_id" ]]; then
          ready_list=\$(echo "\$ready_list" | grep -v "\$current_id" || true)
        fi
        if [[ -n "\$ready_list" ]]; then
          printf "\${BOLD}Ready:\${NC}\n%s\n\n" "\$ready_list"
        fi
      fi
      completed_list=\$(bd list --status closed --flat --limit 8 2>/dev/null || true)
      if [[ -n "\$completed_list" ]]; then
        printf "\${DIM}Done:\n%s\${NC}\n\n" "\$completed_list"
      fi
      if [[ -n "\$current_id" ]]; then
        unblocks=\$(bd show "\$current_id" --json 2>/dev/null | jq -r '.[0].dependents[]? | "  → \(.id): \(.title)"' 2>/dev/null || true)
        if [[ -n "\$unblocks" ]]; then
          printf "\${BOLD}Unblocks:\${NC}\n%s\n\n" "\$unblocks"
        fi
      fi
      closed=\$(bd count --status closed 2>/dev/null || echo 0)
      total=\$(bd count 2>/dev/null || echo 0)
      progress_bar "\$closed" "\$total"
      printf "\n"
      calls=\$(cat '$RALPH_DIR/.call_count' 2>/dev/null || echo 0)
      printf "\${DIM}calls this hour: %s/$CALLS_PER_HOUR\${NC}\n" "\$calls"
    else
      cat '$PLAN_FILE' 2>/dev/null
    fi
  fi
  sleep 1
done
PLAN_SCRIPT
  chmod +x "$RALPH_DIR/.plan-watch.sh"

  # Build ralph re-exec command
  local cmd
  cmd="$(printf '%q' "$SCRIPT_DIR/ralph.sh")"
  for arg in "${RALPH_ORIG_ARGS[@]+"${RALPH_ORIG_ARGS[@]}"}"; do
    [[ "$arg" == "--tmux" ]] && continue
    cmd+=" $(printf '%q' "$arg")"
  done
  cmd+=" --quiet"

  # Create tmux session with panes running commands directly (no send-keys)
  tmux new-session -d -s "$TMUX_SESSION" -c "$PROJECT_DIR" \
    "export _RALPH_TMUX_SESSION=$TMUX_SESSION; $cmd"
  tmux split-window -h -t "$TMUX_SESSION" \
    "bash '$RALPH_DIR/.stream-filter.sh' '$RAW_LOG'"
  tmux split-window -v -t "$TMUX_SESSION:.1" \
    "bash '$RALPH_DIR/.plan-watch.sh'"

  # Pane titles and keep-alive (panes stay visible after process exits)
  tmux select-pane -t "$TMUX_SESSION:.0" -T "(sh) ralph"
  tmux select-pane -t "$TMUX_SESSION:.1" -T "stream"
  tmux select-pane -t "$TMUX_SESSION:.2" -T "plan"
  tmux set-option -t "$TMUX_SESSION" pane-border-status top
  tmux set-option -t "$TMUX_SESSION" pane-border-format \
    "#{?pane_dead, #{pane_title} (dead) — press q to exit , #{pane_title} }"
  tmux set-option -t "$TMUX_SESSION" remain-on-exit on

  # Bind q to kill session when the main (ralph) pane is dead
  tmux bind-key -T root q if-shell \
    "tmux display-message -t '$TMUX_SESSION:.0' -p '#{pane_dead}' | grep -q 1" \
    "kill-session -t '$TMUX_SESSION'"

  tmux select-pane -t "$TMUX_SESSION:.0"

  # Background timer updates pane titles every second
  date +%s > "$RALPH_DIR/.stream-start"
  date +%s > "$RALPH_DIR/.run-start"
  ( while tmux has-session -t "$TMUX_SESSION" 2>/dev/null; do
      _now=$(date +%s)
      # Stream pane: task + per-task elapsed
      _st=$(cat "$RALPH_DIR/.stream-start" 2>/dev/null || echo 0)
      _el=$(( _now - _st ))
      _task=$(cat "$RALPH_DIR/.stream-task" 2>/dev/null || true)
      if [[ -n "$_task" ]]; then
        tmux select-pane -t "$TMUX_SESSION:.1" -T "$_task $(printf '%dm%02ds' $((_el/60)) $((_el%60)))" 2>/dev/null
      else
        tmux select-pane -t "$TMUX_SESSION:.1" -T "stream $(printf '%dm%02ds' $((_el/60)) $((_el%60)))" 2>/dev/null
      fi
      # Ralph pane: branch + per-run elapsed
      _rs=$(cat "$RALPH_DIR/.run-start" 2>/dev/null || echo "$_now")
      _re=$(( _now - _rs ))
      _branch=$(cat "$RALPH_DIR/.run-branch" 2>/dev/null || echo "ralph")
      tmux select-pane -t "$TMUX_SESSION:.0" -T "$_branch $(printf '%dm%02ds' $((_re/60)) $((_re%60)))" 2>/dev/null
      sleep 1
    done ) &

  # Trigger initial plan render so the pane isn't blank on startup
  touch "$RALPH_DIR/.plan-refresh"

  _TMUX_OUTER=true
  tmux attach-session -t "$TMUX_SESSION"
  exit 0
}

# --- State helpers ---
read_state() {
  local val
  if command -v jq &>/dev/null; then
    val=$(jq -r ".$1 // empty" "$STATE_FILE")
  else
    local key="$1"
    val=$(grep "\"$key\"" "$STATE_FILE" | sed 's/.*: *"\?\([^",}]*\)"\?.*/\1/')
  fi
  printf '%s' "$val"
}

write_state() {
  local key="$1" value="$2"
  if command -v jq &>/dev/null; then
    local tmp
    tmp=$(mktemp)
    jq --arg v "$value" ".$key = (\$v | try tonumber catch \$v)" "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"
  else
    local escaped_value
    escaped_value=$(printf '%s' "$value" | sed 's/[&/\]/\\&/g')
    if echo "$value" | grep -qE '^[0-9]+$'; then
      sed -i "s/\"$key\": *[^,}]*/\"$key\": $escaped_value/" "$STATE_FILE"
    else
      sed -i "s/\"$key\": *[^,}]*/\"$key\": \"$escaped_value\"/" "$STATE_FILE"
    fi
  fi
}

# --- Tmux flash helper ---
flash_plan_pane() {
  local sess="${_RALPH_TMUX_SESSION:-}"
  [[ -z "$sess" ]] && return
  tmux set-option -t "$sess" pane-border-style "fg=green" 2>/dev/null
  tmux set-option -t "$sess" pane-active-border-style "fg=green" 2>/dev/null
  ( sleep 2
    tmux set-option -t "$sess" pane-border-style default 2>/dev/null
    tmux set-option -t "$sess" pane-active-border-style default 2>/dev/null
  ) &
}

# --- Signal detection (file-based) ---
# Claude writes signal files instead of echoing tokens to stdout.
# This avoids false positives when Claude reads source files containing tokens.

clear_signal() {
  rm -f "$SIGNAL_COMPLETE_FILE" "$SIGNAL_TASK_FILE" "$SIGNAL_ALL_COMPLETE_FILE"
}

read_feedback() {
  local feedback_file="$RALPH_DIR/feedback"
  if [[ -f "$feedback_file" && -s "$feedback_file" ]]; then
    cat "$feedback_file"
  fi
}

clear_feedback() {
  rm -f "$RALPH_DIR/feedback"
}

check_signal() {
  [[ -f "$SIGNAL_COMPLETE_FILE" ]]
}

check_all_complete() {
  [[ -f "$SIGNAL_ALL_COMPLETE_FILE" ]]
}

check_current_task() {
  [[ -f "$SIGNAL_TASK_FILE" ]]
}

read_current_task() {
  [[ -f "$SIGNAL_TASK_FILE" ]] && head -1 "$SIGNAL_TASK_FILE" 2>/dev/null || true
}

read_signal_summary() {
  local raw=""
  if [[ -f "$SIGNAL_ALL_COMPLETE_FILE" ]]; then
    raw=$(head -1 "$SIGNAL_ALL_COMPLETE_FILE" 2>/dev/null) || true
  elif [[ -f "$SIGNAL_COMPLETE_FILE" ]]; then
    raw=$(head -1 "$SIGNAL_COMPLETE_FILE" 2>/dev/null) || true
  fi
  # Strip any JSON fragments that may have bled into the signal file.
  # Signal summaries are plain text — anything starting with { is garbage.
  if [[ -n "$raw" && "${raw:0:1}" != "{" ]]; then
    # Trim trailing JSON fragment (e.g. 'summary text{"type":...}')
    echo "${raw%%\{*}"
  fi
}

# --- Attempt history ---

_attempts_dir() { echo "$RALPH_DIR/attempts"; }

_attempt_key() {
  local task_id="$1" task_name="$2"
  if [[ -n "$task_id" ]]; then
    echo "$task_id"
  else
    slugify "$task_name"
  fi
}

record_attempt() {
  local task_id="$1" task_name="$2" summary="$3" diff_stat="$4" analysis="$5"
  local key
  key=$(_attempt_key "$task_id" "$task_name")
  [[ -z "$key" ]] && return 0

  local dir
  dir=$(_attempts_dir)
  mkdir -p "$dir"

  local attempt_file="$dir/${key}.log"
  local attempt_num=1
  if [[ -f "$attempt_file" ]]; then
    local prev
    prev=$(grep -c '^### Attempt ' "$attempt_file" 2>/dev/null || echo 0)
    attempt_num=$((prev + 1))
  fi

  {
    echo "### Attempt $attempt_num"
    echo "Task: $task_name"
    if [[ -n "$summary" ]]; then
      echo "Summary: $summary"
    fi
    if [[ -n "$diff_stat" ]]; then
      echo "Changes:"
      echo "$diff_stat"
    else
      echo "Changes: none"
    fi
    echo "Analysis: $analysis"
    echo ""
  } >> "$attempt_file"
}

read_attempt_history() {
  local task_id="$1" task_name="$2"
  local key
  key=$(_attempt_key "$task_id" "$task_name")
  [[ -z "$key" ]] && return 0

  local attempt_file
  attempt_file="$(_attempts_dir)/${key}.log"
  if [[ -f "$attempt_file" ]]; then
    cat "$attempt_file"
  fi
}

clear_attempt_history() {
  local task_id="$1" task_name="$2"
  local key
  key=$(_attempt_key "$task_id" "$task_name")
  [[ -z "$key" ]] && return 0

  local attempt_file
  attempt_file="$(_attempts_dir)/${key}.log"
  rm -f "$attempt_file"
}

# --- Attempt history ---

_attempts_dir() { echo "$RALPH_DIR/attempts"; }

_attempt_key() {
  local task_id="$1" task_name="$2"
  if [[ -n "$task_id" ]]; then
    echo "$task_id"
  else
    slugify "$task_name"
  fi
}

record_attempt() {
  local task_id="$1" task_name="$2" summary="$3" diff_stat="$4" analysis="$5"
  local key
  key=$(_attempt_key "$task_id" "$task_name")
  [[ -z "$key" ]] && return 0

  local dir
  dir=$(_attempts_dir)
  mkdir -p "$dir"

  local attempt_file="$dir/${key}.log"
  local attempt_num=1
  if [[ -f "$attempt_file" ]]; then
    local prev
    prev=$(grep -c '^### Attempt ' "$attempt_file" 2>/dev/null || echo 0)
    attempt_num=$((prev + 1))
  fi

  {
    echo "### Attempt $attempt_num"
    echo "Task: $task_name"
    if [[ -n "$summary" ]]; then
      echo "Summary: $summary"
    fi
    if [[ -n "$diff_stat" ]]; then
      echo "Changes:"
      echo "$diff_stat"
    else
      echo "Changes: none"
    fi
    echo "Analysis: $analysis"
    echo ""
  } >> "$attempt_file"
}

read_attempt_history() {
  local task_id="$1" task_name="$2"
  local key
  key=$(_attempt_key "$task_id" "$task_name")
  [[ -z "$key" ]] && return 0

  local attempt_file
  attempt_file="$(_attempts_dir)/${key}.log"
  if [[ -f "$attempt_file" ]]; then
    cat "$attempt_file"
  fi
}

clear_attempt_history() {
  local task_id="$1" task_name="$2"
  local key
  key=$(_attempt_key "$task_id" "$task_name")
  [[ -z "$key" ]] && return 0

  local attempt_file
  attempt_file="$(_attempts_dir)/${key}.log"
  rm -f "$attempt_file"
}

# --- Attempt history ---

_attempts_dir() { echo "$RALPH_DIR/attempts"; }

_attempt_key() {
  local task_id="$1" task_name="$2"
  if [[ -n "$task_id" ]]; then
    echo "$task_id"
  else
    slugify "$task_name"
  fi
}

record_attempt() {
  local task_id="$1" task_name="$2" summary="$3" diff_stat="$4" analysis="$5"
  local key
  key=$(_attempt_key "$task_id" "$task_name")
  [[ -z "$key" ]] && return 0

  local dir
  dir=$(_attempts_dir)
  mkdir -p "$dir"

  local attempt_file="$dir/${key}.log"
  local attempt_num=1
  if [[ -f "$attempt_file" ]]; then
    local prev
    prev=$(grep -c '^### Attempt ' "$attempt_file" 2>/dev/null || echo 0)
    attempt_num=$((prev + 1))
  fi

  {
    echo "### Attempt $attempt_num"
    echo "Task: $task_name"
    if [[ -n "$summary" ]]; then
      echo "Summary: $summary"
    fi
    if [[ -n "$diff_stat" ]]; then
      echo "Changes:"
      echo "$diff_stat"
    else
      echo "Changes: none"
    fi
    echo "Analysis: $analysis"
    echo ""
  } >> "$attempt_file"
}

read_attempt_history() {
  local task_id="$1" task_name="$2"
  local key
  key=$(_attempt_key "$task_id" "$task_name")
  [[ -z "$key" ]] && return 0

  local attempt_file
  attempt_file="$(_attempts_dir)/${key}.log"
  if [[ -f "$attempt_file" ]]; then
    cat "$attempt_file"
  fi
}

clear_attempt_history() {
  local task_id="$1" task_name="$2"
  local key
  key=$(_attempt_key "$task_id" "$task_name")
  [[ -z "$key" ]] && return 0

  local attempt_file
  attempt_file="$(_attempts_dir)/${key}.log"
  rm -f "$attempt_file"
}

# --- Error fingerprinting ---

_error_hashes_dir() { echo "$RALPH_DIR/error_hashes"; }

extract_errors() {
  local text="$1"
  grep -iE '^(Error|Failed|Exception|panic|FATAL|TypeError|SyntaxError|ReferenceError|RuntimeError|ImportError|ValueError):' <<< "$text" || true
  grep -iE 'exited with (code|status) [1-9]' <<< "$text" || true
  grep -iE 'non-zero exit code' <<< "$text" || true
  grep -iE 'command failed|build failed|compilation failed|test failed' <<< "$text" || true
}

normalize_error() {
  local line="$1"
  line=$(sed -E 's/[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}[^ ]*/TIMESTAMP/g' <<< "$line")
  line=$(sed -E 's/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/UUID/g' <<< "$line")
  line=$(sed -E 's/\bline [0-9]+/line N/g' <<< "$line")
  line=$(sed -E 's/:[0-9]+:[0-9]+/:N:N/g' <<< "$line")
  line=$(sed -E 's|/tmp/[^ ]*|/tmp/TMPPATH|g' <<< "$line")
  line=$(sed -E 's|/var/folders/[^ ]*|/tmp/TMPPATH|g' <<< "$line")
  line=$(echo "$line" | tr -s ' ')
  echo "$line"
}

fingerprint_error() {
  local normalized="$1"
  echo -n "$normalized" | md5sum 2>/dev/null | cut -d' ' -f1 || echo -n "$normalized" | md5 2>/dev/null | tr -d ' '
}

record_error_hash() {
  local task_key="$1" hash="$2"
  [[ -z "$task_key" || -z "$hash" ]] && return 0

  local dir
  dir=$(_error_hashes_dir)
  mkdir -p "$dir"

  local hash_file="$dir/${task_key}.hashes"
  echo "$hash" >> "$hash_file"
}

count_error_hash() {
  local task_key="$1" hash="$2"
  [[ -z "$task_key" || -z "$hash" ]] && { echo 0; return; }

  local hash_file
  hash_file="$(_error_hashes_dir)/${task_key}.hashes"
  if [[ -f "$hash_file" ]]; then
    grep -cF "$hash" "$hash_file" 2>/dev/null || echo 0
  else
    echo 0
  fi
}

clear_error_hashes() {
  local task_key="$1"
  [[ -z "$task_key" ]] && return 0
  rm -f "$(_error_hashes_dir)/${task_key}.hashes"
}

check_repeated_errors() {
  local text="$1" task_key="$2"
  [[ -z "$task_key" ]] && return 1

  local errors
  errors=$(extract_errors "$text")
  [[ -z "$errors" ]] && return 1

  local dominated=false
  while IFS= read -r err_line; do
    [[ -z "$err_line" ]] && continue
    local normalized
    normalized=$(normalize_error "$err_line")
    local hash
    hash=$(fingerprint_error "$normalized")
    record_error_hash "$task_key" "$hash"
    local count
    count=$(count_error_hash "$task_key" "$hash")
    if (( count >= 3 )); then
      dominated=true
    fi
  done <<< "$errors"

  if [[ "$dominated" == true ]]; then
    return 0
  fi
  return 1
}

# --- Error fingerprinting ---

_error_hashes_dir() { echo "$RALPH_DIR/error_hashes"; }

extract_errors() {
  local text="$1"
  grep -iE \
    -e '^(Error|Failed|Exception|panic|FATAL|TypeError|SyntaxError|ReferenceError|RuntimeError|ImportError|ValueError):' \
    -e 'exited with (code|status) [1-9]' \
    -e 'non-zero exit code' \
    -e 'command failed|build failed|compilation failed|test failed' \
    <<< "$text" || true
}

normalize_error() {
  echo "$1" | sed -E \
    -e 's/[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}[^ ]*/TIMESTAMP/g' \
    -e 's/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/UUID/g' \
    -e 's/\bline [0-9]+/line N/g' \
    -e 's/:[0-9]+:[0-9]+/:N:N/g' \
    -e 's|/tmp/[^ ]*|/tmp/TMPPATH|g' \
    -e 's|/var/folders/[^ ]*|/tmp/TMPPATH|g' | \
    tr -s ' '
}

fingerprint_error() {
  local normalized="$1"
  echo -n "$normalized" | md5sum 2>/dev/null | cut -d' ' -f1 || echo -n "$normalized" | md5 2>/dev/null | tr -d ' '
}

record_error_hash() {
  local task_key="$1" hash="$2"
  [[ -z "$task_key" || -z "$hash" ]] && return 0

  local dir
  dir=$(_error_hashes_dir)
  mkdir -p "$dir"

  local hash_file="$dir/${task_key}.hashes"
  echo "$hash" >> "$hash_file"
}

count_error_hash() {
  local task_key="$1" hash="$2"
  [[ -z "$task_key" || -z "$hash" ]] && { echo 0; return; }

  local hash_file
  hash_file="$(_error_hashes_dir)/${task_key}.hashes"
  if [[ -f "$hash_file" ]]; then
    grep -cF "$hash" "$hash_file" 2>/dev/null || echo 0
  else
    echo 0
  fi
}

clear_error_hashes() {
  local task_key="$1"
  [[ -z "$task_key" ]] && return 0
  rm -f "$(_error_hashes_dir)/${task_key}.hashes"
}

check_repeated_errors() {
  local text="$1" task_key="$2"
  [[ -z "$task_key" ]] && return 1

  local errors
  errors=$(extract_errors "$text")
  [[ -z "$errors" ]] && return 1

  local dominated=false
  while IFS= read -r err_line; do
    [[ -z "$err_line" ]] && continue
    local normalized
    normalized=$(normalize_error "$err_line")
    local hash
    hash=$(fingerprint_error "$normalized")
    record_error_hash "$task_key" "$hash"
    local count
    count=$(count_error_hash "$task_key" "$hash")
    if (( count >= 3 )); then
      dominated=true
    fi
  done <<< "$errors"

  if [[ "$dominated" == true ]]; then
    return 0
  fi
  return 1
}

# --- Attempt history ---

_attempts_dir() { echo "$RALPH_DIR/attempts"; }

_attempt_key() {
  local task_id="$1" task_name="$2"
  if [[ -n "$task_id" ]]; then
    echo "$task_id"
  else
    slugify "$task_name"
  fi
}

record_attempt() {
  local task_id="$1" task_name="$2" summary="$3" diff_stat="$4" analysis="$5"
  local key
  key=$(_attempt_key "$task_id" "$task_name")
  [[ -z "$key" ]] && return 0

  local dir
  dir=$(_attempts_dir)
  mkdir -p "$dir"

  local attempt_file="$dir/${key}.log"
  local attempt_num=1
  if [[ -f "$attempt_file" ]]; then
    local prev
    prev=$(grep -c '^### Attempt ' "$attempt_file" 2>/dev/null || echo 0)
    attempt_num=$((prev + 1))
  fi

  {
    echo "### Attempt $attempt_num"
    echo "Task: $task_name"
    if [[ -n "$summary" ]]; then
      echo "Summary: $summary"
    fi
    if [[ -n "$diff_stat" ]]; then
      echo "Changes:"
      echo "$diff_stat"
    else
      echo "Changes: none"
    fi
    echo "Analysis: $analysis"
    echo ""
  } >> "$attempt_file"
}

read_attempt_history() {
  local task_id="$1" task_name="$2"
  local key
  key=$(_attempt_key "$task_id" "$task_name")
  [[ -z "$key" ]] && return 0

  local attempt_file
  attempt_file="$(_attempts_dir)/${key}.log"
  if [[ -f "$attempt_file" ]]; then
    cat "$attempt_file"
  fi
}

clear_attempt_history() {
  local task_id="$1" task_name="$2"
  local key
  key=$(_attempt_key "$task_id" "$task_name")
  [[ -z "$key" ]] && return 0

  local attempt_file
  attempt_file="$(_attempts_dir)/${key}.log"
  rm -f "$attempt_file"
}

# --- Worktree theme renaming ---
_rename_worktree_from_theme() {
  local theme
  theme=$(read_state "theme")

  # Fallback: derive theme from plan file heading if Claude didn't write one
  if [[ -z "$theme" && -f "$PLAN_FILE" ]]; then
    theme=$(head -1 "$PLAN_FILE" | sed 's/^#* *//')
  fi

  # Fallback: derive theme from first bd task title
  if [[ -z "$theme" && "$TASK_BACKEND" == "bd" ]]; then
    theme=$(run_bd list --status=open --flat --json --limit 1 2>/dev/null | jq -r '.[0].title // empty')
  fi

  if [[ -n "$theme" ]]; then
    rename_worktree_for_theme "$theme"
  fi
}

# --- Rate limiting ---
init_call_tracking() {
  local call_count_file="$RALPH_DIR/.call_count"
  local call_hour_file="$RALPH_DIR/.call_hour"
  local current_hour
  current_hour=$(date +%Y%m%d%H)

  if [[ ! -f "$call_hour_file" ]] || [[ "$(cat "$call_hour_file")" != "$current_hour" ]]; then
    echo "0" > "$call_count_file"
    echo "$current_hour" > "$call_hour_file"
  fi
}

check_rate_limit() {
  local call_count_file="$RALPH_DIR/.call_count"
  local call_hour_file="$RALPH_DIR/.call_hour"
  local current_hour
  current_hour=$(date +%Y%m%d%H)

  if [[ "$(cat "$call_hour_file")" != "$current_hour" ]]; then
    echo "0" > "$call_count_file"
    echo "$current_hour" > "$call_hour_file"
    return 0
  fi

  local count
  count=$(cat "$call_count_file")
  if (( count >= CALLS_PER_HOUR )); then
    return 1
  fi
  return 0
}

increment_call_count() {
  local call_count_file="$RALPH_DIR/.call_count"
  local count
  count=$(cat "$call_count_file")
  echo "$((count + 1))" > "$call_count_file"
}

wait_for_rate_reset() {
  local call_hour_file="$RALPH_DIR/.call_hour"
  local call_count_file="$RALPH_DIR/.call_count"
  local stored_hour current_hour seconds_left

  stored_hour=$(cat "$call_hour_file")
  current_hour=$(date +%Y%m%d%H)

  if [[ "$stored_hour" != "$current_hour" ]]; then
    echo "0" > "$call_count_file"
    echo "$current_hour" > "$call_hour_file"
    return 0
  fi

  local current_min current_sec
  current_min=$(date +%M)
  current_sec=$(date +%S)
  seconds_left=$(( (60 - ${current_min#0}) * 60 - ${current_sec#0} ))

  log_warn "Rate limit reached ($CALLS_PER_HOUR calls/hour). Waiting ${seconds_left}s for next hour..."

  while (( seconds_left > 0 )); do
    if [[ -f "$STOP_FILE" ]]; then
      log_warn "Stop file detected during rate limit wait"
      rm -f "$STOP_FILE"
      return 1
    fi
    local display_min=$(( seconds_left / 60 ))
    local display_sec=$(( seconds_left % 60 ))
    printf "\r${YELLOW}[ralph]${NC} Rate limit reset in %02d:%02d " "$display_min" "$display_sec"
    sleep 10
    current_hour=$(date +%Y%m%d%H)
    if [[ "$stored_hour" != "$current_hour" ]]; then
      break
    fi
    current_min=$(date +%M)
    current_sec=$(date +%S)
    seconds_left=$(( (60 - ${current_min#0}) * 60 - ${current_sec#0} ))
  done

  printf "\n"
  echo "0" > "$call_count_file"
  echo "$(date +%Y%m%d%H)" > "$call_hour_file"
  log "Rate limit reset, resuming"
  return 0
}

# --- Run Claude with signal polling ---
# Runs claude in the project dir. Polls the log for signal tokens inline.
# When the signal is detected OR claude exits, we proceed.
run_claude() {
  local prompt="$1"
  local feedback="${2:-}"
  local claude_pid tail_pid
  tail_pid=""

  clear_signal
  rm -f "$RALPH_DIR/.stream-task"

  # Build the prompt that includes ralph loop context
  local raw="${3:-}"
  local rc_task_id="${4:-}"
  local rc_task_name="${5:-}"
  local full_prompt
  if [[ "$raw" == "raw" ]]; then
    full_prompt="$prompt"
  else
    full_prompt=$(build_prompt "$prompt" "$feedback" "$rc_task_id" "$rc_task_name")
  fi

  # Start the stream filter BEFORE Claude so tail -f -n 0 is already
  # watching the raw log when Claude begins writing. This eliminates
  # the race where early JSON output lands in loop.log unfiltered.
  cd "$WORK_DIR"
  write_stream_filter
  local filter_pid=""
  if command -v jq &>/dev/null; then
    bash "$RALPH_DIR/.stream-filter.sh" "$RAW_LOG" >> "$LOG_FILE" &
    filter_pid=$!
    if [[ "$QUIET" == false ]]; then
      tail -f -n 0 "$LOG_FILE" &
      tail_pid=$!
    fi
  elif [[ "$QUIET" == false ]]; then
    tail -f -n 0 "$RAW_LOG" &
    tail_pid=$!
  fi

  # Launch claude in background — filter is already tailing RAW_LOG.
  # Raw JSON goes to RAW_LOG (kept for analyzer + next-iteration context).
  claude --print --verbose --output-format stream-json \
    --add-dir "$WORK_DIR" \
    --add-dir "$RALPH_DIR" \
    --dangerously-skip-permissions \
    -p "$full_prompt" < /dev/null >> "$RAW_LOG" 2>&1 &
  claude_pid=$!
  log "Claude started (PID: $claude_pid)"
  date +%s > "$RALPH_DIR/.stream-start" 2>/dev/null || true

  # Poll for completion signal or Claude exit (inline, no subshell —
  # a background watcher subshell inherits set -e and can die silently
  # on harmless races between check_current_task and read_current_task)
  local task_logged=false signal_detected=false
  while kill -0 "$claude_pid" 2>/dev/null; do
    if [[ "$_interrupted" == true ]]; then
      log_warn "Interrupted — stopping Claude..."
      break
    fi
    if [[ "$task_logged" == false ]] && check_current_task; then
      local task_desc
      task_desc=$(read_current_task) || true
      if [[ -n "$task_desc" ]]; then
        log_task "Working on: $task_desc"
        write_state "last_task" "$task_desc"
        rename_branch_for_task "$task_desc"
        task_logged=true
      fi
    fi
    if check_signal || check_all_complete; then
      local summary
      summary=$(read_signal_summary) || true
      log_task_success "Completed: ${summary:-task done}"
      # Close the bead before killing Claude
      local completed_id
      completed_id=$(get_next_task_id) || true
      if [[ -n "$completed_id" ]]; then
        close_task "$completed_id" "${summary:-task done}"
      fi
      kill "$claude_pid" 2>/dev/null || true
      sleep 2
      kill -0 "$claude_pid" 2>/dev/null && kill -9 "$claude_pid" 2>/dev/null || true
      signal_detected=true
      break
    fi
    sleep "$WATCHER_INTERVAL"
  done

  # Kill Claude and all child processes on interrupt or normal exit
  _kill_children() {
    local pid=$1
    kill "$pid" 2>/dev/null || true
    local waited=0
    while kill -0 "$pid" 2>/dev/null && (( waited < 3 )); do
      sleep 1
      waited=$((waited + 1))
    done
    kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
  }

  if kill -0 "$claude_pid" 2>/dev/null; then
    _kill_children "$claude_pid"
  fi
  wait "$claude_pid" 2>/dev/null || true

  # Clean up stream filter and its children (tail, jq)
  if [[ -n "${filter_pid:-}" ]]; then
    pkill -P "$filter_pid" 2>/dev/null || true
    kill "$filter_pid" 2>/dev/null || true
    wait "$filter_pid" 2>/dev/null || true
  fi
  if [[ -n "$tail_pid" ]]; then
    pkill -P "$tail_pid" 2>/dev/null || true
    kill "$tail_pid" 2>/dev/null || true
    wait "$tail_pid" 2>/dev/null || true
  fi

  # On interrupt, skip signal checks and return immediately
  if [[ "$_interrupted" == true ]]; then
    return 1
  fi

  # Check if signal was written (claude may have exited after writing it)
  if check_signal || check_all_complete; then
    if [[ "$signal_detected" == false ]]; then
      log_task_success "Task completed via signal"
      local completed_id
      completed_id=$(get_next_task_id) || true
      if [[ -n "$completed_id" ]]; then
        local summary
        summary=$(read_signal_summary) || true
        close_task "$completed_id" "${summary:-task done}"
      fi
    fi
    return 0
  fi

  log "Claude exited (no completion signal)"
  return 0
}

# --- Build prompt for Claude ---
build_prompt() {
  local task_prompt="$1"
  local feedback="${2:-}"
  local bp_task_id="${3:-}"
  local bp_task_name="${4:-}"
  local template_file="$PROMPTS_DIR/internal.md"

  if [[ ! -f "$template_file" ]]; then
    log_error "Prompt template not found: $template_file"
    exit 1
  fi

  local result
  result=$(<"$PROMPTS_DIR/shared.md")
  result+=$'\n'
  result+=$(<"$template_file")
  result+=$'\n'
  result+=$(<"$PROMPTS_DIR/reflection.md")
  result+=$'\n'
  result+=$(<"$PROMPTS_DIR/signal.md")

  if [[ -n "$feedback" ]]; then
    local feedback_prompt
    feedback_prompt=$(<"$PROMPTS_DIR/feedback.md")
    feedback_prompt="${feedback_prompt//\{\{FEEDBACK\}\}/$feedback}"
    result+=$'\n\n'"$feedback_prompt"
  fi

  local task_instructions
  task_instructions=$(task_execution_instructions)
  result="${result//\{\{TASK_INSTRUCTIONS\}\}/$task_instructions}"

  # Inject attempt history if this task has been attempted before
  local attempt_history=""
  if [[ -n "$bp_task_id" || -n "$bp_task_name" ]]; then
    local raw_history
    raw_history=$(read_attempt_history "$bp_task_id" "$bp_task_name")
    if [[ -n "$raw_history" ]]; then
      attempt_history="## Previous attempts on this task"$'\n'
      attempt_history+="This task has been attempted before without success. Review what was tried and avoid repeating the same approach."$'\n\n'
      attempt_history+="$raw_history"
    fi
  fi
  result="${result//\{\{ATTEMPT_HISTORY\}\}/$attempt_history}"

  result="${result//\{\{PROJECT_DIR\}\}/$PROJECT_DIR}"
  result="${result//\{\{WORK_DIR\}\}/$WORK_DIR}"
  result="${result//\{\{RALPH_DIR\}\}/$RALPH_DIR}"
  result="${result//\{\{PLAN_FILE\}\}/$PLAN_FILE}"
  result="${result//\{\{TASK_PROMPT\}\}/$task_prompt}"

  printf '%s' "$result"
}

build_refactor_prompt() {
  local recent_files="$1"
  local quality_findings="${2:-}"

  local result
  result=$(<"$PROMPTS_DIR/shared.md")
  result+=$'\n'
  result+=$(<"$PROMPTS_DIR/refactor.md")
  result+=$'\n'
  result+=$(<"$PROMPTS_DIR/signal.md")

  local refactor_style
  refactor_style=$(<"$PROMPTS_DIR/refactor-style.md")
  result="${result//\{\{WORK_DIR\}\}/$WORK_DIR}"
  result="${result//\{\{RALPH_DIR\}\}/$RALPH_DIR}"
  result="${result//\{\{RECENT_FILES\}\}/$recent_files}"
  result="${result//\{\{REFACTOR_STYLE\}\}/$refactor_style}"
  result="${result//\{\{QUALITY_FINDINGS\}\}/$quality_findings}"

  printf '%s' "$result"
}

# --- Planning phase ---
run_planning() {
  log_phase "=== PHASE 1: PLANNING ==="

  if [[ -n "$PLAN_FILE_ARG" && ! -f "$PLAN_FILE" ]]; then
    cp "$PLAN_FILE_ARG" "$PLAN_FILE"
    local total
    total=$(count_total)
    write_state "status" "planned"
    log_task "Copied plan from $PLAN_FILE_ARG ($total tasks)"
    return 0
  fi

  if [[ "$RESUME" == true ]]; then
    local status
    status=$(read_state "status")
    if [[ "$status" != "initialized" ]]; then
      log_task "Resuming execution (status: $status)"
      return 0
    fi
  fi

  # Interactive planning: launch Claude for the user to define spec + plan
  if needs_planning && [[ "$SKIP_PLANNING" != true ]]; then
    log "Starting interactive planning session..."
    log "Task backend: $(task_label)"
    log "Chat with Claude to define your spec and plan. Exit when done."

    local interactive_prompt
    interactive_prompt=$(<"$PROMPTS_DIR/interactive-planning.md")
    interactive_prompt="${interactive_prompt//\{\{WORK_DIR\}\}/$WORK_DIR}"
    interactive_prompt="${interactive_prompt//\{\{RALPH_DIR\}\}/$RALPH_DIR}"
    interactive_prompt="${interactive_prompt//\{\{STATE_FILE\}\}/$STATE_FILE}"
    interactive_prompt="${interactive_prompt//\{\{TASK_INSTRUCTIONS\}\}/$(task_planning_instructions)}"
    if [[ "$TASK_BACKEND" == "bd" ]]; then
      interactive_prompt="${interactive_prompt//\{\{PLAN_FILE_LINE\}\}/}"
    else
      interactive_prompt="${interactive_prompt//\{\{PLAN_FILE_LINE\}\}/- Plan file: $PLAN_FILE}"
    fi

    cd "$WORK_DIR"
    claude --add-dir "$WORK_DIR" \
      --add-dir "$RALPH_DIR" \
      --permission-mode plan \
      --allowedTools "Bash" \
      --system-prompt "$interactive_prompt" || true

    log "Interactive planning session ended."
  fi

  # If interactive session created a plan, use it
  if planning_succeeded; then
    write_state "status" "planned"
    log_task_success "Plan created with $(count_total) tasks"
    return 0
  fi

  # Fallback: autonomous planning if no plan was created interactively
  local planning_context
  if [[ -n "$PROMPT_OVERRIDE" ]]; then
    planning_context="$PROMPT_OVERRIDE"
  else
    planning_context=""
  fi

  local planning_prompt
  planning_prompt=$(<"$PROMPTS_DIR/planning.md")
  planning_prompt="${planning_prompt//\{\{PLANNING_CONTEXT\}\}/$planning_context}"
  planning_prompt="${planning_prompt//\{\{PLAN_FILE\}\}/$PLAN_FILE}"
  planning_prompt="${planning_prompt//\{\{RALPH_DIR\}\}/$RALPH_DIR}"
  planning_prompt="${planning_prompt//\{\{STATE_FILE\}\}/$STATE_FILE}"
  planning_prompt="${planning_prompt//\{\{TASK_INSTRUCTIONS\}\}/$(task_planning_instructions)}"

  run_claude "$planning_prompt"

  if planning_succeeded; then
    write_state "status" "planned"
    log_task_success "Plan created with $(count_total) tasks"
  else
    log_task_error "Planning failed — no tasks created"
    exit 1
  fi
}

# --- Response analyzer ---
# Counters for multi-iteration detection (reset per execution phase)
_stagnant_count=0
_test_only_count=0
_stuck_count=0

# analyze_iteration LOG_FILE START_LINE HEAD_BEFORE [TASK_KEY]
# Sets ANALYSIS_RESULT to one of: continue, warn:<reason>, halt:<reason>
# Updates global counters for multi-iteration detection
analyze_iteration() {
  local log_file="$1"
  local start_line="$2"
  local head_before="$3"
  local task_key="${4:-}"

  ANALYSIS_RESULT="continue"
  ANALYSIS_DETAIL=""

  local iter_log
  iter_log=$(tail -n "+${start_line}" "$log_file" 2>/dev/null || true)

  if [[ -z "$iter_log" ]]; then
    return
  fi

  # Extract only assistant text from stream-json (excludes tool inputs/outputs
  # to avoid false positives from code content like test fixtures).
  local assistant_text=""
  if command -v jq &>/dev/null; then
    assistant_text=$(jq -r '
        select(.type == "assistant") |
        .message.content[]? |
        select(.type == "text") |
        .text
      ' <<< "$iter_log" 2>/dev/null || true)
  else
    assistant_text="$iter_log"
  fi

  # --- Permission denial detection (3+ in single iteration → halt) ---
  local perm_matches=""
  perm_matches=$(grep -iE 'permission denied|cannot write|blocked by sandbox|not allowed' <<< "$assistant_text" | head -5 || true)
  local perm_count=0
  if [[ -n "$perm_matches" ]]; then
    perm_count=$(echo "$perm_matches" | wc -l | tr -d ' ')
  fi
  if (( perm_count >= PERMISSION_DENIAL_THRESHOLD )); then
    ANALYSIS_DETAIL="$perm_matches"
    ANALYSIS_RESULT="halt:permission_denied"
    return
  fi

  # --- Stuck loop detection (skip if task completed via signal) ---
  local stuck_detected=false

  if check_signal || check_all_complete; then
    _stuck_count=0
  else
    if grep -qiE "I'm blocked|I cannot proceed|unable to complete" <<< "$assistant_text"; then
      stuck_detected=true
    fi
  fi

  if [[ "$stuck_detected" == true ]]; then
    _stuck_count=$((_stuck_count + 1))
    if (( _stuck_count >= STUCK_CONFIRMATION_THRESHOLD )); then
      ANALYSIS_RESULT="halt:stuck_loop"
      return
    fi
    ANALYSIS_RESULT="warn:stuck_indicators_detected"
    return
  else
    _stuck_count=0
  fi

  # --- Repeated error detection (same error fingerprint 3x → halt) ---
  if [[ -n "$task_key" ]]; then
    if check_repeated_errors "$assistant_text" "$task_key"; then
      ANALYSIS_RESULT="halt:repeated_error"
      return
    fi
  fi

  # --- Progress detection (used by stagnation and test saturation) ---
  local has_changes=false has_signal=false new_commits=false

  if [[ -n "$(git -C "$WORK_DIR" diff --stat 2>/dev/null)" ]] || \
     [[ -n "$(git -C "$WORK_DIR" diff --cached --stat 2>/dev/null)" ]]; then
    has_changes=true
  fi

  local head_after
  head_after=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null || echo "")
  if [[ -n "$head_before" && "$head_before" != "$head_after" ]]; then
    new_commits=true
    has_changes=true
  fi

  if check_signal || check_all_complete; then
    has_signal=true
  fi

  # --- Stagnation detection (3 consecutive no-change → halt) ---
  if [[ "$has_changes" == false && "$has_signal" == false && "$new_commits" == false ]]; then
    _stagnant_count=$((_stagnant_count + 1))
    if (( _stagnant_count >= STAGNATION_THRESHOLD )); then
      ANALYSIS_RESULT="halt:stagnation"
      return
    fi
  else
    _stagnant_count=0
  fi

  # --- Test saturation detection (3 consecutive test-only → halt) ---
  if [[ "$has_changes" == true ]]; then
    local changed_files
    changed_files=$(git -C "$WORK_DIR" diff --name-only 2>/dev/null || true)
    changed_files+=$'\n'
    changed_files+=$(git -C "$WORK_DIR" diff --cached --name-only 2>/dev/null || true)
    if [[ "$new_commits" == true ]]; then
      changed_files+=$'\n'
      changed_files+=$(git -C "$WORK_DIR" diff --name-only "${head_before}...${head_after}" 2>/dev/null || true)
    fi
    changed_files=$(echo "$changed_files" | grep -v '^$' | sort -u)

    if [[ -n "$changed_files" ]]; then
      local non_test_files=""
      while IFS= read -r f; do
        local base="${f##*/}"
        local top_dir="${f%%/*}"
        if echo "$base" | grep -qiE '(test|spec|_test\.|test_)'; then
          continue
        fi
        if echo "$top_dir" | grep -qiE '(tests?|specs?|__tests__)$'; then
          continue
        fi
        non_test_files+="$f"$'\n'
      done <<< "$changed_files"
      non_test_files=$(echo "$non_test_files" | grep -v '^$' || true)

      if [[ -z "$non_test_files" ]]; then
        _test_only_count=$((_test_only_count + 1))
        if (( _test_only_count >= TEST_SATURATION_THRESHOLD )); then
          ANALYSIS_RESULT="halt:test_saturation"
          return
        fi
      else
        _test_only_count=0
      fi
    fi
  fi
}

# --- Execution phase ---
run_execution() {
  log_phase "=== PHASE 2: EXECUTION ==="

  if [[ -n "$WORKTREE_BRANCH" && "$WORK_DIR" != "$PROJECT_DIR" ]]; then
    if ! rebase_onto_default_branch; then
      write_state "status" "error"
      return 1
    fi
  fi

  init_call_tracking

  # Reset counters on resume
  _stagnant_count=0
  _test_only_count=0
  _stuck_count=0
  local _refactor_pending=false
  local _current_task_id=""
  local _task_attempt_count=0

  local run_iteration=0
  local iteration
  iteration=$(read_state "iteration")
  iteration=${iteration:-0}

  write_state "max_iterations" "$MAX_ITERATIONS"
  write_state "refactor_threshold" "$REFACTOR_THRESHOLD"
  write_state "quality_score" "0"
  while true; do
    MAX_ITERATIONS=$(read_state "max_iterations")
    REFACTOR_THRESHOLD=$(read_state "refactor_threshold")
    REFACTOR_THRESHOLD=${REFACTOR_THRESHOLD:-0}
    if (( run_iteration >= MAX_ITERATIONS )); then break; fi
    # Check for Ctrl-C interrupt
    if [[ "$_interrupted" == true ]]; then
      log_warn "Interrupted — stopping execution"
      write_state "status" "interrupted"
      break
    fi
    # Check stop file
    if [[ -f "$STOP_FILE" ]]; then
      log_warn "Stop file detected - halting"
      rm -f "$STOP_FILE"
      write_state "status" "stopped"
      break
    fi

    # Check remaining tasks
    if ! has_remaining_tasks; then
      if (( run_iteration == 0 )) && (( $(count_total) == 0 )); then
        log_task_error "No tasks found"
        write_state "status" "error"
        break
      fi
      log_task_success "All tasks complete!"
      write_state "status" "completed"
      break
    fi

    run_iteration=$((run_iteration + 1))
    iteration=$((iteration + 1))

    # Each iteration gets its own branch, stacked on the previous
    if (( run_iteration > 1 )); then
      rotate_branch
      # Rebase onto latest main between tasks to pick up squash-merged PRs
      if [[ -n "$WORKTREE_BRANCH" && "$WORK_DIR" != "$PROJECT_DIR" ]]; then
        if ! rebase_onto_default_branch; then
          write_state "status" "error"
          break
        fi
      fi
    fi

    # Adaptive refactoring: if quality assessment flagged pain, run refactor
    if [[ "$_refactor_pending" == true ]]; then
      local quality_score
      quality_score=$(read_state "quality_score")
      log_phase "--- Refactor iteration (quality score: ${quality_score}, threshold: ${REFACTOR_THRESHOLD}) ---"
      local recent_files
      recent_files=$(git -C "$WORK_DIR" diff --name-only HEAD~5 HEAD 2>/dev/null || echo "")

      if [[ -n "$recent_files" ]]; then
        local quality_findings=""
        local findings_file="$RALPH_DIR/.quality-findings"
        if [[ -f "$findings_file" ]]; then
          quality_findings=$(<"$findings_file")
        fi

        local refactor_prompt
        refactor_prompt=$(build_refactor_prompt "$recent_files" "$quality_findings")

        if ! check_rate_limit; then
          if ! wait_for_rate_reset; then
            break
          fi
        fi

        run_claude "$refactor_prompt" "" "raw"
        increment_call_count
        log_task_success "Refactor iteration complete"
      else
        log "No recently changed files — skipping refactor"
      fi
      _refactor_pending=false
      write_state "quality_score" "0"
    fi

    local next_task completed remaining total
    next_task=$(get_next_task)
    completed=$(count_completed)
    remaining=$(count_remaining)
    total=$(count_total)

    local _health _hcolor
    _health=$(cat "$RALPH_DIR/.health" 2>/dev/null || echo "ok")
    case "$_health" in
      halt) _hcolor="$RED" ;;
      warn) _hcolor="$YELLOW" ;;
      *)    _hcolor="$GREEN" ;;
    esac
    log_phase "--- Iteration $run_iteration/$MAX_ITERATIONS ($iteration total) [${_hcolor}${completed}/${total} done${NC}${BOLD}] ---"
    log_task "Next task: $next_task"
    touch "$RALPH_DIR/.plan-refresh"

    # Update state
    write_state "iteration" "$iteration"
    write_state "status" "running"
    write_state "last_task" "$next_task"
    rename_branch_for_task "$next_task"
    printf '%s' "${WORKTREE_BRANCH:-ralph}" > "$RALPH_DIR/.run-branch"

    # Build task prompt
    local task_id
    task_id=$(get_next_task_id)

    # Reset per-task counters when the task changes
    local task_key="${task_id:-$next_task}"
    if [[ "$task_key" != "$_current_task_id" ]]; then
      _current_task_id="$task_key"
      _task_attempt_count=0
      _stagnant_count=0
      _test_only_count=0
      _stuck_count=0
    fi
    _task_attempt_count=$((_task_attempt_count + 1))

    # Skip task if max attempts exceeded
    if (( MAX_TASK_ATTEMPTS > 0 && _task_attempt_count > MAX_TASK_ATTEMPTS )); then
      log_warn "Task '$next_task' exceeded $MAX_TASK_ATTEMPTS attempts — skipping"
      skip_task "$task_id" "exceeded $MAX_TASK_ATTEMPTS attempts"
      clear_attempt_history "$task_id" "$next_task"
      clear_error_hashes "$task_key"
      _current_task_id=""
      _task_attempt_count=0
      continue
    fi

    # Update stream pane title with task context
    if [[ -n "$task_id" ]]; then
      printf '%s: %s' "$task_id" "$next_task" > "$RALPH_DIR/.stream-task"
    else
      printf '%s' "$next_task" > "$RALPH_DIR/.stream-task"
    fi
    local task_prompt="Complete this task: $next_task"
    if [[ -n "$task_id" ]]; then
      task_prompt="Complete this task (bd id: $task_id): $next_task"
    fi

    # Rate limit check
    if ! check_rate_limit; then
      if ! wait_for_rate_reset; then
        break
      fi
    fi

    # Capture log offset and HEAD before running claude
    local log_start_line
    log_start_line=$(( $(wc -l < "$RAW_LOG" 2>/dev/null || echo 0) + 1 ))
    local head_before
    head_before=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null || echo "")

    # Read any queued user feedback
    local feedback=""
    feedback=$(read_feedback) || true
    if [[ -n "$feedback" ]]; then
      log_warn "[feedback] $feedback"
    fi

    # Run claude for this task
    local task_start=$SECONDS
    if ! run_claude "$task_prompt" "$feedback" "" "$task_id" "$next_task"; then
      log_warn "Claude failed on iteration $run_iteration, continuing..."
    fi
    local task_elapsed=$(( SECONDS - task_start ))
    increment_call_count

    # Exit loop immediately if interrupted during run_claude
    if [[ "$_interrupted" == true ]]; then
      log_warn "Interrupted — stopping execution"
      write_state "status" "interrupted"
      break
    fi

    # Clear feedback after Claude completes the iteration
    if [[ -n "$feedback" ]]; then
      clear_feedback
    fi

    # Post-iteration: read signal summary
    local summary=""
    summary=$(read_signal_summary) || true
    if [[ -n "$summary" ]]; then
      log "Summary: $summary"
    fi

    # Recount after claude ran
    completed=$(count_completed)
    local mins=$(( task_elapsed / 60 )) secs=$(( task_elapsed % 60 ))
    log_task "Run iteration $run_iteration complete (${mins}m${secs}s). ${completed}/${total} tasks done."

    # Analyze iteration for problems
    analyze_iteration "$RAW_LOG" "$log_start_line" "$head_before" "$task_key"

    # Write health status for pane title coloring
    case "$ANALYSIS_RESULT" in
      halt:*) printf 'halt' > "$RALPH_DIR/.health" ;;
      warn:*) printf 'warn' > "$RALPH_DIR/.health" ;;
      *)      printf 'ok'   > "$RALPH_DIR/.health" ;;
    esac

    # Record attempt history for this task
    local head_after_attempt
    head_after_attempt=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null || echo "")
    local diff_stat=""
    if [[ -n "$head_before" && "$head_before" != "$head_after_attempt" ]]; then
      diff_stat=$(git -C "$WORK_DIR" diff --stat "${head_before}..${head_after_attempt}" 2>/dev/null || true)
    fi
    if [[ -z "$diff_stat" ]]; then
      diff_stat=$(git -C "$WORK_DIR" diff --stat 2>/dev/null || true)
    fi
    record_attempt "$task_id" "$next_task" "$summary" "$diff_stat" "$ANALYSIS_RESULT"

    # Clear attempt history and reset per-task state when task resolved
    if check_signal || check_all_complete; then
      flash_plan_pane
      clear_attempt_history "$task_id" "$next_task"
      clear_error_hashes "$task_key"
      _current_task_id=""
      _task_attempt_count=0
    fi

    case "$ANALYSIS_RESULT" in
      halt:permission_denied)
        log_error "Halting: permission_denied"
        if [[ -n "$ANALYSIS_DETAIL" ]]; then
          echo "$ANALYSIS_DETAIL" | while IFS= read -r detail_line; do
            log_error "  $detail_line"
          done
        fi
        write_state "status" "halted_permission_denied"
        break
        ;;
      halt:*)
        local reason="${ANALYSIS_RESULT#halt:}"
        log_warn "Task stuck ($reason) — skipping '$next_task'"
        skip_task "$task_id" "$reason"
        clear_attempt_history "$task_id" "$next_task"
        clear_error_hashes "$task_key"
        _current_task_id=""
        _task_attempt_count=0
        _stagnant_count=0
        _test_only_count=0
        _stuck_count=0
        ;;
      warn:*)
        log_warn "Analysis: ${ANALYSIS_RESULT#warn:}"
        ;;
    esac

    # Adaptive quality assessment: check changed files for code quality signals
    if (( REFACTOR_THRESHOLD > 0 )); then
      local head_after
      head_after=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null || echo "")
      if [[ "$head_after" != "$head_before" ]]; then
        local changed_files
        changed_files=$(git -C "$WORK_DIR" diff --name-only "$head_before" HEAD 2>/dev/null || echo "")
        if [[ -n "$changed_files" ]]; then
          local findings_file="$RALPH_DIR/.quality-findings"
          local file_array=()
          while IFS= read -r f; do
            file_array+=("$f")
          done <<< "$changed_files"
          assess_quality "$WORK_DIR" "$findings_file" "${file_array[@]}"
          write_state "quality_score" "$QUALITY_SCORE"
          if (( QUALITY_SCORE >= REFACTOR_THRESHOLD )); then
            log_warn "Quality score ${QUALITY_SCORE} >= threshold ${REFACTOR_THRESHOLD} — scheduling refactor"
            _refactor_pending=true
          elif (( QUALITY_SCORE > 0 )); then
            log "Quality score: ${QUALITY_SCORE} (threshold: ${REFACTOR_THRESHOLD})"
          fi
        fi
      fi
    fi

    # Auto-merge: squash-merge the PR for this iteration's branch into main
    if [[ "$AUTO_MERGE" == true ]] && (check_signal || check_all_complete); then
      auto_merge_current_branch || true
    fi

    echo ""
  done

  if (( run_iteration >= MAX_ITERATIONS )); then
    log_warn "Max iterations ($MAX_ITERATIONS) reached"
    write_state "status" "max_iterations_reached"
  fi
}

# --- Generate resume script ---
generate_resume_script() {
  local extra_args=""
  if [[ "$QUIET" == true ]]; then
    extra_args="$extra_args --quiet"
  fi
  if [[ "$USE_WORKTREE" == false ]]; then
    extra_args="$extra_args --no-worktree"
  fi
  if [[ "$CALLS_PER_HOUR" != 80 ]]; then
    extra_args="$extra_args --calls-per-hour $CALLS_PER_HOUR"
  fi
  if [[ "$MAX_TASK_ATTEMPTS" != 5 ]]; then
    extra_args="$extra_args --max-task-attempts $MAX_TASK_ATTEMPTS"
  fi
  if [[ "${_RALPH_TMUX_SESSION:-}" != "" ]]; then
    extra_args="$extra_args --tmux"
  fi
  if [[ "$AUTO_MERGE" == true ]]; then
    extra_args="$extra_args --auto-merge"
  fi
  cat > "$RESUME_SCRIPT" <<RESUME
#!/usr/bin/env bash
# Ralph Loop - Resume Script
# Generated at: $(date -u +"%Y-%m-%dT%H:%M:%SZ")
exec "$SCRIPT_DIR/ralph.sh" --dir "$PROJECT_DIR" --max "$MAX_ITERATIONS"$extra_args
RESUME
  chmod +x "$RESUME_SCRIPT"
  log "Resume script: $RESUME_SCRIPT"
}

# --- Print summary ---
print_summary() {
  local iteration status
  iteration=$(read_state "iteration")
  status=$(read_state "status")

  echo ""
  log_phase "=== SUMMARY ==="
  log "Status:     $status"
  log "Iterations: $iteration lifetime"

  local completed remaining total
  completed=$(count_completed)
  remaining=$(count_remaining)
  total=$(count_total)
  log_task "Tasks: $completed/$total completed, $remaining remaining"

  log "Log:        $LOG_FILE"
  if [[ "$TASK_BACKEND" == "checklist" ]]; then
    log "Plan:       $PLAN_FILE"
  fi

  if [[ -n "$WORKTREE_BRANCH" && -n "$PROJECT_NAME" ]]; then
    log "Worktree:   $WORK_DIR"
    local branches
    branches=$(git -C "$PROJECT_DIR" branch --list "ralph/$PROJECT_NAME/*" 2>/dev/null | sed 's/^[ *]*//' || true)
    if [[ $(echo "$branches" | wc -l | tr -d ' ') -gt 1 ]]; then
      log "Branches:"
      echo "$branches" | while read -r b; do
        log "  $b"
      done
    else
      log "Branch:     $WORKTREE_BRANCH"
    fi
    log "To merge:   git merge $WORKTREE_BRANCH"
  fi

  if has_remaining_tasks 2>/dev/null; then
    log "Resume:     $RESUME_SCRIPT"
  fi
}

# --- Cleanup on exit ---
cleanup() {
  # Outer tmux wrapper — kill session on interrupt, keep alive on normal exit
  if [[ "$_TMUX_OUTER" == true ]]; then
    if [[ "$_interrupted" == true ]]; then
      [[ -n "$TMUX_SESSION" ]] && tmux kill-session -t "$TMUX_SESSION" 2>/dev/null || true
    fi
    return
  fi
  # Write interrupted status so resume knows what happened
  if [[ "$_interrupted" == true && -d "${RALPH_DIR:-}" ]]; then
    write_state "status" "interrupted"
  fi
  # Kill any backgrounded processes and their children
  for pid in $(jobs -p 2>/dev/null); do
    pkill -P "$pid" 2>/dev/null || true
    kill "$pid" 2>/dev/null || true
  done
  # Clean up unused worktree branch (still named /next = no work committed)
  if [[ -n "${WORKTREE_BRANCH:-}" && "$WORKTREE_BRANCH" == */next && "${WORK_DIR:-}" != "$PROJECT_DIR" ]]; then
    git -C "$PROJECT_DIR" worktree remove --force "$WORK_DIR" 2>/dev/null || true
    git -C "$PROJECT_DIR" branch -D "$WORKTREE_BRANCH" 2>/dev/null || true
  fi
  # Only run summary/resume if .ralph dir was created
  if [[ -d "$RALPH_DIR" ]]; then
    generate_resume_script
    print_summary
  fi
}
_interrupted=false
trap '_interrupted=true' INT TERM
trap cleanup EXIT

# --- Main ---
main() {
  init_ralph_dir

  if [[ "$USE_TMUX" == true ]]; then
    setup_tmux
  fi

  if [[ "$RESUME" == true ]]; then
    stored_backend=$(read_state "task_backend")
    if [[ "$stored_backend" == "bd" || "$stored_backend" == "checklist" ]]; then
      TASK_BACKEND="$stored_backend"
    elif [[ -f "$PLAN_FILE" ]] && grep -qE '^\s*- \[[ x]\]' "$PLAN_FILE"; then
      TASK_BACKEND="checklist"
    fi
  fi

  # Init task backend BEFORE worktree so .beads/.dolt are gitignored first
  _validate_backend
  local pre_init_backend="$TASK_BACKEND"
  init_task_backend
  # bd_init may have fallen back to checklist — re-validate if backend changed
  if [[ "$TASK_BACKEND" != "$pre_init_backend" ]]; then
    _validate_backend
    init_task_backend
  fi
  write_state "task_backend" "$TASK_BACKEND"

  setup_worktree
  printf '%s' "${WORKTREE_BRANCH:-ralph}" > "$RALPH_DIR/.run-branch"

  log_phase "Ralph Loop v${VERSION} (sh)"
  log "Project: $PROJECT_DIR"
  [[ "$WORK_DIR" != "$PROJECT_DIR" ]] && log "Worktree: $WORK_DIR"
  log "Task backend: $(task_label)"
  log "Max iterations: $MAX_ITERATIONS"

  write_state "started_at" "$(date -u +'%Y-%m-%dT%H:%M:%SZ')"

  # Planning
  run_planning
  _rename_worktree_from_theme

  if [[ "$PLAN_ONLY" == true ]]; then
    log "Plan-only mode, exiting"
    exit 0
  fi

  # Execution
  run_execution
}

[[ "${RALPH_SOURCED:-}" == true ]] || main
