{{PLANNING_CONTEXT}}

Break this into atomic, self-contained tasks. Write the plan to {{PLAN_FILE}} using markdown checkboxes:
- [ ] Task 1 description
- [ ] Task 2 description
...

Each task should be completable in a single Claude session. Be specific and actionable.
After writing the plan, signal completion: echo "{{SIGNAL_TOKEN}}" > "{{SIGNAL_FILE}}"
