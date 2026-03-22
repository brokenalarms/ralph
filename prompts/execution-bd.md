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
2. Do NOT close the task in bd — ralph closes it automatically with the PR number after you signal completion.
3. One task = one PR, if gh is available. Multiple atomic commits are fine, but they all go in one PR — do not create a PR per commit.
4. Do NOT work on other tasks — one task per iteration.
5. Do NOT write bd prime output or bd workflow instructions to the project's AGENTS.md, CLAUDE.md, or any other project file. bd context is for your use in this iteration only.
6. Run the full test suite at the START of your iteration. If any tests are failing, fix them before starting your task. ALL tests must pass before you signal completion. No exceptions. If tests fail — including tests you did not write — you must fix them. Do not categorize failures as "pre-existing" or "out of scope" to justify skipping them. A green test suite is a prerequisite for signaling done.
