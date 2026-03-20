#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
  init_ralph_dir

  _stagnant_count=0
  _test_only_count=0
  _stuck_count=0
  ANALYSIS_RESULT="continue"
}

teardown() {
  teardown_test_repo
}

# Proves: ralph stops when blocked by permissions.
@test "Permission denial (3+) triggers halt" {
  local logfile="$RALPH_DIR/test_iter.log"
  cat > "$logfile" <<'EOF'
{"type":"assistant","message":{"content":[{"type":"text","text":"permission denied trying to write\npermission denied again\npermission denied third time"}]}}
EOF
  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null)
  analyze_iteration "$logfile" 1 "$head_before"
  [[ "$ANALYSIS_RESULT" == "halt:permission_denied" ]]
}

# Proves: loop detection works.
@test "Stuck loop detection" {
  local logfile="$RALPH_DIR/test_iter.log"
  cat > "$logfile" <<'EOF'
{"type":"assistant","message":{"content":[{"type":"text","text":"I'm blocked on this issue\nI cannot proceed without access"}]}}
EOF
  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null)

  _stuck_count=0
  analyze_iteration "$logfile" 1 "$head_before"
  [[ "$ANALYSIS_RESULT" == "warn:stuck_indicators_detected" ]]

  analyze_iteration "$logfile" 1 "$head_before"
  [[ "$ANALYSIS_RESULT" == "halt:stuck_loop" ]]
}

# Proves: stops on no progress.
@test "Stagnation (3 no-change iterations)" {
  local logfile="$RALPH_DIR/test_iter.log"
  echo "some output" > "$logfile"
  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null)

  _stagnant_count=0
  analyze_iteration "$logfile" 1 "$head_before"
  [[ "$ANALYSIS_RESULT" == "continue" ]]
  analyze_iteration "$logfile" 1 "$head_before"
  [[ "$ANALYSIS_RESULT" == "continue" ]]
  analyze_iteration "$logfile" 1 "$head_before"
  [[ "$ANALYSIS_RESULT" == "halt:stagnation" ]]
}

# Proves: stops on test-only edits.
@test "Test saturation (3 test-only iterations)" {
  local logfile="$RALPH_DIR/test_iter.log"
  echo "some output" > "$logfile"

  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD)

  _test_only_count=0
  for i in 1 2 3; do
    echo "test change $i" > "$WORK_DIR/test_file.test.js"
    git -C "$WORK_DIR" add -A
    git -C "$WORK_DIR" commit -m "test only $i" -q
    local head_now
    head_now=$(git -C "$WORK_DIR" rev-parse HEAD)
    analyze_iteration "$logfile" 1 "$head_before"
    head_before="$head_now"
    if (( i < 3 )); then
      [[ "$ANALYSIS_RESULT" != "halt:test_saturation" ]]
    fi
  done
  [[ "$ANALYSIS_RESULT" == "halt:test_saturation" ]]
}

# Proves: no false positives on normal progress.
@test "Normal progress resets counters" {
  local logfile="$RALPH_DIR/test_iter.log"
  echo "some output" > "$logfile"
  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null)

  _stagnant_count=2
  _test_only_count=2

  echo "real change" > "$WORK_DIR/src.js"
  git -C "$WORK_DIR" add -A
  git -C "$WORK_DIR" commit -m "real work" -q

  local head_after
  head_after=$(git -C "$WORK_DIR" rev-parse HEAD)
  analyze_iteration "$logfile" 1 "$head_before"
  [[ "$ANALYSIS_RESULT" == "continue" ]]
  [[ "$_stagnant_count" -eq 0 ]]
  [[ "$_test_only_count" -eq 0 ]]
}

@test "Mixed test and source changes reset test-only count" {
  local logfile="$RALPH_DIR/test_iter.log"
  echo "some output" > "$logfile"

  _test_only_count=2

  mkdir -p "$WORK_DIR/AppTests" "$WORK_DIR/src"
  echo "tests" > "$WORK_DIR/AppTests/HTTPClientTests.swift"
  echo "source" > "$WORK_DIR/src/HTTPClient.swift"
  git -C "$WORK_DIR" add -A
  git -C "$WORK_DIR" commit -m "add source and tests" -q

  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD~1)
  analyze_iteration "$logfile" 1 "$head_before"
  [[ "$ANALYSIS_RESULT" == "continue" ]]
  [[ "$_test_only_count" -eq 0 ]]
}

@test "Files under top-level test dir count as test files" {
  local logfile="$RALPH_DIR/test_iter.log"
  echo "some output" > "$logfile"

  _test_only_count=0

  mkdir -p "$WORK_DIR/tests/helpers"
  echo "helper" > "$WORK_DIR/tests/helpers/setup.js"
  git -C "$WORK_DIR" add -A
  git -C "$WORK_DIR" commit -m "test helper" -q

  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD~1)
  analyze_iteration "$logfile" 1 "$head_before"
  [[ "$ANALYSIS_RESULT" == "continue" ]]
  [[ "$_test_only_count" -eq 1 ]]
}

@test "Files under suffixed test dirs (e.g. AppTests, AppUITests) count as test files" {
  local logfile="$RALPH_DIR/test_iter.log"
  echo "some output" > "$logfile"

  _test_only_count=0

  mkdir -p "$WORK_DIR/musicXmusicTests" "$WORK_DIR/musicXmusicUITests"
  echo "helper" > "$WORK_DIR/musicXmusicTests/SetupHelper.swift"
  echo "fixture" > "$WORK_DIR/musicXmusicUITests/UIFixture.swift"
  git -C "$WORK_DIR" add -A
  git -C "$WORK_DIR" commit -m "test helpers" -q

  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD~1)
  analyze_iteration "$logfile" 1 "$head_before"
  [[ "$ANALYSIS_RESULT" == "continue" ]]
  [[ "$_test_only_count" -eq 1 ]]
}

# Proves: ralph detects repeated identical errors across iterations and skips the task.
@test "Repeated error fingerprint (3x same error) triggers halt" {
  local logfile="$RALPH_DIR/test_iter.log"
  local text="Error: cannot find module 'foo'\nsome other output"
  printf '{"type":"assistant","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$text" > "$logfile"
  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null)

  analyze_iteration "$logfile" 1 "$head_before" "test-task-1"
  [[ "$ANALYSIS_RESULT" != *"repeated_error"* ]]

  analyze_iteration "$logfile" 1 "$head_before" "test-task-1"
  [[ "$ANALYSIS_RESULT" != *"repeated_error"* ]]

  analyze_iteration "$logfile" 1 "$head_before" "test-task-1"
  [[ "$ANALYSIS_RESULT" == "halt:repeated_error" ]]
}

# Proves: errors with different volatile parts (timestamps, UUIDs) are treated as the same error.
@test "Error normalization collapses timestamps and UUIDs" {
  local logfile="$RALPH_DIR/test_iter.log"
  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null)

  local text1="Error: 2026-03-20T10:00:00Z request a1b2c3d4-e5f6-7890-abcd-ef1234567890 failed"
  printf '{"type":"assistant","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$text1" > "$logfile"
  analyze_iteration "$logfile" 1 "$head_before" "test-task-2"

  local text2="Error: 2026-03-21T15:30:00Z request 11111111-2222-3333-4444-555555555555 failed"
  printf '{"type":"assistant","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$text2" > "$logfile"
  analyze_iteration "$logfile" 1 "$head_before" "test-task-2"

  local text3="Error: 2026-03-22T09:15:00Z request deadbeef-cafe-babe-dead-beefcafebabe failed"
  printf '{"type":"assistant","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$text3" > "$logfile"
  analyze_iteration "$logfile" 1 "$head_before" "test-task-2"

  [[ "$ANALYSIS_RESULT" == "halt:repeated_error" ]]
}

# Proves: different errors don't trigger the threshold even across many iterations.
@test "Different errors do not trigger repeated error halt" {
  local logfile="$RALPH_DIR/test_iter.log"
  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null)

  local text1="Error: module 'foo' not found"
  printf '{"type":"assistant","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$text1" > "$logfile"
  analyze_iteration "$logfile" 1 "$head_before" "test-task-3"

  local text2="Error: syntax error in bar.js"
  printf '{"type":"assistant","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$text2" > "$logfile"
  analyze_iteration "$logfile" 1 "$head_before" "test-task-3"

  local text3="Error: timeout connecting to database"
  printf '{"type":"assistant","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$text3" > "$logfile"
  analyze_iteration "$logfile" 1 "$head_before" "test-task-3"

  [[ "$ANALYSIS_RESULT" != *"repeated_error"* ]]
}

# Proves: error hashes are cleared when switching tasks, so a new task starts fresh.
@test "Error hashes cleared on task change" {
  local logfile="$RALPH_DIR/test_iter.log"
  local text="Error: cannot find module 'foo'"
  printf '{"type":"assistant","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$text" > "$logfile"
  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null)

  analyze_iteration "$logfile" 1 "$head_before" "task-a"
  analyze_iteration "$logfile" 1 "$head_before" "task-a"
  clear_error_hashes "task-a"

  analyze_iteration "$logfile" 1 "$head_before" "task-a"
  [[ "$ANALYSIS_RESULT" != *"repeated_error"* ]]
}

# Proves: no task key means error fingerprinting is skipped entirely.
@test "No task key skips error fingerprinting" {
  local logfile="$RALPH_DIR/test_iter.log"
  local text="Error: something broke"
  printf '{"type":"assistant","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$text" > "$logfile"
  local head_before
  head_before=$(git -C "$WORK_DIR" rev-parse HEAD 2>/dev/null)

  analyze_iteration "$logfile" 1 "$head_before" ""
  analyze_iteration "$logfile" 1 "$head_before" ""
  analyze_iteration "$logfile" 1 "$head_before" ""
  [[ "$ANALYSIS_RESULT" != *"repeated_error"* ]]
}
