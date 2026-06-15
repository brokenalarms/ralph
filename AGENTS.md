# Architecture
- Read `docs/specs/architecture.md` before any refactoring or architectural work. It describes the target state all changes should move toward.

# Git
- Ralph requires a git repository.
- Agents commit locally on the branch the orchestrator provides. The orchestrator owns all remote operations: pushing, branch creation, PR creation, and merging.
- The orchestrator handles rebase, conflict resolution, and merge programmatically. Agents do not need to manually rebase — the git module ensures the worktree is up to date before any outbound operation.

# Signal files vs state
- **Signal files** (.signal_complete, .signal_current_task, feedback, stop): agent↔orchestrator communication and user commands (`ralph stop`, `ralph feedback`).
- **state.json**: orchestrator-internal state that persists across iterations (iteration count, last task, test results). If it's not agent↔orchestrator communication, it goes in state.json.

# Build
- `go/cmd/ralph/prompts/` is the source of truth for prompt templates, embedded into the binary via `//go:embed`. Edit them directly — no copy step needed.

# Prompts
- Instructions and text for the agent belong in `.md` template files under `go/cmd/ralph/prompts/`, not hardcoded in Go. Go code assembles and interpolates templates but should not contain instructional prose.
- Bead creation guidance (how to create well-formed beads, quality guidelines, anti-patterns) lives in `go/cmd/ralph/prompts/bead-creation.md`. Both the task manager and review agent prompts include this shared section. When adding or updating bead creation rules, edit that file — not the individual prompt files.

# Beads / bd
- We use `bd` as the sole task backend for dependency management and issue tracking.
- Never hardcode bd commands in prompts. All bd knowledge comes from `bd prime` — the agent learns the workflow at runtime. Prompts refer to tasks generically and let `bd prime` teach the specifics.
- **Hard invariant**: `.beads` is the project's permanent task history and must never be deleted, cleared, or force-reinitialized. Only `.ralph` state is ephemeral.

# Testing
- Tests lock in features and prevent regressions.
- Each test should explain in a comment why it exists and what user functionality it proves — not just assert correctness mechanically.
- Do not write tests that assert specific strings from prompt templates. Prompts are natural-language guidance — test behavior, not prose.

## Test synchronization (no fixed sleeps)
- Tests must not use a fixed `time.Sleep` (or `time.After`) as a synchronization proxy — a magic delay chosen to be "long enough" for an asynchronous event. It is flaky under load (the event may not have happened yet) and silently wasteful when over-long.
- Synchronize on the observable condition instead, via the `testutil.WaitFor*` helpers: `WaitFor` (arbitrary predicate), `WaitForFile`, `WaitForSignalFile`, `WaitProcessGone`. They poll the condition and return the instant it holds; the timeout is a bounded upper limit that guards against a hang, not a delay that must be tuned.
- To assert a *non-event* (something does NOT happen), drive the system to an observable state with `WaitFor`, then assert — do not sleep and hope.
- A `forbidigo` lint rule forbids `time.Sleep` in `*_test.go`; the `testutil` wait helpers are the only exception.

## Stubs and test doubles
- A test stub implements exactly the production interface — the same methods, nothing else. It is indistinguishable in shape from the real implementation.
- Stub configuration happens at construction via a `StubXConfig` struct passed to `NewStubX(cfg)`. The constructor is the only configuration seam.
- Every field on a stub type is unexported. Tests never read or write stub state directly after construction. Tests never call methods on a stub that are not part of the production interface.
- Multi-call behavior (pagination, polling, retries) is expressed as sequenced responses on the config: `cfg.ListChecksResponses = [][]CICheckResult{...}`. The stub advances an internal index on each call and plays them back. For tests needing per-call errors, the config carries a parallel `[]error` slice.
- Callback-valued fields on stubs (`ListChecksFunc func(...)`, `CreatePRFunc func(...)`) are forbidden. They smuggle test logic into the stub through a side channel that production code does not have. The subject under test must only see the production interface.
- Partial-stub hybrids — types that embed a stub and override a subset of its methods — are forbidden. The pattern confuses which layer is under test and, in Go, silently breaks when the embedded stub's methods call each other (static dispatch keeps those calls inside the embedded type). Build one stub that fully implements the interface via plain state.
- Stubs live at external boundaries (network, subprocess, filesystem the test does not want to touch), not at module boundaries. When testing module A, use real A with stubs at A's external dependencies. When testing a consumer of A, use a fully-constructed stub A. Never build a half-real, half-stub instance of the same module.

## Go test package naming
Go test files declare either the **internal** package (`package foo`) or the **external** package (`package foo_test`).

- **Internal** (`package foo`): can access unexported symbols. Use this when the test needs unexported functions, structs, or fields. Most tests in this project use internal naming.
- **External** (`package foo_test`): can only use the public API. Use this when the test should verify exported behavior without relying on internals, or when importing test helpers from a package that also imports `foo` (internal naming would create a circular import).

When consolidating test helpers:
- A helper in `test_helpers_test.go` with `package foo` is only visible to other `_test.go` files that also declare `package foo`. It cannot be imported by test files in other packages.
- If multiple packages need the same test double, promote it to an exported helper in a dedicated `testing.go` file (non-test, exported API) or a `testutil` package. Do not duplicate stubs across packages.
