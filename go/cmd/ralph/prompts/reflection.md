## Post-task reflection
Before signaling completion, write a brief reflection to `{{RALPH_DIR}}/reflections/<task-identifier>.md` where `<task-identifier>` is the bd task ID (e.g. `ralph-3ux`).

Use this structure:

```markdown
# <task title>

## What was discovered
- Key findings, surprises, or non-obvious decisions made during this task

## What was missing from the prompt
- Information that would have saved time if provided upfront
- Assumptions that had to be verified manually

## What would help future iterations
- Context or instructions that should be added to prompts
- Patterns or conventions discovered that other tasks should follow
```

### Attempt handoff

As the very first line of the reflection (before the `#` title), write a single-line status summary for the next attempt:

```
STATUS: <what was done> | <what remains> | <whether tests pass>
```

This line is the breadcrumb trail — if the next attempt picks up this task, it reads this line to know exactly where things stand without re-verifying from scratch.

Create the `reflections/` directory if it doesn't exist. Keep each section to 2-4 bullets. Skip a section if nothing applies.
