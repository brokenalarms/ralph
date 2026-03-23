# Git usage
- ralph requires a git repository. Running in a non-git directory exits with an error. Use `--no-worktree` to skip git isolation, but a git repo is still required.
- never push to the base branch (default: develop) - atomic commits comprising a feature should be pushed as a PR from the branch you're working on, this should be part of considering a task finished.
- The final stage of a piece of work is always commiting and pushing to a branched PR- I shouldn't have to ask.
- Before you push, you must rebase onto the latest base branch:
  1. `git fetch origin develop`
  2. `git rebase --update-refs origin/develop`
  3. If step 2 conflicts because a base branch was squash-merged, abort and use: `git rebase --update-refs --onto origin/develop <squash-merged-branch> HEAD` where `<squash-merged-branch>` is the ralph branch ref that was already merged (e.g. `ralph/project/01-task-slug`). This skips the duplicate commits. Then delete the merged branch ref.
  Always use `--update-refs` to keep stacked branch pointers correct.
- It is up to the user to work through these stacks and merge them. Never merge a PR to the base branch without asking first.

# Implementation
- **Go only by default.** The bash implementation (ralph.sh, lib/*.sh) is deprecated. All changes, fixes, and new features go in `go/` unless explicitly told otherwise.

# Architecture: files vs state
- **Signal files** (.signal_complete, .signal_current_task, feedback, stop): for communication between agent and orchestrator outside of stdout, and for user commands into the system via `ralph stop`, `ralph feedback`, etc.
- **state.json**: for all orchestrator-internal state that persists across iterations (iteration count, last task, test results, config overrides). If it's not agent↔orchestrator communication or user input, it goes in state.json.

# Build
- `go/cmd/ralph/prompts/` is the source of truth for prompt templates, embedded into the binary via `//go:embed`. Edit them directly — no copy step needed.

# Prompts
- User-facing text strings and instructions for Claude belong in `.md` files under `prompts/`, not hardcoded in shell scripts. Shell code assembles and templates prompts but should not contain instructional prose.

# Beads / bd
- We use 'beads' (`bd`) as the sole task backend for dependency management and issue tracking. `bd` is a hard requirement.
- Never hardcode bd commands in prompts or scripts. All bd knowledge comes from `bd prime` — Claude learns the workflow at runtime. Prompts should refer to tasks generically (e.g. "add a new task", "close the task") and let `bd prime` teach the specifics.
- **Hard invariant**: `.beads` is the project's permanent task history and must never be deleted, cleared, or force-reinitialized. Only `.ralph` state is ephemeral. Cleanup and reset operations must skip `.beads`.

# Testing
- Tests should be put in place to lock in new features and prevent regressions.
- They should explain in a comment for each why the test is being created, and what user functionality it is proving, so that a test has a specific feature based meaning, and isn't just written to be correct eg assert 1 = true.
- Do not write tests that assert specific strings or phrases from prompt templates (.md files) are present. Prompts are loose, natural-language guidance — testing that a particular word or sentence exists misunderstands the point of tests. Tests should verify behavior and functionality, not pin down prose.
