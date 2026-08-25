# Ralph

Autonomous [Claude Code](https://docs.anthropic.com/en/docs/claude-code) task orchestrator. Picks up tasks from a backlog, works them one at a time in fresh-context iterations, verifies the result, and merges — unattended.

## Quick start

```bash
# Install ralph and dependencies (bd, gh, tmux, Go)
./install.sh

# Create some tasks (run from your project directory)
cd ~/myproject
ralph task

# Run the loop
ralph loop --auto-merge --base-branch main --evolve
```

## Recommended workflow

Add this alias for rapid iteration:

```bash
alias loop='ralph loop --auto-merge --base-branch main --evolve --wait'
```

- `--auto-merge` — squash-merges each PR automatically after CI passes
- `--base-branch main` — rebases and merges into `main` (change to match your repo)
- `--evolve` — re-execs ralph after each merge so improvements take effect immediately
- `--wait` — keep running when the backlog empties, polling for new tasks

With `--wait` the loop runs until you press Ctrl-C; without it, it exits once the backlog is drained. To rebuild the project on each iteration (useful for self-improving loops), add `--post-task <build-script>` alongside `--evolve`.

Run two windows side by side, named `{project}-loop` and `{project}-task`:

```
{project}-loop: loop               # runs the loop alias indefinitely
{project}-task: ralph task         # interactive triage: create tasks, write specs
```

The loop window runs unattended while the task window gives you a live triage interface — add tasks at any time and the loop picks them up immediately. Both windows stay live so you can monitor and triage from anywhere, including from your phone via [Shellfish](https://shellfishapp.com/) (iOS) or any SSH client on Android.

## How it works

Ralph runs Claude Code repeatedly, one task per iteration. Each iteration gets a task from the backlog ([bd](https://github.com/gastownhall/beads)), works it on an isolated git worktree, runs a verification pipeline, and the orchestrator pushes, creates a PR, and merges. Each task produces one commit that stacks linearly on the previous.

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
| `-v, --verbose` | Show all tool calls in stream log | — | |
| `--base-branch <name>` | Base branch for rebase/merge | develop | `RALPH_BASE_BRANCH` |
| `--auto-merge` | Squash-merge PRs after task completion | — | |
| `--evolve` | Self-improving mode: re-exec ralph after each merged task so improvements take effect immediately (requires `--auto-merge`) | — | |
| `--wait` | Keep running after the backlog empties, polling for new tasks | — | |
| `--post-task <script>` | Run a script after each task completes, before evolve re-exec. Receives `RALPH_TASK_ID`, `RALPH_PR_NUMBER`, and `RALPH_MERGED` env vars. | — | |
| `--notify` | Send macOS notification on each task completion | — | |
| `--tmux` | Run in tmux 3-pane layout (status / output / plan) | — | |

All other tuning (timeouts, model selection, attempt limits, thresholds) lives in `.ralph/config.toml` — see [Configuration](#configuration) below.

## Configuration

Ralph reads `.ralph/config.toml` on startup. The file is created automatically with defaults on first run. CLI flags override config file values.

Example `.ralph/config.toml`:

```toml
base_branch = "main"
post_task = "make build"

# Tuning — defaults shown
max_iterations = 50
calls_per_hour = 80
test_timeout = "5m"
working_model = "sonnet"
verify_model = "haiku"
verify_escalation_model = "sonnet"
fix_model = "opus"
qos_clamp = "utility"
```

Run `ralph loop --help` to see all available options and their corresponding config keys.

### Loop process priority (macOS)

`qos_clamp` (default `utility`) runs the loop under a macOS QoS clamp: `ralph loop` re-execs itself through `taskpolicy -c <clamp>` once at startup, and every process it spawns — the working agent, verifier, fix agents, `git`, `gh`, `bd`, and evolve restarts — inherits the clamp, so a busy loop stays below the UI band and never fights the foreground for cores. Accepted values are `utility`, `background`, `maintenance`, and `none` (leave the loop unclamped). Interactive sessions (`ralph task`, `ralph review`) are never clamped. On non-macOS hosts, or when `/usr/sbin/taskpolicy` is missing, the setting is ignored with a single warning.

## Architecture

### Orchestrator-owned lifecycle

The orchestrator owns the entire push/PR/merge lifecycle. The agent writes code and signals completion — it never pushes, creates PRs, or closes tasks. These operations are enforced via disallowed tools:

- `git push` — orchestrator pushes after verification passes
- `gh pr create` — orchestrator creates the PR and links it to the bead via `external-ref`
- `bd close` — orchestrator closes the bead only after successful merge
- `git checkout` / `git branch` — prevents sub-agents from interfering with branch management

### Model configuration

Ralph assigns models per role, configured in `.ralph/config.toml` as bare tier aliases (`sonnet` / `haiku` / `opus`), never pinned model IDs:

| Role | Config key | Default | What it runs | Escalation |
|---|---|---|---|---|
| Working agent | `working_model` | sonnet | The main agent working each iteration | Overridable per bead (see below) |
| Verifier | `verify_model` | haiku | LLM diff-vs-acceptance-criteria review, first attempt | Escalates to `verify_escalation_model` on subsequent attempts |
| Verifier (escalated) | `verify_escalation_model` | sonnet | LLM verification, attempts after the first | — |
| Fix agent | `fix_model` | opus | Test/compile/verify/CI/Copilot/conflict repairs | Runs at `fix_model` from its first attempt, no lower-tier warm-up |

**Per-bead assignment.** The task manager stamps model metadata on every bead it creates — `sonnet` for mechanical, well-specified work, `opus` for work requiring judgment, design, or diagnosis, `fable` for the hardest beads (deep architectural work, subtle concurrency or control-flow diagnosis, or a bead whose previous attempt failed at opus). `resolveAgentModel` in `go/internal/loop/loop.go` applies that override to the working agent only: fix agents and the verifier always use their configured `fix_model`/`verify_model`/`verify_escalation_model`, regardless of a bead's metadata. If a bead's model metadata doesn't match a recognized model family, the loop falls back to `working_model` and logs a warning.

**Verification escalation.** The first verification attempt for a task uses `verify_model`; if the agent needs to retry, later attempts use `verify_escalation_model` instead (see the model selection in `go/internal/verifier/verifier.go`). Fix agents don't have this warm-up — they run at `fix_model` starting with their very first attempt.

### Verification pipeline

Tests run via the project's `ralph:verify` script — a package.json script or Makefile target (or the `verify` key in config.toml). Each iteration starts with a baseline run of that suite plus a compile check, so the agent starts from a known-green tree.

After the agent signals completion:

1. **Test suite** — the full `ralph:verify` suite must pass. If it fails, a fix agent is spawned to address the failures (up to 3 retries).
2. **LLM diff review** — haiku reviews the diff against the bead's acceptance criteria. If rejected, a fix agent addresses the issues (up to 3 retries, escalating to sonnet).

Green test runs are cached by git tree hash: a run on a tree that already passed is skipped instead of re-executed, and the cache persists in `state.json` across iterations. Because squash-merging preserves the verified tree, the baseline check after a merge is normally a cache hit — only a genuine tree change (agent or fix-agent commits, or another change landing on the base) triggers a real run. The first iteration of a fresh session always runs the suite to capture the starting state of the world.

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

Ralph uses [bd](https://github.com/gastownhall/beads) as its task backend. The loop reads from the bd backlog, claims tasks, and closes them after successful merge. Use `ralph task` for interactive triage:

```bash
ralph task
```

## .ralph directory

Ralph stores all runtime state in `.ralph/` inside the project directory. Add it to `.gitignore`.

```
.ralph/
  config.toml         # tuning knobs — created automatically with defaults on first run
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
