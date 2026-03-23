#!/usr/bin/env bash
# Task backend — bd (beads) is the sole task tracking system.

run_bd() { (cd "$PROJECT_DIR" && bd "$@"); }

bd_is_healthy() {
  run_bd count 2>/dev/null >/dev/null
}

bd_init() {
  local bd_err

  if [[ ! -d "$PROJECT_DIR/.beads" ]]; then
    if ! bd_err=$(cd "$PROJECT_DIR" && bd init 2>&1); then
      log_error "bd init failed: $bd_err"
      log_error "bd is required. Run 'bd doctor' to diagnose."
      exit 1
    fi
  fi

  if ! bd_is_healthy; then
    log_warn "Beads health check failed — retrying bd init..."
    if ! bd_err=$(cd "$PROJECT_DIR" && bd init 2>&1); then
      log_error "bd init retry failed: $bd_err"
      log_error "bd is required. Run 'bd doctor' to diagnose."
      exit 1
    fi
    if ! bd_is_healthy; then
      bd_err=$(run_bd count 2>&1) || true
      log_error "bd server unreachable after retry: $bd_err"
      log_error "bd is required. Run 'bd doctor' to diagnose."
      exit 1
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
  local open inp
  open=$(run_bd count --status open 2>/dev/null) || open=0
  inp=$(run_bd count --status in_progress 2>/dev/null) || inp=0
  (( open + inp > 0 ))
}

bd_count_completed()   { run_bd count --status closed 2>/dev/null || echo 0; }
bd_count_remaining() {
  local open inp
  open=$(run_bd count --status open 2>/dev/null) || open=0
  inp=$(run_bd count --status in_progress 2>/dev/null) || inp=0
  echo $(( open + inp ))
}
bd_count_total() {
  local remaining completed
  remaining=$(bd_count_remaining)
  completed=$(bd_count_completed)
  echo $(( remaining + completed ))
}
bd_get_next_task() {
  local ip_json ready_json ip_title ready_title ip_pri ready_pri
  ip_json=$(run_bd list --status in_progress --flat --json --limit 1 2>/dev/null) || ip_json="[]"
  ready_json=$(run_bd ready --limit 1 --json 2>/dev/null) || ready_json="[]"
  ip_title=$(echo "$ip_json" | jq -r '.[0].title // empty')
  ready_title=$(echo "$ready_json" | jq -r '.[0].title // empty')
  if [[ -n "$ip_title" && -n "$ready_title" ]]; then
    ip_pri=$(echo "$ip_json" | jq -r '.[0].priority // 2')
    ready_pri=$(echo "$ready_json" | jq -r '.[0].priority // 2')
    if (( ready_pri < ip_pri )); then
      local ip_id
      ip_id=$(echo "$ip_json" | jq -r '.[0].id // empty')
      [[ -n "$ip_id" ]] && run_bd update "$ip_id" --status=open 2>/dev/null || true
      echo "$ready_title"
      return
    fi
    echo "$ip_title"
    return
  fi
  [[ -n "$ip_title" ]] && echo "$ip_title" && return
  echo "$ready_title"
}
bd_get_next_task_id() {
  local ip_json ready_json ip_id ready_id ip_pri ready_pri
  ip_json=$(run_bd list --status in_progress --flat --json --limit 1 2>/dev/null) || ip_json="[]"
  ready_json=$(run_bd ready --limit 1 --json 2>/dev/null) || ready_json="[]"
  ip_id=$(echo "$ip_json" | jq -r '.[0].id // empty')
  ready_id=$(echo "$ready_json" | jq -r '.[0].id // empty')
  if [[ -n "$ip_id" && -n "$ready_id" ]]; then
    ip_pri=$(echo "$ip_json" | jq -r '.[0].priority // 2')
    ready_pri=$(echo "$ready_json" | jq -r '.[0].priority // 2')
    if (( ready_pri < ip_pri )); then
      run_bd update "$ip_id" --status=open 2>/dev/null || true
      echo "$ready_id"
      return
    fi
    echo "$ip_id"
    return
  fi
  [[ -n "$ip_id" ]] && echo "$ip_id" && return
  echo "$ready_id"
}

bd_close_task() {
  local id="$1" reason="${2:-completed by ralph}"
  [[ -z "$id" ]] && return 0
  local status
  status=$(run_bd show "$id" --json 2>/dev/null | jq -r '.[0].status // empty') || true
  if [[ "$status" == "in_progress" ]]; then
    run_bd close "$id" --reason "$reason" 2>/dev/null || true
  fi
}

bd_skip_task() {
  local id="$1" reason="${2:-skipped by ralph}"
  [[ -z "$id" ]] && return 0
  run_bd close "$id" --reason "blocked: $reason" 2>/dev/null || true
}

bd_has_tasks() { (( $(bd_count_total) > 0 )); }
bd_needs_planning()     { ! bd_has_tasks; }
bd_planning_succeeded() { bd_has_tasks; }

bd_execution_instructions() { cat "$PROMPTS_DIR/execution-bd.md"; }

# --- Dispatch wrappers ---

init_task_backend()          { bd_init "$@"; }
close_task()                 { bd_close_task "$@"; }
skip_task()                  { bd_skip_task "$@"; }
has_remaining_tasks()        { bd_has_remaining "$@"; }
count_completed()            { bd_count_completed "$@"; }
count_remaining()            { bd_count_remaining "$@"; }
count_total()                { bd_count_total "$@"; }
get_next_task()              { bd_get_next_task "$@"; }
get_next_task_id()           { bd_get_next_task_id "$@"; }
has_tasks()                  { bd_has_tasks "$@"; }
needs_planning()             { bd_needs_planning "$@"; }
planning_succeeded()         { bd_planning_succeeded "$@"; }
task_execution_instructions(){ bd_execution_instructions "$@"; }
task_label()                 { echo "beads"; }

task_planning_instructions() {
  echo "Run \`bd prime\` to learn the workflow, then create tasks directly in bd with dependencies. Do NOT write a plan.md file — tasks live exclusively in bd."
}
