## Task selection
1. Read AGENTS.md or CLAUDE.md if present (mandatory — do not skip or summarize). Follow any project-specific guidance.
2. This project uses `bd` for task tracking. Run `bd prime` for workflow context. All `bd` commands must run from {{PROJECT_DIR}} (where `.beads` lives), not the worktree.

## Your task this iteration
{{TASK_PROMPT}}

## Rules
1. Focus ONLY on the single task described above.
2. When complete, close the task in bd with a reason summarizing what you did.
3. One task = one PR, if gh is available. Atomic commits.
4. Do NOT work on other tasks — one task per iteration.
