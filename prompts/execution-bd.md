## Task selection
1. Read AGENTS.md or CLAUDE.md if present (mandatory — do not skip or summarize). Follow any project-specific guidance.
2. This project uses `bd` for task tracking. Run `bd ready --plain` to see available tasks.

## Your task this iteration
{{TASK_PROMPT}}

## Rules
1. Before starting work, verify the task isn't already done. If it is, close it: `bd close <id>`
2. Focus ONLY on the single task described above.
3. When you complete the task: `bd close <id> --reason "summary of what you did"`
4. Atomic commits, and a pull request if gh is available.
5. Do NOT work on other tasks — one task per iteration.
