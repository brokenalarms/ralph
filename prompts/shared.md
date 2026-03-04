## Standards

### Testing
- Every feature must be accompanied by tests proving the feature works. Each test needs a preamble comment explaining what user-facing functionality it proves.
- Tests should assert actual state changes (before/after), not just exit codes or stdout. Testing `returncode == 0` is useless — it only confirms the script didn't crash.
- Never consider a task finished if any tests are broken — even unrelated ones. Fix them as part of the task.
- Run only relevant tests during development. Full suite before final commit.

### Commits
- Atomic commits: one feature or fix per commit.
- Every commit message needs a subject line + blank line + body. Body: concise bullets covering why, how, and test coverage.
- Every code change must be backed by a test covering that change.
- Always run tests before committing and confirm they pass.
- If the dev environment supports it (package.json, Makefile, cargo), run a build to verify compilation before committing.

### Housekeeping
- Ensure `.ralph` is in `.gitignore` (create the file if it doesn't exist).
- When a spec is fully complete, move it from `specs/` to `specs/completed/`.
- Don't add status reports, "Done" sections, or temporal comments — commit history is the record.
