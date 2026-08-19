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

The stack head is the branch the next task should build on. `setStackHead()`
(`go/internal/git/git_branch.go`) derives it from `completedBranches`, the
`completed_tasks[]` branch list in state.json (oldest first):

1. Only the newest completed branch (`completedBranches[len-1]`) is
   considered — everything older is ignored.
2. It must have an open PR: checked via `ListOpenPRBranches` membership. No
   open PR (or an empty `completedBranches`) → stack head is empty, use the
   default branch.
3. It must be cleanly ahead of main: `BranchIsAheadOfMain` (origin's default
   branch is an ancestor of the branch). This rejects branches that landed
   via squash-merge — after a squash-merge the local branch is diverged from
   main (it has commits main lacks, but main has the squashed commit the
   branch lacks), so `BranchIsAheadOfMain` returns false and the stale
   branch is rejected in favor of starting the next task from main.
4. **Adopted-stack exemption**: if the ahead-of-main guard fails, it is
   bypassed anyway when the branch is the adopted leftover-PR branch
   (`top == r.adoptedStackBranch`, set via `SetAdoptedStackBranch` — see
   Startup below), or when it is a descendant of that adopted branch
   (`r.isAncestor(origin/<adoptedStackBranch>, origin/<top>)` after
   fetching it). A branch with an open PR that diverged only because main
   advanced past the user's chosen stack is the intentional keep-stack case,
   not the stale-squash-merge case the guard exists to catch — this also
   covers PRs stacked on top of the adopted branch under a different name,
   which diverge from main the same way the adopted branch does. The
   open-PR requirement from step 2 still applies regardless of the
   exemption.

`setStackHead()` runs:
- On resume in `initRun`, before the first iteration
- At the start of each iteration, before `checkoutExistingBranch`

The result is stored in `git.Manager.PrevBranch` via `SetPrevBranch()`.
This is the **only** place PrevBranch is set. Branch renames do not
affect it.

## Startup: Leftover-PR Adopt-or-Fresh

Before any git branch setup (before `gm.Init`/`SetupWorktree`), `main.go`
calls `checkLeftoverRalphPRs` (`go/cmd/ralph/leftover_prs.go`) exactly once
per loop start.

1. List all PRs (`gm.ListAllPRs`) and filter to ralph-authored ones still
   open from a prior run (`git.LeftoverRalphPRs`). None found → proceed with
   no prompt.
2. **Lone-leftover merge retry**: if exactly one leftover PR exists and its
   base is the default branch, `retryLoneLeftoverMerge` re-runs the
   CI-aware squash-merge (`gm.MergeStack`) for it before any prompt. Such a
   PR is one the loop already committed to merging — `completeTask` closes
   its bead as "verified — merge pending" on the promise the PR will land,
   and only a transient failure (e.g. a GitHub 503) leaves it open with
   nothing left to re-select the bead. On success, the run proceeds fresh
   from the (now-advanced) default branch with no prompt. Chains of two or
   more leftover PRs, or a leftover PR not based on the default branch, are
   left alone — merging the bottom of a stack outside `MergeStack`'s
   bottom-up walk would strand its descendants.
3. **Non-interactive default**: if stdin is not a TTY (`stdinIsTTY`) — the
   common case of ralph running under an orchestrator/supervisor — the
   leftover PRs are logged as a warning and the run silently starts fresh
   from origin/main. No prompt is shown.
4. **Interactive adopt-or-fresh prompt**: on a real TTY, the newest leftover
   PR is presented with three choices:
   - `y` — continue the stack on top of it; calls
     `gm.SetAdoptedStackBranch(top.Head)`, which feeds the adopted-stack
     exemption in `setStackHead` (see Stack Head Derivation above).
   - `n` — start fresh from origin/main; the leftover PR stays open and may
     go stale as main advances.
   - Ctrl-C — quit immediately with no branch/worktree state touched;
     `checkLeftoverRalphPRs` returns `ok=false` and the caller exits before
     `gm.Init` runs.

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
- Wip: `ralph/next` (`WipBranchName()` in `go/internal/git/git_helpers.go`)

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
