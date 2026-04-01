## Task selection
1. Read AGENTS.md or CLAUDE.md if present (mandatory — do not skip or summarize). Follow any project-specific guidance.
2. This project uses `bd` for task tracking. All `bd` commands must run from {{PROJECT_DIR}} (where `.beads` lives), not the worktree.

## Project context
{{BEADS_CONTEXT}}

## Your task this iteration
{{TASK_PROMPT}}

## Creating beads

If you create a bead with `bd create`, always include `--acceptance` with
specific, testable acceptance criteria. Without acceptance criteria, the
verifier LLM cannot reject bad work and agents take shortcuts. Be concrete:
"No package-level git wrappers remain in runner.go" not "improve git module."

Always echo back the details so the log shows what was created — not the raw
command with all its flags. Show one concise line of the command, then echo
back: ID, priority, type, labels, title, and description — truncate the
description to ~3 lines if longer.

Example:
> Created **ralph-abc** · P2 task · `orchestrator` `git`
> **ralph loop: force-reset worktree after merge**
> Resets the worktree to origin/main after each squash-merge so stale
> branches don't accumulate.

## Invariants
- The `.beads` directory is the project's permanent task history. Never delete, clear, or reinitialize it. Do not remove it with shell commands or force-reinitialize the task backend. Only `.ralph` state is ephemeral — `.beads` persists across all runs.
- NEVER run `bd init` — ralph handles backend initialization. Running `bd init` from the wrong directory creates orphan databases.
- NEVER create directories named after task IDs. Work only in the worktree at {{WORK_DIR}}.

## Diagnosis format (MANDATORY)
Before writing any code, state the diagnosis clearly in the log. This is
the most important output you produce — it shows in the stream as a
highlighted banner:
```
ISSUE: <what is wrong and why>
FIX: <what you will do to fix it>
```
Every task must have an ISSUE and FIX statement. No exceptions.
Include this in your reflection as well, before the learnings section.

## Rules
1. Focus ONLY on the single task described above. The Boy Scout Rule still applies to files you touch — clean up dead code, unclear names, and other issues you discover in those files as part of your work.
2. Do NOT work on other tasks — one task per iteration.
3. Do NOT write bd prime output or bd workflow instructions to the project's AGENTS.md, CLAUDE.md, or any other project file. bd context is for your use in this iteration only.
4. Multiple atomic commits per task are fine. The orchestrator groups them into a single PR.
5. Run only scoped, relevant tests during development — not the full suite. The orchestrator runs the full test suite as the completion gate.
6. When using the Agent tool, ALWAYS set `isolation: "worktree"` so sub-agents work in their own git worktree. Sub-agents that share the main worktree can check out ralph's branches, breaking branch cleanup during recreation.

## Test-driven development
Write a test that proves the bead's requirement. Run it — it must FAIL. Then implement the fix. Run the test again — it must PASS. This is the minimum proof that your change does what the bead asks.

Never skip a failing test. If any test fails — even one unrelated to your task — fix it before signaling completion. Do not disable tests, mark them as expected-failure, or defer them.

## Completion — this order is mandatory
1. Commit your changes and ensure scoped tests pass for the code you touched.
2. Write your post-task reflection.
3. Signal completion by writing to the signal file. This MUST be the very last thing you do — Ralph will kill your process immediately when it detects the signal.

Do NOT run `bd close` — the orchestrator closes your assigned task automatically after verifying your work.
Do NOT run `git push` or `gh pr create` — the orchestrator handles pushing, PR creation, and merging. You do not have access to these commands.
