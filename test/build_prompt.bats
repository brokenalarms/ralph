#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
  init_ralph_dir
}

teardown() {
  teardown_test_repo
}

# Proves: correct paths substituted into prompts.
@test "Template variables substituted" {
  local prompt
  prompt=$(build_prompt "test task")
  [[ "$prompt" == *"$WORK_DIR"* ]]
  [[ "$prompt" == *"$RALPH_DIR"* ]]
  [[ "$prompt" == *"$PLAN_FILE"* ]]
  [[ "$prompt" == *"$SIGNAL_FILE"* ]]
  [[ "$prompt" != *"{{WORK_DIR}}"* ]]
  [[ "$prompt" != *"{{RALPH_DIR}}"* ]]
}

# Proves: correct prompt template for each mode.
@test "External vs internal template selection" {
  EXTERNAL_PLAN=false
  local internal_prompt
  internal_prompt=$(build_prompt "Fix auth")
  [[ "$internal_prompt" == *"Your task this iteration"* ]]

  EXTERNAL_PLAN=true
  local external_prompt
  external_prompt=$(build_prompt "")
  [[ "$external_prompt" == *"Task selection"* ]]
}
