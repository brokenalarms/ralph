# Orchestrator/module-boundary refactor — handoff notes

This document is the binding direction for the next agent picking up the
orchestrator/module-boundary refactor (PRs #520, #521, and the work that
follows). It exists because the refactor has been started, drifted,
corrected, drifted, corrected, repeatedly. Every drift came from the
agent reading existing code patterns and unconsciously preserving them
instead of holding the architectural rules in mind. **This document is
the source of truth. Read it before touching any code.**

The authoritative spec is `docs/specs/orchestrator-modules.md`. This
document is the field guide that explains what to do, what NOT to do,
and the specific traps that have caught every prior agent.

## Where things stand

PR #520 merged: spec doc + 5 strict arch tests, 4 of which currently
pass. The fifth (`TestNoModulesInNonLoopStructs`) is intentionally red
and will be turned green by the work below.

PR #521 merged: neutralized Bead*/Copilot* naming, deleted helpers that
took modules as params, restored `*logging.Logger` as the single named
cross-module exception. Four arch tests green after #521.

Commit A (this PR) lands: `loop.cfg.TaskBackend` moved to
`Loop.taskBackend`, `loop.Modules` struct introduced as the struct form
of `loop.New`'s module-reference parameter list, `newTestModules(t, ...)`
helper in place and used by every loop test. `TestNoModulesInNonLoopStructs`
drops from 4 violations to 3 (only the three `VerifierDeps.*` fields
remain — Commit B's scope). Remaining work breaks down into focused
commits below.

## The corrected rules — read before touching code

These are the rules as they stand after many corrections. Earlier
versions of the spec contained loopholes and ambiguities that agents
exploited. This list is the final form.

### Rule 0 — The two load-bearing rules that subsume everything else

These two rules together define the architecture. Every other rule in
this document is a corollary of one or both.

**Rule A (boundary):** Module types from package P never appear in the
public API surface of any other package, anywhere, in any form —
except as parameters to the orchestrator's constructor (`loop.New`),
which is the named composition point where modules from many packages
meet.

**Rule B (immutability):** A module's state changes only through its
own public API methods. Construction is the one allowed entry point;
after that, no external code — not other packages, not tests, not the
orchestrator — mutates the module's fields directly. The public API
methods are the only path in.

#### Rule A explained

A module's public API surface is its exported names: function/method
signatures (parameters and return values), exported struct field
types, exported interface methods, and exported package-level
variables. Inside its own package, a module is free to compose,
mutate, and hold whatever internal state it needs — but nothing
module-shaped escapes through the package's exported boundary, except
through the one designated DI seam.

**Why `loop.New` is the carve-out**: Loop's role IS to compose
modules from many packages. The composition has to happen somewhere.
Construction-time DI via `loop.New`'s parameter list is the explicit,
single, named point where production wires real implementations and
tests wire stubs. **The constructor parameter list is the test
injection seam.** Production calls
`loop.New(cfg, loop.Modules{State: st, Git: gm, TaskBackend: backend, Logger: log})`
with real implementations; tests call the same `loop.New(cfg, mods)`
with stub modules via the `newTestModules(t, ...)` helper. One
constructor, used identically by both. There is no separate "for
tests" injection mechanism (no `NewForTest`, no `TestStub{}`, no
`SetX` mutators).

`loop.Modules` is the struct form of `loop.New`'s parameter list — it
is the only exported struct outside `Loop` itself that holds module
references, because it *is* the constructor's parameter list, just
packaged as a named-field struct rather than positional args.
`loop.New` copies each field onto `Loop`'s private fields and discards
the `Modules` value. The arch test whitelists `Modules` by name for
this reason.

#### Rule B explained

Every module's fields are **unexported**. The constructor (`X.New(cfg)`
or `X.New(args...)`) is the only entry point for placing values onto a
module. After construction, the module's public methods are the only
way to change its state.

This forbids:
- External field mutation: `gm.BaseBranch = "main"` (exported field
  written from outside) — the field must be unexported
- Setter methods: `gm.SetBaseBranch("main")` — same antipattern with
  ceremony; don't add setters as a workaround for unexported fields
- Test field substitution: `l.git = stubGit` after `loop.New(...)` —
  even with same-package visibility, modules enter via the
  constructor, not via field mutation

If a value is configurable, it's either a constructor input (set
once, never mutated) or part of the operation that needs it (passed
via the method's data argument).

The current `cmd/ralph/main.go` violates Rule B for git.Repo:

```go
gm := git.New(cfg.ProjectDir, ralphDir, nil)
gm.BaseBranch = cfg.BaseBranch                                 // ❌
gm.Logger = log                                                // ❌
gm.CIPollTimeout = cfg.CIPollTimeout                           // ❌
gm.CopilotGatedTimeout = cfg.ReviewerGatedTimeout              // ❌
// etc.
```

The fix (Commit C below) is `git.New(git.Config{...})` taking all
those values at construction; the fields on git.Repo become unexported.

#### What to ask when in doubt

If you find yourself debating whether some pattern is allowed, ask
both questions:

1. **Rule A**: Does it put a module type into a public API surface
   other than `loop.New`'s constructor parameters? If yes → forbidden.
2. **Rule B**: Does it modify a module's state from outside the
   module's own methods (whether via field mutation, setter methods,
   or any other mechanism)? If yes → forbidden.

If both answers are "no" (the pattern is purely internal to a
package, or it goes through `loop.New`, or it goes through a
module's own public methods), it's fine.

Every "trap" in the trap list below was a case where I violated one
of these two rules and then rationalized why it was OK.

The only named cross-cutting exception is `*logging.Logger` (rule 5),
which is allowed to appear in module constructors (not just
`loop.New`) as the one stateful utility threaded through the program.
Logger is still subject to Rule B (you don't mutate `*logging.Logger`
fields after construction; you call its methods).

### Rule 1 — `Loop` is the only orchestrator that composes modules from other packages

`Loop` is the orchestrator: its job is to compose modules from multiple
packages (`git`, `state`, `tasks`, `attempts`, `ratelimit`, `agent`) and
call them in sequence. Modules from other packages enter Loop **at
construction time** via `loop.New(...)`'s parameter list and live as
**direct private fields** on the Loop struct.

- ✅ `Loop.git`, `Loop.state`, `Loop.taskBackend`, `Loop.attempts`,
  `Loop.limiter`, `Loop.agent` — direct fields, accessed via the receiver
- ❌ `Loop.cfg.TaskBackend` — `cfg` is data, not a transport for modules
- ❌ `VerifierDeps.Git`, `VerifierDeps.State`, `VerifierDeps.TaskBackend`
  — `VerifierDeps` is a struct that bundles module references, exactly
  the antipattern
- ❌ Any `*Params`/`*Opts`/`*Deps` struct holding a module reference

**Modules may internally compose sub-modules.** A module is allowed to
split its implementation into smaller pieces for clarity. The
constraints on those internal sub-modules:

1. The sub-module reference **never escapes the parent module's public
   API**. The orchestrator never sees the sub-module type and never
   knows it exists.
2. The sub-module follows the same rules internally: no callback fields,
   no functions taking modules as parameters from outside the parent
   package, no field mutation across the parent/sub-module boundary
   from outside the parent's package.

The canonical example: `git.Repo` holds an internal `github` client.
`git.Repo`'s public API returns git-package types (like
`git.ReviewComment`) and never returns or accepts a github type.
`Loop` calls `g.Ship(...)` and never imports the github package, never
references a github type, never knows github exists.

```go
// ✅ Allowed — git internally composes github for clarity.
package git

type Repo struct {
    workDir string
    gh      *github.Client  // internal sub-module, never escapes
    // ...
}

func (r *Repo) Ship(ctx context.Context, opts ShipOpts) (ShipResult, error) {
    // calls r.gh internally
}

// ❌ Forbidden — git.Repo exposes github through its API.
func (r *Repo) GitHub() *github.Client { ... }  // ❌
type ShipOpts struct {
    Reviewer *github.User  // ❌ — github type leaks across the boundary
}
```

The "Loop is special" framing is about the **orchestrator role**, not
about the literal struct name. Loop is the only struct whose *purpose*
is to compose modules from many packages. Other modules can have
internal composition without violating this rule, as long as that
composition stays inside their own package's boundary.

### Rule 2 — No function or method takes a module as a parameter

Outside the constructor, no function or method anywhere in the codebase
takes a module type as a parameter. Helpers that previously took
`tasks.Backend`, `*state.Store`, etc. as arguments are **deleted**, not
refactored. Their bodies move into private methods on Loop that read
`l.git`/`l.state`/`l.taskBackend` etc. via the receiver.

This is mechanical, not stylistic. The category "free function with a
module parameter" does not exist after the refactor. The arch test
`TestNoModulesInFunctionParams` is currently green and stays that way.

### Rule 3 — Loop methods take only `ctx` and per-call data

Methods on `Loop` take parameters only for `context.Context` and for
per-call data that just arrived from a sibling call (e.g., a verify
failure detail returned from `verify.RunTests`). Long-lived state —
current task, head-before, work-dir, retry counters — lives as private
fields on `Loop` and is read directly via the receiver. Threading state
through method parameters is the same antipattern as threading modules
through helper signatures.

```go
// ❌ Wrong — iteration state threaded through the parameter list
func (l *Loop) postSignalVerification(
    ctx context.Context,
    taskID, nextTask, headBefore, workDir, rawLogPath string,
) bool { ... }

// ✅ Right — iteration state lives on l.iter, set when the iteration began
func (l *Loop) postSignalVerification(ctx context.Context) bool {
    // reads l.iter.task, l.iter.headBefore, l.iter.workDir, etc.
}
```

### Rule 4 — `Config` is pure data, never holds modules or callbacks

`loop.Config` is the construction-time data input to `loop.New(cfg, ...)`.
It holds **only** primitives, durations, paths, slices/maps of
primitives, and small data structs from this package. It is **never
mutated** after `loop.New` returns. Anything that's not pure data
belongs elsewhere:

- Module references → Loop's direct fields, passed as constructor args
- Callbacks (`OnVerify`, `IsOnline`, `WaitForInternet`, `NewRunner`,
  `CheckGitHub`, `QueryFn`, `LLMVerify`, `OnIterationStart`,
  `OnPostTask`, `OnRebaseConflict`) → these are test seams that should
  be deleted entirely (the production code paths use real
  implementations, tests inject through a different mechanism — see
  Rule 8)

The rule "cfg shouldn't need values mutated on it or set after init"
applies absolutely. If a config value informs the construction of a
mutable property on Loop, the mutable property is a private field on
Loop, set in `loop.New` from the cfg value.

### Rule 5 — `*logging.Logger` is the single named cross-module exception

Logging is genuinely cross-cutting and package-level state would leak
across parallel tests. So `*logging.Logger` is allowed as a struct
field, allowed as a constructor parameter, and threaded through
`loop.New` and module constructors that need it. **This is the only such
exception in the codebase.** No other module type gets this treatment.

The arch tests `TestNoLoggerAsField` and `TestNoLoggerInFunctionParams`
have been deleted (replaced with a comment block in
`orchestrator_arch_test.go`) because the rule they enforced was wrong.
If a future agent re-introduces a "no logger threading" rule, that's a
spec change that needs explicit user approval.

### Rule 6 — Cross-module data structs use neutral names

Field names, type names, and helper names that cross a module boundary
use neutral names that describe what the data is, not where it came
from. The verifier doesn't know whether the task came from beads,
files, or jira; the config package shouldn't name reviewer-specific
timeouts. Examples already corrected in PR #521:

- `VerifyOpts.BeadTitle` → `Title`
- `prompt.Vars.BeadsContext` → `TasksContext`
- `config.CopilotReviewTimeout` → `ReviewerGatedTimeout`

The arch test `TestNoImplementationLeakInExportedNames` enforces this
by walking exported names for `Bead*`, `Github*`, `Copilot*` prefixes
outside the modules that own those implementations.

### Rule 7 — No field mutation across module boundaries

Code outside a module's defining package may not write to that module's
struct fields. The orchestrator (or `cmd/ralph/main.go`) constructs a
module via its `New` function and accesses it only through public
methods afterward. **Field mutation after construction is forbidden.**

The current `cmd/ralph/main.go` violates this:

```go
gm := git.New(cfg.ProjectDir, ralphDir, nil)
gm.BaseBranch = cfg.BaseBranch                                 // ❌
gm.Logger = log                                                // ❌
gm.CIPollTimeout = cfg.CIPollTimeout                           // ❌
gm.CopilotGatedTimeout = cfg.ReviewerGatedTimeout              // ❌
gm.CopilotOpportunisticTimeout = cfg.ReviewerOpportunisticTimeout // ❌
gm.CodeRabbitTimeout = cfg.CodeRabbitReviewTimeout             // ❌
```

The fix is `git.New(git.Config{...})` taking everything currently
mutated as construction-time data. The fields on `git.Repo` become
unexported. See "C8" below.

### Rule 8 — Test injection without violating the rules

Tests inject stub modules through **the same public constructor
production uses**. Production code calls
`loop.New(cfg, loop.Modules{State: st, Git: gm, TaskBackend: backend, Logger: log})`.
Tests call `loop.New(cfg, newTestModules(t, st, gm, backend))` — the
`newTestModules` helper lives in `loop_test.go`, takes explicit stub
values for the modules the test cares about, and fills in a no-op
`*logging.Logger` by default. The seam is the public constructor; the
helper is a test-only convenience for shaping the `Modules` value.
There is no separate unexported constructor.

The user has been clear that this is the right pattern. **Do not**
revert to:

- Mutating fields after construction in tests
- Adding `func() T` callback factories on Config for runner/queryFn etc.
- Using package-level mutable state in any module

## Traps the next agent must not fall into

These are the specific patterns I (the previous agent) kept proposing,
which the user rejected. If you find yourself reaching for any of these,
**stop and re-read the corrected rules above**.

### Trap 1 — "Helper that takes modules as params is fine if everything is explicit"

❌ Wrong. Even with all parameters spelled out by name, a free function
in the loop package taking `tasks.Backend` or `*state.Store` is the
forbidden category. The helper gets **deleted**, its body moves into a
private Loop method that reads via the receiver.

Why I proposed this: it preserved existing code patterns and avoided
voluminous test rewrites. The user's response: "MODULES AS PARAMS IS
THE EXACT ANTIPATTERN WE'RE TRYING TO FIX."

### Trap 2 — "Logger as package-level state with `SetDefault`"

❌ Wrong. Package-level state leaks across parallel tests. The logger
is a real struct with state (`out`, `logFile`, `streaming`), and the
right Go idiom for "one logger configured at startup, threaded through
the app" is constructor-time DI of the `*logging.Logger`. This is the
single named cross-module exception.

Why I proposed this: standard library packages like `log`, `slog`, `fmt`
use package-level state, so it felt idiomatic. The user's response:
"package-level state, this is ripe for state leaking."

### Trap 3 — "Config-with-callbacks is ergonomic for tests"

❌ Wrong. Callbacks on Config (`OnVerify`, `WaitForInternet`,
`NewRunner`, `IsOnline`, etc.) are exactly the antipattern the rule
forbids: they let tests hand a function back to the loop that the loop
calls, which is a hidden way for the test to inject behavior that the
production loop can't get any other way. They also make Config impure
(the type contains func types). All callback fields on Config get
deleted; tests inject through the dedicated `loop.newWithStubs(...)`
seam.

Why I proposed leaving them: they're useful test seams. The user's
response: callbacks are forbidden, period.

### Trap 4 — "Wrap the verifier in a thin Loop method that does multi-step git work"

❌ Wrong. Methods on Loop exist for orchestration steps that genuinely
mediate between modules. A method like `(l *Loop) tryFixCI(...)` that
calls `l.git.GetCIFailureLog`, `l.verifier.TryFixCI`, `l.git.CommitAll`,
`l.git.HeadRev`, `l.git.Push` is module-internal logic dressed as
orchestrator logic — the git operations belong **inside the git
module's intent-level methods**, not as a sequence the orchestrator
choreographs.

Why I proposed this: it kept the call site simple. The user's response:
"if Git's concern is CI then it encapsulates the retry for free."

### Trap 5 — "Loop.cfg.TaskBackend is fine because cfg is the construction input"

❌ Wrong. `cfg` is **data** passed at construction. It holds primitives
and durations. `tasks.Backend` is a module reference. The fact that
`cfg` is the entry point doesn't make it OK to put modules on it.
Modules go on `Loop` directly via `Loop.taskBackend`, set in
`loop.New(...)` from a separate constructor parameter (sibling of
`state` and `git`).

Why I proposed this: cmd/ralph already has a Config struct with many
fields, adding TaskBackend to it was the smallest diff. The user's
response: "cfg is a struct for data passed in at init."

### Trap 6 — "Each module imports its own config from the config package"

❌ Wrong direction. The "modules pull their own config" alternative
creates implicit coupling: every module imports the config package and
reaches into it. The cleaner pattern is the orchestrator-distributes
pattern: Loop reads its own settings and constructs each module with
**module-scoped** input data (`git.Config{WorkDir, RalphDir, ...}`,
`verifier.Config{Model, Timeout, ...}`), passing only the fields that
module directly uses. This is the meaning of "each module receives only
what it needs."

### Trap 7 — Inventing "intermediate states" that violate the rules temporarily

❌ Wrong. When a refactor is hard to land in one commit, the temptation
is to add "for now" wrappers that violate the rules and intend to
remove them later. **Don't.** The wrappers always survive. If the
correct end state is hard to reach, split into multiple commits where
each commit lands a smaller correct piece, not a temporary
violation-laden bridge.

## Remaining work, in order

Each item is a focused commit. Each turns the build greener (or holds
green) and reduces violations of the rules above. **Read the rule
before doing each commit.** Do not propose alternatives that violate
the rules even if they seem easier.

### Commit A — Move `Loop.cfg.TaskBackend` to `Loop.taskBackend` ✅ DONE

**Rule 1, Rule 4.** Landed. What it did:

1. `loop.Modules` struct introduced with `State`, `Git`, `TaskBackend`,
   `Logger` fields. Comment explains it is the struct form of
   `loop.New`'s parameter list and is the only exported non-`Loop`
   struct permitted to hold module references.
2. `loop.New` signature changed from
   `New(cfg Config, st *state.Store, gm git.Ops, logger *logging.Logger) *Loop`
   to `New(cfg Config, mods Modules) *Loop`. All four fields copied
   onto the corresponding private fields on `Loop`.
3. `Loop.taskBackend` added alongside `state`, `git`.
4. `TaskBackend` field deleted from `Config` struct. Every
   `l.cfg.TaskBackend` rewritten to `l.taskBackend` across
   `internal/loop/*.go`.
5. `cmd/ralph/main.go` constructs a `loop.Modules{State, Git,
   TaskBackend, Logger}` literal and passes it to `loop.New`.
6. `newTestModules(t *testing.T, st, gm, backend, loggerOpt...)` helper
   added in `loop_test.go`. ~185 test call sites migrated from
   `Modules{State: ..., Git: ..., TaskBackend: ..., Logger: ...}`
   literals to `newTestModules(t, ...)` calls.
7. `orchestrator_arch_test.go` whitelists `Modules` by name alongside
   `Loop` (only permitted struct outside the orchestrator to hold
   module references, because it *is* the constructor's parameter
   list).
8. `TestNoModulesInNonLoopStructs` dropped from 4 violations to 3. The
   remaining three (`VerifierDeps.Git`, `VerifierDeps.State`,
   `VerifierDeps.TaskBackend`) are Commit B's scope. Everything else
   green.

Spec doc (`docs/specs/orchestrator-modules.md`) updated in the same PR
to describe `Modules`, the arch-test whitelist, and the `loop.New`
signature. Handoff doc (this file) updated to mark Commit A done.

### Commit B — Strip the `Verifier` struct entirely

**Rule 1.** This is the largest commit in the refactor (~1500 lines
including test rewrites). The user's spec calls for it.

1. Add `iterationState` private struct on `Loop` with the verifier
   counters (`testFixAttempts`, `llmVerifyAttempts`) and the per-iteration
   data the verifier currently reads from `signalParams` (`headBefore`,
   `workDir`, `rawLogPath`, `taskID`, `nextTask`).
2. Add `iter iterationState` field on `Loop`. Add a `(l *Loop) beginIteration(...)`
   method that resets it.
3. Move all 18 `*Verifier` methods to private Loop methods. Each method:
   - Takes `(ctx context.Context)` only (plus per-call data passed in by
     sibling calls)
   - Reads from `l.iter.*` instead of `signalParams`
   - Calls `l.git.X()`, `l.taskBackend.X()`, `l.state.X()`, `l.runner.X()`
     directly via the receiver
   - Uses `l.logger.Emit(...)` directly
4. The five callback factories on `VerifierDeps` get unwound:
   - `Runner func() claudeRunner` → `l.runner` directly
   - `NewRunner func() claudeRunner` → calls a Loop helper that creates a
     fresh runner via `agent.New(l.logger)`
   - `QueryFn` → reads from a Loop field set at construction
   - `LLMVerify` → reads from a Loop field set at construction (test seam)
   - `SkipTask` → calls `l.skipTask(...)` directly
5. Delete `signalParams`, `Verifier`, `VerifierConfig`, `VerifierDeps`,
   `NewVerifier`. Delete `internal/loop/verifier.go` (the file).
6. Update call sites in `loop.go`, `loop_iteration.go`, `loop_verify.go`
   to call the new Loop methods (`l.onSignal(ctx)`, `l.verifyCompletion(ctx)`,
   `l.runPreIterationTests(ctx)`, etc.).
7. Rewrite `verifier_test.go` (29 tests) to construct a `Loop` instead of
   a `Verifier`. Same for `loop_signal_test.go` (11 tests),
   `loop_verify_test.go` (parts of it). Each test that previously did
   `v := newTestVerifier(t, ...)` becomes `l := newTestLoop(t, ...)` and
   the test exercises `l.onSignal(...)` instead of `v.OnSignal(...)`.
8. Build + test pass. `TestNoModulesInNonLoopStructs` drops to zero
   violations and turns green.

**This commit is genuinely large.** Don't try to land it in pieces — the
intermediate states won't compile because removing `Verifier` while it
still has callers requires moving everything in lockstep. Schedule it as
its own multi-hour focused commit.

### Commit C — `git.New(git.Config{...})` and stop field mutation in cmd/ralph

**Rule B.** The user's original concern in this PR thread.

1. Add `git.Config` struct in the git package containing every value
   `cmd/ralph/main.go` currently mutates: `WorkDir`, `RalphDir`, `BaseBranch`,
   `CIPollTimeout`, `CopilotGatedTimeout`, `CopilotOpportunisticTimeout`,
   `CodeRabbitTimeout`, plus `Logger` and `GitHub` for construction.
2. Change `git.New(workDir, ralphDir, gh)` to `git.New(cfg git.Config)`.
3. Make the corresponding fields on `git.Repo` unexported (or keep them
   exported but unwritable from outside the package — Go doesn't have
   package-private mutability, so making them lowercase is the only
   enforcement).
4. `cmd/ralph/main.go`: replace the seven `gm.X = cfg.Y` lines with a
   single `gm := git.New(git.Config{WorkDir: cfg.ProjectDir, RalphDir:
   ralphDir, BaseBranch: cfg.BaseBranch, ...})` call.
5. Add `TestNoCrossModuleFieldAssignments` arch test that walks every
   `*ast.AssignStmt` in `cmd/` and `internal/` (excluding
   `internal/git/` itself) and fails when LHS is a field selector on a
   value whose type comes from a foreign module package. Test stub
   types (`*git.StubRepo`) excluded by name explicitly in the
   whitelist, with a comment.

### Commit C2 — Move PR body construction into git/github

**Rule A** + the principle that modules own their domain's protocol
formatting. The current `(l *Loop) prBody(taskID, summary string) string`
in `loop_git.go` reads task description + acceptance from the backend,
formats them as PR markdown, and passes the string into
`git.Ship(ctx, ShipOpts{Body: ...})`. The orchestrator is doing
GitHub-specific markdown formatting that belongs inside git/github.

The corrected shape:

1. Add `Description`, `Acceptance`, `Summary` fields to `git.ShipOpts`
   (pure data — strings).
2. Remove `Body` field from `git.ShipOpts`.
3. `git.Repo.Ship` constructs the body internally by handing the data
   to the github sub-module: `body := r.gh.formatPRBody(opts.TaskTitle,
   opts.Description, opts.Acceptance, opts.Summary)`.
4. The github sub-module owns the markdown format. Looks roughly like
   the current `(l *Loop) prBody` body, but inside git/github.
5. In Loop's `doShip`, pre-fetch the task data from the backend and
   pass it as ShipOpts fields:
   ```go
   var desc, acceptance string
   if taskID != "" {
       desc, _ = l.taskBackend.GetDescription(taskID)
       acceptance, _ = l.taskBackend.GetAcceptance(taskID)
   }
   result, err := l.git.Ship(ctx, git.ShipOpts{
       TaskID:      taskID,
       TaskTitle:   title,
       Description: desc,
       Acceptance:  acceptance,
       Summary:     summary,
       AutoMerge:   ...,
       Reviewers:   ...,
   })
   ```
6. Delete `(l *Loop) prBody` from `loop_git.go`.
7. The `TestLoopPRBody_*` tests in `loop_finalize_test.go` move into
   the git package as github sub-module unit tests, or get deleted as
   redundant with end-to-end Ship tests.

This is the same pattern as "git owns CI retry/backoff/healing
internally and only escalates with structured stuck-state data" — the
PR body's markdown format is git/github's protocol concern, and the
orchestrator just hands over the underlying task data. The orchestrator
never knows what `## Description` or `## Summary` means; it just
populates the data fields.

The Rule A check: after this commit, `git.ShipOpts` is pure data
(`string` and `bool` and `[]Reviewer` fields, where `Reviewer` is a
git-package data type). No module references. The orchestrator pre-
fetches the data and hands it across the boundary as data.

### Commit D — Other modules get module-scoped Config structs

**Rule 4 generalized.** Audit `state`, `attempts`, `ratelimit`, `agent`,
`tasks`, `analyzer`. For any module whose current `New(...)` signature
has 4+ parameters, mutates fields after construction, or takes a slice
of unrelated arguments, introduce a module-scoped `Config` struct
containing only the fields that module directly uses. The orchestrator
(Loop) populates each module's Config from its own settings at
`loop.New` time.

If a module's current signature is already clean (e.g., `state.NewStore(ralphDir)`
with one arg, no mutation), leave it alone — Config structs for stylistic
uniformity aren't needed.

### Commit E — Ban callbacks across module boundaries

**Rule 4.** Add `TestNoCallbacksAcrossModuleBoundaries` arch test that
walks every struct field and every function parameter in `internal/`,
fails on any `func(...)` type that:

1. Is a field on a struct (any struct, not just `*Params`/`*Opts`)
2. Is a parameter to any function or method

Exceptions: `context.CancelFunc`, the test seam from Rule 8 (whitelisted
by name). The test fails on the current code with violations on
`Config.OnVerify`, `Config.OnIterationStart`, `Config.OnPostTask`,
`Config.IsOnline`, `Config.WaitForInternet`, `Config.NewRunner`,
`Config.CheckGitHub`, `Config.QueryFn`, `Config.LLMVerify`,
`Config.OnRebaseConflict`. Each gets deleted, replaced with a real
implementation in production code, and the test seam (Rule 8) handles
test injection.

### Commit F — Final pass

Run `go test ./...` end-to-end. Confirm every arch test green. Push.
Open follow-up PR if needed.

## Specific things the next agent must DO and must NOT DO

### DO

- **Read this document and `docs/specs/orchestrator-modules.md` before
  touching code.** Especially the "Traps" section above.
- **Keep PRs focused.** One commit per rule when possible. Bundling
  unrelated changes makes review harder and creates merge conflicts.
- **Run the strict arch tests after every commit.** They're the
  ground truth.
- **Push to PR #521 if it's still open**, otherwise create a new branch
  off of `main` after #521 merges and pick up at "Commit A" above.
- **When something is hard, do the harder thing.** Don't add wrappers,
  test seams, or "for now" workarounds.
- **Treat the user's corrections as load-bearing.** They've been right
  about every architectural call so far. If you're tempted to push back
  on a correction, you're probably in one of the traps.

### DO NOT

- **Do not** propose package-level state for any module (rule 5).
- **Do not** propose Config callbacks or func-type fields (rule 4).
- **Do not** propose helpers that take modules as parameters (rule 2).
- **Do not** propose mutating module fields after construction (rule 7).
- **Do not** propose `Loop.cfg.TaskBackend` or any equivalent (rule 1
  + rule 4).
- **Do not** mark a commit complete until the build is green and the
  arch tests are at the expected state.
- **Do not** revert the user's corrections in the spec doc.

## Reference

- **Authoritative spec**: `docs/specs/orchestrator-modules.md`
- **Original module-boundary spec**: `docs/specs/module-boundary-beads.md`
  (the loophole sentence has been removed; defers to orchestrator-modules.md)
- **Strict arch tests**: `go/internal/loop/orchestrator_arch_test.go`
- **PR #520**: spec + arch tests merged
- **PR #521**: this PR (rules 2, 5, 6 + naming + helper deletion +
  logger restoration)
- **The conversation that produced this document**: extensive
  back-and-forth in claude-code session, with the user repeatedly
  correcting drift. The agent (claude-opus-4-6) made the same kinds of
  mistakes multiple times across different sessions. Read the "Traps"
  section as a checklist.
