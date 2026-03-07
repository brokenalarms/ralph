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

# --- Defaults ---
PROJECT_DIR="$(pwd)"
MAX_ITERATIONS=20
PROMPT_OVERRIDE=""
RESUME=false
PLAN_ONLY=false
SKIP_PLANNING=false
SIGNAL_TOKEN="###RALPH_TASK_COMPLETE###"
CURRENT_TASK_TOKEN="###RALPH_CURRENT_TASK###"
WATCHER_INTERVAL=2  # seconds between signal checks
PLAN_FILE_ARG=""
QUIET=false
USE_WORKTREE=true
CALLS_PER_HOUR=80
USE_TMUX=false
TMUX_SESSION=""
_TMUX_OUTER=false
WORK_DIR=""
WORKTREE_BRANCH=""
PROJECT_NAME=""
temp_branch() { echo "ralph/$PROJECT_NAME/next"; }
_TASK_SEQ=0
ALL_COMPLETE_TOKEN="###RALPH_ALL_COMPLETE###"
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
log()         { echo -e "${CYAN}[ralph]${NC} $*" | tee -a "$LOG_FILE"; }
log_success() { echo -e "${GREEN}[ralph]${NC} $*" | tee -a "$LOG_FILE"; }
log_warn()    { echo -e "${YELLOW}[ralph]${NC} $*" | tee -a "$LOG_FILE"; }
log_error()   { echo -e "${RED}[ralph]${NC} $*" | tee -a "$LOG_FILE"; }
log_phase()   { echo -e "${BOLD}${BLUE}[ralph]${NC} ${BOLD}$*${NC}" | tee -a "$LOG_FILE"; }

task_label() { if [[ "$TASK_BACKEND" == "bd" ]]; then echo "beads"; else echo "checklist"; fi; }
log_task()         { echo -e "${CYAN}[$(task_label)]${NC} $*" | tee -a "$LOG_FILE"; }
log_task_success() { echo -e "${GREEN}[$(task_label)]${NC} $*" | tee -a "$LOG_FILE"; }
log_task_error()   { echo -e "${RED}[$(task_label)]${NC} $*" | tee -a "$LOG_FILE"; }

# --- Helpers ---
slugify() {
  echo "$1" | tr '[:upper:]' '[:lower:]' | \
    sed 's/[^a-z0-9]/-/g; s/--*/-/g; s/^-//; s/-$//' | cut -c1-50
}

_BRANCH_RENAMED=false

# --- Usage ---
usage() {
  cat <<EOF
${BOLD}Ralph Loop v${VERSION}${NC} - Autonomous Claude Code task iteration

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
  --tmux                 Run in tmux 3-pane layout (status / output / plan)
  -h, --help             Show this help

${BOLD}EXAMPLES:${NC}
  ralph.sh ~/myproject -n 20
  ralph.sh -p "Fix all failing tests"
  ralph.sh . --plan-file plan.md

${BOLD}HOW IT WORKS:${NC}
  1. Planning: Claude reads the repo and creates .ralph/plan.md with atomic tasks
  2. Execution: Each task runs in a fresh Claude context (~200k tokens)
  3. Completion: Claude writes a signal file when each task is done
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
    local feedback_file="$ralph_dir/feedback"
    if [[ -f "$feedback_file" && -s "$feedback_file" ]]; then
      echo -e "${CYAN}[ralph]${NC} Queued feedback:"
      cat "$feedback_file"
    else
      echo -e "${CYAN}[ralph]${NC} No feedback queued."
    fi
    exit 0
  fi
  echo "$*" >> "$ralph_dir/feedback"
  echo -e "${GREEN}[ralph]${NC} Feedback queued for next iteration: $*"
  exit 0
fi

# --- Save original args (for tmux re-exec) ---
RALPH_ORIG_ARGS=("$@")

# --- Parse args ---
while [[ $# -gt 0 ]]; do
  case "$1" in
    -d|--dir)       PROJECT_DIR="$2"; shift 2 ;;
    -n|--max)       MAX_ITERATIONS="$2"; shift 2 ;;
    -p|--prompt)    PROMPT_OVERRIDE="$2"; shift 2 ;;
    --plan-file)    PLAN_FILE_ARG="$2"; shift 2 ;;
    --plan)         PLAN_ONLY=true; shift ;;
    --skip-planning) SKIP_PLANNING=true; shift ;;
    -q|--quiet)     QUIET=true; shift ;;
    --no-worktree)  USE_WORKTREE=false; shift ;;
    --calls-per-hour) CALLS_PER_HOUR="$2"; shift 2 ;;
    --tmux)         USE_TMUX=true; shift ;;
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

# --- Resolve paths ---
PROJECT_DIR="$(cd "$PROJECT_DIR" && pwd)"
RALPH_DIR="$PROJECT_DIR/.ralph"
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
SIGNAL_FILE="$RALPH_DIR/signal"
STOP_FILE="$RALPH_DIR/stop"
LOG_FILE="$RALPH_DIR/loop.log"
RESUME_SCRIPT="$RALPH_DIR/resume.sh"

# --- Init .ralph directory ---
init_ralph_dir() {
  mkdir -p "$RALPH_DIR"
  touch "$LOG_FILE"

  # Ensure .ralph is gitignored
  local gitignore="$PROJECT_DIR/.gitignore"
  if [[ ! -f "$gitignore" ]] || ! grep -qx '.ralph' "$gitignore"; then
    echo '.ralph' >> "$gitignore"
  fi

  if [[ -f "$STATE_FILE" ]]; then
    local status
    status=$(read_state "status")
    if [[ "$status" == "completed" ]]; then
      log_task "All tasks completed from previous run."
      printf "${YELLOW}[ralph]${NC} Run fresh? (y/n) "
      read -r answer
      if [[ "$answer" == "y" || "$answer" == "Y" ]]; then
        rm -rf "$RALPH_DIR"
        mkdir -p "$RALPH_DIR"
        touch "$LOG_FILE"
      else
        exit 0
      fi
    else
      RESUME=true
      log "Resuming from previous state (status: $status)"
    fi
  fi

  if [[ ! -f "$STATE_FILE" ]]; then
    cat > "$STATE_FILE" <<'STATE'
{
  "iteration": 0,
  "status": "initialized",
  "started_at": null,
  "last_task": null,
  "worktree_dir": null,
  "worktree_branch": null
}
STATE
  fi
}

# --- Worktree setup ---
setup_worktree() {
  WORK_DIR="$PROJECT_DIR"

  if [[ "$USE_WORKTREE" == false ]]; then
    return
  fi

  if ! git -C "$PROJECT_DIR" rev-parse --git-dir &>/dev/null; then
    log_error "Not a git repo — ralph requires git. Use --no-worktree to run without git isolation."
    exit 1
  fi

  # On resume, reuse existing worktree if stored in state
  if [[ "$RESUME" == true ]]; then
    local stored_worktree
    stored_worktree=$(read_state "worktree_dir")
    if [[ -n "$stored_worktree" && "$stored_worktree" != "null" && -d "$stored_worktree" ]]; then
      WORK_DIR="$stored_worktree"
      WORKTREE_BRANCH=$(read_state "worktree_branch")
      PROJECT_NAME=$(basename "$PROJECT_DIR")
      local named_branches
      named_branches=$(git -C "$PROJECT_DIR" branch --list "ralph/$PROJECT_NAME/*" 2>/dev/null | wc -l | tr -d ' ')
      _TASK_SEQ=$((named_branches))
      SIGNAL_FILE="$WORK_DIR/.ralph-signal"
      log "Resuming in worktree: $WORK_DIR (branch: $WORKTREE_BRANCH)"
      return
    fi
  fi

  PROJECT_NAME=$(basename "$PROJECT_DIR")

  local today
  today=$(date +%Y%m%d)
  local run_seq=1
  if [[ -d "$RALPH_DIR/worktrees" ]]; then
    local existing_today
    existing_today=$(find "$RALPH_DIR/worktrees" -maxdepth 1 -name "ralph-${today}-*" -type d 2>/dev/null | wc -l | tr -d ' ')
    run_seq=$((existing_today + 1))
  fi

  WORKTREE_BRANCH=$(temp_branch)
  WORK_DIR="$RALPH_DIR/worktrees/ralph-${today}-$(printf "%02d" $run_seq)"

  mkdir -p "$RALPH_DIR/worktrees"

  # Clean up leftover ralph worktrees and temp branch from previous runs
  git -C "$PROJECT_DIR" worktree prune 2>/dev/null || true
  if git -C "$PROJECT_DIR" rev-parse --verify "$WORKTREE_BRANCH" &>/dev/null; then
    if ! git -C "$PROJECT_DIR" branch -D "$WORKTREE_BRANCH" 2>/dev/null; then
      # Branch can't be deleted — find and remove the ralph worktree holding it
      local existing_wt
      existing_wt=$(git -C "$PROJECT_DIR" worktree list --porcelain 2>/dev/null | grep -B2 "branch refs/heads/$WORKTREE_BRANCH" | grep "^worktree " | sed 's/^worktree //')
      if [[ -n "$existing_wt" && "$existing_wt" == */.ralph/worktrees/* ]]; then
        log_warn "Removing stale ralph worktree: $existing_wt"
        git -C "$PROJECT_DIR" worktree remove --force "$existing_wt" 2>/dev/null || true
        git -C "$PROJECT_DIR" branch -D "$WORKTREE_BRANCH" 2>/dev/null || true
      else
        log_error "Cannot delete branch '$WORKTREE_BRANCH' — it is checked out in a non-ralph worktree: ${existing_wt:-unknown}"
        exit 1
      fi
    fi
  fi

  git -C "$PROJECT_DIR" worktree add -b "$WORKTREE_BRANCH" "$WORK_DIR" HEAD
  git -C "$WORK_DIR" config rebase.updateRefs true
  log "Worktree: $WORK_DIR (branch: $WORKTREE_BRANCH)"

  write_state "worktree_dir" "$WORK_DIR"
  write_state "worktree_branch" "$WORKTREE_BRANCH"

  SIGNAL_FILE="$WORK_DIR/.ralph-signal"
}

rename_branch_for_task() {
  local task_desc="$1"
  if [[ "$_BRANCH_RENAMED" == true || -z "$WORKTREE_BRANCH" || -z "$task_desc" ]]; then
    return
  fi
  if [[ "$WORK_DIR" == "$PROJECT_DIR" ]]; then
    return
  fi

  local slug
  slug=$(slugify "$task_desc")
  if [[ -z "$slug" ]]; then
    return
  fi

  _TASK_SEQ=$((_TASK_SEQ + 1))
  local new_branch="ralph/$PROJECT_NAME/$(printf "%02d" $_TASK_SEQ)-${slug}"
  if git -C "$WORK_DIR" branch -m "$WORKTREE_BRANCH" "$new_branch" 2>/dev/null; then
    WORKTREE_BRANCH="$new_branch"
    write_state "worktree_branch" "$WORKTREE_BRANCH"
    _BRANCH_RENAMED=true
  fi
}

rotate_branch() {
  if [[ -z "$WORKTREE_BRANCH" || "$WORK_DIR" == "$PROJECT_DIR" ]]; then
    return
  fi

  WORKTREE_BRANCH=$(temp_branch)
  git -C "$WORK_DIR" branch -D "$WORKTREE_BRANCH" 2>/dev/null || true
  if git -C "$WORK_DIR" checkout -b "$WORKTREE_BRANCH" 2>/dev/null; then
    write_state "worktree_branch" "$WORKTREE_BRANCH"
    _BRANCH_RENAMED=false
    log "Branch: $WORKTREE_BRANCH (from previous iteration)"
  else
    log_warn "Branch rotation failed, continuing on $WORKTREE_BRANCH"
  fi
}

# --- Stream filter helper ---
write_stream_filter() {
  cat > "$RALPH_DIR/.stream-filter.sh" <<'STREAM'
#!/usr/bin/env bash
set +m
# stream-json: each event has 1 content block. Filter and format.
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
' 2>/dev/null | sed -E \
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

  TMUX_SESSION="ralph-$$"

  write_stream_filter

  tmux new-session -d -s "$TMUX_SESSION" -c "$PROJECT_DIR"
  tmux split-window -h -t "$TMUX_SESSION"
  tmux split-window -v -t "$TMUX_SESSION:.1"

  # Top-right: jq-parsed Claude output
  tmux send-keys -t "$TMUX_SESSION:.1" \
    "bash '$RALPH_DIR/.stream-filter.sh' '$LOG_FILE'" Enter

  # Bottom-right: plan + state watch
  tmux send-keys -t "$TMUX_SESSION:.2" \
    "watch -n 5 'echo \"=== State ===\"; cat \"$STATE_FILE\" 2>/dev/null; echo; echo \"=== Plan ===\"; head -30 \"$PLAN_FILE\" 2>/dev/null'" Enter

  # Left pane: re-exec ralph without --tmux, with --quiet
  local cmd
  cmd="$(printf '%q' "$SCRIPT_DIR/ralph.sh")"
  for arg in "${RALPH_ORIG_ARGS[@]+"${RALPH_ORIG_ARGS[@]}"}"; do
    [[ "$arg" == "--tmux" ]] && continue
    cmd+=" $(printf '%q' "$arg")"
  done
  cmd+=" --quiet"

  tmux send-keys -t "$TMUX_SESSION:.0" \
    "_RALPH_TMUX_SESSION=$TMUX_SESSION $cmd; tmux kill-session -t '$TMUX_SESSION' 2>/dev/null" Enter
  tmux select-pane -t "$TMUX_SESSION:.0"

  _TMUX_OUTER=true
  tmux attach-session -t "$TMUX_SESSION"
  exit 0
}

# --- State helpers ---
read_state() {
  if command -v jq &>/dev/null; then
    jq -r ".$1" "$STATE_FILE"
  else
    local key="$1"
    grep "\"$key\"" "$STATE_FILE" | sed 's/.*: *"\?\([^",}]*\)"\?.*/\1/'
  fi
}

write_state() {
  local key="$1" value="$2"
  if command -v jq &>/dev/null; then
    local tmp
    tmp=$(mktemp)
    jq --arg v "$value" ".$key = (\$v | try tonumber catch \$v)" "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"
  else
    if echo "$value" | grep -qE '^[0-9]+$'; then
      sed -i "s/\"$key\": *[^,}]*/\"$key\": $value/" "$STATE_FILE"
    else
      sed -i "s/\"$key\": *[^,}]*/\"$key\": \"$value\"/" "$STATE_FILE"
    fi
  fi
}

# --- Signal file mechanism ---
clear_signal() {
  rm -f "$SIGNAL_FILE"
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
  [[ -f "$SIGNAL_FILE" ]] && grep -q "$SIGNAL_TOKEN" "$SIGNAL_FILE"
}

check_all_complete() {
  [[ -f "$SIGNAL_FILE" ]] && grep -q "$ALL_COMPLETE_TOKEN" "$SIGNAL_FILE"
}

check_current_task() {
  [[ -f "$SIGNAL_FILE" ]] && grep -q "$CURRENT_TASK_TOKEN" "$SIGNAL_FILE"
}

read_current_task() {
  [[ -f "$SIGNAL_FILE" ]] && grep "$CURRENT_TASK_TOKEN" "$SIGNAL_FILE" | sed "s/.*$CURRENT_TASK_TOKEN *//" | head -1
}

read_signal_summary() {
  [[ -f "$SIGNAL_FILE" ]] && grep "$SIGNAL_TOKEN" "$SIGNAL_FILE" | sed "s/.*$SIGNAL_TOKEN *//" | head -1
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
# Runs claude in the project dir. Polls the signal file inline.
# When the signal is detected OR claude exits, we proceed.
run_claude() {
  local prompt="$1"
  local feedback="${2:-}"
  local claude_pid tail_pid
  tail_pid=""

  clear_signal

  # Build the prompt that includes ralph loop context
  local full_prompt
  full_prompt=$(build_prompt "$prompt" "$feedback")

  # Launch claude in background
  # Use stream-json output format so output flows to the log file in real-time
  # (default text format batches all output until exit)
  cd "$WORK_DIR"
  claude --print --verbose --output-format stream-json \
    --add-dir "$WORK_DIR" \
    --permission-mode acceptEdits \
    --allowedTools "Bash" \
    -p "$full_prompt" < /dev/null >> "$LOG_FILE" 2>&1 &
  claude_pid=$!
  log "Claude started (PID: $claude_pid)"

  # Stream parsed output to terminal unless --quiet
  if [[ "$QUIET" == false ]]; then
    if command -v jq &>/dev/null; then
      write_stream_filter
      bash "$RALPH_DIR/.stream-filter.sh" "$LOG_FILE" &
    else
      tail -f -n 0 "$LOG_FILE" &
    fi
    tail_pid=$!
  fi

  # Poll for completion signal or Claude exit (inline, no subshell —
  # a background watcher subshell inherits set -e and can die silently
  # on harmless races between check_current_task and read_current_task)
  local task_logged=false signal_detected=false
  while kill -0 "$claude_pid" 2>/dev/null; do
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
      kill "$claude_pid" 2>/dev/null || true
      sleep 2
      kill -0 "$claude_pid" 2>/dev/null && kill -9 "$claude_pid" 2>/dev/null || true
      signal_detected=true
      break
    fi
    sleep "$WATCHER_INTERVAL"
  done

  # Reap claude process
  wait "$claude_pid" 2>/dev/null || true

  # Clean up stream filter and its children (tail, jq)
  if [[ -n "$tail_pid" ]]; then
    pkill -P "$tail_pid" 2>/dev/null || true
    kill "$tail_pid" 2>/dev/null || true
    wait "$tail_pid" 2>/dev/null || true
  fi

  # Check if signal was written (claude may have exited after writing it)
  if check_signal || check_all_complete; then
    [[ "$signal_detected" == false ]] && log_task_success "Task completed via signal"
    return 0
  fi

  log "Claude exited (no completion signal)"
  return 0
}

# --- Build prompt for Claude ---
build_prompt() {
  local task_prompt="$1"
  local feedback="${2:-}"
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
  result+=$(<"$PROMPTS_DIR/signal.md")

  if [[ -n "$feedback" ]]; then
    result+=$'\n\n## User feedback\nThe user has provided the following feedback. Incorporate this into your approach:\n\n'
    result+="$feedback"
  fi

  local task_instructions
  task_instructions=$(task_execution_instructions)
  result="${result//\{\{TASK_INSTRUCTIONS\}\}/$task_instructions}"

  result="${result//\{\{WORK_DIR\}\}/$WORK_DIR}"
  result="${result//\{\{RALPH_DIR\}\}/$RALPH_DIR}"
  result="${result//\{\{PLAN_FILE\}\}/$PLAN_FILE}"
  result="${result//\{\{SIGNAL_FILE\}\}/$SIGNAL_FILE}"
  result="${result//\{\{SIGNAL_TOKEN\}\}/$SIGNAL_TOKEN}"
  result="${result//\{\{CURRENT_TASK_TOKEN\}\}/$CURRENT_TASK_TOKEN}"
  result="${result//\{\{ALL_COMPLETE_TOKEN\}\}/$ALL_COMPLETE_TOKEN}"
  result="${result//\{\{TASK_PROMPT\}\}/$task_prompt}"

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

  if [[ -f "$PLAN_FILE" ]] && [[ "$RESUME" == true ]]; then
    log "Existing plan found, resuming"
    return 0
  fi

  # Interactive planning: launch Claude for the user to define spec + plan
  if [[ ! -f "$PLAN_FILE" && "$SKIP_PLANNING" != true ]]; then
    log "Starting interactive planning session..."
    log "Chat with Claude to define your spec and plan. Exit when done."

    local interactive_prompt
    interactive_prompt=$(<"$PROMPTS_DIR/interactive-planning.md")
    interactive_prompt="${interactive_prompt//\{\{WORK_DIR\}\}/$WORK_DIR}"
    interactive_prompt="${interactive_prompt//\{\{RALPH_DIR\}\}/$RALPH_DIR}"
    interactive_prompt="${interactive_prompt//\{\{PLAN_FILE\}\}/$PLAN_FILE}"

    cd "$WORK_DIR"
    claude --add-dir "$WORK_DIR" \
      --permission-mode acceptEdits \
      --allowedTools "Bash" \
      --system-prompt "$interactive_prompt" || true

    log "Interactive planning session ended."
  fi

  # If interactive session created a plan, use it
  local total
  total=$(count_total)
  if [[ "$TASK_BACKEND" == "bd" && "$total" -gt 0 ]]; then
    write_state "status" "planned"
    log_task_success "Plan created with $total tasks"
    return 0
  elif [[ -f "$PLAN_FILE" ]]; then
    if (( total == 0 )); then
      log_task_error "Plan file exists but contains no tasks in checkbox format (- [ ] ...)"
      log_error "Re-run to try again, or provide a plan with --plan-file"
      rm "$PLAN_FILE"
      exit 1
    fi
    write_state "status" "planned"
    log_task_success "Plan created with $total tasks"
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
  planning_prompt="${planning_prompt//\{\{SIGNAL_TOKEN\}\}/$SIGNAL_TOKEN}"
  planning_prompt="${planning_prompt//\{\{SIGNAL_FILE\}\}/$SIGNAL_FILE}"

  run_claude "$planning_prompt"

  total=$(count_total)
  if [[ "$TASK_BACKEND" == "bd" ]]; then
    if (( total == 0 )); then
      log_task_error "Planning failed - no tasks created"
      exit 1
    fi
    write_state "status" "planned"
    log_task_success "Plan created with $total tasks"
  else
    if [[ ! -f "$PLAN_FILE" ]]; then
      log_task_error "Planning failed - no plan.md created"
      exit 1
    fi
    if (( total == 0 )); then
      log_task_error "Plan file exists but contains no tasks in checkbox format (- [ ] ...)"
      log_error "Re-run to try again, or provide a plan with --plan-file"
      rm "$PLAN_FILE"
      exit 1
    fi
    write_state "status" "planned"
    log_task_success "Plan created with $total tasks"
  fi
}

# --- Response analyzer ---
# Counters for multi-iteration detection (reset per execution phase)
_stagnant_count=0
_test_only_count=0
_stuck_count=0

# analyze_iteration LOG_FILE START_LINE HEAD_BEFORE
# Sets ANALYSIS_RESULT to one of: continue, warn:<reason>, halt:<reason>
# Updates global counters for multi-iteration detection
analyze_iteration() {
  local log_file="$1"
  local start_line="$2"
  local head_before="$3"

  ANALYSIS_RESULT="continue"
  ANALYSIS_DETAIL=""

  local iter_log
  iter_log=$(tail -n "+${start_line}" "$log_file" 2>/dev/null || true)

  if [[ -z "$iter_log" ]]; then
    return
  fi

  # --- Permission denial detection (3+ in single iteration → halt) ---
  local perm_matches=""
  perm_matches=$(grep -iE 'permission denied|cannot write|blocked by sandbox|not allowed' <<< "$iter_log" | head -5 || true)
  local perm_count=0
  if [[ -n "$perm_matches" ]]; then
    perm_count=$(echo "$perm_matches" | wc -l | tr -d ' ')
  fi
  if (( perm_count >= 3 )); then
    ANALYSIS_DETAIL="$perm_matches"
    ANALYSIS_RESULT="halt:permission_denied"
    return
  fi

  # --- Stuck loop detection ---
  local stuck_detected=false

  if grep -qiE "I'm blocked|I cannot proceed|unable to complete" <<< "$iter_log"; then
    stuck_detected=true
  fi

  if [[ "$stuck_detected" == false ]]; then
    local max_repeats=0
    if command -v jq &>/dev/null; then
      max_repeats=$(jq -r '
          select(.type == "assistant") |
          .message.content[]? |
          select(.type == "tool_use") |
          (.name + ":" + (.input.command // .input.file_path // .input.pattern // ""))
        ' <<< "$iter_log" 2>/dev/null | sort | uniq -c | sort -rn | head -1 | awk '{print $1+0}') || true
    else
      max_repeats=$(grep -oE '"(command|file_path)"\s*:\s*"[^"]*"' <<< "$iter_log" | \
        sort | uniq -c | sort -rn | head -1 | awk '{print $1+0}') || true
    fi
    max_repeats=${max_repeats:-0}
    if (( max_repeats >= 3 )); then
      stuck_detected=true
    fi
  fi

  if [[ "$stuck_detected" == true ]]; then
    _stuck_count=$((_stuck_count + 1))
    if (( _stuck_count >= 2 )); then
      ANALYSIS_RESULT="halt:stuck_loop"
      return
    fi
    ANALYSIS_RESULT="warn:stuck_indicators_detected"
    return
  else
    _stuck_count=0
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
    if (( _stagnant_count >= 3 )); then
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
        if (( _test_only_count >= 3 )); then
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

  # Rebase onto default branch to pick up changes merged since worktree was created
  if [[ -n "$WORKTREE_BRANCH" && "$WORK_DIR" != "$PROJECT_DIR" ]]; then
    local default_branch
    default_branch=$(git -C "$PROJECT_DIR" symbolic-ref refs/remotes/origin/HEAD 2>/dev/null | sed 's|refs/remotes/origin/||') || true
    default_branch=${default_branch:-main}
    git -C "$WORK_DIR" fetch origin "$default_branch" 2>/dev/null || true
    if git -C "$WORK_DIR" rebase --update-refs "origin/$default_branch" 2>/dev/null; then
      log "Rebased onto origin/$default_branch"
    else
      git -C "$WORK_DIR" rebase --abort 2>/dev/null || true
      log_warn "Rebase onto $default_branch failed (conflicts), continuing on current base"
    fi
  fi

  init_call_tracking

  # Reset response analyzer counters
  _stagnant_count=0
  _test_only_count=0
  _stuck_count=0

  local run_iteration=0
  local iteration
  iteration=$(read_state "iteration")
  iteration=${iteration:-0}

  while (( run_iteration < MAX_ITERATIONS )); do
    # Check stop file
    if [[ -f "$STOP_FILE" ]]; then
      log_warn "Stop file detected - halting"
      write_state "status" "stopped"
      break
    fi

    # Check remaining tasks
    if ! has_remaining_tasks; then
      if (( run_iteration == 0 )) && (( $(count_total) == 0 )); then
        log_task_error "Plan has no tasks in checkbox format"
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
    fi

    local next_task completed remaining total
    next_task=$(get_next_task)
    completed=$(count_completed)
    remaining=$(count_remaining)
    total=$(count_total)

    log_phase "--- Iteration $run_iteration/$MAX_ITERATIONS ($iteration total) [${completed}/${total} done] ---"
    log_task "Next task: $next_task"

    # Update state
    write_state "iteration" "$iteration"
    write_state "status" "running"
    write_state "last_task" "$next_task"
    rename_branch_for_task "$next_task"

    # Build task prompt
    local task_id
    task_id=$(get_next_task_id)
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
    log_start_line=$(( $(wc -l < "$LOG_FILE" 2>/dev/null || echo 0) + 1 ))
    local head_before
    head_before=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null || echo "")

    # Read any queued user feedback
    local feedback=""
    feedback=$(read_feedback) || true
    if [[ -n "$feedback" ]]; then
      log "Feedback: \"$feedback\""
    fi

    # Run claude for this task
    if ! run_claude "$task_prompt" "$feedback"; then
      log_warn "Claude failed on iteration $run_iteration, continuing..."
    fi
    increment_call_count

    # Clear feedback only after Claude has consumed it
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
    log_task "Iteration $run_iteration complete. ${completed}/${total} tasks done."

    # Analyze iteration for problems
    analyze_iteration "$LOG_FILE" "$log_start_line" "$head_before"
    case "$ANALYSIS_RESULT" in
      halt:*)
        log_error "Halting: ${ANALYSIS_RESULT#halt:}"
        if [[ -n "$ANALYSIS_DETAIL" ]]; then
          echo "$ANALYSIS_DETAIL" | while IFS= read -r detail_line; do
            log_error "  $detail_line"
          done
        fi
        write_state "status" "halted_${ANALYSIS_RESULT#halt:}"
        break
        ;;
      warn:*)
        log_warn "Analysis: ${ANALYSIS_RESULT#warn:}"
        ;;
    esac
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
  if [[ "${_RALPH_TMUX_SESSION:-}" != "" ]]; then
    extra_args="$extra_args --tmux"
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
  log "Iterations: $iteration total"

  local completed remaining total
  completed=$(count_completed)
  remaining=$(count_remaining)
  total=$(count_total)
  log_task "Tasks: $completed/$total completed, $remaining remaining"

  log "Log:        $LOG_FILE"
  log "Plan:       $PLAN_FILE"

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
  # If this is the outer tmux process, just kill the session
  if [[ "$_TMUX_OUTER" == true ]]; then
    [[ -n "$TMUX_SESSION" ]] && tmux kill-session -t "$TMUX_SESSION" 2>/dev/null || true
    return
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
trap cleanup EXIT

# --- Main ---
main() {
  init_ralph_dir

  if [[ "$USE_TMUX" == true ]]; then
    setup_tmux
  fi

  setup_worktree
  init_task_backend

  log_phase "Ralph Loop v${VERSION}"
  log "Project: $PROJECT_DIR"
  [[ "$WORK_DIR" != "$PROJECT_DIR" ]] && log "Worktree: $WORK_DIR"
  log "Task backend: $(task_label)"
  log "Max iterations: $MAX_ITERATIONS"

  write_state "started_at" "$(date -u +'%Y-%m-%dT%H:%M:%SZ')"

  # Planning
  run_planning

  if [[ "$PLAN_ONLY" == true ]]; then
    log "Plan-only mode, exiting"
    exit 0
  fi

  # Execution
  run_execution
}

[[ "${RALPH_SOURCED:-}" == true ]] || main
