# Ralph Architecture — Target State

This spec describes the desired end-state architecture. All refactoring work
should move toward this structure. Agents must read this before starting
architectural or refactoring tasks.

## Principles

1. **Orchestrator composes, modules own domains.** The loop tells the story
   by calling module functions. Modules encapsulate all logic for their domain.
   No git commands outside the git module. No verification logic outside verify.

2. **Declarative functional style.** Build up requirements in a struct through
   the function lifecycle, then call a single function to produce the result.
   Avoid imperative mutation scattered through function bodies.

3. **Early guard returns over nesting.** Flat code reads top-to-bottom.
   Guard clauses at the top, happy path at the bottom.

4. **Compositional orchestrators + stateless helpers.** Orchestrator functions
   glue stateless helpers together. The orchestrator tells a readable story
   through named function calls. Each helper is a pure, unit-testable function.
   See Tabi's `elementPredicates.ts` / `elementTraversal.ts` (stateless helpers)
   vs `HintMode.ts` / `ElementGatherer.ts` (orchestrators) for the reference pattern.

5. **Options structs for multi-parameter functions.** Functions with 3+ parameters
   use a named struct. No positional string juggling.

6. **Single source of truth.** Flag definitions, log prefixes, prompt templates —
   each defined in one place. No duplication across code and config.

## Package Structure

```
cmd/ralph/
  main.go              — entry point, flag parsing, subcommand dispatch
  helpers.go           — evolve restart, signal file cleanup
  prompts/             — embedded prompt templates (source of truth)

internal/
  agent/
    agent.go           — centralized agent module: all agent invocations go through here
    sandbox.go         — macOS sandbox-exec container isolation
    The agent module is the single code path for spawning agents. Loop, task,
    review, verification, and fix agents all route through agent.Runner.
    Container isolation via sandbox-exec is applied by default when available.

  loop/
    loop.go            — iteration control, task selection, stop/wait
    loop_verify.go     — post-signal verification, LLM escalation, fix agent spawning
    The loop owns the orchestration story. It composes git, verify, and agent
    modules. No git commands, no gh commands, no exec.Command in this package.

  git/
    git.go             — worktree setup, sync guard (stash/fetch/rebase/pop), branch ops
    git_merge.go       — merge with retry, conflict resolution, push, PR creation
    git_rebase.go      — rebase onto base branch, auto-resolve mechanical conflicts,
                         squash-merge detection
    github.go          — GitHub interface for PR/CI operations.
                         Production impl wraps gh CLI. Tests inject stubs.
    ci.go              — CI check evaluation, polling, status types

  claude/
    claude.go          — agent process runner, lifecycle management (internal to agent module)
    claude_stream.go   — stream filtering, tool call batching, formatting

  verify/
    verify.go          — test execution, LLM diff verification, PR diff retrieval

  tasks/
    tasks.go           — backend interface
    bd.go              — bd CLI wrapper
    checklist.go       — (remove once bd is sole backend)

  config/
    config.go          — Config struct, Defaults, Validate
    flags.go           — Flag parsing, env vars, config file
                         Single flag definition struct auto-generates help text.

  workctx/
    workctx.go         — WorkContext struct (ProjectDir, WorkDir, RalphDir, PromptsDir)

  logging/
    logging.go         — Logger with actor/domain tag system: [loop], [agent],
                         [git], [ci], [beads]. Colors defined in one place.

  state/               — state.json persistence
  attempts/            — attempt history tracking
  analyzer/            — iteration analysis
  planning/            — (remove once planning phase is removed)
  prompt/              — prompt assembly from templates
  quality/             — code quality signals
  ratelimit/           — rate limiting
  server/              — HTTP status server
  tmux/                — tmux session management
```

## Key Interfaces

### GitHub (in git/github.go)

```go
type GitHub interface {
    Available() bool
    FindOpenPR(branch, repoURL string) (string, error)
    CreatePR(opts CreatePROpts) error
    MergePR(prNumber, repoURL string, opts MergeOpts) (string, error)
    UpdateBranch(dir, nwo, prNumber string) (bool, error)
    ListChecks(prNumber, repoURL string) ([]CICheckResult, error)
    GetRunLog(prNumber, workDir string) string
}

type CreatePROpts struct {
    Head, Base, Title, Body, Repo string
}
```

### Backend (in tasks/tasks.go)

Already exists. Remove checklist once bd is sole backend.

### claudeRunner (in loop/)

Already exists as an interface for test stubbing.

## Data Flow

```
loop.Run()
  ├── tasks: get next task + full context
  ├── prompt: assemble from templates
  ├── agent: run iteration agent (sandboxed)
  │     └── on signal:
  │           ├── verify: run test suite
  │           ├── agent: LLM review via Query (fast model, sandboxed)
  │           ├── agent: fix agent if rejected (sandboxed)
  │           └── agent: LLM review via Query (smart model escalation)
  ├── git: merge with retry
  │     ├── sync worktree to latest base branch
  │     ├── attempt merge via GitHub
  │     ├── on conflict: rebase + auto-resolve + force-push + retry
  │     └── on CI failure: agent: spawn fix agent + retry
  └── tasks: close bead (only after successful merge)
```

## Log Tag System

```
[loop]   — orchestrator decisions, iteration flow
[agent]  — claude output (text, bash, edits)
[git]    — push, merge, rebase, branch operations
[ci]     — CI check polling, pass/fail
[beads]  — task open/close/state changes
[verify] — test results, LLM verification
```

Each tag has a fixed color. Defined once in logging/logging.go.

## What NOT to Do

- No `exec.Command("claude", ...)` outside the agent and claude packages
- No `exec.Command("git", ...)` outside the git package
- No `exec.Command("gh", ...)` outside git/github.go
- No `exec.Command("bd", ...)` outside tasks/bd.go
- No prompt text hardcoded in Go — use templates in cmd/ralph/prompts/
- No flag definitions in multiple places — single source
- No planning phase when using bd backend
- No `[beads]` tag for orchestrator messages — that's `[loop]`
