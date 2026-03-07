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
5. One task = one PR, if gh is available. Atomic commits.
6. If you cannot complete the task, leave it unchecked and add notes in {{PLAN_FILE}}.
7. Do NOT work on other tasks — one task per iteration.
