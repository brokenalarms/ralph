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

# Proves: ralph feedback shows usage when called without args.
@test "ralph feedback without args shows usage" {
  run "$RALPH_SH" feedback "$PROJECT_DIR"
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"Usage"* ]]
}

# Proves: ralph feedback fails when no active task in state.
@test "ralph feedback fails without active task" {
  echo '{}' > "$RALPH_DIR/state.json"
  run "$RALPH_SH" feedback "$PROJECT_DIR" fix the tests
  [[ "$status" -eq 1 ]]
  [[ "$output" == *"No active task"* ]]
}

# Proves: ralph feedback creates empty signal file for agent restart.
@test "ralph feedback creates signal file" {
  # Set up state with active task
  echo '{"last_task_id": "ralph-test"}' > "$RALPH_DIR/state.json"

  # Mock bd so it doesn't require a real database
  local mock_bd="$PROJECT_DIR/mock-bd"
  cat > "$mock_bd" << 'MOCK'
#!/bin/bash
echo "mock bd called: $@"
exit 0
MOCK
  chmod +x "$mock_bd"
  PATH="$(dirname "$mock_bd"):$PATH"

  run "$RALPH_SH" feedback "$PROJECT_DIR" fix the tests
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"Feedback sent"* ]]

  # Signal file should exist
  [[ -f "$RALPH_DIR/feedback" ]]
  # Signal file should be empty (content is on the bead)
  [[ ! -s "$RALPH_DIR/feedback" ]]
}
