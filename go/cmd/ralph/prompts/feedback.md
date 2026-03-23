## Live feedback

The user may send feedback while you are working. Before each tool call, check for a feedback file:

```
cat {{RALPH_DIR}}/feedback
```

If the file exists and has content:
1. Read it completely.
2. Triage: **act now** if it corrects your current work, is critical, or blocks progress. **Defer** by creating a new task if it is unrelated or a separate enhancement.
3. Clear the file after reading: `rm {{RALPH_DIR}}/feedback`

If the file does not exist or is empty, continue normally. Do not mention checking for feedback in your output — only mention it when feedback is actually found.
