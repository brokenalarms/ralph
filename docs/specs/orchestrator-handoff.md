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

PR #521 (this PR) is in review with 6 commits:
1. `e276420` — neutralize Bead*/Copilot* naming on cross-module surfaces
2. `a1276ed` — **wrong direction**: logger as package import (followed by
   the fix below)
3. `b0312a8` — delete `loop_git.go` helpers that took modules as params
4. `69b21de` — delete `loop_prompt.go` helpers that took modules as params
5. `81e577c` — delete `getTaskDescription`/`getTaskAcceptance` helpers
6. `a458381` — **fix to commit 2**: restore `*logging.Logger` as the
   single named cross-module exception

After PR #521 merges, four arch tests are green. The remaining work
breaks down into focused commits below.

## The corrected rules — read before touching code

These are the rules as they stand after many corrections. Earlier
versions of the spec contained loopholes and ambiguities that agents
exploited. This list is the final form.

### Rule 1 — Only `Loop` holds module references

Modules (`git.Repo`, `state.Store`, `tasks.Backend`, `attempts.Tracker`,
`ratelimit.Limiter`, `agent.Agent`, etc.) are constructed once and held
as **direct private fields** on the `Loop` struct. No other struct in
the codebase holds a module reference. Specifically:

- ✅ `Loop.git`, `Loop.state`, `Loop.taskBackend`, `Loop.attempts`,
  `Loop.limiter`, `Loop.agent` — direct fields, accessed via the
  receiver
- ❌ `Loop.cfg.TaskBackend` — `cfg` is data, not a transport for modules
- ❌ `VerifierDeps.Git`, `VerifierDeps.State`, `VerifierDeps.TaskBackend`
  — `VerifierDeps` is a struct that bundles module references, exactly
  the antipattern
- ❌ Any `*Params`/`*Opts`/`*Deps` struct holding a module reference

Modules enter Loop **at construction time** via `loop.New(...)`'s
parameter list, the same way `state.Store` and `git.Repo` already enter
today. The constructor parameter is the one allowed entry point.
Modules are then placed on Loop's direct fields and accessed only via
the receiver.

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

Tests that need to inject stub modules use a clearly-named
**unexported** test seam in the loop package. Production code calls
`loop.New(cfg, st, gm, taskBackend, logger)`. Tests call something like
`loop.newWithStubs(cfg, stubs)` where `stubs` is a struct of optional
module references. The seam is unexported so production cannot reach it.
The arch test allows the seam by name.

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

### Commit A — Move `Loop.cfg.TaskBackend` to `Loop.taskBackend`

**Rule 1, Rule 4.** This is the next commit. Status: I attempted it,
got 80% done, reverted because the inline `TaskBackend: &testutil.StubBackend{...}`
patterns in test files needed manual extraction and I ran out of
context. The work is mechanical:

1. Add `taskBackend tasks.Backend` field to `Loop` struct (next to
   `state`, `git`).
2. Add `taskBackend tasks.Backend` parameter to `loop.New(cfg, st, gm,
   taskBackend, logger)`.
3. `loop.New` stores it: `l.taskBackend = taskBackend`.
4. Delete `TaskBackend` field from `Config` struct.
5. Sed-replace every `l.cfg.TaskBackend` with `l.taskBackend` in
   `internal/loop/*.go` (non-test files). Done by:
   `find internal/loop -type f -name '*.go' ! -name '*_test.go' -exec sed -i '' 's|l\.cfg\.TaskBackend|l.taskBackend|g' {} \;`
6. `cmd/ralph/main.go`: remove `TaskBackend: backend` from the
   `loop.Config{...}` literal; change `loop.New(..., st, gm, log)` to
   `loop.New(..., st, gm, backend, log)`.
7. **Test files (the painful part)**: every test that constructs a Loop
   has `TaskBackend: ...` in its `Config{...}` literal and `}, st, gm,
   logging.New(nil))` (or similar) at the call site. The fix:
   - Where `TaskBackend: backend,` appears with a `backend := ...`
     declaration above, sed handles it: drop the field, add `backend` to
     the New call.
   - Where `TaskBackend: tt.backend,` appears (table-driven tests), the
     value is `tt.backend` — needs per-file Edit.
   - Where `TaskBackend: &testutil.StubBackend{...},` appears inline,
     extract the literal to a `backend := &testutil.StubBackend{...}`
     declaration before the Config literal, then drop the field from
     Config and add `backend` to the New call.
   - Where `TaskBackend: nil,` appears (one case in `loop_test.go`),
     pass `nil` as the backend arg to New.
   - Where `Config{TaskBackend: backend}` appears as a single-line
     literal (in `loop_task_polling_test.go`), needs careful Edit.
8. After all test fixes, build green and tests pass (1278 expected,
   only `TestNoModulesInNonLoopStructs` failing on the 3 remaining
   `VerifierDeps.*` violations).

There are about 25–30 sites across 7–8 test files. Mostly mechanical.
The traps are the inline literal cases — those need manual extraction.

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

**Rule 7.** The user's original concern in this PR thread.

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
