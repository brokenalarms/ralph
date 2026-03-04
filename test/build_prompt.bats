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

# Proves: shared quality standards are included in all prompts.
@test "Shared prompt included in both modes" {
  EXTERNAL_PLAN=false
  local internal_prompt
  internal_prompt=$(build_prompt "task")
  [[ "$internal_prompt" == *"Standards"* ]]
  [[ "$internal_prompt" == *".gitignore"* ]]

  EXTERNAL_PLAN=true
  local external_prompt
  external_prompt=$(build_prompt "")
  [[ "$external_prompt" == *"Standards"* ]]
  [[ "$external_prompt" == *".gitignore"* ]]
}

# Proves: user feedback is injected into the prompt when provided.
@test "Feedback included in prompt when provided" {
  local prompt
  prompt=$(build_prompt "task" "make it generic, use plugins")
  [[ "$prompt" == *"User feedback"* ]]
  [[ "$prompt" == *"make it generic"* ]]
}

# Proves: no feedback section when none provided.
@test "No feedback section when feedback is empty" {
  local prompt
  prompt=$(build_prompt "task" "")
  [[ "$prompt" != *"User feedback"* ]]
}
