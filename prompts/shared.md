## Standards

### Testing
- Test behavioral logic — code with branching, state, algorithms, or business rules. Don't write tests for static content, markup, configuration, or simple data changes where a build check is sufficient.
- If the project has an existing test framework, use it. Don't invent ad-hoc test scripts for projects that don't have tests.
- Tests should assert actual state changes (before/after), not just exit codes or stdout. Testing `returncode == 0` is useless — it only confirms the script didn't crash.
- Each test needs a preamble comment explaining what user-facing functionality it proves.
- Never consider a task finished if any tests are broken — even unrelated ones. Fix them as part of the task.
- Run only scoped and relevant tests during development, not the full suite if possible, unless a change affects interrelated concerns.
- Unit tests are the building block — prefer them for verifying logic. Visual/UI/integration/end-to-end tests are expensive and should only run when significant UI changes have been made, not as routine verification for non-UI work.
- Always run tests in fast-fail mode with minimal output. Only show output for failing tests — passing test noise wastes context.
- Always run tests before committing and confirm they pass.
- Try to keep testing cycles below 20% of total work.
- Always run a final full test run before committing and confirm they pass.
- If the dev environment supports it (package.json, Makefile, cargo, xcodegen in MacOS), additionally run a build to verify compilation before committing,
  and make sure that any auto-generated project files (e.g., .xcodeproj) are up to date with the changes.

### Typing
- Use typed languages and variants from the outset: TypeScript over JavaScript, typed Python (type hints + mypy/pyright), etc.
- When creating new projects or files, always set up the build system to accommodate typing (e.g., `tsconfig.json` for TypeScript, pyproject.toml with pyrefly for Python type-checking).
- If adding code to an existing untyped project, introduce typing incrementally — add types to new files and functions you touch, don't rewrite the whole codebase.
- Prefer strict type-checking settings where the ecosystem supports it (e.g., `"strict": true` in tsconfig).

### Commits
- Atomic commits: one feature or fix per commit.
- Every commit message needs a subject line + blank line + body. Body: concise bullets covering why, how, and test coverage.
- Behavioral code changes should be backed by tests. Static content, markup, and config changes don't need tests — a passing build is verification enough.

### Github
- Grouped commits for a task should end in a pull request, if the environment supports it (gh is available). Never push directly to main.
- If you end up pushing a commit to a pull request after creating the request, the pull request title and description need to be regenerated to capture all commits.

### Boy Scout Rule
Before committing, glance at the files you touched. If you see something genuinely worth cleaning up, do it:
- Dead code: unused functions, unreachable branches, commented-out code — delete it, git remembers
- Unclear names in code you modified — names should reveal intent
- Files growing past ~500 lines — consider whether they have distinct responsibilities worth splitting
But don't clean up for the sake of it. Don't extract one-line helpers used once. Don't create abstractions for one-time operations. Three similar lines are better than a premature abstraction. If the code reads fine, leave it alone.

### Housekeeping
- Specs are design documents describing features for a contextless agent — not task lists or fix lists. Never create spec files containing lists of small fixes; those belong in the plan.
- When a spec is fully complete, move it from `specs/` to `specs/completed/`.
- Don't add status reports, "Done" sections, or temporal comments — commit history is the record.
