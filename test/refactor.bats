#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
  init_ralph_dir
  REFACTOR_EVERY=5
}

teardown() {
  teardown_test_repo
}

# Proves: refactor prompt includes shared standards (Boy Scout Rule) and refactor-specific instructions.
@test "build_refactor_prompt includes shared standards and refactor instructions" {
  local prompt
  prompt=$(build_refactor_prompt "ralph.sh server.js")
  [[ "$prompt" == *"Boy Scout Rule"* ]]
  [[ "$prompt" == *"refactor-only iteration"* ]]
  [[ "$prompt" == *"ralph.sh server.js"* ]]
}

# Proves: refactor prompt resolves template variables for work dir and signal.
@test "build_refactor_prompt resolves template variables" {
  local prompt
  prompt=$(build_refactor_prompt "lib/tasks.sh")
  [[ "$prompt" == *"$WORK_DIR"* ]]
  [[ "$prompt" == *"$SIGNAL_FILE"* ]]
  [[ "$prompt" != *"{{WORK_DIR}}"* ]]
  [[ "$prompt" != *"{{SIGNAL_FILE}}"* ]]
  [[ "$prompt" != *"{{RECENT_FILES}}"* ]]
}

# Proves: refactor prompt injects the recent files list so Claude knows what to review.
@test "build_refactor_prompt includes recent files in output" {
  local prompt
  prompt=$(build_refactor_prompt "src/auth.sh
src/utils.sh
test/auth.bats")
  [[ "$prompt" == *"src/auth.sh"* ]]
  [[ "$prompt" == *"src/utils.sh"* ]]
  [[ "$prompt" == *"test/auth.bats"* ]]
}

# Proves: iterations_since_refactor state defaults to 0 when not set.
@test "iterations_since_refactor defaults to 0" {
  local val
  val=$(read_state "iterations_since_refactor")
  [[ -z "$val" || "$val" == "null" ]]
}

# Proves: iterations_since_refactor state can be written and read back.
@test "iterations_since_refactor tracks across writes" {
  write_state "iterations_since_refactor" "3"
  local val
  val=$(read_state "iterations_since_refactor")
  [[ "$val" == "3" ]]
}

# Proves: REFACTOR_EVERY default is 5, providing a reasonable cadence out of the box.
@test "REFACTOR_EVERY defaults to 5" {
  [[ "$REFACTOR_EVERY" == "5" ]]
}

# Proves: planning prompt includes debt assessment instructions so Claude scans for tech debt.
@test "planning prompt includes debt assessment section" {
  local planning_prompt
  planning_prompt=$(<"$PROMPTS_DIR/planning.md")
  [[ "$planning_prompt" == *"Debt assessment"* ]]
  [[ "$planning_prompt" == *"Duplicated patterns"* ]]
  [[ "$planning_prompt" == *"refactor tasks"* ]]
}

# Proves: shared prompt includes Boy Scout Rule so every execution iteration cleans up touched files.
@test "shared prompt includes Boy Scout Rule" {
  local shared
  shared=$(<"$PROMPTS_DIR/shared.md")
  [[ "$shared" == *"Boy Scout Rule"* ]]
  [[ "$shared" == *"Dead code"* ]]
  [[ "$shared" == *"scoped to files you changed"* ]]
}

# Proves: refactor prompt enforces behavior preservation — refactoring must not change functionality.
@test "refactor prompt enforces no behavior change" {
  local prompt
  prompt=$(build_refactor_prompt "file.sh")
  [[ "$prompt" == *"Do NOT add new features or change behavior"* ]]
}

# Proves: refactor prompt requires refactor: commit prefix for clear git history.
@test "refactor prompt requires refactor: commit prefix" {
  local prompt
  prompt=$(build_refactor_prompt "file.sh")
  [[ "$prompt" == *'refactor:'* ]]
}
