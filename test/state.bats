#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: state persistence between iterations.
@test "write_state and read_state round-trip" {
  init_ralph_dir
  write_state "status" "running"
  result=$(read_state "status")
  [[ "$result" == "running" ]]
}

# Proves: iteration counter works.
@test "write_state handles numeric values" {
  init_ralph_dir
  write_state "iteration" "5"
  result=$(read_state "iteration")
  [[ "$result" == "5" ]]
}

# Proves: clean start.
@test "init_ralph_dir creates fresh state" {
  rm -f "$STATE_FILE"
  init_ralph_dir
  [[ -f "$STATE_FILE" ]]
  result=$(read_state "status")
  [[ "$result" == "initialized" ]]
}

# Proves: resume doesn't lose progress.
@test "init_ralph_dir preserves state on resume" {
  init_ralph_dir
  write_state "iteration" "3"
  write_state "status" "running"
  RESUME=true
  init_ralph_dir
  result=$(read_state "iteration")
  [[ "$result" == "3" ]]
  result=$(read_state "status")
  [[ "$result" == "running" ]]
}
