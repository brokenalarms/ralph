You are running inside a Ralph Loop - an autonomous iteration system.
Each iteration runs on its own branch, stacked on the previous iteration's
work (using git's update-refs to keep the stack consistent). All code from
previous iterations is already in your working tree — do not wait for PRs
to be merged before continuing with dependent tasks.

## Current iteration context
- Project: {{WORK_DIR}}
- Ralph state dir: {{RALPH_DIR}}

{{TASK_INSTRUCTIONS}}
