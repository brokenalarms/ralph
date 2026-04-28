## Task selection
1. Read AGENTS.md or CLAUDE.md if present (mandatory — do not skip or summarize). Follow any project-specific guidance.

## Project context
{{BEADS_CONTEXT}}

## Your task this iteration
{{TASK_PROMPT}}

## Invariants
- Work only in the worktree at {{WORK_DIR}}. Do not create or modify files outside this directory.
- NEVER create directories named after task IDs.
- Never define aliases, shell functions, or env vars that shadow bd, dolt, gh, git push, mysql, sqlite3, psql, or other restricted commands.
- Never read .beads/ files directly (cat, sed, awk, less, Read, etc.) — use bd commands exclusively.

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

## Verification before completion — confidence is not evidence

Before signaling completion, you MUST have concrete evidence that your change works:
- **Run the relevant tests** and see them pass. "I wrote a test" is not evidence — "I ran it and it passed" is.
- **Read back any files you changed** if unsure whether edits landed correctly. Do not trust memory of what you wrote.
- **If you wrote a test, confirm it fails without your fix.** A test that passes with AND without the fix proves nothing.

"Should work" is not acceptable. "I'm confident" is not acceptable. Only command output counts.

## Anti-rationalization

You will be tempted to skip rules above. Here are the specific rationalizations that lead to rejected work — and why each one is wrong:

| You might think | Why it's wrong |
|---|---|
| "The fix is so simple it doesn't need a test" | Simple fixes break silently. The verifier rejects untested code changes regardless of complexity. |
| "The existing tests cover this implicitly" | If no test explicitly asserts the behavior from the acceptance criteria, the verifier cannot confirm it passed. Write the explicit test. |
| "I'll skip TDD since I already know the fix" | TDD is not about discovery — it's proof. A test that never failed might pass for the wrong reason. |
| "This test failure is unrelated to my task" | Every set of AC implicitly includes "all existing tests pass." Unrelated failures are your problem. |
| "I'll skip diagnosis since the fix is obvious" | The ISSUE/FIX banner is how the stream log surfaces what you did. Omitting it means no one can audit your work. |
| "Running tests will waste context" | Skipping tests wastes an entire iteration when the verifier rejects for missing evidence. |
| "I'll commit after running the full suite" | Commit FIRST. If the process is killed before the suite finishes, your work is lost. WIP commits prevent this. |

## Completion — this order is mandatory
1. Verify your change works (run tests, read output, confirm evidence — see above).
2. Commit your changes.
3. Write your post-task reflection.
4. Signal completion by writing to the signal file. This MUST be the very last thing you do — Ralph will kill your process immediately when it detects the signal.

Do NOT run `git push` or `gh pr create` — the orchestrator handles pushing, PR creation, and merging.
