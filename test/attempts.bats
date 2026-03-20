#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: attempt log files are created in the attempts directory with the task id as filename.
@test "record_attempt creates attempt file for bd task" {
  record_attempt "ralph-abc" "Fix the bug" "did stuff" "2 files changed" "continue"
  [[ -f "$RALPH_DIR/attempts/ralph-abc.log" ]]
}

# Proves: checklist tasks (no id) use a slugified task name as the attempt key.
@test "record_attempt uses slugified name when no task id" {
  record_attempt "" "Fix the auth bug" "tried auth" "" "continue"
  [[ -f "$RALPH_DIR/attempts/fix-the-auth-bug.log" ]]
}

# Proves: multiple attempts on the same task append sequentially with incrementing numbers.
@test "record_attempt appends with incrementing attempt numbers" {
  record_attempt "t1" "Task one" "first try" "" "continue"
  record_attempt "t1" "Task one" "second try" "" "warn:stuck"

  local content
  content=$(<"$RALPH_DIR/attempts/t1.log")
  [[ "$content" == *"### Attempt 1"* ]]
  [[ "$content" == *"### Attempt 2"* ]]
  [[ "$content" == *"first try"* ]]
  [[ "$content" == *"second try"* ]]
}

# Proves: recorded attempts include the summary, diff stat, and analysis result.
@test "record_attempt captures summary, diff stat, and analysis" {
  record_attempt "t2" "Deploy service" "deployed v2" "3 files changed, 10 insertions" "continue"

  local content
  content=$(<"$RALPH_DIR/attempts/t2.log")
  [[ "$content" == *"Summary: deployed v2"* ]]
  [[ "$content" == *"3 files changed"* ]]
  [[ "$content" == *"Analysis: continue"* ]]
}

# Proves: when no diff stat, the log says "Changes: none" instead of blank.
@test "record_attempt shows 'Changes: none' when no diff stat" {
  record_attempt "t3" "Empty task" "nothing happened" "" "continue"

  local content
  content=$(<"$RALPH_DIR/attempts/t3.log")
  [[ "$content" == *"Changes: none"* ]]
}

# Proves: read_attempt_history returns the full log content for lookup.
@test "read_attempt_history returns recorded attempts" {
  record_attempt "t4" "My task" "try one" "" "continue"

  local history
  history=$(read_attempt_history "t4" "My task")
  [[ "$history" == *"### Attempt 1"* ]]
  [[ "$history" == *"try one"* ]]
}

# Proves: read_attempt_history returns empty when no attempts exist.
@test "read_attempt_history returns empty for new task" {
  local history
  history=$(read_attempt_history "new-task" "Brand new task")
  [[ -z "$history" ]]
}

# Proves: clear_attempt_history removes the attempt file so re-attempts start fresh after resolution.
@test "clear_attempt_history removes attempt file" {
  record_attempt "t5" "Done task" "completed" "" "continue"
  [[ -f "$RALPH_DIR/attempts/t5.log" ]]

  clear_attempt_history "t5" "Done task"
  [[ ! -f "$RALPH_DIR/attempts/t5.log" ]]
}

# Proves: attempt history is injected into the prompt when previous attempts exist.
@test "build_prompt includes attempt history when present" {
  record_attempt "t6" "Fix login" "broke it" "" "warn:stuck"

  local prompt
  prompt=$(build_prompt "Fix login" "" "t6" "Fix login")
  [[ "$prompt" == *"Previous attempts on this task"* ]]
  [[ "$prompt" == *"broke it"* ]]
}

# Proves: no attempt history section in prompt for fresh tasks.
@test "build_prompt omits attempt history for fresh task" {
  local prompt
  prompt=$(build_prompt "New task" "" "fresh-id" "New task")
  [[ "$prompt" != *"Previous attempts"* ]]
  [[ "$prompt" != *"{{ATTEMPT_HISTORY}}"* ]]
}
