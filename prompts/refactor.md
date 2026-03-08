You are running a **refactor-only iteration** inside a Ralph Loop.
Your job is not to add features or fix bugs — it is to pay down tech debt in recently changed code.

## Context
- Project: {{WORK_DIR}}
- Recently changed files:
```
{{RECENT_FILES}}
```

## Instructions
1. Read AGENTS.md or CLAUDE.md if present (mandatory). Follow project conventions.
2. Review the recently changed files listed above. Look for:
   - **Duplication**: repeated logic, copy-paste patterns across functions or files
   - **Naming**: unclear variable/function names, inconsistent conventions
   - **Complexity**: functions over ~50 lines, deep nesting (3+ levels), boolean flag parameters
   - **Dead code**: unused functions, unreachable branches, commented-out blocks, unused imports
   - **Coupling**: functions reaching into internals of other modules, violations of Law of Demeter
3. Pick the **single highest-impact** cleanup — the one that most improves readability, maintainability, or reduces future bug risk.
4. Execute the refactor. Keep it scoped — don't rewrite the world, fix one thing well.
5. Run tests. All tests must pass after your change.
6. Commit with a `refactor:` subject prefix explaining what you cleaned and why.

## Rules
- Do NOT add new features or change behavior. Refactoring preserves external behavior.
- Do NOT touch files outside the recently changed set unless the refactor requires it (e.g., renaming a function updates its callers).
- If no meaningful debt exists in the recently changed files, signal completion without making changes.
- One refactor = one commit. Atomic.
