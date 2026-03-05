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

# Proves: prompt includes task selection and task content.
@test "Prompt includes task selection and task content" {
  local prompt
  prompt=$(build_prompt "Fix auth")
  [[ "$prompt" == *"Task selection"* ]]
  [[ "$prompt" == *"Fix auth"* ]]
}

# Proves: shared quality standards are included in prompts.
@test "Shared prompt included" {
  local prompt
  prompt=$(build_prompt "task")
  [[ "$prompt" == *"Standards"* ]]
  [[ "$prompt" == *".gitignore"* ]]
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
