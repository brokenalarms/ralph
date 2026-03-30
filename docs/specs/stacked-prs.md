# Stacked Branch Workflow

## Overview

Tasks build linearly on each other. Each task gets its own branch and PR.
When auto-merge is on and PRs merge immediately, the stack stays at depth
0-1 and every task effectively targets the default branch. When merges are
delayed (CI pending, stacked waiting), branches stack and merge bottom-up.

```
default ← task-1 (PR #1) ← task-2 (PR #2) ← task-3 (PR #3)
```

## Stack Head Derivation

The stack head is the branch the next task should build on. It's derived
from the completed tasks array in state.json:

1. Walk completed tasks backwards (most recent first)
2. For each: get its external-ref, parse the PR number
3. If no PR → skip (work wasn't pushed)
4. Check PR state via `gh pr view`
5. If MERGED → skip (branch deleted by auto-delete, work is on default branch)
6. If OPEN → verify branch exists on remote → this is the stack head
7. If all completed tasks are merged or have no PR → stack head is empty, use default branch

`setStackHead()` runs:
- On resume in `initRun`, before the first iteration
- At the start of each iteration, before `checkoutExistingBranch`

The result is stored in `git.Manager.PrevBranch` via `SetPrevBranch()`.
This is the **only** place PrevBranch is set. Branch renames do not
affect it.

## Resume Flow

1. `tryResumeWorktree` — restores worktree path and state from state.json. No git operations.
2. `initRun` — calls `setStackHead()` to derive PrevBranch, then `EnsureUpToDate` which rebases onto PrevBranch (or default branch if no stack).

## Iteration Flow

1. `setStackHead()` — re-derive PrevBranch (previous task may have just merged)
2. `checkoutExistingBranch` — check bead metadata for stored branch name:
   - Remote has commits and branch is on default branch → check out remote branch
   - Remote has commits but diverged → delete stale remote, start fresh with branch name
   - No remote commits → reuse branch name locally from current HEAD
   - No stored branch → generate name via `BranchName()`, store in bead metadata
3. Agent runs on the branch
4. Agent signals completion → orchestrator verifies
5. `pushSignalPR` — push branch, create PR targeting PrevBranch (or default branch)
6. `mergeIfEnabled` — if auto-merge and PR targets default branch, run CI-aware merge:
   - Wait for CI
   - CI fails → spawn fix agent → retry
   - Fix agent exhausted → skip task
   - CI passes → merge → `PostMergeUpdateMain`
   - PR targets non-default (stacked) → skip merge, close bead
7. `closeOrRetryTask` — only close if PR exists (PRNum non-empty). No PR = task stays open.

## PR Targeting

`PushAndCreatePR` uses `PrevBranch` for the PR base:
- PrevBranch set (open PR in stack) → PR targets that branch
- PrevBranch empty → PR targets default branch
- If no commits between PrevBranch and HEAD → fall back to default branch

## EnsureUpToDate

Rebases onto `PrevBranch` when set, otherwise the default branch. Used:
- On resume (via `initRun`)
- Between tasks when the task changes (via `handleRebase` in the main loop)

## Branch Naming

All branch names go through `normalizeBranch()` which strips duplicate
`ralph/` prefixes:

- With taskID: `ralph/<taskID>-<slug>`
- Without taskID: `ralph/<slug>`
- Wip: `ralph/wip`

Branch names are stored in bead metadata (`bd update --metadata`) when
first created. On subsequent iterations for the same task, the stored
name is reused.

## resolveByPRState (Resume with Existing PR)

When `resumeViaPR` finds an existing PR for a task:

- **MERGED** → close bead, update main, move on
- **OPEN** → run `prChainIsHealthy`:
  - Check head branch exists and is on default branch
  - If unhealthy → return false, agent re-runs task fresh
  - If healthy → run CI-aware merge (same as post-signal), close on success
- **CLOSED** → return false, agent re-runs task fresh

## Bead Lifecycle

A bead is only closed when:
1. A PR exists (PRNum non-empty), AND
2. Either: merge succeeded, OR auto-merge is off, OR PR is stacked (will merge when base lands)

A bead is skipped when:
- Merge failed after fix agent exhausted retries

A bead stays open when:
- No PR was created (push failed, PR creation failed)
- PR chain is unhealthy (stale branch, diverged from default)

## Completed Tasks Array

`state.json` contains `completed_tasks[]` with entries added by
`persistCompletedTask` after successful bead closure:

```json
{
  "completed_tasks": [
    {"id": "ralph-abc", "title": "...", "pr_number": "285", "close_reason": "Fixed in PR #285"}
  ]
}
```

Only tasks with PRs appear here. This array is the source of truth for
stack head derivation.
