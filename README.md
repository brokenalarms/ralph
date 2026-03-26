# Ralph

Autonomous [Claude Code](https://docs.anthropic.com/en/docs/claude-code) task orchestrator. Picks up tasks from a backlog, works them one at a time in fresh-context iterations, verifies the result, and merges — unattended.

## How it works

Ralph runs Claude Code repeatedly, one task per iteration, with fresh context each time. Each iteration gets a task from the backlog ([bd](https://github.com/brokenalarms/bd)), works it on an isolated git worktree branch, runs the test suite, gets the diff reviewed by a verification LLM, and squash-merges the result. Then it force-resets the worktree to the updated base branch and picks up the next task.

```
ralph loop
  ├── pick task from bd backlog
  ├── create fresh worktree branch
  ├── iteration 1 → agent works, signals done
  │     ├── run test suite
  │     ├── LLM verification of diff against acceptance criteria
  │     └── fix agent if rejected (up to N retries)
  ├── orchestrator pushes, creates PR, merges
  ├── force-reset worktree to base
  └── next task
```

## Three subcommands

| Command | Purpose |
|---|---|
| `ralph loop` | Autonomous executor — picks tasks, writes code, verifies, merges |
| `ralph task` | Interactive triage session — create tasks, write specs, manage backlog |
| `ralph command` | Full four-pane tmux layout: loop + task manager + stream filter + plan |

Run `ralph task` to build up a backlog, then `ralph loop` to work through it. Or run `ralph command` to get both in a single tmux session with live log streaming.

## Quick start

```bash
# Install ralph and dependencies (bd, gh, tmux, Go)
./install.sh

# Create some tasks
ralph task ~/myproject

# Run the loop
ralph loop --dir ~/myproject --auto-merge --evolve
```

## Install

Requirements:
- [Go](https://go.dev/dl/) 1.22+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and authenticated
- Homebrew (for dependency installation)

```bash
./install.sh
```

This installs `bd`, `gh`, and `tmux` via Homebrew if missing, builds the Go binary, and places it at `~/.local/bin/ralph`.

To build manually:

```bash
make build    # produces ./ralph binary
make install  # copies to ~/.local/bin/ralph
make test     # runs the test suite
```

### Build scripts

| Script | Purpose |
|---|---|
| `scripts/build-go.sh` | Local-only build — compiles the binary, no version tagging or push |
| `scripts/rebuild-go.sh` | Full release build — bumps patch version, tags, pushes, polls for the new tag, rebuilds with exponential backoff |

`rebuild-go.sh` is used by evolve mode to self-update after each merge. `build-go.sh` is for local development.

## Usage

```bash
# Run against current directory
ralph loop

# Specify project and iteration cap
ralph loop --dir ~/myproject --max 20

# Auto-merge completed tasks and self-update after each merge
ralph loop --auto-merge --evolve

# Prompt override for one-off work
ralph loop -p "Fix all failing tests"

# Four-pane tmux layout
ralph command ~/myproject
```

### Loop options

| Flag | Description | Default |
|---|---|---|
| `-d, --dir <path>` | Project directory | cwd |
| `-n, --max <N>` | Max iterations | 50 |
| `-p, --prompt <text>` | Prompt override | — |
| `-q, --quiet` | Suppress streaming output (log only) | — |
| `--calls-per-hour <N>` | Max Claude calls per hour | 80 |
| `--base-branch <name>` | Base branch for rebase/merge | develop |
| `--auto-merge` | Squash-merge PRs after task completion | — |
| `--evolve` | Self-improving: pull, rebuild, restart after each merge | — |
| `--merge-admin` | Bypass branch protection on merge (requires `--auto-merge`) | — |
| `--wait` | Keep running after all tasks complete, polling for new work | — |
| `--tmux` | Run in tmux 3-pane layout | — |
| `--refactor-every <N>` | Refactor every N iterations | 0 |
| `--idle-timeout <dur>` | Kill idle session after duration | 10m |

### Controlling a running loop

```bash
ralph stop              # halt after the current iteration
ralph feedback "msg"    # queue feedback for the next iteration
```

## Architecture

See [docs/specs/architecture.md](docs/specs/architecture.md) for the full target-state architecture, package structure, and key interfaces.

### Orchestrator-owned lifecycle

The orchestrator owns the entire push/PR/merge lifecycle. The agent writes code and signals completion — it never pushes, creates PRs, or closes tasks. These operations are enforced via disallowed tools:

- `git push` — orchestrator pushes after verification passes
- `gh pr create` — orchestrator creates the PR and links it to the bead via `external-ref` (full PR URL)
- `bd close` — orchestrator closes the bead only after successful merge
- `git checkout` / `git branch` — prevents sub-agents from interfering with ralph's branch management

### Agent constraints

The agent's execution prompt makes explicit:
- **ISSUE/FIX diagnosis format is mandatory** — every code change must be preceded by an ISSUE/FIX block explaining what's wrong and how the fix addresses it
- **Never skip failing tests** — tests that fail must be fixed, not deleted or skipped
- **Agent cannot push or create PRs** — the orchestrator handles all git remote operations

### Feedback

When a user writes feedback via `ralph feedback "msg"`, the feedback file is written to `.ralph/feedback`. The orchestrator kills the running agent and restarts it with a fresh context that includes the feedback. The agent must acknowledge feedback with a `FEEDBACK:` line before proceeding.

### Verification pipeline

After the agent signals completion:

1. **Test suite** — full `make test` (or equivalent) must pass
2. **LLM diff review** — a fast model reviews the diff against the bead's acceptance criteria. UI/UX concerns are flagged but left to the agent's discretion.
3. **Fix agent** — if rejected, a fix agent is spawned to address the issues
4. **Escalation** — unresolved rejections escalate to a smarter model
5. **Skip after repeated failures** — tasks that fail verification N times are skipped

### CI evaluation

CI check polling only fails on **required checks** fetched from the branch protection API. Non-required checks (Netlify deploy previews, optional linters, etc.) are ignored. This prevents flaky optional checks from blocking the merge pipeline.

### EnsureUpToDate

All rebase operations go through a single `EnsureUpToDate` path that tells a story of escalating retry strategies:

1. Stash agent work, fetch, attempt rebase
2. On conflict, force-reset and replay agent commits via cherry-pick
3. On unresolvable conflict, auto-recreate the worktree from the base branch

This replaced multiple ad-hoc rebase paths (`RebaseOntoDefaultBranch`, inline rebase in merge, etc.) with one composable function.

### PR-to-bead linking

When the orchestrator creates a PR, it stores the full PR URL as an `external-ref` on the bead. The close reason also includes the PR URL when available. PR numbers are parsed from both URL format and legacy `gh-123` format.

## Stacked PRs (planned)

The current strategy resets the worktree to `origin/main` after every squash-merge. A planned evolution moves to stacked single-commit PRs where each task produces one commit and the worktree never resets. See [docs/specs/stacked-prs.md](docs/specs/stacked-prs.md) for the full design.

## Evolve mode

With `--auto-merge --evolve`, ralph enters a self-improving cycle: after each successful squash-merge, it pulls the updated base branch, rebuilds itself from source (`scripts/rebuild-go.sh`), and restarts the loop with the new binary. This means ralph can work on its own codebase — improvements to prompts, verification logic, or merge behavior take effect on the next iteration.

## Git workflow

Ralph creates a git worktree per run so the agent works on an isolated branch while the main branch stays clean. The workflow is opinionated:

1. **Fresh branch per task** — each task starts on a new branch off the base
2. **Squash-merge** — all commits for a task are squash-merged into the base branch
3. **Force-reset** — after merge, the worktree is force-reset to the updated base

Branch names follow the pattern `ralph/<project>/<seq>-<beadID>-<slug>`.

## Task management

Ralph uses [bd](https://github.com/brokenalarms/bd) as its task backend. The loop reads from the bd backlog, claims tasks, and closes them after successful merge. Use `ralph task` for interactive triage:

```bash
ralph task ~/myproject    # opens an interactive Claude session for task management
```

## Signal protocol

The agent communicates with the orchestrator via signal files in `.ralph/`:

| File | Direction | Purpose |
|---|---|---|
| `.signal_current_task` | agent → loop | Written when agent picks a task |
| `.signal_complete` | agent → loop | Written when agent finishes — triggers verification |
| `feedback` | user → agent | Kills running agent, restart includes feedback in context |
| `stop` | user → loop | Halts after current iteration |

## .ralph directory

Ralph stores all runtime state in `.ralph/` inside the project directory. Add it to `.gitignore`.

```
.ralph/
  state.json          # iteration count, status, current task, skipped tasks
  reflections/        # post-task reflections from the agent
  worktrees/          # git worktree directories
  .signal_complete    # agent completion signal
  .signal_current_task # current task signal
  feedback            # queued user feedback
  stop                # create to halt gracefully
```

## Four-pane tmux layout

`ralph command` starts a tmux session with four panes:

```
┌──────────────────┬──────────────────┐
│                  │                  │
│   ralph loop     │  stream filter   │
│   (orchestrator) │  (agent output)  │
│                  │                  │
├──────────────────┼──────────────────┤
│                  │                  │
│   ralph task     │  plan / state    │
│   (triage)       │  (bd + status)   │
│                  │                  │
└──────────────────┴──────────────────┘
```

## License

MIT
