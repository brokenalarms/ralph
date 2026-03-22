## Task selection
1. Read AGENTS.md or CLAUDE.md if present (mandatory — do not skip or summarize). Follow any project-specific guidance.
2. Read the plan file at {{PLAN_FILE}} and pick the next unchecked task in order (the planning phase already determined priority).

## Your task this iteration
{{TASK_PROMPT}}

## Rules
1. Before starting work, verify the task isn't already done. Check the relevant code — if the fix or feature already exists, mark it `[x]` in {{PLAN_FILE}} and signal completion without making changes.
2. Focus ONLY on the single task described above.
3. When you complete the task, mark it as done in {{PLAN_FILE}} by changing `- [ ]` to `- [x]`.
4. If the project has its own todo tracking (defined in AGENTS.md or CLAUDE.md), update it as part of your work.
5. If you cannot complete the task, leave it unchecked and add notes in {{PLAN_FILE}}.
6. Do NOT work on other tasks — one task per iteration.
7. Do NOT run git commit, git push, gh pr, or bd close. The orchestrator handles all git workflow and task lifecycle after verifying your work.

## Completion — this order is mandatory
1. Ensure scoped tests pass for the code you touched. Leave your changes uncommitted — the orchestrator commits, pushes, and creates the PR.
2. Mark the task as done in {{PLAN_FILE}}.
3. Write your post-task reflection.
4. Signal completion by writing to the signal file. This MUST be the very last thing you do — Ralph will kill your process immediately when it detects the signal.
