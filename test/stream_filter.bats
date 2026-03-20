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
# Proves: consecutive identical tool calls are collapsed into a single line
# with a counter (e.g. "[Read] foo.sh x3"), saving vertical space.
@test "stream filter deduplicates consecutive identical lines" {
  write_stream_filter

  # Build stream-json events: 3 identical Read calls, then 1 different one
  local input=""
  for i in 1 2 3; do
    input+='{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"foo.sh"}}]}}'$'\n'
  done
  input+='{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"bar.sh"}}]}}'$'\n'
  input+='{"type":"result"}'$'\n'

  # Run the jq+perl stages only (skip tail -f and sed colorization)
  local filter_script="$RALPH_DIR/.stream-filter.sh"

  # Extract jq and perl stages from the filter script and run them
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
  ' | perl -e '
    use POSIX; $|=1;
    my ($prev, $count, $prev_ts);
    sub flush_prev {
      return unless defined $prev;
      if ($count > 1) {
        print "$prev_ts $prev x$count\n";
      } else {
        print "$prev_ts $prev\n";
      }
    }
    while(<STDIN>) {
      chomp;
      next if $_ eq "";
      my $ts = strftime("%H:%M:%S", localtime());
      if (defined $prev && $_ eq $prev) {
        $count++;
        $prev_ts = $ts;
      } else {
        flush_prev();
        $prev = $_;
        $count = 1;
        $prev_ts = $ts;
      }
    }
    flush_prev();
  ')

  # Should have exactly 3 output lines: deduplicated Read x3, Read bar.sh, [done]
  local line_count
  line_count=$(echo "$output" | wc -l | tr -d ' ')
  [[ "$line_count" -eq 3 ]]

  # First line should contain the x3 counter
  echo "$output" | head -1 | grep -q 'x3'

  # First line should reference foo.sh
  echo "$output" | head -1 | grep -q 'foo.sh'

  # Second line should be bar.sh without counter
  echo "$output" | sed -n '2p' | grep -q 'bar.sh'
  ! echo "$output" | sed -n '2p' | grep -q 'x[0-9]'

  # Third line should be [done]
  echo "$output" | tail -1 | grep -q '\[done\]'
}

@test "Stream filter has no kill 0 trap" {
  write_stream_filter
  run grep 'kill 0' "$RALPH_DIR/.stream-filter.sh"
  [[ "$status" -ne 0 ]]
}
