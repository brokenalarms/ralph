# Ralph

Autonomous [Claude Code](https://docs.anthropic.com/en/docs/claude-code) task iteration loop. Runs the Claude Code CLI in fresh-context iterations against a project repo.

Ralph is deliberately simple and non-opinionated. There are no wizards, no scaffolding, no framework to adopt. It's a bash script that runs Claude in a loop with a signal protocol so each iteration knows when to stop. Point it at an existing project that already has its own commit conventions, task management, and agent instructions — ralph just orchestrates the loop.

## How it works

Claude Code has ~200k tokens of context per session. Ralph runs Claude repeatedly, one task per session, with fresh context each time. Claude signals when it's done, ralph moves to the next task.

```
ralph.sh
  ├── planning phase (optional) — Claude reads the repo + creates a task list
  └── execution loop
       ├── iteration 1 → Claude picks task, works, signals done
       ├── iteration 2 → fresh context, next task
       ├── ...
       └── iteration N → all tasks complete (or max reached)
```

### Two modes

**Managed mode** (default) — On the first run, ralph launches an interactive Claude session where you chat to define a spec and task plan. Once you exit, ralph takes over and works through the tasks autonomously. Use `--no-plan` to skip planning.

**External plan mode** (`--plan-file`) — Your repo already has task files and agent instructions. Ralph skips planning and defers to your project's workflow. This is the mode for existing projects — point `--plan-file` at your AGENTS.md or TODO.md and ralph handles the iteration loop while your project's own rules handle everything else.

```bash
# Existing project with its own AGENTS.md defining tasks and workflow
ralph.sh --plan-file AGENTS.md -d ~/myproject
```

### Signal protocol

Claude communicates with ralph via a file (`.ralph-signal` in the worktree):

```
###RALPH_CURRENT_TASK### <description>    # written when Claude picks a task
###RALPH_TASK_COMPLETE### <summary>        # written when done — triggers next iteration
```

The signal instructions are appended to every prompt automatically. If Claude gets stuck, it still writes the completion signal so the loop can proceed.

## Requirements

- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and authenticated
- bash
- jq (optional — enables parsed output streaming)
- tmux (optional — enables 3-pane layout)
- Node.js 18+ (for the HTTP server only)

## Usage

```bash
# Run against current directory
ralph.sh

# Specify project and iteration cap
ralph.sh -d ~/myproject -n 20

# External plan mode — use your repo's existing task file
ralph.sh --plan-file AGENTS.md

# With a prompt override
ralph.sh -p "Fix all failing tests"

# Plan only — creates .ralph/plan.md, then exits
ralph.sh --plan

# Resume a previous run
ralph.sh --resume

# Tmux 3-pane layout (loop status | claude output | plan + state)
ralph.sh --tmux
```

### Options

| Flag | Description | Default |
|---|---|---|
| `-d, --dir <path>` | Project directory | cwd |
| `-n, --max <N>` | Max iterations | 50 |
| `-p, --prompt <text>` | Prompt override | — |
| `--plan-file <path>` | External plan file (skips planning phase) | — |
| `--resume` | Resume from previous state | — |
| `--plan` | Run planning phase only | — |
| `--skip-planning` | Skip interactive planning, go straight to autonomous execution | — |
| `-q, --quiet` | Suppress streaming output (log only) | — |
| `--no-worktree` | Run directly in project dir (no git isolation) | — |
| `--calls-per-hour <N>` | Rate limit Claude calls per hour | 80 |
| `--tmux` | 3-pane tmux layout | — |

### Controlling a running loop

- **Stop gracefully:** `touch .ralph/stop` — halts after the current iteration finishes
- **Send feedback:** `ralph feedback "your message"` — queues feedback for the next iteration. Multiple calls stack up. Feedback is injected into Claude's prompt once, then cleared.
- **Resume:** run the generated `.ralph/resume.sh`, or `ralph.sh --resume`
- **Rate limiting:** ralph tracks calls per clock hour and pauses with a countdown when the cap is reached, then resumes automatically

## Prompts

Ralph's prompts live in `prompts/` and are designed to be read and modified. They're short — the longest is ~20 lines. There's no hidden logic or complex prompt engineering. They provide just enough structure for the iteration loop to work, and defer to your project's own AGENTS.md / CLAUDE.md for everything else.

| File | Used when | Purpose |
|---|---|---|
| `shared.md` | Always (prepended to all prompts) | Baseline quality standards: testing, commits, housekeeping |
| `interactive-planning.md` | First run, interactive planning | System prompt for the interactive spec/plan session |
| `planning.md` | Managed mode, autonomous planning fallback | Tells Claude to create a checkbox task list |
| `internal.md` | Managed mode, execution | Gives Claude the task and rules for one iteration |
| `external.md` | `--plan-file` mode | Tells Claude to read the project's own agent instructions for task selection |
| `signal.md` | Always (appended to all prompts) | Documents the signal protocol |

Placeholders like `{{WORK_DIR}}`, `{{PLAN_FILE}}`, `{{SIGNAL_FILE}}` are substituted at runtime. Edit the prompts to change how Claude behaves in your loops.

## Git worktrees

By default, ralph creates a git worktree so Claude works on an isolated branch while your main branch stays clean. Branches are named by project and task:

```
ralph/myproject/01-add-authentication
ralph/myproject/02-fix-failing-tests
```

The branch is created with a sequence number at the start and renamed to include a task slug once Claude picks its first task. Merge when ready with `git merge ralph/myproject/01-add-authentication`.

The worktree has `rebase.updateRefs` enabled, so rebasing any branch in the stack automatically updates all intermediate branch pointers. This means you can rebase the entire stack onto an updated main with a single `git rebase --update-refs origin/main` from the top branch.

Use `--no-worktree` to skip this and work directly in the project directory.

## Response analyzer

Ralph watches for problems after each iteration and halts early rather than burning through iterations with no progress:

- **Permission denials** — 3+ in a single iteration → halt
- **Stagnation** — 3 consecutive iterations with no file changes → halt
- **Test saturation** — 3 consecutive iterations modifying only test files → halt
- **Stuck loops** — repeated identical tool calls or "I'm blocked" language → warn, then halt

## .ralph directory

Ralph stores all state in `.ralph/` inside the project directory. Add it to `.gitignore`.

```
.ralph/
  state.json       # iteration count, status, worktree info
  plan.md          # task list (managed mode)
  loop.log         # full Claude output (stream-json)
  resume.sh        # auto-generated resume script
  stop             # create this file to halt gracefully
  feedback         # queued user feedback (consumed at next iteration)
  worktrees/       # git worktree directories
```

## HTTP server

Optional HTTP wrapper for remote monitoring and control.

```bash
node server.js
# or: npm start
```

| Variable | Default | Description |
|---|---|---|
| `RALPH_PORT` | `3411` | Server port |
| `RALPH_HOST` | `127.0.0.1` | Bind address (localhost only) |

Bind to a Tailscale IP for network access without exposing to LAN:

```bash
RALPH_HOST=$(tailscale ip -4) node server.js
```

### Endpoints

| Route | Method | Description |
|---|---|---|
| `/` | GET | Server info |
| `/start` | POST | Start a ralph loop |
| `/status` | GET | Loop status and state |
| `/stop` | POST | Graceful stop |
| `/feedback` | POST | Queue feedback for next iteration |
| `/kill` | POST | Kill running process |
| `/log` | GET | Tail the log (`?lines=50`) |
| `/plan` | GET | View the plan file |
| `/reset` | DELETE | Clean .ralph state |

```bash
# Start a loop
curl -X POST http://localhost:3411/start \
  -H 'Content-Type: application/json' \
  -d '{"dir": "/home/user/myproject", "plan_file": "AGENTS.md"}'

# Check status
curl http://localhost:3411/status

# Graceful stop
curl -X POST http://localhost:3411/stop
```

## License

MIT
