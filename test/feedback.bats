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

# Proves: `ralph feedback <msg>` queues and `ralph feedback` lists it.
@test "ralph feedback queues and lists feedback" {
  run "$RALPH_SH" feedback "$PROJECT_DIR" fix the tests
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"Feedback queued"* ]]

  run "$RALPH_SH" feedback "$PROJECT_DIR"
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"fix the tests"* ]]
}

# Proves: feedback is read without deleting the file.
@test "read_feedback returns content without deleting" {
  echo "make it generic" > "$RALPH_DIR/feedback"
  local fb
  fb=$(read_feedback)
  [[ "$fb" == "make it generic" ]]
  [[ -f "$RALPH_DIR/feedback" ]]
}

# Proves: clear_feedback removes the file.
@test "clear_feedback deletes the file" {
  echo "some feedback" > "$RALPH_DIR/feedback"
  clear_feedback
  [[ ! -f "$RALPH_DIR/feedback" ]]
}

# Proves: no crash when no feedback exists.
@test "read_feedback returns empty when no file" {
  local fb
  fb=$(read_feedback)
  [[ -z "$fb" ]]
}

# Proves: multiple feedback messages stack up before being read.
@test "feedback file appends multiple messages" {
  echo "first feedback" >> "$RALPH_DIR/feedback"
  echo "second feedback" >> "$RALPH_DIR/feedback"
  local fb
  fb=$(read_feedback)
  [[ "$fb" == *"first feedback"* ]]
  [[ "$fb" == *"second feedback"* ]]
  [[ -f "$RALPH_DIR/feedback" ]]
}
