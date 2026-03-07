Read the project at {{WORK_DIR}}.

1. Read AGENTS.md or CLAUDE.md if AGENTS.md is not present (mandatory — do not skip or summarize).
2. Read todo files, specs/, README.md, and any task-related files for context.

If the project's AGENTS.md or CLAUDE.md defines task priority or selection order, it is the sole authority. Do NOT pick tasks based on recency, specificity, or your own judgment of what seems easiest. If no such instructions exist, plan in order of most high-leverage impactful task first. Recency of entry is not something that should be accorded any weight in this decision.

{{PLANNING_CONTEXT}}

## Specs
If `specs/` contains spec files, derive the plan from them — break each spec into atomic tasks.
Before creating tasks for a spec, check whether the described feature already exists in the codebase. Read the spec's acceptance criteria and verify against actual code. Skip specs whose work is already implemented.

Do NOT create new spec files. Specs are design artifacts that frame how work gets done — getting them wrong is worse than not having them. Without a user in the loop to validate design decisions, stick to deriving plans from existing specs or repo context.

## Output
Break the work into atomic, self-contained tasks. If `bd` is available, run `bd prime` to learn the workflow, then create tasks directly in bd with dependencies during this planning session. There is no plan.md when using bd. If `bd` is not available, write the plan to {{PLAN_FILE}} using markdown checkboxes:
- [ ] Task 1 description
- [ ] Task 2 description

Each task should be completable in a single Claude session. Be specific and actionable.
After creating the plan, signal completion: echo "{{SIGNAL_TOKEN}}" > "{{SIGNAL_FILE}}"
