#!/usr/bin/env bash
set -euo pipefail

# Ralph Loop - Autonomous Claude Code task iteration
# Runs Claude Code CLI in fresh-context iterations against a project repo.
# Prompts live in ./prompts/ — edit them to change Claude's behavior.

VERSION="0.1.0"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROMPTS_DIR="$SCRIPT_DIR/prompts"

# --- Defaults ---
PROJECT_DIR="$(pwd)"
MAX_ITERATIONS=20
PROMPT_OVERRIDE=""
RESUME=false
PLAN_ONLY=false
SIGNAL_TOKEN="###RALPH_TASK_COMPLETE###"
CURRENT_TASK_TOKEN="###RALPH_CURRENT_TASK###"
WATCHER_INTERVAL=2  # seconds between signal checks
EXTERNAL_PLAN=false
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
_BRANCH_SEQ=0
LOG_FILE="/dev/null"  # real path set after dir resolution

# --- Colors ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

# --- Logging ---
log()         { echo -e "${CYAN}[ralph]${NC} $*" | tee -a "$LOG_FILE"; }
log_success() { echo -e "${GREEN}[ralph]${NC} $*" | tee -a "$LOG_FILE"; }
log_warn()    { echo -e "${YELLOW}[ralph]${NC} $*" | tee -a "$LOG_FILE"; }
log_error()   { echo -e "${RED}[ralph]${NC} $*" | tee -a "$LOG_FILE"; }
log_phase()   { echo -e "${BOLD}${BLUE}[ralph]${NC} ${BOLD}$*${NC}" | tee -a "$LOG_FILE"; }

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
  --plan-file <path>     Use external plan file (skip planning, lighter prompt)
  --resume               Resume from previous state
  --plan                 Run planning phase only
  -q, --quiet            Suppress Claude output streaming (log only)
  --no-worktree          Run directly in project dir (no git worktree isolation)
  --calls-per-hour <N>   Max Claude calls per hour (default: 80)
  --tmux                 Run in tmux 3-pane layout (status / output / plan)
  -h, --help             Show this help

${BOLD}EXAMPLES:${NC}
  ralph.sh ~/myproject -n 20
  ralph.sh --resume
  ralph.sh -p "Fix all failing tests"
  ralph.sh . --plan-file docs/TODO.md

${BOLD}HOW IT WORKS:${NC}
  1. Planning: Claude reads the repo and creates .ralph/plan.md with atomic tasks
  2. Execution: Each task runs in a fresh Claude context (~200k tokens)
  3. Completion: Claude writes a signal file when each task is done
  4. Repeat: Loop continues until all tasks complete or iteration cap is hit

${BOLD}CONTROL:${NC}
  Create .ralph/stop to halt after the current iteration.
  The repo's CLAUDE.md is the source of truth for Claude's behavior.
EOF
}

# --- Save original args (for tmux re-exec) ---
RALPH_ORIG_ARGS=("$@")

# --- Parse args ---
while [[ $# -gt 0 ]]; do
  case "$1" in
    -d|--dir)       PROJECT_DIR="$2"; shift 2 ;;
    -n|--max)       MAX_ITERATIONS="$2"; shift 2 ;;
    -p|--prompt)    PROMPT_OVERRIDE="$2"; shift 2 ;;
    --plan-file)    PLAN_FILE_ARG="$2"; EXTERNAL_PLAN=true; shift 2 ;;
    --resume)       RESUME=true; shift ;;
    --plan)         PLAN_ONLY=true; shift ;;
    -q|--quiet)     QUIET=true; shift ;;
    --no-worktree)  USE_WORKTREE=false; shift ;;
    --calls-per-hour) CALLS_PER_HOUR="$2"; shift 2 ;;
    --tmux)         USE_TMUX=true; shift ;;
    -h|--help)      usage; exit 0 ;;
    -*)             log_error "Unknown option: $1"; usage; exit 1 ;;
    *)              PROJECT_DIR="$1"; shift ;;
  esac
done

# --- Resolve paths ---
PROJECT_DIR="$(cd "$PROJECT_DIR" && pwd)"
RALPH_DIR="$PROJECT_DIR/.ralph"
if [[ "$EXTERNAL_PLAN" == true ]]; then
  # Resolve plan-file to absolute path
  if [[ "$PLAN_FILE_ARG" == /* ]]; then
    PLAN_FILE="$PLAN_FILE_ARG"
  else
    PLAN_FILE="$(cd "$(dirname "$PROJECT_DIR/$PLAN_FILE_ARG")" && pwd)/$(basename "$PLAN_FILE_ARG")"
  fi
else
  PLAN_FILE="$RALPH_DIR/plan.md"
fi
ORIG_PLAN_FILE="$PLAN_FILE"
STATE_FILE="$RALPH_DIR/state.json"
SIGNAL_FILE="$RALPH_DIR/signal"
STOP_FILE="$RALPH_DIR/stop"
LOG_FILE="$RALPH_DIR/loop.log"
RESUME_SCRIPT="$RALPH_DIR/resume.sh"

# --- Init .ralph directory ---
init_ralph_dir() {
  mkdir -p "$RALPH_DIR"
  touch "$LOG_FILE"

  if [[ ! -f "$STATE_FILE" ]] || [[ "$RESUME" == false ]]; then
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
    log_warn "Not a git repo, skipping worktree"
    return
  fi

  # On resume, reuse existing worktree if stored in state
  if [[ "$RESUME" == true ]]; then
    local stored_worktree
    stored_worktree=$(read_state "worktree_dir")
    if [[ -n "$stored_worktree" && "$stored_worktree" != "null" && -d "$stored_worktree" ]]; then
      WORK_DIR="$stored_worktree"
      WORKTREE_BRANCH=$(read_state "worktree_branch")
      PROJECT_NAME=$(basename "$PROJECT_DIR")
      local existing
      existing=$(git -C "$PROJECT_DIR" branch --list "ralph/$PROJECT_NAME/*" 2>/dev/null | wc -l | tr -d ' ')
      _BRANCH_SEQ=$((existing))
      SIGNAL_FILE="$WORK_DIR/.ralph-signal"
      log "Resuming in worktree: $WORK_DIR (branch: $WORKTREE_BRANCH)"
      remap_plan_file
      return
    fi
  fi

  PROJECT_NAME=$(basename "$PROJECT_DIR")
  local existing
  existing=$(git -C "$PROJECT_DIR" branch --list "ralph/$PROJECT_NAME/*" 2>/dev/null | wc -l | tr -d ' ')
  _BRANCH_SEQ=$((existing + 1))

  WORKTREE_BRANCH="ralph/$PROJECT_NAME/$(printf "%02d" $_BRANCH_SEQ)"
  WORK_DIR="$RALPH_DIR/worktrees/ralph-${PROJECT_NAME}-$(printf "%02d" $_BRANCH_SEQ)"

  mkdir -p "$RALPH_DIR/worktrees"
  git -C "$PROJECT_DIR" worktree add -b "$WORKTREE_BRANCH" "$WORK_DIR" HEAD
  log "Worktree: $WORK_DIR (branch: $WORKTREE_BRANCH)"

  write_state "worktree_dir" "$WORK_DIR"
  write_state "worktree_branch" "$WORKTREE_BRANCH"

  SIGNAL_FILE="$WORK_DIR/.ralph-signal"
  remap_plan_file
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

  local new_branch="${WORKTREE_BRANCH}-${slug}"
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

  _BRANCH_SEQ=$((_BRANCH_SEQ + 1))
  local new_branch="ralph/$PROJECT_NAME/$(printf "%02d" $_BRANCH_SEQ)"

  if git -C "$WORK_DIR" checkout -b "$new_branch" 2>/dev/null; then
    WORKTREE_BRANCH="$new_branch"
    write_state "worktree_branch" "$WORKTREE_BRANCH"
    _BRANCH_RENAMED=false
    log "Branch: $WORKTREE_BRANCH (from previous iteration)"
  fi
}

remap_plan_file() {
  if [[ "$EXTERNAL_PLAN" == true && "$WORK_DIR" != "$PROJECT_DIR" ]]; then
    local rel_plan="${PLAN_FILE#$PROJECT_DIR/}"
    if [[ "$rel_plan" == /* ]]; then
      rel_plan="$(basename "$PLAN_FILE")"
    fi
    local worktree_plan="$WORK_DIR/$rel_plan"
    # On resume, the worktree already has the plan file (possibly modified by
    # previous iterations). Only copy on first run to seed the worktree.
    if [[ "$RESUME" != true ]]; then
      mkdir -p "$(dirname "$worktree_plan")"
      cp "$PLAN_FILE" "$worktree_plan"
    fi
    PLAN_FILE="$worktree_plan"
  fi
}

# --- Stream filter helper ---
write_stream_filter() {
  cat > "$RALPH_DIR/.stream-filter.sh" <<'STREAM'
#!/usr/bin/env bash
tail -f "$1" | jq --raw-input --join-output --unbuffered '
  fromjson? // empty |
  if .type == "assistant" then
    [.message.content[]? |
      if .type == "text" then .text
      elif .type == "tool_use" then
        if .name == "TodoWrite" then
          ([.input.todos[]? | .content] | if length == 0 then "[]"
            else join(", ") end) as $items |
          "\n[TodoWrite] " + $items + "\n"
        else
          (.input.file_path // .input.command // .input.pattern //
            .input.query // .input.url // .input.description //
            null) as $target |
          if $target then "\n[" + .name + "] " + $target + "\n"
          else "\n[" + .name + "]\n"
          end
        end
      else empty end
    ] | join("")
  elif .type == "result" then
    "\n[done]\n"
  else empty end
' 2>/dev/null
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
  local relaunch_args=()
  for arg in "${RALPH_ORIG_ARGS[@]}"; do
    [[ "$arg" == "--tmux" ]] && continue
    relaunch_args+=("$arg")
  done
  local cmd
  cmd="$(printf '%q' "$SCRIPT_DIR/ralph.sh")"
  for arg in "${relaunch_args[@]}"; do
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

# --- Task helpers ---
has_remaining_tasks() {
  [[ -f "$PLAN_FILE" ]] || return 1
  if [[ "$EXTERNAL_PLAN" == true ]]; then
    return 0
  else
    grep -qE '^\s*- \[ \]' "$PLAN_FILE"
  fi
}

count_completed() {
  [[ -f "$PLAN_FILE" ]] && { grep -cE '^\s*- \[x\]' "$PLAN_FILE" 2>/dev/null || true; } || echo 0
}

count_remaining() {
  [[ -f "$PLAN_FILE" ]] && { grep -cE '^\s*- \[ \]' "$PLAN_FILE" 2>/dev/null || true; } || echo 0
}

count_total() {
  [[ -f "$PLAN_FILE" ]] && { grep -cE '^\s*- \[[ x]\]' "$PLAN_FILE" 2>/dev/null || true; } || echo 0
}

get_next_task() {
  [[ -f "$PLAN_FILE" ]] && grep -m1 -E '^\s*- \[ \]' "$PLAN_FILE" | sed 's/^\s*- \[ \] *//'
}

# --- Signal file mechanism ---
clear_signal() {
  rm -f "$SIGNAL_FILE"
}

check_signal() {
  [[ -f "$SIGNAL_FILE" ]] && grep -q "$SIGNAL_TOKEN" "$SIGNAL_FILE"
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
  local claude_pid tail_pid
  tail_pid=""

  clear_signal

  # Build the prompt that includes ralph loop context
  local full_prompt
  full_prompt=$(build_prompt "$prompt")

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
        log "Working on: $task_desc"
        write_state "last_task" "$task_desc"
        rename_branch_for_task "$task_desc"
        task_logged=true
      fi
    fi
    if check_signal; then
      local summary
      summary=$(read_signal_summary) || true
      log_success "Completed: ${summary:-task done}"
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

  # Clean up stream filter
  [[ -n "$tail_pid" ]] && kill "$tail_pid" 2>/dev/null || true
  [[ -n "$tail_pid" ]] && wait "$tail_pid" 2>/dev/null || true

  # Check if signal was written (claude may have exited after writing it)
  if check_signal; then
    [[ "$signal_detected" == false ]] && log_success "Task completed via signal"
    return 0
  fi

  log "Claude exited (no completion signal)"
  return 0
}

# --- Build prompt for Claude ---
build_prompt() {
  local task_prompt="$1"
  local template_file

  if [[ "$EXTERNAL_PLAN" == true ]]; then
    template_file="$PROMPTS_DIR/external.md"
  else
    template_file="$PROMPTS_DIR/internal.md"
  fi

  if [[ ! -f "$template_file" ]]; then
    log_error "Prompt template not found: $template_file"
    exit 1
  fi

  local result
  result=$(<"$template_file")
  result+=$'\n'
  result+=$(<"$PROMPTS_DIR/signal.md")

  result="${result//\{\{WORK_DIR\}\}/$WORK_DIR}"
  result="${result//\{\{RALPH_DIR\}\}/$RALPH_DIR}"
  result="${result//\{\{PLAN_FILE\}\}/$PLAN_FILE}"
  result="${result//\{\{SIGNAL_FILE\}\}/$SIGNAL_FILE}"
  result="${result//\{\{SIGNAL_TOKEN\}\}/$SIGNAL_TOKEN}"
  result="${result//\{\{CURRENT_TASK_TOKEN\}\}/$CURRENT_TASK_TOKEN}"
  result="${result//\{\{TASK_PROMPT\}\}/$task_prompt}"

  printf '%s' "$result"
}

# --- Planning phase ---
run_planning() {
  log_phase "=== PHASE 1: PLANNING ==="

  if [[ "$EXTERNAL_PLAN" == true ]]; then
    log "Using external plan file: $PLAN_FILE"
    if [[ ! -f "$PLAN_FILE" ]]; then
      log_error "External plan file not found: $PLAN_FILE"
      exit 1
    fi
    return 0
  fi

  if [[ -f "$PLAN_FILE" ]] && [[ "$RESUME" == true ]]; then
    log "Existing plan found, resuming"
    return 0
  fi

  local planning_context
  if [[ -n "$PROMPT_OVERRIDE" ]]; then
    planning_context="$PROMPT_OVERRIDE"
  else
    planning_context="Read this repository and understand what needs to be done.
Look at CLAUDE.md, prompt.md, README.md, or any task-related files for context.

Create a plan of atomic, self-contained tasks."
  fi

  local planning_prompt
  planning_prompt=$(<"$PROMPTS_DIR/planning.md")
  planning_prompt="${planning_prompt//\{\{PLANNING_CONTEXT\}\}/$planning_context}"
  planning_prompt="${planning_prompt//\{\{PLAN_FILE\}\}/$PLAN_FILE}"
  planning_prompt="${planning_prompt//\{\{SIGNAL_TOKEN\}\}/$SIGNAL_TOKEN}"
  planning_prompt="${planning_prompt//\{\{SIGNAL_FILE\}\}/$SIGNAL_FILE}"

  run_claude "$planning_prompt"

  if [[ ! -f "$PLAN_FILE" ]]; then
    log_error "Planning failed - no plan.md created"
    exit 1
  fi

  local total
  total=$(count_total)
  write_state "status" "planned"
  log_success "Plan created with $total tasks"
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

  local iter_log
  iter_log=$(tail -n "+${start_line}" "$log_file" 2>/dev/null || true)

  if [[ -z "$iter_log" ]]; then
    return
  fi

  # --- Permission denial detection (3+ in single iteration → halt) ---
  local perm_count=0
  perm_count=$(grep -ciE 'permission denied|cannot write|blocked by sandbox|not allowed' <<< "$iter_log" || true)
  if (( perm_count >= 3 )); then
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

  if check_signal; then
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
      local non_test_files
      non_test_files=$(echo "$changed_files" | grep -viE '(test|spec|_test\.|test_)' || true)

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
    if git -C "$WORK_DIR" rebase "origin/$default_branch" 2>/dev/null; then
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
      log_success "All tasks complete!"
      write_state "status" "completed"
      break
    fi

    run_iteration=$((run_iteration + 1))
    iteration=$((iteration + 1))

    # Each iteration gets its own branch, stacked on the previous
    if (( run_iteration > 1 )); then
      rotate_branch
    fi

    if [[ "$EXTERNAL_PLAN" == true ]]; then
      local bullet_count
      bullet_count=$(grep -cE '^\s*[-*]' "$PLAN_FILE" 2>/dev/null || true)
      bullet_count=${bullet_count:-0}
      log_phase "--- Iteration $run_iteration/$MAX_ITERATIONS ($iteration total) ---"

      # Update state
      write_state "iteration" "$iteration"
      write_state "status" "running"

      # Snapshot plan file before iteration
      cp "$PLAN_FILE" "$RALPH_DIR/.plan_snapshot" 2>/dev/null || true

      # External mode: no task prompt (agent picks via project docs)
      local task_prompt=""

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

      # Run claude
      if ! run_claude "$task_prompt"; then
        log_warn "Claude failed on iteration $run_iteration, continuing..."
      fi
      increment_call_count

      # Post-iteration: read signal summary
      local summary=""
      summary=$(read_signal_summary) || true
      if [[ -n "$summary" ]]; then
        log "Summary: $summary"
      fi

      # Post-iteration: diff plan file
      if [[ -f "$RALPH_DIR/.plan_snapshot" ]]; then
        local removed added
        removed=$(diff "$RALPH_DIR/.plan_snapshot" "$PLAN_FILE" 2>/dev/null | grep -c '^<' || true)
        added=$(diff "$RALPH_DIR/.plan_snapshot" "$PLAN_FILE" 2>/dev/null | grep -c '^>' || true)
        if [[ $removed -gt 0 || $added -gt 0 ]]; then
          log "Plan diff: $removed removed, $added added"
        fi
      fi

      log "Iteration $run_iteration complete."

      # Analyze iteration for problems
      analyze_iteration "$LOG_FILE" "$log_start_line" "$head_before"
      case "$ANALYSIS_RESULT" in
        halt:*)
          log_error "Halting: ${ANALYSIS_RESULT#halt:}"
          write_state "status" "halted_${ANALYSIS_RESULT#halt:}"
          break
          ;;
        warn:*)
          log_warn "Analysis: ${ANALYSIS_RESULT#warn:}"
          ;;
      esac
    else
      local next_task completed remaining total
      next_task=$(get_next_task)
      completed=$(count_completed)
      remaining=$(count_remaining)
      total=$(count_total)

      log_phase "--- Iteration $run_iteration/$MAX_ITERATIONS ($iteration total) [${completed}/${total} done] ---"
      log "Next task: $next_task"

      # Update state
      write_state "iteration" "$iteration"
      write_state "status" "running"
      write_state "last_task" "$next_task"
      rename_branch_for_task "$next_task"

      # Build task prompt
      local task_prompt="Complete this task: $next_task"

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

      # Run claude for this task
      if ! run_claude "$task_prompt"; then
        log_warn "Claude failed on iteration $run_iteration, continuing..."
      fi
      increment_call_count

      # Post-iteration: read signal summary
      local summary=""
      summary=$(read_signal_summary) || true
      if [[ -n "$summary" ]]; then
        log "Summary: $summary"
      fi

      # Recount after claude ran
      completed=$(count_completed)
      log "Iteration $run_iteration complete. ${completed}/${total} tasks done."

      # Analyze iteration for problems
      analyze_iteration "$LOG_FILE" "$log_start_line" "$head_before"
      case "$ANALYSIS_RESULT" in
        halt:*)
          log_error "Halting: ${ANALYSIS_RESULT#halt:}"
          write_state "status" "halted_${ANALYSIS_RESULT#halt:}"
          break
          ;;
        warn:*)
          log_warn "Analysis: ${ANALYSIS_RESULT#warn:}"
          ;;
      esac
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
  if [[ "$EXTERNAL_PLAN" == true ]]; then
    extra_args=" --plan-file \"$ORIG_PLAN_FILE\""
  fi
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
exec "$SCRIPT_DIR/ralph.sh" --dir "$PROJECT_DIR" --max "$MAX_ITERATIONS"$extra_args --resume
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

  if [[ "$EXTERNAL_PLAN" == true ]]; then
    local last_task
    last_task=$(read_state "last_task")
    [[ -n "$last_task" && "$last_task" != "null" ]] && log "Last task:  $last_task"
  else
    local completed remaining total
    completed=$(count_completed)
    remaining=$(count_remaining)
    total=$(count_total)
    log "Tasks:      $completed/$total completed, $remaining remaining"
  fi

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
  # Kill any backgrounded processes
  jobs -p | xargs -r kill 2>/dev/null || true
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

  log_phase "Ralph Loop v${VERSION}"
  log "Project: $PROJECT_DIR"
  [[ "$WORK_DIR" != "$PROJECT_DIR" ]] && log "Worktree: $WORK_DIR"
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

main
