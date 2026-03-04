#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: tracking initializes.
@test "init_call_tracking creates counter files" {
  init_call_tracking
  [[ -f "$RALPH_DIR/.call_count" ]]
  [[ -f "$RALPH_DIR/.call_hour" ]]
}

# Proves: calls not blocked prematurely.
@test "check_rate_limit passes under limit" {
  init_call_tracking
  echo "5" > "$RALPH_DIR/.call_count"
  run check_rate_limit
  [[ "$status" -eq 0 ]]
}

# Proves: rate limiting engages.
@test "check_rate_limit fails at limit" {
  init_call_tracking
  echo "$CALLS_PER_HOUR" > "$RALPH_DIR/.call_count"
  run check_rate_limit
  [[ "$status" -ne 0 ]]
}

# Proves: each call tracked.
@test "increment_call_count advances counter" {
  init_call_tracking
  echo "0" > "$RALPH_DIR/.call_count"
  increment_call_count
  result=$(cat "$RALPH_DIR/.call_count")
  [[ "$result" == "1" ]]
  increment_call_count
  result=$(cat "$RALPH_DIR/.call_count")
  [[ "$result" == "2" ]]
}

# Proves: hourly budget refreshes.
@test "hour rollover resets counter" {
  init_call_tracking
  echo "75" > "$RALPH_DIR/.call_count"
  echo "1999010100" > "$RALPH_DIR/.call_hour"
  run check_rate_limit
  [[ "$status" -eq 0 ]]
  result=$(cat "$RALPH_DIR/.call_count")
  [[ "$result" == "0" ]]
}
