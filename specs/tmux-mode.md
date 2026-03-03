# Tmux Mode

## Problem
Ralph's loop status and Claude's streaming output interleave in the same terminal. Hard to distinguish ralph's own logging (iteration counts, task info, warnings) from Claude's work output (tool calls, reasoning text).

## Solution
Optional `--tmux` flag creates a 3-pane tmux session that separates concerns.

## Layout
```
┌──────────────────────┬──────────────────────────┐
│                      │                          │
│   Ralph loop         │   Claude output          │
│   (status, tasks,    │   (jq-parsed stream)     │
│    warnings)         │                          │
│                      │                          │
│                      ├──────────────────────────┤
│                      │                          │
│                      │   Plan / progress        │
│                      │   (watch state + plan)   │
│                      │                          │
└──────────────────────┴──────────────────────────┘
```

- **Left pane:** Ralph's own loop output (iterations, task selection, warnings, summary)
- **Top-right pane:** `tail -f` on log file piped through jq (Claude's streaming output)
- **Bottom-right pane:** Periodic display of plan progress and state

## Implementation

### New flag
`--tmux` in arg parsing. Add `USE_TMUX=false` default.

### Function: `setup_tmux()`
**File:** `ralph.sh`

```bash
setup_tmux() {
  if ! command -v tmux &>/dev/null; then
    log_error "tmux not found, falling back to inline mode"
    USE_TMUX=false
    return
  fi

  local session="ralph-$$"

  tmux new-session -d -s "$session" -c "$PROJECT_DIR"

  # Split right side
  tmux split-window -h -t "$session"

  # Split right pane vertically
  tmux split-window -v -t "$session:.1"

  # Top-right: claude output stream
  tmux send-keys -t "$session:.1" \
    "tail -f '$LOG_FILE' | jq --raw-input --join-output --unbuffered '...jq filter...'" Enter

  # Bottom-right: plan + state monitor
  tmux send-keys -t "$session:.2" \
    "watch -n 5 'echo \"=== State ===\"; cat \"$STATE_FILE\" 2>/dev/null; echo; echo \"=== Plan ===\"; head -30 \"$PLAN_FILE\" 2>/dev/null'" Enter

  # Left pane: ralph loop (this is where main() continues)
  tmux select-pane -t "$session:.0"
  tmux attach-session -t "$session"
}
```

### Behavior changes in tmux mode
- When `USE_TMUX=true`, skip the inline `tail -f | jq` streaming in `run_claude()` (top-right pane handles it)
- Set `QUIET=true` internally for the jq streaming (ralph still logs its own status to terminal)
- On cleanup, kill the tmux session

### Fallback
Without `--tmux`, behavior is identical to current: interleaved output in single terminal.

## Acceptance criteria
- `--tmux` creates 3-pane layout with correct content in each
- Left pane shows ralph iteration status, warnings, summary
- Top-right shows Claude's parsed output (text + tool calls)
- Bottom-right shows plan file progress and state.json
- Session cleans up when ralph exits (Ctrl+C or natural completion)
- Without `--tmux`, no change to current behavior
- Graceful fallback if tmux not installed
