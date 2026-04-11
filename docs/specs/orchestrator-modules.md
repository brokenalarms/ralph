# Orchestrator + module boundaries

This spec defines the architecture between the loop orchestrator and the
domain modules it composes. It supersedes the "How to prevent regression"
section of `module-boundary-beads.md`, which contained a loophole that
agents have repeatedly satisfied in letter while violating in spirit.

## The two load-bearing rules

> **Rule A (boundary):** Module types from package P never appear in the
> public API surface of any other package, anywhere, in any form —
> except as parameters to the orchestrator's constructor (`loop.New`),
> which is the named composition point where modules from many
> packages meet.

> **Rule B (immutability):** A module's state changes only through its
> own public API methods. Construction is the one allowed entry point;
> after that, no external code — not other packages, not tests, not the
> orchestrator — mutates the module's fields directly. The public API
> methods are the only path in.

A module's public API surface is its exported names: function/method
signatures (parameters and return values), exported struct field types,
exported interface methods, and exported package-level variables.
Inside its own package, a module is free to compose, mutate, and hold
whatever internal state it needs — but nothing module-shaped escapes
through the package's exported boundary, except through the one
designated DI seam.

Every other rule in this document is a corollary of that one. If a
proposal puts a module type into a parameter list (other than
`loop.New`), a return value, an exported struct field, or an
interface method that crosses a package boundary, it's forbidden —
regardless of how convenient it would be.

The only named cross-cutting exception is `*logging.Logger` (rule 5),
which is allowed to appear in module constructors (not just `loop.New`)
as the one stateful utility threaded through the entire program.

### Why `loop.New` is the only carve-out

Loop's role IS to compose modules from many packages — that's what
"orchestrator" means. The composition has to happen somewhere. Putting
module types in `loop.New`'s parameter list is the explicit, named,
single point in the dependency graph where production code wires real
implementations and test code wires stubs. **Construction-time DI via
the constructor parameter list is the test injection seam.**

```go
// Production (cmd/ralph/main.go):
gm := git.New(git.Config{...})
st := state.NewStore(ralphDir)
backend := initTaskBackend(cfg, ...)
logger := logging.New(logFileWriter)

execLoop := loop.New(loop.Config{...}, loop.Modules{
    State:       st,
    Git:         gm,
    TaskBackend: backend,
    Logger:      logger,
})
execLoop.Run(ctx)
```

```go
// Test (internal/loop/some_test.go):
gm := &git.StubRepo{...}              // stub git.Ops
st := newTestState(t)                  // stub or real state.Store
backend := &testutil.StubBackend{...}  // stub tasks.Backend

l := loop.New(loop.Config{...}, newTestModules(t, st, gm, backend))
l.Run(ctx)
// assertions about what l did to the stubs
```

`loop.Modules` is the struct form of `loop.New`'s module-reference parameter
list. It is the **only** exported struct in the codebase permitted to hold
module references outside `Loop` itself. Its purpose is purely to name the
fields of `loop.New`'s call: `cmd/ralph/main.go` constructs a `Modules`
literal, passes it to `loop.New`, and `loop.New` copies each field onto
`Loop`'s private fields and discards the struct. After `loop.New` returns,
`Modules` is gone. It is the constructor's parameter list, packaged as a
struct; Rule A's carve-out for `loop.New`'s parameters applies to `Modules`
exactly as it would to a positional parameter list.

Production and tests use **the same constructor**. The constructor
parameter list is the seam. There is no separate `loop.NewForTest(...)`,
no `TestStub{}` injection struct, no `loop.SetGit(...)` mutator after
construction. One constructor, used identically by both.

**Implications for Rule A:**

- Loop's New is the **only** function in the codebase whose public API
  contains module types. Every other helper, method, factory, and
  package function either takes data (no modules) or operates entirely
  inside its own package's boundary.
- Tests substitute behavior by constructing module instances (real or
  stub) and passing them through Loop's constructor. They never reach
  inside Loop after construction to swap fields or attach callbacks.
- If you find yourself wanting to add a second module-typed constructor
  (e.g. `loop.NewWithRunner(cfg, ..., runner)`), you're adding a second
  composition point and breaking the rule. The fix is to add the
  parameter to `loop.New` itself.

### Why Rule B exists

Rule B is the universal corollary of Go's encapsulation model: the
exported names of a package are its API, and exported writable struct
fields are an API surface that lets external code mutate state without
going through the type's methods. That's a leak — the type loses
control over its own invariants because anyone with a pointer can
write to its fields.

**Every module in this codebase has unexported fields and exposes
state changes only through methods.** The constructor is the one
exception: it accepts construction-time data and places it on fields
the type itself owns thereafter.

```go
// ❌ Forbidden — exported writable fields let external code mutate
// after construction.
package git
type Repo struct {
    BaseBranch string  // ❌ exported, writable from outside
    CIPollTimeout time.Duration  // ❌
}

// External code:
gm := git.New(...)
gm.BaseBranch = "main"          // ❌ external mutation
gm.CIPollTimeout = 30 * time.Second  // ❌

// ✅ Correct — fields are unexported; values enter through the
// constructor's data struct.
package git
type Config struct {
    BaseBranch    string
    CIPollTimeout time.Duration
    // ... whatever the module needs to know at construction
}

type Repo struct {
    baseBranch    string  // unexported, only git's own methods write
    ciPollTimeout time.Duration
}

func New(cfg Config) *Repo {
    return &Repo{
        baseBranch:    cfg.BaseBranch,
        ciPollTimeout: cfg.CIPollTimeout,
    }
}

// External code:
gm := git.New(git.Config{
    BaseBranch:    cfg.BaseBranch,
    CIPollTimeout: cfg.CIPollTimeout,
})
// No mutation possible — the fields are unexported.
```

**Setter methods are the same antipattern in disguise.** A
`SetBaseBranch(b string)` method is just an exported writable field
with extra ceremony. Don't add setters as a workaround for unexported
fields. If a value needs to be configurable, it's a constructor input
or it's part of the operation that needs it (passed via the method
call's data argument).

**This applies to tests too.** Same-package test files have visibility
into unexported fields, but the rule says modules' state changes go
through their public API methods only. Tests construct stub modules
*before* calling `loop.New(...)` and pass them through the
constructor. They never construct Loop and then poke at its fields
afterward to substitute behavior.

## The shape

```
cmd/ralph/main.go        — parses flags, does shell setup, constructs and runs Loop
  ↓
internal/loop (Loop)     — THE orchestrator. Owns the iteration sequence,
                            iteration state, run state. Has private methods
                            that read its own fields and call modules.
  ↓
internal/git, /verify,   — domain modules. Each owns its own state, retries,
/state, /attempts, etc.    error handling, healing within its domain. A module
                            may internally compose sub-modules for clarity, but
                            those sub-module references never escape the
                            parent module's public API and the orchestrator
                            never sees them.
  ↓
internal/git/<github>    — internal sub-module, unexported, never escapes the
                            parent's API. The canonical example. Loop never
                            sees github types; it sees git types.
```

## The five rules

### 1. `Loop` is the only orchestrator that composes modules from other packages

`Loop` is the orchestrator: its job is to compose modules from multiple
packages and call them in sequence. Modules from other packages enter Loop at
construction time via `loop.New(...)`'s parameter list, the same way
`state.Store` and `git.Repo` enter today, and live as direct private fields
on the Loop struct.

```go
// Allowed — Loop is the orchestrator.
type Loop struct {
    cfg         Config
    git         *git.Repo
    state       *state.Store
    attempts    *attempts.Tracker
    limiter     *ratelimit.Limiter
    agent       *agent.Agent
    taskBackend tasks.Backend
    analyzer    *analyzer.Analyzer
    // ... iteration state and run state below
}

// Forbidden — Verifier is a module, not the orchestrator. It is not a
// composer of other modules from external packages.
type Verifier struct {
    git     git.Ops          // ❌ holds another module from a different package
    state   *state.Store     // ❌
    backend tasks.Backend    // ❌
}

// Forbidden — Deps/Opts/Params struct that bundles modules is the same
// antipattern via a different shape.
type VerifierDeps struct {
    Git   git.Ops             // ❌
    State *state.Store        // ❌
}
```

**Modules may internally compose sub-modules** for their own implementation
clarity, with two constraints:

1. The sub-module reference **never escapes the parent module's public API**.
   The orchestrator never sees the sub-module type and never knows it exists.
2. The sub-module follows the same rules internally: no callback fields, no
   functions taking modules as parameters from outside the parent package, no
   field mutation across the parent/sub-module boundary from outside.

The canonical example is `internal/git` holding an internal `github` client.
`git.Repo`'s public API returns git-package types only; `Loop` calls
`g.Ship(...)` and never imports the github package.

```go
// ✅ Allowed — git internally composes github for clarity.
package git

type Repo struct {
    workDir string
    gh      *github.Client  // internal sub-module, never escapes
    // ...
}

func (r *Repo) Ship(ctx context.Context, opts ShipOpts) (ShipResult, error) {
    // calls r.gh internally; Loop never sees github
}

// ❌ Forbidden — git.Repo exposes github across its public API.
func (r *Repo) GitHub() *github.Client { ... }  // ❌
type ShipOpts struct {
    Reviewer *github.User  // ❌ — github type leaks through the boundary
}
```

The "Loop is special" framing is about the **orchestrator role**, not about
the literal struct name. Loop is the only struct whose *purpose* is to
compose modules from many packages. Other modules can have internal
composition without violating this rule, as long as that composition stays
inside their own package's boundary.

#### Peer modules vs sub-modules

A **peer module** lives in its own top-level package under `internal/`,
gets constructed in `cmd/ralph/main.go` alongside other modules, and is
passed into `loop.Modules` for the orchestrator to call. Peer modules are
peers of `git`, `state`, `tasks`, etc. — they have the same status. The
canonical example is `internal/verifier`: verification has enough cohesive
state and behavior (configs, models, fix-agent submodule, retry-aware
operations) to deserve its own package, but it is not the orchestrator.

A peer module:

- Holds **zero references to other peer modules**. No git, no state, no
  tasks, no other peer fields. The orchestrator owns the cross-module
  composition; peer modules expose stateless operations the orchestrator
  calls in sequence, threading data between them.
- May own **internal sub-modules** (rule 1's existing carve-out). The
  fix-agent runner factory inside `verifier` is a sub-module: it never
  escapes verifier's public API.
- Is constructed in `main.go` exactly like every other module. `loop.New`
  receives it through `loop.Modules`, never reaches into it, and
  references it via a single private field on `Loop`.

A **sub-module**, by contrast, lives *inside* its parent package
(`internal/git/<github>`), is unexported, and never appears in
`loop.Modules` or any public API outside its parent. The github client
inside `git.Repo` is the canonical example.

```go
// ✅ Allowed — verifier is a peer module. main.go constructs it
// alongside the others; loop.Modules carries it in.
package verifier

type Verifier struct {
    cfg       Config              // pure data
    logger    *logging.Logger
    newRunner func() Runner       // sub-module factory, internal
    // NO git, state, tasks, or other peer modules.
}

func New(cfg Config, logger *logging.Logger, newRunner RunnerFactory) *Verifier
```

```go
// ✅ Allowed — main.go composes peer modules and hands them to loop.New.
gm := git.New(...)
st := state.NewStore(...)
backend := tasks.New(...)
vrf := verifier.New(verifier.Config{...}, logger, nil)

loop.New(loop.Config{...}, loop.Modules{
    State:       st,
    Git:         gm,
    TaskBackend: backend,
    Verifier:    vrf,
    Logger:      logger,
})
```

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

// Modules is the struct form of loop.New's parameter list — see the
// "Why loop.New is the only carve-out" section above.
type Modules struct {
    State       *state.Store
    Git         git.Ops
    TaskBackend tasks.Backend
    Logger      *logging.Logger
}

func New(cfg Config, mods Modules) *Loop {
    return &Loop{
        state:       mods.State,
        git:         mods.Git,
        taskBackend: mods.TaskBackend,
        logger:      mods.Logger,
        // ... other fields derived from cfg
    }
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
| `loop` (`Loop` struct) | yes — modules + run state + iteration state | `New(cfg Config, mods Modules) *Loop`, `(l *Loop) Run(ctx) error`, `(l *Loop) SessionTasks() []CompletedTask` | The only struct that holds module references. Has private methods organized around orchestration steps. `loop.Modules` is the struct form of `New`'s module-reference parameter list. |
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
        // ... cfg fields (no module references)
    }, loop.Modules{
        State:       s,
        Git:         g,
        TaskBackend: bk,
        Logger:      logging.New(nil),
        // agent, attempts, limiter, analyzer join Modules as rule-1
        // cleanup lands; they remain private on Loop either way.
    })

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
`loop` named `Loop` (the orchestrator) and `Modules` (the struct form of
`loop.New`'s parameter list — see "Why `loop.New` is the only carve-out"
above). Forbidden: any field whose type resolves to a struct or interface
from another package that has methods. Specifically banned types include
`git.Ops`, `*git.Repo`, `*state.Store`, `*attempts.Tracker`,
`*ratelimit.Limiter`, `*analyzer.Analyzer`, `tasks.Backend`, `*agent.Agent`,
`*Verifier`, `claudeRunner`. Whitelisted: primitives, data structs from this
package, `context.Context`, `claude.SignalPaths` and similar pure data types.

### `TestNoModulesInFunctionParams`
Walks every `FuncDecl` in `go/internal/`. Fails when a parameter type is a
module. Constructors (`New*`) exempt — this is how `loop.New` legitimately
receives a `Modules` value. `Loop` private methods exempt for modules they
hold (because they access via the receiver, not via the parameter — but the
test doesn't need a special case because Loop methods shouldn't take module
parameters at all).

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
