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
MAX_ITERATIONS=50
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
WORK_DIR=""
WORKTREE_BRANCH=""
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
      SIGNAL_FILE="$WORK_DIR/.ralph-signal"
      log "Resuming in worktree: $WORK_DIR (branch: $WORKTREE_BRANCH)"
      remap_plan_file
      return
    fi
  fi

  local session_id
  session_id="ralph-$(date +%s)"
  WORKTREE_BRANCH="$session_id"
  WORK_DIR="$RALPH_DIR/worktrees/$session_id"

  mkdir -p "$RALPH_DIR/worktrees"
  git -C "$PROJECT_DIR" worktree add -b "$WORKTREE_BRANCH" "$WORK_DIR" HEAD
  log "Worktree: $WORK_DIR (branch: $WORKTREE_BRANCH)"

  write_state "worktree_dir" "$WORK_DIR"
  write_state "worktree_branch" "$WORKTREE_BRANCH"

  SIGNAL_FILE="$WORK_DIR/.ralph-signal"
  remap_plan_file
}

remap_plan_file() {
  if [[ "$EXTERNAL_PLAN" == true && "$WORK_DIR" != "$PROJECT_DIR" ]]; then
    local rel_plan="${PLAN_FILE#$PROJECT_DIR/}"
    if [[ "$rel_plan" == /* ]]; then
      # Plan file is outside project dir — copy to worktree root
      rel_plan="$(basename "$PLAN_FILE")"
    fi
    local worktree_plan="$WORK_DIR/$rel_plan"
    mkdir -p "$(dirname "$worktree_plan")"
    cp "$PLAN_FILE" "$worktree_plan"
    PLAN_FILE="$worktree_plan"
  fi
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
    local tmp
    tmp=$(mktemp)
    if echo "$value" | grep -qE '^[0-9]+$'; then
      sed "s/\"$key\": *[^,}]*/\"$key\": $value/" "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"
    else
      sed "s/\"$key\": *[^,}]*/\"$key\": \"$value\"/" "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"
    fi
  fi
}

# --- Task helpers ---
has_remaining_tasks() {
  [[ -f "$PLAN_FILE" ]] || return 1
  if [[ "$EXTERNAL_PLAN" == true ]]; then
    grep -qE '^\s*[-*]' "$PLAN_FILE"
  else
    grep -qE '^\s*- \[ \]' "$PLAN_FILE"
  fi
}

count_completed() {
  local count
  count=$(grep -cE '^\s*- \[x\]' "$PLAN_FILE" 2>/dev/null) || true
  echo "${count:-0}"
}

count_remaining() {
  local count
  count=$(grep -cE '^\s*- \[ \]' "$PLAN_FILE" 2>/dev/null) || true
  echo "${count:-0}"
}

count_total() {
  local count
  count=$(grep -cE '^\s*- \[[ x]\]' "$PLAN_FILE" 2>/dev/null) || true
  echo "${count:-0}"
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

# --- Run Claude with watcher ---
# Runs claude in the project dir. A background watcher monitors the signal file.
# When the signal is detected OR claude exits, we proceed.
run_claude() {
  local prompt="$1"
  local claude_pid watcher_pid tail_pid exit_code
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
      tail -f -n 0 "$LOG_FILE" | jq --raw-input --join-output --unbuffered '
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
      ' 2>/dev/null &
    else
      tail -f -n 0 "$LOG_FILE" &
    fi
    tail_pid=$!
  fi

  # Background watcher: polls signal file
  (
    task_logged=false
    while kill -0 "$claude_pid" 2>/dev/null; do
      # Log current task when agent signals it (once)
      if [[ "$task_logged" == false ]] && check_current_task; then
        task_desc=$(read_current_task)
        log "Working on: $task_desc"
        write_state "last_task" "$task_desc"
        task_logged=true
      fi
      # Kill on completion signal
      if check_signal; then
        summary=$(read_signal_summary)
        log_success "Completed: ${summary:-task done}"
        kill "$claude_pid" 2>/dev/null || true
        [[ -n "$tail_pid" ]] && kill "$tail_pid" 2>/dev/null || true
        exit 0
      fi
      sleep "$WATCHER_INTERVAL"
    done
  ) &
  watcher_pid=$!

  # Wait for claude to finish
  exit_code=0
  wait "$claude_pid" 2>/dev/null || exit_code=$?

  # Clean up tail and watcher
  [[ -n "$tail_pid" ]] && kill "$tail_pid" 2>/dev/null || true
  [[ -n "$tail_pid" ]] && wait "$tail_pid" 2>/dev/null || true
  kill "$watcher_pid" 2>/dev/null || true
  wait "$watcher_pid" 2>/dev/null || true

  # Check if signal was written (claude may have exited after writing it)
  if check_signal; then
    log_success "Task completed via signal"
    return 0
  fi

  if [[ $exit_code -eq 0 ]]; then
    log "Claude exited cleanly (no signal)"
    return 0
  else
    log_warn "Claude exited with code $exit_code"
    return $exit_code
  fi
}

# --- Build prompt for Claude ---
build_prompt() {
  local task_prompt="$1"
  local template

  if [[ "$EXTERNAL_PLAN" == true ]]; then
    template="$PROMPTS_DIR/external.md"
  else
    template="$PROMPTS_DIR/internal.md"
  fi

  if [[ ! -f "$template" ]]; then
    log_error "Prompt template not found: $template"
    exit 1
  fi

  local escaped_task
  escaped_task=$(printf '%s' "$task_prompt" | sed 's/[&|\]/\\&/g')

  local subs=(
    -e "s|{{WORK_DIR}}|$WORK_DIR|g"
    -e "s|{{RALPH_DIR}}|$RALPH_DIR|g"
    -e "s|{{PLAN_FILE}}|$PLAN_FILE|g"
    -e "s|{{SIGNAL_FILE}}|$SIGNAL_FILE|g"
    -e "s|{{SIGNAL_TOKEN}}|$SIGNAL_TOKEN|g"
    -e "s|{{CURRENT_TASK_TOKEN}}|$CURRENT_TASK_TOKEN|g"
    -e "s|{{TASK_PROMPT}}|$escaped_task|g"
  )

  sed "${subs[@]}" "$template"
  echo ""
  sed "${subs[@]}" "$PROMPTS_DIR/signal.md"
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

  local escaped_context
  escaped_context=$(printf '%s' "$planning_context" | sed 's/[&|\]/\\&/g')

  local planning_prompt
  planning_prompt=$(sed \
    -e "s|{{PLANNING_CONTEXT}}|$escaped_context|g" \
    -e "s|{{PLAN_FILE}}|$PLAN_FILE|g" \
    -e "s|{{SIGNAL_TOKEN}}|$SIGNAL_TOKEN|g" \
    -e "s|{{SIGNAL_FILE}}|$SIGNAL_FILE|g" \
    "$PROMPTS_DIR/planning.md")

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

# --- Execution phase ---
run_execution() {
  log_phase "=== PHASE 2: EXECUTION ==="

  local iteration
  iteration=$(read_state "iteration")
  iteration=${iteration:-0}

  while (( iteration < MAX_ITERATIONS )); do
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

    iteration=$((iteration + 1))

    if [[ "$EXTERNAL_PLAN" == true ]]; then
      local bullet_count
      bullet_count=$(grep -cE '^\s*[-*]' "$PLAN_FILE" 2>/dev/null || echo 0)
      log_phase "--- Iteration $iteration/$MAX_ITERATIONS [$bullet_count items in plan] ---"

      # Update state
      write_state "iteration" "$iteration"
      write_state "status" "running"

      # Snapshot plan file before iteration
      cp "$PLAN_FILE" "$RALPH_DIR/.plan_snapshot" 2>/dev/null || true

      # External mode: no task prompt (agent picks via project docs)
      local task_prompt=""

      # Run claude
      if ! run_claude "$task_prompt"; then
        log_warn "Claude failed on iteration $iteration, continuing..."
      fi

      # Post-iteration: read signal summary
      local summary
      summary=$(read_signal_summary)
      if [[ -n "$summary" ]]; then
        log "Summary: $summary"
      fi

      # Post-iteration: diff plan file
      if [[ -f "$RALPH_DIR/.plan_snapshot" ]]; then
        local removed added
        removed=$(diff "$RALPH_DIR/.plan_snapshot" "$PLAN_FILE" 2>/dev/null | grep -c '^<' || echo 0)
        added=$(diff "$RALPH_DIR/.plan_snapshot" "$PLAN_FILE" 2>/dev/null | grep -c '^>' || echo 0)
        if [[ $removed -gt 0 || $added -gt 0 ]]; then
          log "Plan diff: $removed removed, $added added"
        fi
      fi

      log "Iteration $iteration complete."
    else
      local next_task completed remaining total
      next_task=$(get_next_task)
      completed=$(count_completed)
      remaining=$(count_remaining)
      total=$(count_total)

      log_phase "--- Iteration $iteration/$MAX_ITERATIONS [${completed}/${total} done] ---"
      log "Next task: $next_task"

      # Update state
      write_state "iteration" "$iteration"
      write_state "status" "running"
      write_state "last_task" "$next_task"

      # Build task prompt
      local task_prompt="Complete this task: $next_task"

      # Run claude for this task
      if ! run_claude "$task_prompt"; then
        log_warn "Claude failed on iteration $iteration, continuing..."
      fi

      # Post-iteration: read signal summary
      local summary
      summary=$(read_signal_summary)
      if [[ -n "$summary" ]]; then
        log "Summary: $summary"
      fi

      # Recount after claude ran
      completed=$(count_completed)
      log "Iteration $iteration complete. ${completed}/${total} tasks done."
    fi
    echo ""
  done

  if (( iteration >= MAX_ITERATIONS )); then
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
  log "Iterations: $iteration/$MAX_ITERATIONS"

  if [[ "$EXTERNAL_PLAN" == true ]]; then
    local bullet_count last_task
    bullet_count=$(grep -cE '^\s*[-*]' "$PLAN_FILE" 2>/dev/null || echo 0)
    last_task=$(read_state "last_task")
    log "Items left: $bullet_count"
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

  if [[ -n "$WORKTREE_BRANCH" ]]; then
    log "Worktree:   $WORK_DIR"
    log "Branch:     $WORKTREE_BRANCH"
    log "To merge:   git merge $WORKTREE_BRANCH"
  fi

  if has_remaining_tasks 2>/dev/null; then
    log "Resume:     $RESUME_SCRIPT"
  fi
}

# --- Cleanup on exit ---
cleanup() {
  # Kill any backgrounded processes
  local pids
  pids=$(jobs -p) || true
  [[ -n "$pids" ]] && kill $pids 2>/dev/null || true
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
