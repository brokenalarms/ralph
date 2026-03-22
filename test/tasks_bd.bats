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
          open)        echo "3" ;;
          closed)      echo "2" ;;
          in_progress) echo "0" ;;
        esac
        ;;
      *)
        echo "5"
        ;;
    esac
    ;;
  list)
    # Handle --status in_progress --flat --json
    if [[ "$*" == *"--status"*"in_progress"*"--json"* ]]; then
      echo '[]'
    fi
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

# Proves: bd backend counts both open and in-progress as remaining
@test "bd: count_remaining returns open + in_progress count" {
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

# Proves: in-progress tasks are resumed when priority is equal to ready tasks
@test "bd: get_next_task resumes in-progress at same priority" {
  local mock_dir="$TEST_TMPDIR/mock_bin"
  cat > "$mock_dir/bd" <<'MOCK'
#!/usr/bin/env bash
case "$1" in
  list)
    if [[ "$*" == *"--status"*"in_progress"*"--json"* ]]; then
      echo '[{"id":"wip-42","title":"Half-done feature","priority":2}]'
    fi
    ;;
  ready)
    echo '[{"id":"abc123","title":"Fix the auth module","priority":2}]'
    ;;
esac
MOCK
  chmod +x "$mock_dir/bd"
  result=$(get_next_task)
  [[ "$result" == "Half-done feature" ]]
}

# Proves: in-progress task id is returned when priorities are equal
@test "bd: get_next_task_id resumes in-progress at same priority" {
  local mock_dir="$TEST_TMPDIR/mock_bin"
  cat > "$mock_dir/bd" <<'MOCK'
#!/usr/bin/env bash
case "$1" in
  list)
    if [[ "$*" == *"--status"*"in_progress"*"--json"* ]]; then
      echo '[{"id":"wip-42","title":"Half-done feature","priority":2}]'
    fi
    ;;
  ready)
    echo '[{"id":"abc123","title":"Fix the auth module","priority":2}]'
    ;;
esac
MOCK
  chmod +x "$mock_dir/bd"
  result=$(get_next_task_id)
  [[ "$result" == "wip-42" ]]
}

# Proves: higher-priority ready task preempts lower-priority in-progress task
@test "bd: get_next_task preempts lower-priority in-progress" {
  local mock_dir="$TEST_TMPDIR/mock_bin"
  cat > "$mock_dir/bd" <<'MOCK'
#!/usr/bin/env bash
case "$1" in
  list)
    if [[ "$*" == *"--status"*"in_progress"*"--json"* ]]; then
      echo '[{"id":"wip-1","title":"P3 feature","priority":3}]'
    fi
    ;;
  ready)
    echo '[{"id":"hot-1","title":"P0 critical bug","priority":0}]'
    ;;
  update)
    echo "$2" > "$TEST_TMPDIR/reopened_id"
    ;;
esac
MOCK
  chmod +x "$mock_dir/bd"
  result=$(get_next_task)
  [[ "$result" == "P0 critical bug" ]]
}

# Proves: lower-priority ready task does not preempt in-progress task
@test "bd: get_next_task keeps higher-priority in-progress" {
  local mock_dir="$TEST_TMPDIR/mock_bin"
  cat > "$mock_dir/bd" <<'MOCK'
#!/usr/bin/env bash
case "$1" in
  list)
    if [[ "$*" == *"--status"*"in_progress"*"--json"* ]]; then
      echo '[{"id":"wip-1","title":"P1 important","priority":1}]'
    fi
    ;;
  ready)
    echo '[{"id":"new-1","title":"P3 backlog","priority":3}]'
    ;;
esac
MOCK
  chmod +x "$mock_dir/bd"
  result=$(get_next_task)
  [[ "$result" == "P1 important" ]]
}

# Proves: falls back to ready queue when no in-progress tasks
@test "bd: get_next_task falls back to ready when nothing in-progress" {
  result=$(get_next_task)
  [[ "$result" == "Fix the auth module" ]]
}

# Proves: checklist backend returns empty id (no bd integration)
@test "checklist: get_next_task_id returns empty string" {
  TASK_BACKEND="checklist"
  result=$(get_next_task_id)
  [[ "$result" == "" ]]
}

# Proves: init_task_backend creates .beads dir in PROJECT_DIR and updates gitignore
@test "bd: init_task_backend initializes bd and updates gitignore" {
  init_task_backend
  [[ -d "$PROJECT_DIR/.beads" ]]
  grep -qx '.beads' "$PROJECT_DIR/.gitignore"
  grep -qx '.dolt' "$PROJECT_DIR/.gitignore"
}

# Proves: init_task_backend is idempotent for gitignore
@test "bd: init_task_backend doesn't duplicate gitignore entry" {
  printf '.beads\n.dolt\n' > "$PROJECT_DIR/.gitignore"
  init_task_backend
  [[ $(grep -cx '.beads' "$PROJECT_DIR/.gitignore") == "1" ]]
  [[ $(grep -cx '.dolt' "$PROJECT_DIR/.gitignore") == "1" ]]
}

# Proves: checklist init is a no-op
@test "checklist: init_task_backend is a no-op" {
  TASK_BACKEND="checklist"
  init_task_backend
  [[ ! -d "$PROJECT_DIR/.beads" ]]
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

# Proves: bd has_tasks returns true when tasks exist
@test "bd: has_tasks true with tasks" {
  run has_tasks
  [[ "$status" -eq 0 ]]
}

# Proves: bd needs_planning returns false when tasks exist
@test "bd: needs_planning false with tasks" {
  run needs_planning
  [[ "$status" -ne 0 ]]
}

# Proves: bd planning_succeeded returns true when tasks exist
@test "bd: planning_succeeded true with tasks" {
  run planning_succeeded
  [[ "$status" -eq 0 ]]
}

# Proves: bd_init falls back to checklist when Dolt server is unreachable
@test "bd: init falls back to checklist when server is unhealthy" {
  # Replace bd mock with one that fails on count (simulates bad Dolt server)
  local mock_dir="$TEST_TMPDIR/mock_bin"
  cat > "$mock_dir/bd" <<'MOCK'
#!/usr/bin/env bash
case "$1" in
  init) mkdir -p .beads ;;
  count) echo "Error: database not found" >&2; exit 1 ;;
  *) exit 1 ;;
esac
MOCK
  chmod +x "$mock_dir/bd"

  TASK_BACKEND="bd"
  init_task_backend
  [[ "$TASK_BACKEND" == "checklist" ]]
}

# Proves: bd_init retries init when .beads exists but server is stale
@test "bd: init retries when .beads exists but health check fails initially" {
  # Pre-create .beads to simulate a previous run
  mkdir -p "$PROJECT_DIR/.beads"

  # bd mock: init reconnects (makes count work), count fails until init is called
  local mock_dir="$TEST_TMPDIR/mock_bin"
  local flag_file="$TEST_TMPDIR/bd_reinited"
  cat > "$mock_dir/bd" <<MOCK
#!/usr/bin/env bash
case "\$1" in
  init) mkdir -p .beads; touch "$flag_file" ;;
  count)
    if [[ -f "$flag_file" ]]; then echo "5"; else exit 1; fi
    ;;
  *) exit 1 ;;
esac
MOCK
  chmod +x "$mock_dir/bd"

  TASK_BACKEND="bd"
  init_task_backend
  [[ "$TASK_BACKEND" == "bd" ]]
  [[ -f "$flag_file" ]]
}

# Proves: bd_init falls back to checklist when bd init itself fails
@test "bd: init falls back to checklist when bd init fails" {
  # Replace bd mock with one that always fails
  local mock_dir="$TEST_TMPDIR/mock_bin"
  cat > "$mock_dir/bd" <<'MOCK'
#!/usr/bin/env bash
exit 1
MOCK
  chmod +x "$mock_dir/bd"

  # Remove .beads so init is attempted
  rm -rf "$PROJECT_DIR/.beads"
  TASK_BACKEND="bd"
  init_task_backend
  [[ "$TASK_BACKEND" == "checklist" ]]
}

# Proves: _validate_backend catches missing functions
@test "_validate_backend passes for valid backends" {
  TASK_BACKEND="bd"
  run _validate_backend
  [[ "$status" -eq 0 ]]

  TASK_BACKEND="checklist"
  run _validate_backend
  [[ "$status" -eq 0 ]]
}

# Proves: bd skip_task closes the task with a blocked reason
@test "bd: skip_task closes with blocked reason" {
  local mock_dir="$TEST_TMPDIR/mock_bin"
  local close_log="$TEST_TMPDIR/close_log"
  cat > "$mock_dir/bd" <<MOCK
#!/usr/bin/env bash
case "\$1" in
  close) echo "\$@" > "$close_log" ;;
esac
MOCK
  chmod +x "$mock_dir/bd"
  skip_task "abc123" "stuck_loop"
  [[ -f "$close_log" ]]
  grep -q "blocked: stuck_loop" "$close_log"
}

# Proves: checklist skip_task marks the current task as skipped
@test "checklist: skip_task marks task with [s]" {
  TASK_BACKEND="checklist"
  cat > "$PLAN_FILE" <<'EOF'
- [ ] Fix auth bug
- [ ] Add tests
EOF
  skip_task "" "stagnation"
  grep -q '\[s\] Fix auth bug (stagnation)' "$PLAN_FILE"
  grep -q '\[ \] Add tests' "$PLAN_FILE"
}
