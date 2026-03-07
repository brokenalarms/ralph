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
permission denied trying to write
permission denied again
permission denied third time
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
I'm blocked on this issue
I cannot proceed without access
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

  # Commit .ralph files so they don't appear as changes in later diffs
  git -C "$WORK_DIR" add -A
  git -C "$WORK_DIR" commit -m "baseline" -q

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

@test "Source files under test-named directories are not test-only" {
  local logfile="$RALPH_DIR/test_iter.log"
  echo "some output" > "$logfile"

  git -C "$WORK_DIR" add -A
  git -C "$WORK_DIR" commit -m "baseline" -q

  _test_only_count=2

  mkdir -p "$WORK_DIR/AppTests/App/Engine" "$WORK_DIR/AppTests/AppTests"
  echo "source" > "$WORK_DIR/AppTests/App/Engine/HTTPClient.swift"
  echo "tests" > "$WORK_DIR/AppTests/AppTests/HTTPClientTests.swift"
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

  git -C "$WORK_DIR" add -A
  git -C "$WORK_DIR" commit -m "baseline" -q

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
