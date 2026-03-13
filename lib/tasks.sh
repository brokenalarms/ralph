#!/usr/bin/env bash
# Task backend abstraction — dispatches on TASK_BACKEND (bd | checklist)

run_bd() { (cd "$PROJECT_DIR" && bd "$@"); }

# --- bd health check & fallback ---

bd_is_healthy() {
  # Quick check: can bd actually talk to its server?
  # "bd count" is lightweight and exercises the DB connection.
  run_bd count 2>/dev/null >/dev/null
}

_fallback_to_checklist() {
  local reason="${1:-unknown}"
  log_warn "Beads/Dolt unavailable: $reason"
  printf "${YELLOW}[ralph]${NC} Continue with checklist backend instead? (y/n) "
  read -r answer
  if [[ "$answer" == "y" || "$answer" == "Y" ]]; then
    TASK_BACKEND="checklist"
  else
    log_error "Fix beads/dolt and retry. Run 'bd doctor' to diagnose."
    exit 1
  fi
}

# --- bd backend ---

bd_init() {
  local bd_err

  if [[ ! -d "$PROJECT_DIR/.beads" ]]; then
    if ! bd_err=$(cd "$PROJECT_DIR" && bd init 2>&1); then
      _fallback_to_checklist "bd init failed: $bd_err"
      return
    fi
  fi

  # Verify the server is actually reachable; retry init if stale
  if ! bd_is_healthy; then
    log_warn "Beads health check failed — retrying bd init..."
    if ! bd_err=$(cd "$PROJECT_DIR" && bd init 2>&1); then
      _fallback_to_checklist "bd init retry failed: $bd_err"
      return
    fi
    if ! bd_is_healthy; then
      bd_err=$(run_bd count 2>&1) || true
      _fallback_to_checklist "server unreachable after retry: $bd_err"
      return
    fi
  fi

  local gitignore="$PROJECT_DIR/.gitignore"
  local changed=false
  for entry in .beads .dolt; do
    if [[ ! -f "$gitignore" ]] || ! grep -qE "^\\${entry}(/\\*?)?$" "$gitignore"; then
      echo "$entry" >> "$gitignore"
      changed=true
    fi
  done
  if $changed && git -C "$PROJECT_DIR" rev-parse --git-dir &>/dev/null; then
    git -C "$PROJECT_DIR" add .gitignore 2>/dev/null || true
    git -C "$PROJECT_DIR" commit -m "Add beads/dolt to .gitignore" 2>/dev/null || true
  fi
}

bd_has_remaining() {
  local c
  c=$(run_bd count --status open 2>/dev/null) || c=0
  (( c > 0 ))
}

bd_count_completed()   { run_bd count --status closed 2>/dev/null || echo 0; }
bd_count_remaining()   { run_bd count --status open 2>/dev/null || echo 0; }
bd_count_total()       { run_bd count 2>/dev/null || echo 0; }
bd_get_next_task()     { run_bd ready --limit 1 --json 2>/dev/null | jq -r '.[0].title // empty'; }
bd_get_next_task_id()  { run_bd ready --limit 1 --json 2>/dev/null | jq -r '.[0].id // empty'; }

bd_has_tasks() { (( $(bd_count_total) > 0 )); }
bd_needs_planning()     { ! bd_has_tasks; }
bd_planning_succeeded() { bd_has_tasks; }

bd_execution_instructions() { cat "$PROMPTS_DIR/execution-bd.md"; }

# --- checklist backend ---

checklist_init() { :; }

checklist_has_remaining() {
  [[ -f "$PLAN_FILE" ]] && grep -qE '^\s*- \[ \]' "$PLAN_FILE"
}

checklist_count_completed() {
  [[ -f "$PLAN_FILE" ]] && { grep -cE '^\s*- \[x\]' "$PLAN_FILE" 2>/dev/null || true; } || echo 0
}

checklist_count_remaining() {
  [[ -f "$PLAN_FILE" ]] && { grep -cE '^\s*- \[ \]' "$PLAN_FILE" 2>/dev/null || true; } || echo 0
}

checklist_count_total() {
  [[ -f "$PLAN_FILE" ]] && { grep -cE '^\s*- \[[ x]\]' "$PLAN_FILE" 2>/dev/null || true; } || echo 0
}

checklist_get_next_task() {
  [[ -f "$PLAN_FILE" ]] && grep -m1 -E '^\s*- \[ \]' "$PLAN_FILE" | sed 's/^\s*- \[ \] *//'
}

checklist_get_next_task_id() { echo ""; }

checklist_has_tasks() {
  [[ -f "$PLAN_FILE" ]] && (( $(checklist_count_total) > 0 ))
}

checklist_needs_planning()     { [[ ! -f "$PLAN_FILE" ]]; }
checklist_planning_succeeded() { checklist_has_tasks; }

checklist_execution_instructions() { cat "$PROMPTS_DIR/execution-checklist.md"; }

# --- Generic dispatch ---

init_task_backend()          { "${TASK_BACKEND}_init" "$@"; }
has_remaining_tasks()        { "${TASK_BACKEND}_has_remaining" "$@"; }
count_completed()            { "${TASK_BACKEND}_count_completed" "$@"; }
count_remaining()            { "${TASK_BACKEND}_count_remaining" "$@"; }
count_total()                { "${TASK_BACKEND}_count_total" "$@"; }
get_next_task()              { "${TASK_BACKEND}_get_next_task" "$@"; }
get_next_task_id()           { "${TASK_BACKEND}_get_next_task_id" "$@"; }
has_tasks()                  { "${TASK_BACKEND}_has_tasks" "$@"; }
needs_planning()             { "${TASK_BACKEND}_needs_planning" "$@"; }
planning_succeeded()         { "${TASK_BACKEND}_planning_succeeded" "$@"; }
task_execution_instructions(){ "${TASK_BACKEND}_execution_instructions" "$@"; }
task_label() { if [[ "$TASK_BACKEND" == "bd" ]]; then echo "beads"; else echo "checklist"; fi; }

task_planning_instructions() {
  if [[ "$TASK_BACKEND" == "bd" ]]; then
    echo "Run \`bd prime\` to learn the workflow, then create tasks directly in bd with dependencies. Do NOT write a plan.md file — tasks live exclusively in bd."
  else
    cat <<INST
Write tasks to $PLAN_FILE in markdown checkbox format:
- [ ] Task 1 description
- [ ] Task 2 description
IMPORTANT: beads/bd is NOT installed in this environment. Do NOT attempt to use bd commands — they will fail. You MUST write the plan file directly. If the user asks to use beads, explain that bd is not installed and they need to install it and restart ralph.
INST
  fi
}

_validate_backend() {
  local fns=(init has_remaining count_completed count_remaining count_total
             get_next_task get_next_task_id has_tasks needs_planning
             planning_succeeded execution_instructions)
  for fn in "${fns[@]}"; do
    if ! declare -f "${TASK_BACKEND}_${fn}" &>/dev/null; then
      log_error "Task backend '$TASK_BACKEND' missing function: ${fn}"
      exit 1
    fi
  done
}
