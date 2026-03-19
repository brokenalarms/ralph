#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: offset advances so old tokens are ignored.
@test "clear_signal resets offset to current log length" {
  echo "old line" >> "$LOG_FILE"
  echo "$SIGNAL_TOKEN old task" >> "$LOG_FILE"
  clear_signal
  run check_signal
  [[ "$status" -ne 0 ]]
}

# Proves: task-done detection.
@test "check_signal detects completion token" {
  clear_signal
  echo "$SIGNAL_TOKEN done with auth fix" >> "$LOG_FILE"
  run check_signal
  [[ "$status" -eq 0 ]]
}

# Proves: no false positives.
@test "check_signal false without token" {
  clear_signal
  echo "some unrelated content" >> "$LOG_FILE"
  run check_signal
  [[ "$status" -ne 0 ]]
}

# Proves: summary capture from signal.
@test "read_signal_summary extracts text" {
  clear_signal
  echo "$SIGNAL_TOKEN Fixed the login bug" >> "$LOG_FILE"
  result=$(read_signal_summary)
  [[ "$result" == "Fixed the login bug" ]]
}

# Proves: mid-iteration task tracking.
@test "check_current_task and read_current_task" {
  clear_signal
  echo "$CURRENT_TASK_TOKEN Working on auth" >> "$LOG_FILE"
  run check_current_task
  [[ "$status" -eq 0 ]]
  result=$(read_current_task)
  [[ "$result" == "Working on auth" ]]
}

# Proves: ralph stops when Claude says everything is done.
@test "ALL_COMPLETE signal detected" {
  clear_signal
  echo "$ALL_COMPLETE_TOKEN All tasks finished" >> "$LOG_FILE"
  run check_all_complete
  [[ "$status" -eq 0 ]]
}
