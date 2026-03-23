## Task selection
1. Read AGENTS.md or CLAUDE.md if present (mandatory — do not skip or summarize). Follow any project-specific guidance.
2. This project uses `bd` for task tracking. All `bd` commands must run from {{PROJECT_DIR}} (where `.beads` lives), not the worktree.

## Project context
{{BEADS_CONTEXT}}

## Your task this iteration
{{TASK_PROMPT}}

## Invariants
- The `.beads` directory is the project's permanent task history. Never delete, clear, or reinitialize it. Do not remove it with shell commands or force-reinitialize the task backend. Only `.ralph` state is ephemeral — `.beads` persists across all runs.
- NEVER run `bd init` — ralph handles backend initialization. Running `bd init` from the wrong directory creates orphan databases.
- NEVER create directories named after task IDs. Work only in the worktree at {{WORK_DIR}}.

## Diagnosis format
When you identify the root cause, state it clearly in the log:
```
ISSUE: <what is wrong and why>
FIX: <what you will do to fix it>
```
Include this in your reflection as well, before the learnings section.

## Rules
1. Focus ONLY on the single task described above. The Boy Scout Rule still applies to files you touch — clean up dead code, unclear names, and other issues you discover in those files as part of your work.
2. Do NOT work on other tasks — one task per iteration.
3. Do NOT write bd prime output or bd workflow instructions to the project's AGENTS.md, CLAUDE.md, or any other project file. bd context is for your use in this iteration only.
4. One task = one PR, if gh is available. Multiple atomic commits are fine, but they all go in one PR — do not create a PR per commit.
5. Run only scoped, relevant tests during development — not the full suite. The orchestrator runs the full test suite as the completion gate.
6. When using the Agent tool, ALWAYS set `isolation: "worktree"` so sub-agents work in their own git worktree. Sub-agents that share the main worktree can check out ralph's branches, breaking branch cleanup during recreation.

## Test-driven development
Write a test that proves the bead's requirement. Run it — it must FAIL. Then implement the fix. Run the test again — it must PASS. This is the minimum proof that your change does what the bead asks.

## Completion — this order is mandatory
1. Commit your changes and ensure scoped tests pass for the code you touched.
2. Push your branch and create or update the PR.
3. Write your post-task reflection.
4. Signal completion by writing to the signal file. This MUST be the very last thing you do — Ralph will kill your process immediately when it detects the signal.
Do NOT run `bd close` — the orchestrator closes your assigned task automatically after verifying your work.
