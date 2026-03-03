#!/usr/bin/env bash
set -euo pipefail

# Ralph Loop - Autonomous Claude Code task iteration
# Runs Claude Code CLI in fresh-context iterations against a project repo.
# The repo is the source of truth: CLAUDE.md / prompt.md guide Claude's work.

VERSION="0.1.0"

# --- Defaults ---
PROJECT_DIR="$(pwd)"
MAX_ITERATIONS=50
PROMPT_OVERRIDE=""
RESUME=false
PLAN_ONLY=false
SIGNAL_TOKEN="###RALPH_TASK_COMPLETE###"
WATCHER_INTERVAL=2  # seconds between signal checks

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
  ralph.sh [OPTIONS]

${BOLD}OPTIONS:${NC}
  -d, --dir <path>       Project directory (default: cwd)
  -n, --max <N>          Max iterations (default: 50)
  -p, --prompt <text>    Prompt override (otherwise Claude reads repo context)
  --resume               Resume from previous state
  --plan                 Run planning phase only
  -h, --help             Show this help

${BOLD}EXAMPLES:${NC}
  ralph.sh -d ~/myproject -n 20
  ralph.sh --resume
  ralph.sh -p "Fix all failing tests"

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
    --resume)       RESUME=true; shift ;;
    --plan)         PLAN_ONLY=true; shift ;;
    -h|--help)      usage; exit 0 ;;
    *)              log_error "Unknown option: $1"; usage; exit 1 ;;
  esac
done

# --- Resolve paths ---
PROJECT_DIR="$(cd "$PROJECT_DIR" && pwd)"
RALPH_DIR="$PROJECT_DIR/.ralph"
PLAN_FILE="$RALPH_DIR/plan.md"
STATE_FILE="$RALPH_DIR/state.json"
SIGNAL_FILE="$RALPH_DIR/signal"
STOP_FILE="$RALPH_DIR/stop"
LOG_FILE="$RALPH_DIR/loop.log"
RESUME_SCRIPT="$RALPH_DIR/resume.sh"

# --- Init .ralph directory ---
init_ralph_dir() {
  mkdir -p "$RALPH_DIR"
  touch "$LOG_FILE"

  if [[ ! -f "$STATE_FILE" ]]; then
    cat > "$STATE_FILE" <<'STATE'
{
  "iteration": 0,
  "status": "initialized",
  "started_at": null,
  "last_task": null
}
STATE
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
    if echo "$value" | grep -qE '^[0-9]+$'; then
      sed -i "s/\"$key\": *[^,}]*/\"$key\": $value/" "$STATE_FILE"
    else
      sed -i "s/\"$key\": *[^,}]*/\"$key\": \"$value\"/" "$STATE_FILE"
    fi
  fi
}

# --- Task helpers ---
has_remaining_tasks() {
  [[ -f "$PLAN_FILE" ]] && grep -qE '^\s*- \[ \]' "$PLAN_FILE"
}

count_completed() {
  [[ -f "$PLAN_FILE" ]] && grep -cE '^\s*- \[x\]' "$PLAN_FILE" 2>/dev/null || echo 0
}

count_remaining() {
  [[ -f "$PLAN_FILE" ]] && grep -cE '^\s*- \[ \]' "$PLAN_FILE" 2>/dev/null || echo 0
}

count_total() {
  [[ -f "$PLAN_FILE" ]] && grep -cE '^\s*- \[[ x]\]' "$PLAN_FILE" 2>/dev/null || echo 0
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

# --- Run Claude with watcher ---
# Runs claude in the project dir. A background watcher monitors the signal file.
# When the signal is detected OR claude exits, we proceed.
run_claude() {
  local prompt="$1"
  local claude_pid watcher_pid exit_code

  clear_signal

  # Build the prompt that includes ralph loop context
  local full_prompt
  full_prompt=$(build_prompt "$prompt")

  # Launch claude in background
  cd "$PROJECT_DIR"
  claude --print --verbose "$full_prompt" >> "$LOG_FILE" 2>&1 &
  claude_pid=$!
  log "Claude started (PID: $claude_pid)"

  # Background watcher: polls signal file
  (
    while kill -0 "$claude_pid" 2>/dev/null; do
      if check_signal; then
        log_success "Signal detected - task complete"
        kill "$claude_pid" 2>/dev/null || true
        exit 0
      fi
      sleep "$WATCHER_INTERVAL"
    done
  ) &
  watcher_pid=$!

  # Wait for claude to finish
  wait "$claude_pid" 2>/dev/null || true
  exit_code=$?

  # Clean up watcher
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

  cat <<PROMPT
You are running inside a Ralph Loop - an autonomous iteration system.

## Current iteration context
- Project: $PROJECT_DIR
- Ralph state dir: $RALPH_DIR
- Plan file: $PLAN_FILE

## Your task this iteration
$task_prompt

## Rules
1. Focus ONLY on the single task described above.
2. When you complete the task, mark it as done in $PLAN_FILE by changing \`- [ ]\` to \`- [x]\`.
3. After marking the task done, write the completion signal:
   echo "$SIGNAL_TOKEN" > "$SIGNAL_FILE"
4. If you cannot complete the task, leave it unchecked and add notes in $PLAN_FILE.
5. Do NOT work on other tasks - one task per iteration.
6. Read CLAUDE.md if it exists for project-specific guidance.
PROMPT
}

# --- Planning phase ---
run_planning() {
  log_phase "=== PHASE 1: PLANNING ==="

  if [[ -f "$PLAN_FILE" ]] && [[ "$RESUME" == true ]]; then
    log "Existing plan found, resuming"
    return 0
  fi

  local planning_prompt
  if [[ -n "$PROMPT_OVERRIDE" ]]; then
    planning_prompt="$PROMPT_OVERRIDE

Break this into atomic, self-contained tasks. Write the plan to $PLAN_FILE using markdown checkboxes:
- [ ] Task 1 description
- [ ] Task 2 description
...

Each task should be completable in a single Claude session. Be specific and actionable.
After writing the plan, signal completion: echo \"$SIGNAL_TOKEN\" > \"$SIGNAL_FILE\""
  else
    planning_prompt="Read this repository and understand what needs to be done.
Look at CLAUDE.md, prompt.md, README.md, or any task-related files for context.

Create a plan of atomic, self-contained tasks and write it to $PLAN_FILE using markdown checkboxes:
- [ ] Task 1 description
- [ ] Task 2 description
...

Each task should be completable in a single Claude session. Be specific and actionable.
After writing the plan, signal completion: echo \"$SIGNAL_TOKEN\" > \"$SIGNAL_FILE\""
  fi

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

    # Recount after claude ran
    completed=$(count_completed)
    log "Iteration $iteration complete. ${completed}/${total} tasks done."
    echo ""
  done

  if (( iteration >= MAX_ITERATIONS )); then
    log_warn "Max iterations ($MAX_ITERATIONS) reached"
    write_state "status" "max_iterations_reached"
  fi
}

# --- Generate resume script ---
generate_resume_script() {
  cat > "$RESUME_SCRIPT" <<RESUME
#!/usr/bin/env bash
# Ralph Loop - Resume Script
# Generated at: $(date -u +"%Y-%m-%dT%H:%M:%SZ")
exec "$(realpath "$0")" --dir "$PROJECT_DIR" --max "$MAX_ITERATIONS" --resume
RESUME
  chmod +x "$RESUME_SCRIPT"
  log "Resume script: $RESUME_SCRIPT"
}

# --- Print summary ---
print_summary() {
  local completed remaining total iteration status
  completed=$(count_completed)
  remaining=$(count_remaining)
  total=$(count_total)
  iteration=$(read_state "iteration")
  status=$(read_state "status")

  echo ""
  log_phase "=== SUMMARY ==="
  log "Status:     $status"
  log "Iterations: $iteration/$MAX_ITERATIONS"
  log "Tasks:      $completed/$total completed, $remaining remaining"
  log "Log:        $LOG_FILE"
  log "Plan:       $PLAN_FILE"

  if [[ "$remaining" -gt 0 ]]; then
    log "Resume:     $RESUME_SCRIPT"
  fi
}

# --- Cleanup on exit ---
cleanup() {
  # Kill any backgrounded processes
  jobs -p | xargs -r kill 2>/dev/null || true
  generate_resume_script
  print_summary
}
trap cleanup EXIT

# --- Main ---
main() {
  log_phase "Ralph Loop v${VERSION}"
  log "Project: $PROJECT_DIR"
  log "Max iterations: $MAX_ITERATIONS"

  init_ralph_dir

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
