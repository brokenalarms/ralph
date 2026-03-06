#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: stream filter script is generated with correct structure.
@test "write_stream_filter creates executable script" {
  write_stream_filter
  [[ -f "$RALPH_DIR/.stream-filter.sh" ]]
  [[ -x "$RALPH_DIR/.stream-filter.sh" ]]
}

# Proves: stream filter child processes (tail, jq) are cleaned up by
# pkill -P, preventing orphaned processes from accumulating across iterations.
@test "pkill -P kills stream filter children" {
  write_stream_filter

  # Run in a subshell without set -e to avoid bats interference with
  # process management and signal handling
  local result
  result=$(set +e; bash -c '
    bash "'"$RALPH_DIR"'/.stream-filter.sh" "'"$LOG_FILE"'" > /dev/null 2>&1 &
    fpid=$!
    sleep 1

    kill -0 "$fpid" 2>/dev/null || { echo "FAIL:not_running"; exit 0; }

    children=$(pgrep -P "$fpid" 2>/dev/null | wc -l | tr -d " ")
    if [[ "$children" -eq 0 ]]; then
      echo "FAIL:no_children"; exit 0
    fi

    pkill -P "$fpid" 2>/dev/null
    kill "$fpid" 2>/dev/null
    wait "$fpid" 2>/dev/null
    sleep 1

    remaining=$(pgrep -P "$fpid" 2>/dev/null | wc -l | tr -d " ")
    echo "OK:before=$children:after=$remaining"
  ')

  [[ "$result" == OK:* ]]
  [[ "$result" == *":after=0" ]]
}

# Proves: stream filter does not contain process-group-killing traps
# that would terminate the parent ralph process.
@test "Stream filter has no kill 0 trap" {
  write_stream_filter
  run grep 'kill 0' "$RALPH_DIR/.stream-filter.sh"
  [[ "$status" -ne 0 ]]
}
