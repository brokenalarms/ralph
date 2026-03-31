# Orchestrator Refactor

The loop orchestrator (`go/internal/loop/`) has grown into multiple interleaved
code paths that duplicate logic and make it hard to reason about sequencing.
This spec describes the target state: a flat orchestrator that reads like a
narrative, with modules that own their domains and maintain their own invariants.

## Problems Being Solved

1. `Run()` is 220 lines with 6 continue paths and 4 break paths. You cannot
   read it top-to-bottom and know what happens.

2. The same operations happen in multiple places with different logic:
   `setStackHead` called in 3 places, `checkoutExistingBranch` in 2,
   `PrepareForNextTask` in 3, bead closing duplicated between
   `handlePostSignal` and `finalizePR`.

3. Two parallel completion paths: `resumeViaPR` and the normal post-signal
   flow both do merge/close with different code.

4. Compensating operations (`EnsureUpToDate`, `PostMergeUpdateMain`) exist
   because the operations that caused state drift don't clean up after
   themselves. Callers must remember to call them; forgetting causes bugs.

5. External refs written in two formats (`gh-42` and full URLs) depending on
   which code path created the PR. The URL is always available but some paths
   discard it.

6. PR link formatting scattered across 30+ call sites, each independently
   resolving `nwo` and calling `logging.PRLink`. Missing calls produce
   unlinked `PR #N` strings.

7. The loop reaches through `l.git.GH()` in 7 places to call GitHub directly,
   bypassing the git module's encapsulation.

8. Iteration counter manipulation via `*int` pointers passed through multiple
   functions.

## Design Principles

- **Orchestrator composes, modules own domains.** The orchestrator calls module
  functions. It does not assemble git commands, format PR references, or
  sequence push/squash/rebase.

- **Operations maintain their own invariants.** Push ensures the branch is
  up-to-date and squashed before writing to remote. Merge leaves local main
  synced after completion. No compensating calls from the orchestrator.

- **One completion path.** Whether the task is new, resumed from a PR, or
  retried after a fix — the verify/push/merge/close sequence is the same code.

- **The orchestrator makes cross-module calls.** Git doesn't know about beads.
  The task backend doesn't know about state.json. The orchestrator is the only
  thing that knows all modules exist, so it coordinates between them — but each
  coordination happens in exactly one place.

- **Modules own their data formats.** The logger owns PR hyperlink rendering.
  The git module owns PR URLs. The task backend stores what the git module gives
  it. No call site assembles formats.

## Target Orchestrator

```go
func (l *Loop) Run(ctx context.Context) error {
    if err := l.initialize(ctx); err != nil {
        return err
    }

    for {
        task, action := l.selectNextTask(ctx)
        if action == actionWait {
            if !l.waitForTasks(ctx) { break }
            continue
        }
        if action == actionDone { break }

        if task.changed {
            l.git.SwitchTask(task.id, task.desc)
        }

        if l.resumeExistingWork(ctx, task) { continue }

        result, action := l.runAgent(ctx, task)
        if action == actionDone { break }
        if action == actionRetry { continue }

        action = l.completeTask(ctx, task, result)
        if action == actionDone { break }
    }
    return nil
}
```

### `selectNextTask`

Encapsulates all stop conditions: max iterations, context cancelled, stop file,
no remaining tasks. Returns a `taskContext` and a `loopAction`. The orchestrator
does not check stop conditions anywhere else.

### `completeTask`

The single post-completion pipeline. Both the normal agent path and
`resumeExistingWork` converge here for anything that needs shipping.

```go
func (l *Loop) completeTask(ctx context.Context, task, result) loopAction {
    if !l.verifyAndFix(ctx, task, result) { return actionRetry }

    l.tasks.SetState(task.id, "phase", "verified", reason)

    shipResult, err := l.git.Ship(ctx, shipOpts)

    l.tasks.Close(task.id, shipResult.PRURL)
    l.state.RecordCompleted(task.id)
    l.postTaskHooks(ctx, task, shipResult)

    if shipResult.Merged && l.cfg.Evolve { return actionDone }
    return actionProceed
}
```

### `loopAction`

One type, returned by every phase. Replaces `resultBreak` vs `signalEvolve`
vs `halt` vs `*int` pointer manipulation.

```go
type loopAction int
const (
    actionProceed loopAction = iota
    actionRetry
    actionWait
    actionDone
)
```

## Git Module Changes

### `Push` — the single remote write

Internally: ensure up-to-date with base, squash commits above base into one,
force-push. No separate `EnsureUpToDate`, `SquashToOneCommit`, or `ForcePush`
exposed on the interface. Push IS squash-rebase-push.

### `SwitchTask(id, desc string)`

Replaces the scattered `setStackHead` + `ResetToDefaultBranch` +
`handleRebase` + `checkoutExistingBranch` + `RenameBranchForTask` calls.
Internally: find stack head from open PRs, reset to correct base if no stack,
rebase, checkout existing branch or rename for task. Called once per task
change by the orchestrator.

### `Ship(ctx, opts) → ShipResult`

The full "get work into a PR and optionally merge" pipeline:
1. Auto-commit uncommitted changes
2. Push (which internally ensures up-to-date, squashes, force-pushes)
3. Create or update PR (handles closed PRs, API fallback, reopen)
4. Write external ref as full URL to task backend
5. If auto-merge: merge via `MergeWithRetry` (handles CI wait, conflict
   resolution, post-merge main sync)

```go
type ShipOpts struct {
    TaskID    string
    TaskDesc  string
    Body      string
    AutoMerge bool
    OnCIFail  func(FixProblem) bool
    OnConflict func(FixProblem) bool
}

type ShipResult struct {
    PRNumber string
    PRURL    string
    Merged   bool
}
```

### Remove from `GitOps` interface

- `GH()` — the loop never touches GitHub directly
- `EnsureUpToDate()` — internal to Push
- `ForcePush()` — alias for Push, internal
- `PostMergeUpdateMain()` — internal to merge
- `CommitAll()` — internal to Ship (auto-commit)

## Fix Loop

One loop, parameterized by validation strategy:

```go
type FixValidator func(ctx context.Context) error

func (l *Loop) fixLoop(ctx context.Context, problem FixProblem, validate FixValidator) bool {
    for attempt := 0; attempt < problem.MaxAttempts; attempt++ {
        result := l.spawnFixAgent(ctx, problem)
        if !result.SignalDetected { return false }

        err := validate(ctx)
        if err == nil { return true }
        problem.ErrorOutput = err.Error()
    }
    return false
}
```

Replaces `testFixLoop`, `verifyWithFixLoop`, and the CI fix / conflict
resolution retry logic in `MergeWithRetry`. Callers pass their validator:

- Local (tests): `verify.RunTests(ctx, dir)`
- Local (LLM review): `verify.LLMVerifyPR(opts)`
- Remote (CI): `l.git.Push(ctx)` then `l.git.AwaitCI(ctx, pr)`

## Logger Changes

Log methods accept a structured `Opts` value. All context (domain, PR
number, branch) is a field on `Opts`. The logger appends rendered fields
after the message — PR becomes a clickable hyperlink at the end of the
line, branch gets a colored tag.

```go
l.logger.Log(logging.Opts{
    Domain: "git",
    PR:     prNumber,
    Branch: branch,
}, "Found PR — resolving")
// renders: [o][git] Found PR — resolving  PR #42
//                                         ^^^^^^ clickable link
```

The logger is initialized with `nwo` (from remote URL, set once via
`SetRepo`). `logging.PRLink` becomes unexported. No `NWOFromRemote` at
call sites. This is a large API migration best done as a clean pass after
the structural refactor.

## External Refs

Always full URLs. Written by `Ship` (one write site). The `gh-` prefix
format is eliminated. `parsePRNumber` is replaced by URL parsing (extract
number from `/pull/N` suffix).

`FindOpenPR` is updated to return URL alongside number, or callers use
`FindPR` which already returns `(number, title, url, error)`.

## Task Lifecycle

Bead is source of truth for the phase state machine
(`implementing → verified → closed`). The orchestrator makes these calls
but each transition happens in exactly one place:

- `runAgent` → `SetState(id, "phase", "implementing", ...)`
- `completeTask` → `SetState(id, "phase", "verified", ...)`
- `completeTask` → `CloseTask(id, reason)`

## Files Affected

| Current | Change |
|---|---|
| `loop.go` | `Run()` becomes ~80-line orchestrator. `initialize()` for one-time setup. |
| `loop_iteration.go` | `handlePostSignal`, `pushSignalPR`, `finalizePR` → deleted, replaced by `completeTask` |
| `loop_git.go` | `resumeViaPR`, `resolveByPRState`, `setStackHead`, `checkoutExistingBranch` → collapse into `resumeExistingWork` + git.SwitchTask |
| `loop_helpers.go` | `initRun` shrinks to one-time setup. Counter logic moves into `selectNextTask`. |
| `loop_verify.go` | `tryFixCI`, `tryFixConflict` → use generic `fixLoop` |
| `verifier.go` | `testFixLoop`, `verifyWithFixLoop` → use generic `fixLoop` |
| `loop_prompt.go` | Unchanged |
| `loop_utils.go` | `skipTask` stays. `parsePRNumber` replaced by URL parsing. |
| `loop_refactor.go` | Unchanged |
| `git/gitops.go` | Remove `GH()`, `EnsureUpToDate`, `ForcePush`, `PostMergeUpdateMain`, `CommitAll`. Add `SwitchTask`, `Ship`. |
| `git/git_merge.go` | `Push` internalizes rebase+squash. `Ship` added. `FlushUnpushedWork` uses `Ship`. |
| `git/github.go` | `FindOpenPR` returns URL. Merge uses `gh api` for structured responses (follow-up). |
| `logging/logging.go` | `PRLink` becomes internal. Logger initialized with `nwo`, formats PR refs automatically. |

## Testing Strategy

Integration tests exist in `loop_integration_test.go` (written before this
refactor). They call `Run()` with stubs and assert on outcomes: beads closed,
external refs set as URLs, merges happened, branch state consistent. These
tests must pass unchanged after the refactor.

Existing unit tests that test deleted functions (`handlePostSignal`,
`finalizePR`, `pushSignalPR`) will be removed and replaced by tests for the
new functions (`completeTask`, `selectNextTask`, `fixLoop`).

## Known Bugs Folded In

### ralph-laun: isMergeConflictError catches CI-gated failures

`executeMerge` checks `isMergeConflictError` before `isCIGatedError`. The
pattern `"not mergeable"` in `isMergeConflictError` matches when GitHub says
the PR is not mergeable because CI failed — but that's a CI gate, not a merge
conflict. The loop tries a pointless rebase instead of waiting for CI.

Fix: in the refactored merge pipeline, check CI-gated first, or remove
`"not mergeable"` from the conflict patterns since it's ambiguous. Test
exists in `ci_test.go` documenting the current behavior.

### ralph-1u38: PR link formatting (superseded)

The logger change (PR formatting owned by logger, not call sites) eliminates
this entire class of bugs. The bead was closed as superseded.

## Execution Order

This is a sequential refactor — each step changes interfaces the next depends on.

1. **Git: `Push` internalizes rebase + squash.** Remove `EnsureUpToDate` and
   `ForcePush` from `GitOps`. Update callers.

2. **Git: `SwitchTask`.** Consolidate stack head + rebase + checkout/rename.
   Remove `PrepareForNextTask`, `ResetToDefaultBranch`, `setStackHead`,
   `checkoutExistingBranch` as separate orchestrator concerns.

3. **Git: `Ship`.** Compose auto-commit + push + PR create + merge. Remove
   `PushAndCreatePR`, `CommitAll`, `PostMergeUpdateMain`, `GH()` from
   `GitOps`. `Ship` writes external ref as URL.

4. **Logger: PR formatting.** Initialize with `nwo`. Make `PRLink` internal.
   Update all call sites to pass raw PR numbers.

5. **External refs: always URLs.** Update `FindOpenPR` to return URL.
   Remove `gh-` prefix handling. Replace `parsePRNumber` with URL parsing.

6. **Fix loop.** Extract generic `fixLoop` with `FixValidator`. Replace
   `testFixLoop`, `verifyWithFixLoop`, CI fix, conflict resolution.

7. **Orchestrator rewrite.** `Run()` becomes flat. `selectNextTask`,
   `completeTask`, `resumeExistingWork`. Delete `handlePostSignal`,
   `finalizePR`, `pushSignalPR`, `handleRunResult`, `processRunOutcome`.

8. **Delete dead code.** Remove functions, methods, and interface members
   that no longer have callers.

## Follow-up: State Module Owns All .ralph/ File Management

The orchestrator currently makes bare `os.WriteFile`, `touchFile`,
`updateStreamTask`, `writeRunBranch`, `recordCompletedTask`, and
`checkStopFile` calls directly. These are all state transitions persisted
as files in `.ralph/`. The state module should own all of them.

From the orchestrator's perspective, there is no difference between
writing `state.json` and writing `.signal_complete` — both are state
transitions. The orchestrator says "task started" or "signal complete"
and the state module handles the concrete file operations.

Signals (`.signal_complete`, `feedback`, `stop`) are IPC between the
agent subprocess and the orchestrator. They can be composed internally
by the state module — the orchestrator calls `state.CheckStop()` not
`checkStopFile()`, calls `state.BeginIteration()` not `touchFile` +
`updateStreamTask` + `writeRunBranch` separately.

## Follow-up: Module Boundary Enforcement

Every module currently uses a god object pattern (e.g. `git.Manager`,
`tasks.BD`) where all methods hang off a struct that holds shared state.
Any method can reach into any other method's concerns through the shared
struct. This defeats encapsulation — the module boundary exists in name
only.

The target state: each module exposes public functions that take what they
need as parameters and return results. Private functions compose internally.
No struct that connects everything. The package itself is the encapsulation
boundary — lowercase functions are private, uppercase are the API.

This applies to: git, tasks, verify, logging, claude/agent. It's a
separate phase that follows the orchestrator refactor.

## What This Does NOT Change

- The `tasks.Backend` interface — it's the raw storage API, stays as-is
- Prompt building (`loop_prompt.go`, `prompt/`)
- The verifier's internal LLM escalation logic
- The agent module
- State.json persistence (moved into operations but same data)
- The `claude.RunConfig` structure
