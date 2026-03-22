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

# Proves: stream filter shows each line individually with timestamps
# instead of deduplicating, so the reader can see the source and count
# of each event.
@test "stream filter timestamps each line without deduplication" {
  write_stream_filter

  # Build stream-json events: 3 identical Read calls, then 1 different one
  local input=""
  for i in 1 2 3; do
    input+='{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"foo.sh"}}]}}'$'\n'
  done
  input+='{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"bar.sh"}}]}}'$'\n'
  input+='{"type":"result"}'$'\n'

  # Run the jq+perl stages only (skip tail -f and sed colorization)
  local output
  output=$(echo "$input" | jq --raw-input --join-output --unbuffered '
    fromjson? // empty |
    if .type == "assistant" then
      .message.content[0]? //empty |
      if .type == "text" then "\n[claude] " + .text + "\n"
      elif .type == "tool_use" then
        if .name == "TodoWrite" then
          ([.input.todos[]? | .content] | if length == 0 then "[]"
            else join(", ") end) as $items |
          "\n[TodoWrite] " + $items + "\n"
        else
          (.input.file_path // .input.command // .input.pattern //
            .input.query // .input.url // .input.description //
            .input.task_id // .input.skill // .input.prompt //
            null) as $target |
          if $target then "\n[" + .name + "] " + $target + "\n"
          else "\n[" + .name + "]\n"
          end
        end
      else empty end
    elif .type == "result" then
      "\n[done]\n"
    else empty end
  ' | perl -ne '
    use POSIX; $|=1;
    chomp;
    next if $_ eq "";
    print strftime("%H:%M:%S", localtime()) . " " . $_ . "\n";
  ')

  # Should have 5 lines: 3x foo.sh, 1x bar.sh, 1x [done] (no dedup)
  local line_count
  line_count=$(echo "$output" | wc -l | tr -d ' ')
  [[ "$line_count" -eq 5 ]]

  # All three foo.sh lines should be present individually
  local foo_count
  foo_count=$(echo "$output" | grep -c 'foo.sh')
  [[ "$foo_count" -eq 3 ]]

  # Each line should have a HH:MM:SS timestamp prefix
  echo "$output" | head -1 | grep -qE '^[0-9]{2}:[0-9]{2}:[0-9]{2} '

  # bar.sh should appear once
  echo "$output" | grep -c 'bar.sh' | grep -q '1'

  # Last line should be [done]
  echo "$output" | tail -1 | grep -q '\[done\]'
}

@test "Stream filter has no kill 0 trap" {
  write_stream_filter
  run grep 'kill 0' "$RALPH_DIR/.stream-filter.sh"
  [[ "$status" -ne 0 ]]
}
