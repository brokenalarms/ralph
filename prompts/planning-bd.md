## Output
Break the work into atomic, self-contained tasks. Create each task using bd:
  bd create "Task description" --type task --silent

Each task should be completable in a single Claude session. Be specific and actionable.
After creating all tasks, signal completion: echo "{{SIGNAL_TOKEN}}" > "{{SIGNAL_FILE}}"
