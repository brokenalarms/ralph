#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: ralph drives task selection via task prompt.
@test "Execution passes task description as prompt" {
  init_ralph_dir
  local prompt
  prompt=$(build_prompt "Complete this task: Fix auth")
  [[ "$prompt" == *"Complete this task: Fix auth"* ]]
}

# Proves: has_remaining_tasks detects unchecked items.
@test "has_remaining_tasks detects unchecked tasks" {
  echo "- [ ] Some task" > "$PLAN_FILE"
  run has_remaining_tasks
  [[ "$status" -eq 0 ]]
}

# Proves: has_remaining_tasks returns false when all done.
@test "has_remaining_tasks returns false when all checked" {
  echo "- [x] Done task" > "$PLAN_FILE"
  run has_remaining_tasks
  [[ "$status" -ne 0 ]]
}
