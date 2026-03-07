#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: loop continues when unchecked items exist.
@test "has_remaining_tasks detects unchecked items" {
  cat > "$PLAN_FILE" <<'EOF'
- [ ] Fix auth bug
- [x] Add tests
EOF
  run has_remaining_tasks
  [[ "$status" -eq 0 ]]
}

# Proves: loop stops when all items are checked.
@test "has_remaining_tasks false when all done" {
  cat > "$PLAN_FILE" <<'EOF'
- [x] Fix auth bug
- [x] Add tests
EOF
  run has_remaining_tasks
  [[ "$status" -ne 0 ]]
}

# Proves: correct counts for mixed plans.
@test "count_completed/remaining/total accurate" {
  cat > "$PLAN_FILE" <<'EOF'
- [x] Done task 1
- [ ] Todo task 2
- [x] Done task 3
- [ ] Todo task 4
- [ ] Todo task 5
EOF
  [[ "$(count_completed)" == "2" ]]
  [[ "$(count_remaining)" == "3" ]]
  [[ "$(count_total)" == "5" ]]
}

# Proves: ralph drives task selection in correct order.
@test "get_next_task returns first unchecked" {
  cat > "$PLAN_FILE" <<'EOF'
- [x] Done task
- [ ] Second task is next
- [ ] Third task
EOF
  result=$(get_next_task)
  [[ "$result" == "Second task is next" ]]
}

# Proves: has_tasks returns true when plan has checkbox items.
@test "checklist: has_tasks true with tasks" {
  cat > "$PLAN_FILE" <<'EOF'
- [ ] A task
EOF
  run has_tasks
  [[ "$status" -eq 0 ]]
}

# Proves: has_tasks returns false with no plan file.
@test "checklist: has_tasks false without plan" {
  rm -f "$PLAN_FILE"
  run has_tasks
  [[ "$status" -ne 0 ]]
}

# Proves: needs_planning returns true when no plan file exists.
@test "checklist: needs_planning true without plan" {
  rm -f "$PLAN_FILE"
  run needs_planning
  [[ "$status" -eq 0 ]]
}

# Proves: needs_planning returns false when plan file exists.
@test "checklist: needs_planning false with plan" {
  touch "$PLAN_FILE"
  run needs_planning
  [[ "$status" -ne 0 ]]
}

# Proves: planning_succeeded requires actual tasks, not just a file.
@test "checklist: planning_succeeded false with empty plan" {
  echo "no tasks here" > "$PLAN_FILE"
  run planning_succeeded
  [[ "$status" -ne 0 ]]
}

# Proves: planning_succeeded true when plan has checkboxes.
@test "checklist: planning_succeeded true with tasks" {
  echo "- [ ] A task" > "$PLAN_FILE"
  run planning_succeeded
  [[ "$status" -eq 0 ]]
}
