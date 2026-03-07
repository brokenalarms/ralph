#!/usr/bin/env bash
# Task backend abstraction — dispatches on TASK_BACKEND (bd | checklist)

run_bd() { (cd "$WORK_DIR" && bd "$@"); }

init_task_backend() {
  if [[ "$TASK_BACKEND" != "bd" ]]; then
    return
  fi

  if [[ ! -d "$WORK_DIR/.beads" ]]; then
    run_bd init
  fi

  local gitignore="$WORK_DIR/.gitignore"
  if [[ ! -f "$gitignore" ]] || ! grep -qx '.beads' "$gitignore"; then
    echo '.beads' >> "$gitignore"
  fi
}

has_remaining_tasks() {
  if [[ "$TASK_BACKEND" == "bd" ]]; then
    local count
    count=$(run_bd count --status open 2>/dev/null) || count=0
    (( count > 0 ))
  else
    [[ -f "$PLAN_FILE" ]] || return 1
    grep -qE '^\s*- \[ \]' "$PLAN_FILE"
  fi
}

count_completed() {
  if [[ "$TASK_BACKEND" == "bd" ]]; then
    run_bd count --status closed 2>/dev/null || echo 0
  else
    [[ -f "$PLAN_FILE" ]] && { grep -cE '^\s*- \[x\]' "$PLAN_FILE" 2>/dev/null || true; } || echo 0
  fi
}

count_remaining() {
  if [[ "$TASK_BACKEND" == "bd" ]]; then
    run_bd count --status open 2>/dev/null || echo 0
  else
    [[ -f "$PLAN_FILE" ]] && { grep -cE '^\s*- \[ \]' "$PLAN_FILE" 2>/dev/null || true; } || echo 0
  fi
}

count_total() {
  if [[ "$TASK_BACKEND" == "bd" ]]; then
    run_bd count 2>/dev/null || echo 0
  else
    [[ -f "$PLAN_FILE" ]] && { grep -cE '^\s*- \[[ x]\]' "$PLAN_FILE" 2>/dev/null || true; } || echo 0
  fi
}

get_next_task() {
  if [[ "$TASK_BACKEND" == "bd" ]]; then
    run_bd ready --limit 1 --json 2>/dev/null | jq -r '.[0].title // empty'
  else
    [[ -f "$PLAN_FILE" ]] && grep -m1 -E '^\s*- \[ \]' "$PLAN_FILE" | sed 's/^\s*- \[ \] *//'
  fi
}

get_next_task_id() {
  if [[ "$TASK_BACKEND" == "bd" ]]; then
    run_bd ready --limit 1 --json 2>/dev/null | jq -r '.[0].id // empty'
  else
    echo ""
  fi
}

task_execution_instructions() {
  cat "$PROMPTS_DIR/execution-${TASK_BACKEND}.md"
}

task_planning_instructions() {
  cat "$PROMPTS_DIR/planning-${TASK_BACKEND}.md"
}

interactive_planning_instructions() {
  cat "$PROMPTS_DIR/interactive-planning-${TASK_BACKEND}.md"
}
