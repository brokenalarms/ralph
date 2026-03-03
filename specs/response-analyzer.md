# Response Analyzer

## Problem
The signal protocol works when Claude cooperates, but when Claude is stuck, confused, or blocked by permissions, it doesn't signal — and ralph has no fallback. Iterations burn with no progress and no early exit.

## Solution
After each iteration, analyze the log output and worktree state to detect problems. The analyzer returns a status that the main loop acts on.

## Detection categories

### Permission denials
Claude repeatedly fails to write files. Patterns in stream-json output:
- Tool results containing "permission denied", "cannot write", "sandbox", "blocked"
- Multiple failed Write/Edit/Bash tool calls in a single iteration

Threshold: 3+ permission failures in one iteration → halt.

### Stagnation
Claude completes an iteration but makes no meaningful changes.
- `git -C "$WORK_DIR" diff --stat` shows no modifications
- No signal file written
- No new commits

Track consecutive stagnant iterations. Threshold: 3 consecutive → halt.
Reset counter whenever progress is detected (file changes OR signal written).

### Test saturation
Claude only runs/modifies test files without implementation changes.
- `git diff --name-only` shows only files matching `*test*`, `*spec*`, `*_test.*`, `test_*`
- No non-test files modified

Track consecutive test-only iterations. Threshold: 3 consecutive → halt.

### Stuck loops
Claude's text output contains indicators of being stuck:
- Repeated identical tool calls (same command/file 3+ times)
- Text containing "I'm blocked", "I cannot proceed", "unable to"

Threshold: detected in a single iteration → warn. Detected in 2 consecutive → halt.

## Implementation

### Function: `analyze_iteration()`
**File:** `ralph.sh` (inline, not a separate lib file — keeps it a single script)

Input: path to the log file, line offset for current iteration's start
Output: echoes one of `continue`, `warn:<reason>`, `halt:<reason>`

Uses the stream-json log to extract tool results and text content. Uses `git diff` for file change detection. Tracks counters in shell variables (not files — they reset per execution phase, which is correct).

### Integration in run_execution()
After each `run_claude()` call and post-iteration logging:
```bash
local analysis
analysis=$(analyze_iteration)
case "$analysis" in
  halt:*)
    log_error "Halting: ${analysis#halt:}"
    write_state "status" "halted_${analysis#halt:}"
    break
    ;;
  warn:*)
    log_warn "${analysis#warn:}"
    ;;
esac
```

### Log offset tracking
Before `run_claude()`, capture `wc -l < "$LOG_FILE"` as `log_start_line`.
Pass to `analyze_iteration` so it only reads the current iteration's output.

## Acceptance criteria
- Permission denials: ralph halts after one iteration of repeated write failures
- Stagnation: ralph halts after 3 consecutive no-change iterations
- Test saturation: ralph halts after 3 consecutive test-only iterations
- Stuck loops: ralph warns then halts on repeated stuck indicators
- Counters reset on progress (not cumulative across productive iterations)
- Status written to state.json reflects halt reason
