#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: clean slate between iterations.
@test "clear_signal removes signal file" {
  echo "some signal" > "$SIGNAL_FILE"
  clear_signal
  [[ ! -f "$SIGNAL_FILE" ]]
}

# Proves: task-done detection.
@test "check_signal detects completion token" {
  echo "$SIGNAL_TOKEN done with auth fix" > "$SIGNAL_FILE"
  run check_signal
  [[ "$status" -eq 0 ]]
}

# Proves: no false positives.
@test "check_signal false without token" {
  echo "some unrelated content" > "$SIGNAL_FILE"
  run check_signal
  [[ "$status" -ne 0 ]]
}

# Proves: summary capture from signal.
@test "read_signal_summary extracts text" {
  echo "$SIGNAL_TOKEN Fixed the login bug" > "$SIGNAL_FILE"
  result=$(read_signal_summary)
  [[ "$result" == "Fixed the login bug" ]]
}

# Proves: mid-iteration task tracking.
@test "check_current_task and read_current_task" {
  echo "$CURRENT_TASK_TOKEN Working on auth" > "$SIGNAL_FILE"
  run check_current_task
  [[ "$status" -eq 0 ]]
  result=$(read_current_task)
  [[ "$result" == "Working on auth" ]]
}

# Proves: ralph stops when Claude says everything is done.
@test "ALL_COMPLETE signal detected" {
  echo "$ALL_COMPLETE_TOKEN All tasks finished" > "$SIGNAL_FILE"
  run check_all_complete
  [[ "$status" -eq 0 ]]
}
