## RALPH SIGNAL PROTOCOL (you MUST do both)
1. When you pick a task, signal what you're working on:
   echo "{{CURRENT_TASK_TOKEN}} <one-line task description>" > "{{SIGNAL_FILE}}"
2. When you finish, signal completion (overwrites the file):
   echo "{{SIGNAL_TOKEN}} <one-line summary of what you did>" > "{{SIGNAL_FILE}}"
If blocked, still write {{SIGNAL_TOKEN}} so the loop can proceed to the next iteration.
