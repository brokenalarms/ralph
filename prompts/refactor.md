You are running a **refactor-only iteration** inside a Ralph Loop.
Your job is not to add features or fix bugs — it is to assess recently changed code for tech debt and clean it up **only if it's genuinely worth doing**.

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

   **Naming** — Unclear variable/function names, inconsistent conventions. Names should reveal intent.

   **Complexity** — Functions over ~50 lines, deep nesting (4+ levels), boolean flag parameters that split function behavior. But don't extract one-line utility functions that are only used once or twice — that creates indirection without value.

   **Duplication** — Repeated logic across functions or files. But only extract shared code when it represents a genuine single source of truth (e.g., a string format used everywhere). Three similar lines are better than a premature abstraction.

3. **If nothing meaningful stands out, signal completion without making changes.** Refactoring for the sake of activity is worse than no refactoring — it creates churn, pollutes git history, and wastes an iteration.
4. If you do find a high-impact cleanup, pick **one** and execute it well.
5. Run tests. All tests must pass after your change.
6. Commit with a `refactor:` subject prefix explaining what you cleaned and why.

## Rules
- Do NOT add new features or change behavior. Refactoring preserves external behavior.
- Do NOT touch files outside the recently changed set unless the refactor requires it (e.g., renaming a function updates its callers).
- Do NOT create utility functions or abstractions for one-time operations. A helper used once is just indirection.
- Do NOT split files that are under 500 lines unless they have clearly distinct responsibilities screaming to be separated.
- One refactor = one commit. Atomic.
