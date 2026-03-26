# Stacked Single-Commit PRs

## Problem

The current git strategy resets the worktree to `origin/main` after every
squash-merge. Each task starts from scratch. If anything goes wrong (GitHub
down, CI flaky, merge conflict from parallel pushes), the next task starts
from a stale main and potentially conflicts with everything in between.

Work is fragile: a single merge failure can cascade into repeated conflicts
across subsequent tasks. The `RemoteBranchHasWork`, `ResetAndReplay`, and
`PostMergeReset` machinery exists to recover from these failures — complexity
that wouldn't be needed if tasks built on each other linearly.

## Design

Each task produces exactly one commit. The worktree is never reset to main.
Tasks build linearly on each other:

```
main ← task-1 (1 commit) ← task-2 (1 commit) ← task-3 (1 commit)
```

Each commit gets its own PR targeting the previous PR's branch (or main
for the first). PRs are one commit each and fast-forward mergeable.

### Commit discipline

The agent squashes all its work into a single commit before signaling
completion. The orchestrator enforces this: if HEAD is more than 1 commit
ahead of the previous task's commit, the orchestrator squashes before pushing.

### PR creation

```
PR #1: task-1 branch → main        (1 commit ahead of main)
PR #2: task-2 branch → task-1      (1 commit ahead of task-1)
PR #3: task-3 branch → task-2      (1 commit ahead of task-2)
```

When PR #1 merges (fast-forward into main), PR #2's base is now main.
GitHub auto-updates the base branch. PR #2 becomes 1 commit ahead of main.

### No more PostMergeReset

After a PR merges:
1. The worktree stays where it is — on the next task's commit
2. No `git reset --hard origin/main`
3. No branch deletion and recreation
4. No `RecreateFromMain`

The worktree just keeps advancing linearly.

### Merge order

PRs merge in order: #1 before #2 before #3. The orchestrator merges the
oldest unmerged PR first. If #1 can't merge (CI failure, conflict), #2
and #3 wait. The agent keeps working on #4, #5 etc. — work isn't blocked,
only merging is.

### Resolving conflicts

When the stack conflicts with main (someone pushed directly):

1. `git rebase --update-refs origin/main` from the tip of the stack
2. Resolve conflicts at the first conflicting commit
3. Subsequent commits replay cleanly (they're independent changes)
4. Force-push all branches: `git push --force-with-lease origin branch1 branch2 ...`
5. GitHub PRs auto-update

This is one rebase operation, not N separate conflict resolutions.

### Failed CI

If task-3's PR fails CI:
- Task-4, task-5 etc. continue building on top (CI failure doesn't block work)
- A fix agent (or the main agent via stdin pipe) fixes the issue
- The fix becomes part of task-3's single commit (amend)
- Force-push task-3's branch, CI re-runs
- Downstream PRs are unaffected (their base is task-3's branch, not its content)

### Session boundary

When the loop stops and restarts:
1. Resume from the latest commit on the stack
2. `git rebase --update-refs origin/main` to sync with any changes merged while stopped
3. Continue building on top

No worktree recreation, no branch detection heuristics.

## What gets removed

- `PostMergeReset` — no reset after merge
- `RecreateFromMain` — no worktree recreation
- `RemoteBranchHasWork` — no stale branch detection
- `resumeFromRemoteBranch` — no legacy resume path
- `ResetAndReplay` — no cherry-pick replay
- `RotateBranch` — no branch rotation to `/next`
- `TempBranch()` / `/next` — no placeholder branch name
- Squash-merge detection (`IsBranchSquashMerged`, `findLastSquashMergedBranch`)

All replaced by: linear commits, `--update-refs` rebase, sequential PR merge.

## What stays

- `EnsureUpToDate` — still needed for the initial sync on startup
- Branch naming (`ralph/<project>/<seq>-<beadID>-<slug>`) — each commit gets a branch
- `PushAndCreatePR` — still creates PRs, but targets previous branch, not main
- `AutoMergeCurrentBranch` — still merges, but only the oldest unmerged PR
- CI polling — unchanged
- LLM verification — unchanged

## Migration

1. Current worktrees continue working — the first task after migration starts
   a new linear chain from main
2. Old remote branches (pre-migration) are cleaned up on first run
3. No database migration needed — beads are unaffected
4. The `--update-refs` flag is already used in rebase paths

## Risks

- **GitHub base branch updates**: When PR #1 merges, GitHub should auto-update
  PR #2's base from `task-1` to `main`. If it doesn't, the orchestrator needs
  to manually update the base via API.

- **Long stacks**: A stack of 50 PRs is hard to manage. Consider auto-merging
  completed PRs eagerly (as today) to keep the stack short.

- **Amend + force-push**: Squashing agent work into one commit requires
  amending. If the branch is already pushed, this needs force-push. The
  orchestrator owns push so this is safe, but it changes the commit hash
  which invalidates CI runs.

- **Reviewer experience**: Each PR is one commit, one task — easy to review.
  But the PR diff shows changes relative to the previous task's branch, not
  main. Reviewers need to understand the stack context.
