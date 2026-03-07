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
  if [[ "$TASK_BACKEND" == "bd" ]]; then
    cat <<'INST'
## Task selection
1. Read AGENTS.md or CLAUDE.md if present (mandatory — do not skip or summarize). Follow any project-specific guidance.
2. This project uses `bd` for task tracking. Run `bd ready --plain` to see available tasks.

## Your task this iteration
{{TASK_PROMPT}}

## Rules
1. Before starting work, verify the task isn't already done. If it is, close it: `bd close <id>`
2. Focus ONLY on the single task described above.
3. When you complete the task: `bd close <id> --reason "summary of what you did"`
4. Atomic commits, and a pull request if gh is available.
5. Do NOT work on other tasks — one task per iteration.
INST
  else
    cat <<'INST'
## Task selection
1. Read AGENTS.md or CLAUDE.md if present (mandatory — do not skip or summarize). Follow any project-specific guidance.
2. Read the plan file at {{PLAN_FILE}} and pick the next unchecked task in order (the planning phase already determined priority).

## Your task this iteration
{{TASK_PROMPT}}

## Rules
1. Before starting work, verify the task isn't already done. Check the relevant code — if the fix or feature already exists, mark it `[x]` in {{PLAN_FILE}} and signal completion without making changes.
2. Focus ONLY on the single task described above.
3. When you complete the task, mark it as done in {{PLAN_FILE}} by changing `- [ ]` to `- [x]`.
4. If the project has its own todo tracking (defined in AGENTS.md or CLAUDE.md), update it as part of your work.
5. Atomic commits, and a pull request if gh is available.
6. If you cannot complete the task, leave it unchecked and add notes in {{PLAN_FILE}}.
7. Do NOT work on other tasks — one task per iteration.
INST
  fi
}

task_planning_instructions() {
  if [[ "$TASK_BACKEND" == "bd" ]]; then
    cat <<'INST'
## Output
Break the work into atomic, self-contained tasks. Create each task using bd:
  bd create "Task description" --type task --silent

Each task should be completable in a single Claude session. Be specific and actionable.
After creating all tasks, signal completion: echo "{{SIGNAL_TOKEN}}" > "{{SIGNAL_FILE}}"
INST
  else
    cat <<'INST'
## Output
Break the work into atomic, self-contained tasks. Write the plan to {{PLAN_FILE}} using markdown checkboxes:
- [ ] Task 1 description
- [ ] Task 2 description

Each task should be completable in a single Claude session. Be specific and actionable.
After writing the plan, signal completion: echo "{{SIGNAL_TOKEN}}" > "{{SIGNAL_FILE}}"
INST
  fi
}

interactive_planning_instructions() {
  if [[ "$TASK_BACKEND" == "bd" ]]; then
    cat <<'INST'
1. **Task list** — atomic tasks created via `bd create "Task description" --type task --silent` that Ralph will execute one per iteration.
INST
  else
    cat <<'INST'
1. **Task checklist** at `{{PLAN_FILE}}` — atomic tasks in markdown checkbox format that Ralph will execute one per iteration:
   ```
   - [ ] Task 1 description
   - [ ] Task 2 description
   ```
INST
  fi
}
