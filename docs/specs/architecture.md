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
    The agent module is the single code path for spawning agents. Loop, task,
    review, verification, and fix agents all route through agent.Runner.

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

### gitHub (in git/github.go)

```go
// gitHub abstracts GitHub CLI operations. Unexported — the production
// implementation (ghCLI) is always constructed by New(). Git-package
// tests inject stubGitHub via newStubGitHub (same-package, unexported).
type gitHub interface {
	Available() bool
	FindOpenPR(ctx context.Context, branch, repoURL string) (prNumber int, err error)
	CreatePR(ctx context.Context, opts CreatePROpts) (prNumber int, err error)
	MergePR(ctx context.Context, prNumber int, repoURL string, opts MergeOpts) MergeResult
	ListChecks(ctx context.Context, prNumber int, repoURL string) ([]CICheckResult, error)
	EditPR(ctx context.Context, prNumber int, repoURL, title, body string) error
	// EditPRBase retargets a PR to the given base branch via PATCH /repos/{nwo}/pulls/{number}.
	EditPRBase(ctx context.Context, prNumber int, repoURL, base string) error
	GetRunLog(ctx context.Context, prNumber int, workDir string) string
	FindPR(ctx context.Context, branch, repoURL string) (number int, title, url string, err error)
	PRDiff(ctx context.Context, repoURL string, prNumber int) (string, error)
	GetPR(ctx context.Context, nwo string, prNumber int) (*PRDetail, error)
	ListOpenPRBranches(ctx context.Context, repoURL string) ([]string, error)
	ReopenPR(ctx context.Context, prNumber int, repoURL string) error
	CreatePRViaAPI(ctx context.Context, nwo string, opts CreatePROpts) (prNumber int, err error)
	GetJobStepCount(ctx context.Context, nwo string, prNumber int) (int, error)
	// GetRunningJobSteps resolves the workflow run for the PR's current head
	// SHA (not the latest pull_request-event run repo-wide — see the NOTE on
	// GetJobStepCount for why that is unsafe with parallel PRs in flight) and
	// returns, for each job with a step currently in_progress, the job name,
	// that step's name and 1-based index, and the job's total step count.
	GetRunningJobSteps(ctx context.Context, nwo string, prNumber int) ([]JobStepStatus, error)
	// GetFailedJobAnnotations resolves the workflow run for the PR's current
	// head SHA (the same resolution GetRunningJobSteps uses) and returns, for
	// each failed job of that run, the job's failure-level check-run
	// annotation messages. A job's id is also its check-run id, so the
	// annotations come from the check-runs endpoint keyed by job id.
	GetFailedJobAnnotations(ctx context.Context, nwo string, prNumber int) ([]JobAnnotations, error)
	// ListAllPRs returns all PRs (open and closed) for chain-walking during stack merge.
	ListAllPRs(ctx context.Context, workDir string) ([]PRInfo, error)
	// ListOpenPRs returns only open PRs — the cheap alternative to ListAllPRs
	// for callers that don't need closed/merged history (e.g. the startup
	// leftover-PR check).
	ListOpenPRs(ctx context.Context, workDir string) ([]PRInfo, error)
	// DetectActiveReviewers queries the repo's installed GitHub Apps and cross-
	// references against the Known reviewer registry. For Copilot it also checks
	// rulesets to set the correct timeout. Returns the active reviewer list.
	DetectActiveReviewers(ctx context.Context, nwo string) ([]Reviewer, error)
	// PollReview polls for a review from the given bot username on the given PR,
	// returning it with inline comments when found. Returns nil without error if
	// the timeout expires before a review arrives.
	PollReview(ctx context.Context, nwo string, botUsername string, prNumber int, timeout time.Duration) (*AutoReview, error)
	// GetRequiredChecks returns the required status check context names for the
	// given branch from branch protection rulesets. Returns an empty slice when
	// no required checks are configured, which means all checks are evaluated.
	GetRequiredChecks(ctx context.Context, nwo, branch string) ([]string, error)
	// ReplyToReviewComment posts a reply to an inline review comment thread.
	ReplyToReviewComment(ctx context.Context, nwo string, prNumber, commentID int, body string) error
	// FetchReviewThreadIDs returns a map from REST comment database ID to GraphQL
	// thread node ID for all review threads on the given PR. Used to resolve threads
	// after addressing review feedback.
	FetchReviewThreadIDs(ctx context.Context, nwo string, prNumber int, commentIDs []int) (map[int]string, error)
	// ResolveReviewThread resolves a review thread by its GraphQL node ID.
	ResolveReviewThread(ctx context.Context, threadID string) error
	// Ping verifies that GitHub is reachable. Returns nil when reachable, an
	// error otherwise (including timeout).
	Ping(ctx context.Context) error
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
  ├── agent: run iteration agent
  │     └── on signal:
  │           ├── verify: run test suite
  │           ├── agent: LLM review via Query (fast model)
  │           ├── agent: fix agent if rejected
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
