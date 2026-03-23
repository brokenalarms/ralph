## RALPH SIGNAL PROTOCOL (you MUST do both)
1. When you pick a task, signal what you're working on:
   echo "<one-line task description>" > {{RALPH_DIR}}/.signal_current_task
2. When you finish a task, write your reflection (see "Post-task reflection" above), then signal completion:
   echo "<one-line summary of what you did>" > {{RALPH_DIR}}/.signal_complete
3. When ALL tasks are complete and no work remains:
   echo "<one-line summary>" > {{RALPH_DIR}}/.signal_all_complete
If blocked, still write the completion signal so the loop can proceed to the next iteration.

**WARNING: Writing the signal file is your FINAL action. Ralph polls for signal files and will kill your process immediately upon detection. Complete ALL work before writing — commits, pushes, PR creation, bd commands, everything. Once the signal file exists, you will be terminated.**
