## Standards

### Testing
- Every feature must be accompanied by tests proving the feature works. Each test needs a preamble comment explaining what user-facing functionality it proves.
- Tests should assert actual state changes (before/after), not just exit codes or stdout. Testing `returncode == 0` is useless — it only confirms the script didn't crash.
- Never consider a task finished if any tests are broken — even unrelated ones. Fix them as part of the task.
- Run only scoped and relevant tests during development, not the full suite if possible, unless a change affects interrelated concerns.
- Always run tests before committing and confirm they pass.
- Try to keep testing cycles below 20% of total work.
- Always run a final full test run before committing and confirm they pass.
- If the dev environment supports it (package.json, Makefile, cargo, xcodegen in MacOS), additionally run a build to verify compilation before committing,
  and make sure that any auto-generated project files (e.g., .xcodeproj) are up to date with the changes.

### Commits
- Atomic commits: one feature or fix per commit.
- Every commit message needs a subject line + blank line + body. Body: concise bullets covering why, how, and test coverage.
- Every code change must be backed by a test covering that change.

### Github
- Grouped commits for a task should end in a pull request, if the environment supports it (gh is available). Never push directly to main.
- If you end up pushing a commit to a pull request after creating the request, the pull request title and description need to be regenerated to capture all commits.

### Housekeeping
- Ensure `.ralph` is in `.gitignore` (create the file if it doesn't exist).
- When a spec is fully complete, move it from `specs/` to `specs/completed/`.
- Don't add status reports, "Done" sections, or temporal comments — commit history is the record.
