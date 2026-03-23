## Task selection
1. Read AGENTS.md or CLAUDE.md if present (mandatory — do not skip or summarize). Follow any project-specific guidance.
2. Read the plan file at {{PLAN_FILE}} and pick the next unchecked task in order (the planning phase already determined priority).

## Your task this iteration
{{TASK_PROMPT}}

## Rules
1. Before starting work, verify the task isn't already done. Check the relevant code — if the fix or feature already exists, mark it `[x]` in {{PLAN_FILE}} and signal completion without making changes.
2. Focus ONLY on the single task described above. The Boy Scout Rule still applies to files you touch — clean up dead code, unclear names, and other issues you discover in those files as part of your work.
3. When you complete the task, mark it as done in {{PLAN_FILE}} by changing `- [ ]` to `- [x]`.
4. If the project has its own todo tracking (defined in AGENTS.md or CLAUDE.md), update it as part of your work.
5. One task = one PR, if gh is available. Multiple atomic commits are fine, but they all go in one PR — do not create a PR per commit.
6. If you cannot complete the task, leave it unchecked and add notes in {{PLAN_FILE}}.
7. Do NOT work on other tasks — one task per iteration.
8. When using the Agent tool, ALWAYS set `isolation: "worktree"` so sub-agents work in their own git worktree. Sub-agents that share the main worktree can check out ralph's branches, breaking branch cleanup during recreation.

## Completion — this order is mandatory
1. Commit your changes and ensure scoped tests pass for the code you touched.
2. Push your branch and create or update the PR.
3. Mark the task as done in {{PLAN_FILE}}.
4. Write your post-task reflection.
5. Signal completion by writing to the signal file. This MUST be the very last thing you do — Ralph will kill your process immediately when it detects the signal.
