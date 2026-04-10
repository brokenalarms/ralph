# Orchestrator + module boundaries

This spec defines the architecture between the loop orchestrator and the
domain modules it composes. It supersedes the "How to prevent regression"
section of `module-boundary-beads.md`, which contained a loophole that
agents have repeatedly satisfied in letter while violating in spirit.

## The shape

```
cmd/ralph/main.go        — parses flags, does shell setup, constructs and runs Loop
  ↓
internal/loop (Loop)     — THE orchestrator. Owns the iteration sequence,
                            iteration state, run state. Has private methods
                            that read its own fields and call modules.
  ↓
internal/git, /verify,   — domain modules. Each owns its own state, retries,
/state, /attempts, etc.    error handling, healing within its domain. Modules
                            never hold references to other modules and never
                            accept other modules as parameters.
  ↓
internal/git/<github>    — a module's internal sub-modules are unexported
                            and never escape the parent module's API. Loop
                            never sees github types; it sees git types.
```

## The five rules

### 1. Loop is the only struct that holds module references

`Loop` is the orchestrator. It legitimately holds module instances as fields,
constructed in `Loop.New()` (or per iteration where the module's lifecycle
demands it). No other struct in the codebase may hold a field whose type is a
module from another package.

```go
// Allowed — Loop is the orchestrator.
type Loop struct {
    cfg      Config
    git      *git.Repo
    state    *state.Store
    attempts *attempts.Tracker
    limiter  *ratelimit.Limiter
    agent    *agent.Agent
    backend  tasks.Backend
    analyzer *analyzer.Analyzer
    // ... iteration state and run state below
}

// Forbidden — Verifier is a module, not the orchestrator. Cannot hold
// other modules. Replace with package-level functions or with a struct
// that holds only its own state + config.
type Verifier struct {
    git     git.Ops          // ❌ holds another module
    state   *state.Store     // ❌ holds another module
    backend tasks.Backend    // ❌ holds another module
}

// Forbidden — Deps/Opts/Params struct that bundles modules is the same
// antipattern via a different shape.
type VerifierDeps struct {
    Git   git.Ops             // ❌
    State *state.Store        // ❌
}
```

**Exception: a module's own internal sub-modules.** A module may compose an
unexported sub-module that lives inside the same package (or an internal
package nested under it) and is never exposed across the module boundary. The
canonical example is `internal/git` holding a private `github` client for
GitHub API calls — Loop never sees `*github.Client`, only `git.Repo` methods
that internally route through it.

### 2. Modules don't accept other modules as parameters

A module's public methods take **module-specific data structs** for the data
they need. The orchestrator pre-fetches whatever data a target module requires
and passes it in. No module ever takes another module type in its function
signature, anywhere.

```go
// Forbidden — verifier.Check accepts a git reference.
func Check(g git.Ops, taskID string) Result { ... }

// Forbidden — opts struct carries a module reference.
type CheckOpts struct {
    Git    git.Ops      // ❌
    TaskID string
}

// Correct — pure data in.
type CheckInput struct {
    Ctx         context.Context
    Diff        string  // pre-fetched by orchestrator
    DiffSource  string  // "PR" or "iteration"
    Title       string
    Description string
    Acceptance  string
    PromptsDir  string
    Model       string
}

func Check(in CheckInput) CheckResult { ... }
```

The only allowed parameter types in any function or method, anywhere outside
of `cmd/ralph` and `Loop`'s own private methods, are:

- `context.Context`
- Go primitives, slices, maps of primitives
- Pure data structs (no methods, no module-typed fields)
- The package's own internal types

`*logging.Logger` is **not** on the allow list — see rule 5.

### 3. Loop methods read state from the receiver, not from parameters

Methods on `Loop` exist for orchestration steps. They take parameters only
for `context.Context` and for **per-call data that just arrived from a
sibling call** (a verify failure detail, a rejection reason, an analyzer
outcome). Long-lived state — current task, current head, current workdir,
current raw log path, retry counters, active reviewers — lives as private
fields on `Loop` and is read directly via the receiver.

```go
// Forbidden — iteration state threaded through the parameter list.
// Reveals that the state isn't actually owned by Loop yet.
func (l *Loop) postSignalVerification(
    ctx context.Context,
    taskID, nextTask, headBefore, workDir, rawLogPath string,
) bool { ... }

// Correct — those five strings are iteration state on l.iter, set when
// the iteration began. The method reads l.iter.* directly.
func (l *Loop) postSignalVerification(ctx context.Context) bool {
    // reads l.iter.task, l.iter.headBefore, l.iter.workDir,
    // l.iter.rawLogPath, l.iter.testFixAttempts, l.iter.llmVerifyAttempts
    // mutates l.iter.testFixAttempts, l.iter.llmVerifyAttempts
    // calls verify.RunTests(...), verify.Check(...),
    //       l.spawnTestFix(...), l.spawnVerifyFix(...)
}
```

`Loop`'s field layout reflects the lifecycle of its state:

```go
type Loop struct {
    cfg Config

    // Module instances — DI at construction.
    git      *git.Repo
    state    *state.Store
    attempts *attempts.Tracker
    limiter  *ratelimit.Limiter
    agent    *agent.Agent
    backend  tasks.Backend
    analyzer *analyzer.Analyzer

    // Run-lifetime state — accumulates across all iterations of one Run.
    sessionTasks      []CompletedTask
    activeReviewers   []git.Reviewer
    reviewersDetected bool
    runIteration      int
    lastAction        analyzer.Action
    lastTaskMerged    bool

    // Iteration-lifetime state — reset by beginIteration each loop pass.
    iter iterationState
}

type iterationState struct {
    task              taskContext
    meta              tasks.Meta
    headBefore        string
    workDir           string
    rawLogPath        string
    diff              string
    diffSource        string
    testFixAttempts   int
    llmVerifyAttempts int
}
```

`iterationState` is **state organization**, not a module — see rule 4.

### 4. State organization vs module

A struct in package `P` is **state organization** when it lives in package
`P`, has no public API (no constructor, no exported methods, no exported
fields), and is mutated only by code in package `P`. Such structs may be
used as fields on other types in `P`, including as nested fields on `Loop`,
with **direct field access**.

A struct in package `P` is a **module** when it has a public constructor or
public methods that other packages call. Modules are accessed only through
their public API; their fields are never read or written by code outside
their defining package, even by the orchestrator that holds an instance.

```go
// State organization — l.iter.testFixAttempts++ is fine because iter is
// Loop's own data, not a module.
type iterationState struct {
    testFixAttempts   int
    llmVerifyAttempts int
}

// Module — l.git.WorktreeBranch = "x" would be a violation. Code outside
// the git package only calls git.Repo methods, never reads or writes its
// fields directly.
type Repo struct {
    workDir        string  // private field, only git package code touches it
    worktreeBranch string
}
func (r *Repo) BranchForTask(ctx context.Context, ...) (string, error) { ... }
```

The line: methods + public API → module → call only the API, never field
access. No methods, no API → state organization → field access fine.

### 5. Logger is the single named cross-module exception

`*logging.Logger` is the **only** module type allowed to be passed
through, held as a struct field, or used as a function parameter. Logging
is genuinely cross-cutting — every package needs to log — and
package-level state would leak across parallel tests. So the logger gets
constructed once in `cmd/ralph/main.go` and threaded through `loop.New`
and module constructors that need it.

This is the only such exception in the codebase. Every other module
follows the no-passing-through rule strictly. The exception is named in
the spec, named in the arch tests (or rather: those tests are explicit
no-ops with a comment pointing here), and applies to no other type.

```go
// Allowed — logger is the named exception.
type Loop struct {
    logger *logging.Logger
    // ... other state
}

func New(cfg Config, st *state.Store, gm git.Ops, logger *logging.Logger) *Loop {
    return &Loop{logger: logger, ...}
}

// Forbidden — git.Ops is not the exception.
type Verifier struct {
    git git.Ops  // ❌ — only Loop holds module references
}
```

### 6. Module-boundary data structs use neutral names

Field names, function names, type names, and interface methods that cross a
module boundary use **neutral names that describe what the data is**, not
where it came from. The task backend may be beads, files, jira, or anything
else — the verifier sees a "title", "description", "acceptance", not a
"BeadTitle". The agent runner may be Claude or another LLM. The version
control may be git or fossil.

```go
// Forbidden — leaks "bead" through the verifier API surface.
type CheckInput struct {
    BeadTitle       string
    BeadDescription string
    BeadAcceptance  string
}

// Correct — neutral. The fact that these strings come from beads is
// the orchestrator's local concern.
type CheckInput struct {
    Title       string
    Description string
    Acceptance  string
}
```

The same rule applies to function and helper names: `getBeadDescription` is
a leak, `getDescription` is neutral. `BeadOpen` becomes `IsOpen` or similar.
"Bead", "Github", "Claude", "Copilot" all stay inside the modules that own
them.

## Module enumeration

| Module | Holds state? | Public surface | Notes |
|---|---|---|---|
| `loop` (`Loop` struct) | yes — modules + run state + iteration state | `New(cfg) *Loop`, `(l *Loop) Run(ctx) error`, `(l *Loop) SessionTasks() []CompletedTask` | The only struct that holds module references. Has private methods organized around orchestration steps. |
| `git` (`Repo` struct) | yes — workdir, branch, internal github client | `New(...)`, `BranchForTask`, `ResumeTask`, `Ship`, `FlushUnpushedWork`, query methods | Owns all git+GitHub retries/backoffs/healing internally. Surfaces `Stuck*` data when external code change is needed. Internal sub-module: github. |
| `verify` | no — package functions | `RunTests`, `CompileCheck`, `Check`, `DetectTestCommand`, `CapModel` | The Verifier struct from the loop package is deleted. Verification is stateless functions. Counters that were on Verifier move to `Loop.iter`. |
| `agent` | yes if streaming/cancellation state matters | `Run`, `Query`, `StopStreaming`, `InjectMessage` (or package functions if no per-instance state) | TBD on inspection during refactor — instance vs package functions. |
| `state` (`Store` struct) | yes — file-backed KV | intent methods (no raw KV exposed) | Held by Loop. |
| `attempts` (`Tracker` struct) | yes — per-task counters | intent methods | Held by Loop. |
| `ratelimit` (`Limiter` struct) | yes — call counters, persistence | `WaitOrAllow`, `Count` | Held by Loop. |
| `tasks` (`Backend` interface) | yes — backend-specific | `Next`, `GetMeta`, `CloseTask`, `SkipTask`, etc. | Held by Loop. Names neutral — no "Bead*" methods. |
| `analyzer` | minimal | `Analyze(IterationState) Result` | Held by Loop. |
| `logging` | no — package functions | `Emit(opts, format, args...)` | Imported, not held. |

## Domain ownership: who handles retries

Each module owns its domain's retry/error/healing logic completely.

- **Git owns CI retries.** When `git.Ship` runs, it pushes, creates the PR,
  polls reviewers, attempts the merge, polls CI checks, retries on transient
  failures with backoff, heals branch state on conflict — all internal. The
  orchestrator never sees a transient git error and never implements a
  retry loop for git operations.

- **Git escalates to the orchestrator only when it has exhausted its own
  knowledge.** A required CI check failed in a way that needs code changes,
  or there's a merge conflict git can't resolve, or a reviewer left
  actionable comments — git can't write code, so it returns a `Stuck*`
  variant of `ShipResult` with the data describing the situation already
  fetched (CI log, conflict diff, comment list).

- **Git knows nothing about fix agents.** It never calls verifier or agent.
  It surfaces "stuck" with data; the orchestrator decides what to do.

- **The orchestrator routes data between modules.** When `git.Ship` returns
  `StuckCI`, the orchestrator passes the data to the verifier (or whatever
  module owns spawning fix agents) as a module-specific input struct, awaits
  the agent's commit, then calls `git.Ship` again. Git picks up where it
  left off internally.

```go
// Inside Loop.shipAndComplete — the orchestrator's ship section after
// the refactor. Three intent calls per cycle, no transient retry handling,
// no git operation choreography.
for {
    result, err := l.git.Ship(ctx, l.shipOpts())
    if err != nil { return false }
    if result.Merged { l.iter.merged = true; break }

    switch {
    case result.StuckCI != nil:
        if !l.spawnCIFix(ctx, result.StuckCI) { return false }
        continue  // back to git.Ship
    case result.StuckConflict != nil:
        if !l.spawnConflictFix(ctx, result.StuckConflict) { return false }
        continue
    case result.StuckReview != nil:
        if !l.spawnReviewFix(ctx, result.StuckReview) { return false }
        continue
    }
    break
}
```

`spawnCIFix`, `spawnConflictFix`, `spawnReviewFix` are private Loop methods
that call `l.agent.Run(...)` with a templated prompt built from the stuck
data plus iteration state from `l.iter.*`. They don't touch git.

## Stub packages for testing

Every module ships a sibling stub package so integration tests can drive
behavior without making real calls. The stub package returns the *real*
module type (`*git.Repo`, `tasks.Backend`, etc.) wired internally to a
recording stub for any underlying dependency, plus functional options for
canned return values.

```
internal/git/        — Repo, GitHub interface, intent methods
internal/git/stubgit — stubgit.New(rec, opts...) returns a *git.Repo wired
                       to a stub GitHub. Records every method call to rec.
                       Options: WithShipResult, WithCIChecks, WithStuckCI, etc.
internal/tasks/stubtasks
internal/state/stubstate
internal/attempts/stubattempts
internal/agent/stubagent
internal/ratelimit/stublimiter
```

Each stub package:

1. Provides a constructor returning the real module type wired to internal
   stubs for any dependency.
2. Records every method call to a shared `*recorder.Recorder` passed at
   construction. The recorder captures `(package.Method, argDigest)` tuples.
3. Provides functional options for canned return values and callbacks.

A shared `internal/testutil/recorder` package provides the `Recorder` type
and `AssertSequence(t, "pkg.Method", ...)` helper.

The crucial property: **a test that needs to drive GitHub-side behavior
imports `stubgit`, not `internal/git/github`**. The github sub-module stays
hidden inside git. Tests that need to control reviewer detection or CI
results call `stubgit.WithCIChecks(...)`, never `&github.Stub{...}`.

## The canonical sequence test

The integration test that locks down orchestrator behavior is a recording
test that walks `Loop.Run` top to bottom and asserts the exact sequence of
module method calls. It is the contract: if `Run` changes its order or
inserts a step, the test fails with a clear diff.

```go
func TestLoop_HappyPath_SequencesModuleCallsInOrder(t *testing.T) {
    rec := recorder.New()

    g  := stubgit.New(rec,
        stubgit.WithBranchForTask("ralph/seq1-add-auth"),
        stubgit.WithResumeTask(git.ResumeTaskResult{Handled: false}),
        stubgit.WithShipResult(git.ShipResult{Merged: true, PRNumber: 42}),
    )
    bk := stubtasks.New(rec, stubtasks.WithTasks(
        stubtasks.Task{ID: "ralph-seq1", Title: "Add auth", Description: "...", Acceptance: "..."},
    ))
    a  := stubagent.New(rec, stubagent.WithSignal())
    s  := stubstate.New(rec)
    at := stubattempts.New(rec)
    lim := stublimit.New(rec)

    l := loop.New(loop.Config{
        TaskBackend: bk,
        // ... cfg
    })
    l.WireModules(g, a, s, at, lim)  // or via New() options

    if err := l.Run(context.Background()); err != nil {
        t.Fatalf("Run: %v", err)
    }

    rec.AssertSequence(t,
        "tasks.Next",
        "git.BranchForTask",
        "tasks.SetMeta",
        "git.ResumeTask",
        "ratelimit.WaitOrAllow",
        "agent.Run",
        "verify.RunTests",
        "verify.CompileCheck",
        "git.PRDiffForTask",
        "verify.Check",
        "git.Ship",
        "tasks.CloseTask",
        "attempts.Clear",
        "git.TagTaskEnd",
        "tasks.Next",
    )
}
```

That list **is the spec**. Reading it top to bottom is reading `Run()` top
to bottom. Sister tests cover branches: resume-via-PR-merged, test failure
triggering test-fix, LLM rejection triggering verify-fix, ship returning
each `Stuck*` variant.

When a future agent considers changing orchestration order, the test fails
on the first call that's out of place. When a future agent considers hiding
a step inside a module method, the recorder catches it because recording
lives in the stub module, not in the test wiring.

## Strict arch tests

The following tests live in `go/internal/` and run on every build. Each
fails on the current code today; the failures map exactly to the work
remaining in the refactor. They are the executable form of the rules above.

### `TestNoModulesInNonLoopStructs`
Walks every `type X struct` in `go/internal/`. Whitelist: structs in package
`loop` named `Loop` (the orchestrator). Forbidden: any field whose type
resolves to a struct or interface from another package that has methods.
Specifically banned types include `git.Ops`, `*git.Repo`, `*state.Store`,
`*attempts.Tracker`, `*ratelimit.Limiter`, `*analyzer.Analyzer`,
`tasks.Backend`, `*agent.Agent`, `*Verifier`, `claudeRunner`. Whitelisted:
primitives, data structs from this package, `context.Context`,
`claude.SignalPaths` and similar pure data types.

### `TestNoModulesInFunctionParams`
Walks every `FuncDecl` in `go/internal/`. Fails when a parameter type is a
module. Constructors (`New*`) exempt. `Loop` private methods exempt for
modules they hold (because they access via the receiver, not via the
parameter — but the test doesn't need a special case because Loop methods
shouldn't take module parameters at all).

### `TestNoLoggerAsField`
Walks all struct fields in `go/internal/`. Fails on any field of type
`*logging.Logger`. Logger is imported.

### `TestNoLoggerInFunctionParams`
Walks all function parameters. Fails on any `*logging.Logger` parameter.

### `TestNoImplementationLeakInExportedNames`
Walks all exported type names, field names, and function names in
`go/internal/`. Fails on names that contain implementation prefixes
(`Bead*`, `Github*`, `Claude*`, `Copilot*`) outside the module that owns
that implementation.

### `TestNoFieldAccessOnForeignModuleType`
Walks all assignment statements. Fails when the left-hand side is a field
selector on a value whose type comes from another package and has methods.
Catches `l.git.WorktreeBranch = "x"` style violations.

### `TestSequenceLockedDown` (the integration sequence test)
The canonical happy-path sequence test plus its sister tests. They exist
in `internal/loop` and exercise `Loop.Run` against stub modules. They don't
just assert behavior — they assert ordering, and the assertion is the
intent contract.

## Implementation order

The refactor lands in commits, each driven by a failing test or a failing
arch check. The order:

1. **Spec doc** (this file) and the **strict arch tests** (above). The arch
   tests fail on current code; the failures are the punch list.
2. **Stub packages** (`stubgit`, `stubtasks`, `stubagent`, etc.) and the
   **canonical sequence test** in failing form. The sequence test is the
   single source of truth for `Run`'s shape.
3. **GitHub → unexported sub-module of git.** Reviewer types, Copilot
   detection, comment filtering hide inside git. Loop sees git types only.
4. **Git absorbs Ship retry/backoff/healing.** `git.Ship` runs the full
   push→PR→poll→CI→merge flow with internal retries. Surfaces `StuckCI`,
   `StuckConflict`, `StuckReview` only when external code change is needed.
5. **Verifier struct deleted.** `verify` is package functions. Counters
   move to `Loop.iter`. Loop's post-signal verification is a private method
   that reads `l.iter.*` and calls `verify.RunTests`, `verify.Check`,
   `l.spawnTestFix`, `l.spawnVerifyFix`.
6. **Logger cleanup.** All `*logging.Logger` fields and parameters removed;
   replaced with `logging.Emit(...)` package-level calls.
7. **Neutral naming sweep.** `Bead*`, `Github*`, etc. removed from
   cross-module API surfaces.
8. **Final pass.** All arch tests green, sequence test green, full test
   suite green, push, PR.

## What this supersedes

The `module-boundary-beads.md` "How to prevent regression" section ends with:

> Top-level structs (Loop, Verifier, git.Repo) may hold module references
> as fields — that's dependency injection at construction. But params/opts
> structs passed to functions must not carry them.

That sentence is the loophole. Every prior agent satisfied the
params/opts rule and pointed at it as authorization for `Verifier` to keep
holding `git.Ops`/`*state.Store`/`tasks.Backend` and for the Loop receiver
to smuggle every module through transparently. The corrected rule, in this
spec, is **only `Loop`**, and the arch tests enforce it mechanically.
