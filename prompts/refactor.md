You are running a **refactor-only iteration** inside a Ralph Loop.

Claude doesn't feel the friction that forces human developers to refactor. You understand tangled code just fine — you'll explain it, extend it, work around it. But a human reading this code later won't have that luxury. Your job is to be the pain signal that's gone missing: look at recently changed code through a human's eyes and clean it up **only if it's genuinely worth doing**.

## Context
- Project: {{WORK_DIR}}
- Recently changed files:
```
{{RECENT_FILES}}
```

## Instructions
1. Read AGENTS.md or CLAUDE.md if present (mandatory). Follow project conventions.
2. Review the recently changed files listed above. Assess for **human readability** — that is the goal, not arbitrary rules:

   **Size** — Files approaching 500+ lines should be considered for splitting. But splitting for the sake of it creates navigation overhead. Split when a file has genuinely distinct responsibilities, not because a line count was exceeded.

   **Dead code** — Unused functions, unreachable branches, commented-out code, unused imports. These are never worth keeping. Git remembers. Delete them.

   **Naming** — Unclear variable/function names, inconsistent conventions. Names should reveal intent. If something is 'hardcoded' eg ANMATE = true, consider that this has more likely been a config flag extracted for easy testing. Consider extracting these to a more obvious shared config object or module.

   **Complexity** — Functions over ~50 lines, deep nesting (4+ levels), boolean flag parameters that split function behavior. But don't extract one-line utility functions that are only used once or twice — that creates indirection without value.

   **Duplication** — Repeated logic across functions or files. But only extract shared code when it represents a genuine single source of truth (e.g., a string format used everywhere). Three similar lines are better than a premature abstraction.

3. **If nothing meaningful stands out, signal completion without making changes.** Refactoring for the sake of activity is worse than no refactoring — it creates churn, pollutes git history, and wastes an iteration. There is no need to 'poke' the code or make minimal, one-line changes. Exiting this cycle without refactoring is a valid outcome.
4. If you do find cleanup worth doing, start with the highest-impact issue. But if a better pattern emerges as you work — a coherent set of related changes that only becomes visible once you've started — follow it. Don't artificially limit yourself to one change if several form a natural unit of cleanup.
5. Run tests. All tests must pass after your change.
6. Commit with a `refactor:` subject prefix explaining what you cleaned and why.

## Rules
- Do NOT add new features or change behavior. Refactoring preserves external behavior.
- Do NOT attempt big architectural rewrites. You are not trustworthy for those — things get missed, unnecessary fallbacks creep in, corner cases aren't covered. Keep changes scoped to what you can fully verify with tests.
- Do NOT touch files outside the recently changed set unless the refactor requires it (e.g., renaming a function updates its callers).
- Do NOT create utility functions or abstractions for one-time operations. A helper used once is just indirection.
- Do NOT split files that are under 500 lines unless they have clearly distinct responsibilities screaming to be separated.
- One refactor = one commit. Atomic.
