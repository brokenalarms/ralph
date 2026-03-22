## Task selection
1. Read AGENTS.md or CLAUDE.md if present (mandatory — do not skip or summarize). Follow any project-specific guidance.
2. This project uses `bd` for task tracking. Run `bd prime` for workflow context. All `bd` commands must run from {{PROJECT_DIR}} (where `.beads` lives), not the worktree.

## Your task this iteration
{{TASK_PROMPT}}

## Invariants
- The `.beads` directory is the project's permanent task history. Never delete, clear, or reinitialize it. Do not remove it with shell commands or force-reinitialize the task backend. Only `.ralph` state is ephemeral — `.beads` persists across all runs.
- NEVER run `bd init` — ralph handles backend initialization. Running `bd init` from the wrong directory creates orphan databases.
- NEVER create directories named after task IDs. Work only in the worktree at {{WORK_DIR}}.

## Rules
1. Focus ONLY on the single task described above.
2. Do NOT work on other tasks — one task per iteration.
3. Do NOT write bd prime output or bd workflow instructions to the project's AGENTS.md, CLAUDE.md, or any other project file. bd context is for your use in this iteration only.
4. Run only scoped, relevant tests during development — not the full suite. The orchestrator runs the full test suite as the completion gate.
5. Do NOT run git commit, git push, gh pr, or bd close. The orchestrator handles all git workflow and task lifecycle after verifying your work.

## Completion — this order is mandatory
1. Ensure scoped tests pass for the code you touched. Leave your changes uncommitted — the orchestrator commits, pushes, and creates the PR.
2. Write your post-task reflection.
3. Signal completion by writing to the signal file. This MUST be the very last thing you do — Ralph will kill your process immediately when it detects the signal.
