#!/usr/bin/env bash
# Task backend abstraction — dispatches on TASK_BACKEND (bd | checklist)

run_bd() { (cd "$PROJECT_DIR" && bd "$@"); }

# --- bd backend ---

bd_init() {
  if [[ ! -d "$PROJECT_DIR/.beads" ]]; then
    (cd "$PROJECT_DIR" && bd init)
  fi

  local gitignore="$PROJECT_DIR/.gitignore"
  if [[ ! -f "$gitignore" ]] || ! grep -qx '.beads' "$gitignore"; then
    echo '.beads' >> "$gitignore"
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
