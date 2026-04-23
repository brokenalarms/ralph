## Task selection
1. Read AGENTS.md or CLAUDE.md if present (mandatory — do not skip or summarize). Follow any project-specific guidance.

## Project context
{{BEADS_CONTEXT}}

## Your task this iteration
{{TASK_PROMPT}}

## Invariants
- Work only in the worktree at {{WORK_DIR}}. Do not create or modify files outside this directory.
- NEVER create directories named after task IDs.

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
3. Multiple atomic commits per task are fine. The orchestrator groups them into a single PR.
4. Run only scoped, relevant tests during development — not the full suite. The orchestrator runs the full test suite as the completion gate.
5. When using the Agent tool, ALWAYS set `isolation: "worktree"` so sub-agents work in their own git worktree. Sub-agents that share the main worktree can check out ralph's branches, breaking branch cleanup during recreation.

## Test-driven development
Write a test that proves the bead's requirement. Run it — it must FAIL. Then implement the fix. Run the test again — it must PASS. This is the minimum proof that your change does what the bead asks.

Never skip a failing test. If any test fails — even one unrelated to your task — fix it before signaling completion. Do not disable tests, mark them as expected-failure, or defer them.

## Completion — this order is mandatory
1. Commit your changes and ensure scoped tests pass for the code you touched.
2. Write your post-task reflection.
3. Signal completion by writing to the signal file. This MUST be the very last thing you do — Ralph will kill your process immediately when it detects the signal.

Do NOT run `git push` or `gh pr create` — the orchestrator handles pushing, PR creation, and merging.
