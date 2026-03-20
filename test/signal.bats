#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: clear_signal removes all signal files so old signals are ignored.
@test "clear_signal removes signal files" {
  echo "old task" > "$SIGNAL_COMPLETE_FILE"
  echo "old current" > "$SIGNAL_TASK_FILE"
  clear_signal
  run check_signal
  [[ "$status" -ne 0 ]]
  run check_current_task
  [[ "$status" -ne 0 ]]
}

# Proves: task-done detection via file presence.
@test "check_signal detects completion file" {
  clear_signal
  echo "done with auth fix" > "$SIGNAL_COMPLETE_FILE"
  run check_signal
  [[ "$status" -eq 0 ]]
}

# Proves: no false positives when file doesn't exist.
@test "check_signal false without file" {
  clear_signal
  run check_signal
  [[ "$status" -ne 0 ]]
}

# Proves: summary capture from signal file.
@test "read_signal_summary extracts text from completion file" {
  clear_signal
  echo "Fixed the login bug" > "$SIGNAL_COMPLETE_FILE"
  result=$(read_signal_summary)
  [[ "$result" == "Fixed the login bug" ]]
}

# Proves: all_complete summary takes precedence over regular completion.
@test "read_signal_summary prefers all_complete file" {
  clear_signal
  echo "regular task done" > "$SIGNAL_COMPLETE_FILE"
  echo "All tasks finished" > "$SIGNAL_ALL_COMPLETE_FILE"
  result=$(read_signal_summary)
  [[ "$result" == "All tasks finished" ]]
}

# Proves: mid-iteration task tracking via file.
@test "check_current_task and read_current_task" {
  clear_signal
  echo "Working on auth" > "$SIGNAL_TASK_FILE"
  run check_current_task
  [[ "$status" -eq 0 ]]
  result=$(read_current_task)
  [[ "$result" == "Working on auth" ]]
}

# Proves: ralph stops when Claude says everything is done.
@test "ALL_COMPLETE signal detected" {
  clear_signal
  echo "All tasks finished" > "$SIGNAL_ALL_COMPLETE_FILE"
  run check_all_complete
  [[ "$status" -eq 0 ]]
}
