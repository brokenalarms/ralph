# Ralph

Autonomous Claude Code task iteration loop. Runs Claude Code CLI in fresh-context iterations against a project repo, with an optional HTTP server for remote monitoring via Tailscale.

## How it works

Ralph runs Claude Code repeatedly in a loop. Each iteration gets fresh context (~200k tokens). Claude works on one task per iteration, signals completion, and ralph moves to the next.

**Two modes:**

1. **Managed mode** (default) — Ralph creates `.ralph/plan.md` with checkbox tasks. Claude checks them off one by one.
2. **External plan mode** (`--plan-file`) — Your repo already has task files (TODO.md, etc.) and agent instructions (AGENTS.md). Ralph defers to your project's workflow and just orchestrates the loop + signal protocol.

### Signal protocol

Claude communicates with ralph via a signal file (`.ralph/signal`):

```
###RALPH_CURRENT_TASK### <description>   # agent writes when it picks a task
###RALPH_TASK_COMPLETE### <summary>       # agent writes when done (triggers next iteration)
```

## Requirements

- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and authenticated
- Node.js 18+ (for the HTTP server only)
- bash

## Running ralph.sh

```bash
# Basic: run against current directory
bash ralph.sh

# Specify project and iteration cap
bash ralph.sh -d ~/myproject -n 20

# With a prompt override
bash ralph.sh -p "Fix all failing tests"

# Resume a previous run
bash ralph.sh --resume

# Plan only (creates .ralph/plan.md, then exits)
bash ralph.sh --plan

# External plan mode: use your repo's existing task file
bash ralph.sh --plan-file docs/TODO.md
```

### Options

| Flag | Description |
|---|---|
| `-d, --dir <path>` | Project directory (default: cwd) |
| `-n, --max <N>` | Max iterations (default: 50) |
| `-p, --prompt <text>` | Prompt override |
| `--plan-file <path>` | Use external plan file (skip planning, lighter prompt) |
| `--resume` | Resume from previous state |
| `--plan` | Run planning phase only |

### Controlling a running loop

- Create `.ralph/stop` to halt after the current iteration
- The generated `.ralph/resume.sh` script resumes from where you left off

## Running server.js

The HTTP server wraps ralph.sh for remote monitoring and control.

```bash
# Start the server (localhost only by default)
node server.js

# Or via npm
npm start
```

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `RALPH_PORT` | `3411` | Server port |
| `RALPH_HOST` | `127.0.0.1` | Bind address |

### Tailscale access

The server binds to `127.0.0.1` (localhost) by default, which means it is **not** accessible from the network. To expose via Tailscale only:

```bash
# Find your Tailscale IP
tailscale ip -4
# e.g. 100.64.1.23

# Bind to your Tailscale IP
RALPH_HOST=100.64.1.23 node server.js
```

This binds the server to only the Tailscale network interface. It won't be accessible from your LAN or the public internet — only other devices on your Tailscale network.

**Do not use `RALPH_HOST=0.0.0.0`** unless you want the server accessible on all network interfaces.

### API endpoints

| Route | Method | Description |
|---|---|---|
| `/` | GET | Server info |
| `/start` | POST | Start a ralph loop |
| `/status` | GET | Get loop status and state |
| `/stop` | POST | Request graceful stop |
| `/kill` | POST | Kill running process |
| `/log` | GET | Tail the loop log |
| `/plan` | GET | View the plan file |
| `/reset` | DELETE | Clean .ralph state |

### Examples

```bash
# Start a loop
curl -X POST http://100.64.1.23:3411/start \
  -H 'Content-Type: application/json' \
  -d '{"dir": "/home/user/myproject", "max": 20}'

# Start with external plan file
curl -X POST http://100.64.1.23:3411/start \
  -H 'Content-Type: application/json' \
  -d '{"dir": "/home/user/myproject", "plan_file": "docs/TODO.md"}'

# Check status
curl http://100.64.1.23:3411/status

# View logs
curl http://100.64.1.23:3411/log?lines=50

# Graceful stop
curl -X POST http://100.64.1.23:3411/stop
```

## .ralph directory

Ralph stores its state in `.ralph/` inside the project directory:

```
.ralph/
  plan.md         # Task list (managed mode only)
  state.json      # Loop state (iteration, status, timestamps)
  signal          # Agent-to-ralph communication file
  stop            # Create this to halt gracefully
  loop.log        # Full output log
  resume.sh       # Auto-generated resume script
  .plan_snapshot  # Pre-iteration plan snapshot (for diffing)
```
