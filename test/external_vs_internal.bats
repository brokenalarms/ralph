#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: worktree gets its own copy of the plan.
@test "External: remap_plan_file copies plan into worktree" {
  EXTERNAL_PLAN=true
  local orig_plan="$TEST_TMPDIR/todo.md"
  echo "- Task one" > "$orig_plan"
  PLAN_FILE="$orig_plan"

  init_ralph_dir
  setup_worktree

  [[ "$PLAN_FILE" == "$WORK_DIR"* ]]
  [[ -f "$PLAN_FILE" ]]
  diff -q "$orig_plan" "$PLAN_FILE" >/dev/null
}

# Proves: prior modifications preserved on resume.
@test "External: remap_plan_file skips copy on resume" {
  EXTERNAL_PLAN=true
  local orig_plan="$TEST_TMPDIR/todo.md"
  echo "- Task one" > "$orig_plan"
  PLAN_FILE="$orig_plan"

  init_ralph_dir
  setup_worktree
  local worktree_plan="$PLAN_FILE"

  echo "- Task one MODIFIED" > "$worktree_plan"

  RESUME=true
  PLAN_FILE="$orig_plan"
  setup_worktree

  result=$(cat "$PLAN_FILE")
  [[ "$result" == *"MODIFIED"* ]]
}

# Proves: no wasted API calls for external plans.
@test "External: run_planning skips Claude invocation" {
  EXTERNAL_PLAN=true
  init_ralph_dir
  echo "- [ ] Some task" > "$PLAN_FILE"
  run run_planning
  [[ "$status" -eq 0 ]]
}

# Proves: ralph delegates completion detection to Claude signal.
@test "External: has_remaining_tasks always returns true" {
  EXTERNAL_PLAN=true
  echo "all done, no bullets" > "$PLAN_FILE"
  run has_remaining_tasks
  [[ "$status" -eq 0 ]]
}

# Proves: Claude picks its own task in external mode.
@test "External: execution uses empty task prompt" {
  EXTERNAL_PLAN=true
  init_ralph_dir
  echo "- [ ] Some task" > "$PLAN_FILE"
  local prompt
  prompt=$(build_prompt "")
  [[ "$prompt" != *"Complete this task"* ]]
}

# Proves: ralph drives task selection in internal mode.
@test "Internal: execution passes task description as prompt" {
  EXTERNAL_PLAN=false
  init_ralph_dir
  local prompt
  prompt=$(build_prompt "Complete this task: Fix auth")
  [[ "$prompt" == *"Complete this task: Fix auth"* ]]
}
