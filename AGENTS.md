# Git usage
- ralph requires a git repository. Running in a non-git directory exits with an error. Use `--no-worktree` to skip git isolation, but a git repo is still required.
- never push to main - atomic commits comprising a feature should be pushed as a PR from the branch you're working on, this should be part of considering a task finished.
- The final stage of a piece of work is always commiting and pushing to a branched PR- I shouldn't have to ask.
- Before you push, you must rebase onto the latest main:
  1. `git fetch origin main`
  2. `git rebase --update-refs origin/main`
  3. If step 2 conflicts because a base branch was squash-merged, abort and use: `git rebase --update-refs --onto origin/main <squash-merged-branch> HEAD` where `<squash-merged-branch>` is the ralph branch ref that was already merged (e.g. `ralph/project/01-task-slug`). This skips the duplicate commits. Then delete the merged branch ref.
  Always use `--update-refs` to keep stacked branch pointers correct.
- It is up to the user to work through these stacks and merge them. Never merge a PR to main without asking first.

# Prompts
- User-facing text strings and instructions for Claude belong in `.md` files under `prompts/`, not hardcoded in shell scripts. Shell code assembles and templates prompts but should not contain instructional prose.

# Beads / bd
- We use 'beads' (`bd`) for dependency management and and issue tracking, with markdown planning as a fallback if not available.
- Never hardcode bd commands in prompts or scripts. All bd knowledge comes from `bd prime` — Claude learns the workflow at runtime. Prompts should refer to tasks generically (e.g. "add a new task", "close the task") and let `bd prime` teach the specifics.

# Testing
- Tests should be put in place to lock in new features and prevent regressions.
- They should explain in a comment for each why the test is being created, and what user functionality it is proving, so that a test has a specific feature based meaning, and isn't just written to be correct eg assert 1 = true.
- Do not write tests that assert specific strings or phrases from prompt templates (.md files) are present. Prompts are loose, natural-language guidance — testing that a particular word or sentence exists misunderstands the point of tests. Tests should verify behavior and functionality, not pin down prose.
