# Rate Limiting

## Problem
Claude Pro/Max subscriptions have hourly message caps. Ralph can burn through an entire hour's quota in minutes if left running, leaving the user unable to use Claude interactively until the window resets.

## Solution
Track iterations per clock hour. When the cap is reached, pause with a countdown until the next hour boundary, then resume automatically.

## Implementation

### New defaults in ralph.sh
```bash
MAX_CALLS_PER_HOUR=80
CALL_COUNT_FILE="$RALPH_DIR/.call_count"
CALL_HOUR_FILE="$RALPH_DIR/.call_hour"
```

### New flag
`--calls-per-hour <N>` — override the default. Add to arg parsing and usage text.

### Helper functions
```bash
init_call_tracking() — read current hour, compare to $CALL_HOUR_FILE. Reset counter if new hour.
check_rate_limit() — read counter from $CALL_COUNT_FILE. Return 1 if >= MAX_CALLS_PER_HOUR.
increment_call_count() — increment and write counter.
wait_for_rate_reset() — calculate seconds until next hour boundary, sleep with countdown log.
```

### Integration in run_execution()
Before each `run_claude()` call:
1. `init_call_tracking`
2. If `check_rate_limit` fails, call `wait_for_rate_reset`, then `init_call_tracking` again

After each `run_claude()`:
1. `increment_call_count`

### State files
- `$RALPH_DIR/.call_count` — plain integer, current hour's count
- `$RALPH_DIR/.call_hour` — `YYYYMMDDHH` string for current tracking hour

Both reset when the hour changes.

## Acceptance criteria
- Setting `--calls-per-hour 3` pauses after 3 iterations with a visible countdown
- Counter resets automatically at the next clock hour
- Counter persists across resume (files in .ralph/)
- Default of 80 leaves headroom for interactive use
