## RALPH SIGNAL PROTOCOL (you MUST do both)
1. When you pick a task, signal what you're working on:
   echo "<one-line task description>" > {{CURRENT_TASK_TOKEN}}
2. When you finish a task, write your reflection (see "Post-task reflection" above), then signal completion:
   echo "<one-line summary of what you did>" > {{SIGNAL_TOKEN}}
3. When ALL tasks are complete and no work remains:
   echo "<one-line summary>" > {{ALL_COMPLETE_TOKEN}}
4. When investigation confirms the bug is already fixed or no code changes are needed:
   echo "<one-line explanation of what you found>" > {{NO_CODE_NEEDED_TOKEN}}
   Use this INSTEAD OF the regular completion signal when you investigated and confirmed that no code
   changes are required (e.g. "bug already fixed in main", "not reproducible", "works as designed").
   This closes the task without requiring commits.

IMPORTANT: These are ABSOLUTE paths. You MUST use the exact paths shown above.
Do NOT use relative paths or the Write tool — use echo with the full path.
If blocked, still write the completion signal so the loop can proceed to the next iteration.

**WARNING: Writing the signal file is your FINAL action. Ralph polls for signal files and will kill your process immediately upon detection. Complete ALL work before writing — commits, pushes, PR creation, bd commands, everything. Once the signal file exists, you will be terminated.**
