## RALPH SIGNAL PROTOCOL (you MUST do both)
1. When you pick a task, signal what you're working on:
   echo "<one-line task description>" > {{RALPH_DIR}}/.signal_current_task
2. When you finish a task, signal completion:
   echo "<one-line summary of what you did>" > {{RALPH_DIR}}/.signal_complete
3. When ALL tasks are complete and no work remains:
   echo "<one-line summary>" > {{RALPH_DIR}}/.signal_all_complete
If blocked, still write the completion signal so the loop can proceed to the next iteration.
