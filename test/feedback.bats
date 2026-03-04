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

# Proves: feedback is read and file is cleaned up after consumption.
@test "consume_feedback reads and clears feedback file" {
  echo "make it generic" > "$RALPH_DIR/feedback"
  local fb
  fb=$(consume_feedback)
  [[ "$fb" == "make it generic" ]]
  [[ ! -f "$RALPH_DIR/feedback" ]]
}

# Proves: no crash when no feedback exists.
@test "consume_feedback returns empty when no file" {
  local fb
  fb=$(consume_feedback)
  [[ -z "$fb" ]]
}

# Proves: multiple feedback messages stack up before being consumed.
@test "feedback file appends multiple messages" {
  echo "first feedback" >> "$RALPH_DIR/feedback"
  echo "second feedback" >> "$RALPH_DIR/feedback"
  local fb
  fb=$(consume_feedback)
  [[ "$fb" == *"first feedback"* ]]
  [[ "$fb" == *"second feedback"* ]]
  [[ ! -f "$RALPH_DIR/feedback" ]]
}
