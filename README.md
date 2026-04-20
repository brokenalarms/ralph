# Ralph

Autonomous [Claude Code](https://docs.anthropic.com/en/docs/claude-code) task orchestrator. Picks up tasks from a backlog, works them one at a time in fresh-context iterations, verifies the result, and merges — unattended.

## Quick start

```bash
# Install ralph and dependencies (bd, gh, tmux, Go)
./install.sh

# Create some tasks
ralph task ~/myproject

# Run the loop
ralph loop --wait --auto-merge --base-branch main --evolve
```

## Recommended workflow

Add this alias for rapid iteration:

```bash
alias loop='ralph loop --wait --auto-merge --base-branch main --evolve'
```

- `--wait` — keeps the loop running after the backlog empties, polling for new tasks
- `--auto-merge` — squash-merges each PR automatically after CI passes
- `--base-branch main` — rebases and merges into `main` (change to match your repo)
- `--evolve` — re-execs ralph after each merge so improvements take effect immediately

Run two tmux windows side by side, named `{project}-loop` and `{project}-task`:

```
{project}-loop: loop               # runs the loop alias
{project}-task: ralph task         # interactive triage: create tasks, write specs
```

Both windows stay live so you can monitor the loop and add tasks from anywhere — including from your phone via [Shellfish](https://shellfishapp.com/) (iOS) or any SSH client on Android.

## How it works

Ralph runs Claude Code repeatedly, one task per iteration. Each iteration gets a task from the backlog ([bd](https://github.com/brokenalarms/bd)), works it on an isolated git worktree, runs a verification pipeline, and the orchestrator pushes, creates a PR, and merges. Each task produces one commit that stacks linearly on the previous.

```
ralph loop
  │
  ├─ pick next task from bd backlog
  ├─ create branch, start agent
  │
  │   ┌──────────────────────────────────────────┐
  │   │  agent works → signals complete          │
  │   │         ↓                                │
  │   │  test suite                              │
  │   │         ↓                                │
  │   │  fails? → fix agent → re-test (×3)       │
  │   │         ↓                                │
  │   │  LLM verification against criteria       │
  │   │         ↓                                │
  │   │  rejected? → fix agent → re-verify (×3)  │
  │   └──────────────────────────────────────────┘
  │
  │   ┌──────────────────────────────────────────┐
  │   │  rebase onto latest base                 │
  │   │         ↓                                │
  │   │  squash to one commit, push              │
  │   │         ↓                                │
  │   │  wait for CI                             │
  │   │         ↓                                │
  │   │  CI fails? → fix agent → loop (×4)       │
  │   │  base moved? → loop                      │
  │   │         ↓                                │
  │   │  merge PR                                │
  │   └──────────────────────────────────────────┘
  │
  └─ next task (stacks on previous commit)
```

## Commands

| Command | Purpose |
|---|---|
| `ralph loop` | Autonomous executor — picks tasks, writes code, verifies, merges |
| `ralph task` | Interactive triage session — create tasks, write specs, manage backlog |
| `ralph stop` | Halt after the current iteration |
| `ralph feedback` | Append feedback to bead notes and restart the agent |
| `ralph attach` | Attach to a running loop's tmux session |
| `ralph review` | Post-mortem review of reflections, tests, and refactoring opportunities |
| `ralph merge` | Rebase and merge a stacked PR chain bottom-up |

Run `ralph task` to build up a backlog, then `ralph loop` to work through it.

## Loop flags

| Flag | Description | Default | Env var |
|---|---|---|---|
| `-n, --max <N>` | Max iterations | 50 | `RALPH_MAX_ITERATIONS` |
| `-p, --prompt <text>` | Prompt override | — | |
| `-q, --quiet` | Suppress streaming output (log only) | — | |
| `-v, --verbose` | Show all tool calls in stream log | — | |
| `--calls-per-hour <N>` | Max Claude calls per hour | 80 | |
| `--base-branch <name>` | Base branch for rebase/merge | develop | `RALPH_BASE_BRANCH` |
| `--auto-merge` | Squash-merge PRs after task completion | — | |
| `--evolve` | Re-exec ralph after each merged task to incorporate the latest version. Requires `--auto-merge`. Use with `--post-task` to rebuild before re-exec. | — | |
| `--post-task <script>` | Run a command after each task completes, before evolve re-exec. Receives `RALPH_TASK_ID`, `RALPH_PR_NUMBER`, and `RALPH_MERGED` env vars. | — | |
| `--verify-build <script>` | Run external script before pre-iteration tests to check project-level build health | — | |
| `--wait` | Keep running after all tasks complete, polling for new work | — | |
| `--notify` | Send macOS notification on each task completion | — | |
| `--tmux` | Run in tmux 3-pane layout (status / output / plan) | — | |
| `--idle-timeout <dur>` | Kill idle session after duration | 10m | `RALPH_IDLE_TIMEOUT` |
| `--idle-timeout-progress <dur>` | Shorter idle timeout when progress detected | 5m | `RALPH_IDLE_TIMEOUT_PROGRESS` |
| `--post-signal-timeout <dur>` | Timeout for post-signal operations | 15m | `RALPH_POST_SIGNAL_TIMEOUT` |
| `--model-ceiling <model>` | Model ceiling for all LLM interactions (loop agent, fix agents, verification) | claude-sonnet-4-6 | |
| `--agent-escalation-model <model>` | Model for agent on retry attempts | claude-opus-4-6 | |
| `--verify-model <model>` | Model for LLM verification (first attempt) | claude-haiku-4-5-20251001 | |
| `--verify-escalation-model <model>` | Model for verification escalation (subsequent attempts) | claude-sonnet-4-6 | |
| `--fix-model <model>` | Model for fix agents (first attempt) | claude-sonnet-4-6 | |
| `--fix-escalation-model <model>` | Model for fix agents on retry attempts | claude-opus-4-6 | |

## Architecture

### Orchestrator-owned lifecycle

The orchestrator owns the entire push/PR/merge lifecycle. The agent writes code and signals completion — it never pushes, creates PRs, or closes tasks. These operations are enforced via disallowed tools:

- `git push` — orchestrator pushes after verification passes
- `gh pr create` — orchestrator creates the PR and links it to the bead via `external-ref`
- `bd close` — orchestrator closes the bead only after successful merge
- `git checkout` / `git branch` — prevents sub-agents from interfering with branch management

### Model escalation

The loop uses a tiered model strategy to balance cost and quality:

- **Agent first pass** — sonnet (default `--model-ceiling`)
- **Agent retry** — opus (default `--agent-escalation-model`); escalated automatically on subsequent attempts
- **Verification first pass** — haiku (default `--verify-model`)
- **Verification retry** — sonnet (default `--verify-escalation-model`)
- **Fix agent first pass** — sonnet (default `--fix-model`); covers test/compile/verify/CI/Copilot/conflict fix agents
- **Fix agent retry** — opus (default `--fix-escalation-model`); escalated automatically on subsequent attempts

`--model-ceiling` acts as a ceiling — no LLM call may use a higher-tier model than the value set.

### Verification pipeline

After the agent signals completion:

1. **Test suite** — full `make test` (or equivalent) must pass. If it fails, a fix agent is spawned to address the failures (up to 3 retries).
2. **LLM diff review** — haiku reviews the diff against the bead's acceptance criteria. If rejected, a fix agent addresses the issues (up to 3 retries, escalating to sonnet).

### Merge pipeline

After verification passes:

1. **Rebase** onto the latest base branch
2. **Squash** to a single commit and push
3. **Wait for CI** — only required checks from branch protection are evaluated
4. **CI fix loop** — if CI fails, a fix agent patches, force-pushes, and re-checks (up to 4 retries). If the base moved during the loop, rebase and retry.
5. **Merge** the PR

### Feedback

`ralph feedback "msg"` appends the message to the bead's notes via `bd update --append-notes`, then kills the running agent via context cancellation — aborting any in-flight operation (CI polling, merge wait, review wait). The loop restarts immediately with a fresh context. The agent must acknowledge feedback with a `FEEDBACK:` line before proceeding.

### SIGINT (Ctrl-C)

Ctrl-C cancels the running operation but leaves the bead open. The loop exits cleanly — no bead is closed or skipped. Resume by restarting the loop.

### Git strategy: stacked single-commit PRs

Each task produces one commit, stacked linearly on the previous. PRs target the previous task's branch (not the base), so each PR shows only its own changes.

When the base moves (e.g. direct pushes), Ralph rebases onto the latest base on startup. If the rebase conflicts, the stack diverges — Ralph continues building on top without trying to auto-resolve. To resolve a diverged stack later: `git rebase --update-refs origin/main` from the stack tip.

### Evolve mode

With `--evolve`, ralph re-execs itself after each successful merge to pick up the latest binary. This means ralph can work on its own codebase — improvements to prompts, verification logic, or merge behavior take effect on the next iteration. Use `--post-task` to run a rebuild script before the re-exec.

## Install

Requirements:
- [Go](https://go.dev/dl/) 1.26+
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

## Task backend

Ralph uses [bd](https://github.com/brokenalarms/bd) as its task backend. The loop reads from the bd backlog, claims tasks, and closes them after successful merge. Use `ralph task` for interactive triage:

```bash
ralph task ~/myproject
```

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

## Tmux layout

`ralph loop --tmux` starts a tmux session with three panes:

```
┌──────────────────┬──────────────────┐
│                  │                  │
│   ralph loop     │  stream filter   │
│   (orchestrator) │  (agent output)  │
│                  │                  │
├──────────────────┴──────────────────┤
│                                     │
│   plan / state watcher              │
│                                     │
└─────────────────────────────────────┘
```

Use `ralph attach` to connect to a running loop's session from another terminal.

## License

MIT
