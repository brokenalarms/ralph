# Architecture
- Read `docs/specs/architecture.md` before any refactoring or architectural work. It describes the target state all changes should move toward.

# Git
- Ralph requires a git repository.
- Agents commit locally on the branch the orchestrator provides. The orchestrator owns all remote operations: pushing, branch creation, PR creation, and merging.
- The orchestrator handles rebase, conflict resolution, and merge programmatically. Agents do not need to manually rebase — the git module ensures the worktree is up to date before any outbound operation.

# Signal files vs state
- **Signal files** (.signal_complete, .signal_current_task, feedback, stop): agent↔orchestrator communication and user commands (`ralph stop`, `ralph feedback`).
- **state.json**: orchestrator-internal state that persists across iterations (iteration count, last task, test results). If it's not agent↔orchestrator communication, it goes in state.json.

# Build
- `go/cmd/ralph/prompts/` is the source of truth for prompt templates, embedded into the binary via `//go:embed`. Edit them directly — no copy step needed.

# Prompts
- Instructions and text for the agent belong in `.md` template files under `go/cmd/ralph/prompts/`, not hardcoded in Go. Go code assembles and interpolates templates but should not contain instructional prose.

# Beads / bd
- We use `bd` as the sole task backend for dependency management and issue tracking.
- Never hardcode bd commands in prompts. All bd knowledge comes from `bd prime` — the agent learns the workflow at runtime. Prompts refer to tasks generically and let `bd prime` teach the specifics.
- **Hard invariant**: `.beads` is the project's permanent task history and must never be deleted, cleared, or force-reinitialized. Only `.ralph` state is ephemeral.

# Testing
- Tests lock in features and prevent regressions.
- Each test should explain in a comment why it exists and what user functionality it proves — not just assert correctness mechanically.
- Do not write tests that assert specific strings from prompt templates. Prompts are natural-language guidance — test behavior, not prose.
