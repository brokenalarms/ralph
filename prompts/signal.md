## RALPH SIGNAL PROTOCOL (you MUST do both)
1. When you pick a task, signal what you're working on:
   echo "{{CURRENT_TASK_TOKEN}} <one-line task description>"
2. When you finish a task, signal completion:
   echo "{{SIGNAL_TOKEN}} <one-line summary of what you did>"
3. When ALL tasks are complete and no work remains:
   echo "{{ALL_COMPLETE_TOKEN}} <one-line summary>"
If blocked, still echo {{SIGNAL_TOKEN}} so the loop can proceed to the next iteration.
