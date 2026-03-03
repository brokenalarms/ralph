# Plan File Sync

## Problem
When using worktrees, ralph copies the plan file INTO the worktree at startup but never syncs changes BACK. If Claude marks tasks done, adds notes, or removes items, the original plan file stays stale. The user sees outdated progress when looking at their TODO.md.

## Solution
After each iteration, copy the worktree's plan file back to the original location.

## Implementation

### In `run_execution()`, after each iteration's post-processing
**File:** `ralph.sh`

Add at the end of both the external-plan and internal-plan iteration blocks, before the `echo ""`:

```bash
if [[ "$WORK_DIR" != "$PROJECT_DIR" && -f "$PLAN_FILE" ]]; then
  cp "$PLAN_FILE" "$ORIG_PLAN_FILE"
fi
```

`$ORIG_PLAN_FILE` is already set (line 112) to preserve the original path before worktree remapping.

## Acceptance criteria
- After each iteration in worktree mode, the original plan file reflects Claude's changes
- Works for both external plan mode and internal plan mode
- No-op when not using worktrees ($WORK_DIR == $PROJECT_DIR)
- No-op if plan file doesn't exist (shouldn't happen, but defensive)
