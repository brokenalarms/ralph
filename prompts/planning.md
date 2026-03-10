Read the project at {{WORK_DIR}}.

1. Read AGENTS.md or CLAUDE.md if AGENTS.md is not present (mandatory — do not skip or summarize).
2. Read todo files, specs/, README.md, and any task-related files for context.

If the project's AGENTS.md or CLAUDE.md defines task priority or selection order, it is the sole authority. Do NOT pick tasks based on recency, specificity, or your own judgment of what seems easiest. If no such instructions exist, plan in order of most high-leverage impactful task first. Recency of entry is not something that should be accorded any weight in this decision.

{{PLANNING_CONTEXT}}

## Debt assessment
Before writing tasks, scan the areas you plan to touch for readability issues:
- **Bloated files**: Files over ~500 lines with distinct responsibilities that should be split. But don't split files that are cohesive just because they're long.
- **Dead code**: Unused functions, commented-out blocks, unused imports. These are never worth keeping — git remembers.
- **Unclear naming**: Variable/function names that don't reveal intent.
- **Deep nesting**: 4+ levels of indentation — use early returns, guard clauses, extract functions.
- **Genuine duplication**: Copy-paste code that represents a single source of truth being maintained in multiple places. But three similar lines are better than a premature abstraction. Don't extract one-line utilities used once or twice.

If you find high-value debt in code the planned work will touch, add refactor tasks to the plan. Interleave them with feature tasks — don't batch all refactors at the end. Refactor tasks should use a `refactor:` prefix. But don't add refactor tasks just because you can — only when the cleanup genuinely improves human readability.

## Specs
If `specs/` contains spec files, derive the plan from them — break each spec into tasks.
Before creating tasks for a spec, check whether the described feature already exists in the codebase. Read the spec's acceptance criteria and verify against actual code. Skip specs whose work is already implemented.

Do NOT create new spec files. Specs are design artifacts that frame how work gets done — getting them wrong is worse than not having them. Without a user in the loop to validate design decisions, stick to deriving plans from existing specs or repo context.

## Output
Break the work into tasks. Each task is a single working change — the project must build and pass tests after every task. If a change spans multiple files (removing a module and its references, renaming an interface and its implementors), that is one task, not several. Do not split changes that must happen together to keep the project working.

{{TASK_INSTRUCTIONS}}

Each task should be completable in a single Claude session. Be specific and actionable.
After creating the plan:
1. Write a short theme (2-5 words) summarizing the overall work into state. This names the worktree, so keep it concise and descriptive. Use jq:
   jq --arg v "auth middleware rewrite" '.theme = $v' "{{STATE_FILE}}" > /tmp/ralph-state && mv /tmp/ralph-state "{{STATE_FILE}}"
2. Signal completion: echo "{{SIGNAL_TOKEN}}"
