#!/usr/bin/env bats

load test_helper

# Mock bd command that simulates bd CLI behavior
setup_bd_mock() {
  local mock_dir="$TEST_TMPDIR/mock_bin"
  mkdir -p "$mock_dir"
  cat > "$mock_dir/bd" <<'MOCK'
#!/usr/bin/env bash
case "$1" in
  init)
    mkdir -p .beads
    ;;
  count)
    case "${2:-}" in
      --status)
        case "$3" in
          open)   echo "3" ;;
          closed) echo "2" ;;
        esac
        ;;
      *)
        echo "5"
        ;;
    esac
    ;;
  ready)
    if [[ "${2:-}" == "--limit" && "${4:-}" == "--json" ]]; then
      echo '[{"id":"abc123","title":"Fix the auth module"}]'
    elif [[ "${2:-}" == "--plain" ]]; then
      echo "abc123  Fix the auth module"
    fi
    ;;
  create)
    echo "created"
    ;;
  close)
    echo "closed"
    ;;
esac
MOCK
  chmod +x "$mock_dir/bd"
  export PATH="$mock_dir:$PATH"
}

setup() {
  source_ralph_functions
  setup_test_repo
  setup_bd_mock
  TASK_BACKEND="bd"
}

teardown() {
  teardown_test_repo
}

# Proves: bd backend detects open tasks
@test "bd: has_remaining_tasks returns true when open tasks exist" {
  run has_remaining_tasks
  [[ "$status" -eq 0 ]]
}

# Proves: bd backend returns correct completed count
@test "bd: count_completed returns closed count" {
  result=$(count_completed)
  [[ "$result" == "2" ]]
}

# Proves: bd backend returns correct remaining count
@test "bd: count_remaining returns open count" {
  result=$(count_remaining)
  [[ "$result" == "3" ]]
}

# Proves: bd backend returns correct total count
@test "bd: count_total returns all tasks" {
  result=$(count_total)
  [[ "$result" == "5" ]]
}

# Proves: bd backend picks the next ready task by title
@test "bd: get_next_task returns first ready task title" {
  result=$(get_next_task)
  [[ "$result" == "Fix the auth module" ]]
}

# Proves: bd backend returns the task id for prompt inclusion
@test "bd: get_next_task_id returns first ready task id" {
  result=$(get_next_task_id)
  [[ "$result" == "abc123" ]]
}

# Proves: checklist backend returns empty id (no bd integration)
@test "checklist: get_next_task_id returns empty string" {
  TASK_BACKEND="checklist"
  result=$(get_next_task_id)
  [[ "$result" == "" ]]
}

# Proves: init_task_backend creates .beads dir and gitignore entry
@test "bd: init_task_backend initializes bd and updates gitignore" {
  init_task_backend
  [[ -d "$WORK_DIR/.beads" ]]
  grep -qx '.beads' "$WORK_DIR/.gitignore"
}

# Proves: init_task_backend is idempotent for gitignore
@test "bd: init_task_backend doesn't duplicate gitignore entry" {
  echo '.beads' > "$WORK_DIR/.gitignore"
  init_task_backend
  local count
  count=$(grep -cx '.beads' "$WORK_DIR/.gitignore")
  [[ "$count" == "1" ]]
}

# Proves: checklist init is a no-op
@test "checklist: init_task_backend is a no-op" {
  TASK_BACKEND="checklist"
  init_task_backend
  [[ ! -d "$WORK_DIR/.beads" ]]
}

# Proves: bd execution instructions mention bd commands
@test "bd: task_execution_instructions references bd" {
  result=$(task_execution_instructions)
  [[ "$result" == *"bd prime"* ]]
  [[ "$result" == *"close the task"* ]]
}

# Proves: on resume, stored task_backend=checklist is honored even when bd is available
@test "resume preserves checklist backend when bd is available" {
  init_ralph_dir
  write_state "task_backend" "checklist"
  RESUME=true
  stored_backend=$(read_state "task_backend")
  if [[ "$stored_backend" == "bd" || "$stored_backend" == "checklist" ]]; then
    TASK_BACKEND="$stored_backend"
  fi
  [[ "$TASK_BACKEND" == "checklist" ]]
}

# Proves: migration — old state without task_backend infers checklist from plan file
@test "resume infers checklist backend from plan file when no stored backend" {
  init_ralph_dir
  echo '- [ ] Do something' > "$PLAN_FILE"
  TASK_BACKEND="bd"
  RESUME=true
  stored_backend=$(read_state "task_backend")
  if [[ "$stored_backend" == "bd" || "$stored_backend" == "checklist" ]]; then
    TASK_BACKEND="$stored_backend"
  elif [[ -f "$PLAN_FILE" ]] && grep -qE '^\s*- \[[ x]\]' "$PLAN_FILE"; then
    TASK_BACKEND="checklist"
  fi
  [[ "$TASK_BACKEND" == "checklist" ]]
}

# Proves: checklist execution instructions reference plan file
@test "checklist: task_execution_instructions references plan file" {
  TASK_BACKEND="checklist"
  result=$(task_execution_instructions)
  [[ "$result" == *"{{PLAN_FILE}}"* ]]
  [[ "$result" == *"[x]"* ]]
}

