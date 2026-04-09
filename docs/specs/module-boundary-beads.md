# Module Boundary Enforcement — Bead Breakdown

This document breaks the remaining orchestrator refactor and module boundary
work (from orchestrator-refactor.md §Follow-ups) into small, greppable beads.
Each bead has acceptance criteria that constrain the **call site** — the agent
cannot satisfy them by moving code without changing the dependency graph.

## Context

The orchestrator-refactor spec was partially executed. `Run()` is ~65 lines
and reads linearly. But the extracted package functions (`runAndComplete`,
`handlePostSignal`, `finalizePR`, `resumeViaPR`, `prepareBranch`) receive
params structs that mirror the full Loop state. The god object didn't go away
— it was renamed from `*Loop` to `runAndCompleteParams`.

## Target architecture

The `git` package exposes a **focused interface** (`git.Repo`) with ~10
intent-level methods. A concrete implementation holds internal state (worktree
dir, branch, GitHub connection). The orchestrator creates one instance per
iteration and calls methods that express intent.

```go
// Created once — methods are the public API
g := git.New(dir, ralphDir, gh)
g.BranchForTask(ctx, taskID, title, meta)
g.ResumeTask(ctx, meta, opts)
g.Ship(ctx, opts)
g.FlushUnpushedWork(ctx, taskID, desc, merge)
g.HeadRev()
g.HasDiff()
g.WorkDir()
g.RemoveWorktree()
```

Key properties:

- **Intent-level methods.** Each method represents an intent ("ship this
  work"), not an implementation step ("squash, rebase, force-push, create PR").
  The interface can have as many methods as needed — the constraint is that
  it's called directly by the orchestrator, never threaded through params
  structs to downstream functions.
- **GitHub is fully internal.** The `GitHub` interface exists inside the git
  package but is never exposed. No caller passes a GitHub value, imports the
  GitHub type, or knows GitHub exists. Git could use GitLab or a local daemon
  without changing any call site. The GitHub dependency is injected at
  construction time: `git.New(dir, ralphDir, gh)` — production passes the
  real implementation, tests pass a stub. No test-specific code in production.
- **Instance-per-iteration.** Created with just `dir`, `ralphDir`, and
  the GitHub implementation. Internal state (branch name, PR number) is
  managed by the instance, not passed around by the orchestrator.
- **Two test levels.** Unit tests define a local interface in the test
  file with only the methods the test calls — Go's structural typing
  matches automatically, no exported interface needed. Integration tests
  use the real `*git.Repo` against temp dirs, passing a GitHub stub at
  construction time.

The loop orchestrator expresses **intent** — "I need a branch for this task",
"ship this work" — and the git module owns the **how**. The orchestrator
does not choreograph internal git steps.

## What this eliminates

- `git.Manager` (97 methods, 20 fields)
- `git.GitOps` (52-method god interface)
- `git.StubGitOps` (52 methods to implement for every test)
- `git.StubGitHub` defaults footgun (OpenPR/PRNumber sync issues)
- `NewStubGitHub()` — replaced by passing a GitHub stub at construction
  time: `git.New(dir, ralphDir, stubGH)` for integration tests, or a
  simple `Git` interface stub for unit tests
- Every params struct in the loop that passes `git.GitOps` through
- The `merge.go` partial-Manager-construction problem

## Ordering

Beads are listed in dependency order. Each bead is independent enough for
one agent iteration but later beads may depend on earlier ones.

---

## Phase 1: git module owns its internals

### Bead 1: git.BranchForTask replaces prepareBranch

**Current state:** `loop_git.go:prepareBranch` receives `branchParams{git,
backend, state, logger}` and choreographs `PrepareForNextTask`, `setStackHead`,
`ResetToDefaultBranch`, `EnsureUpToDate`, `checkoutExistingBranch`,
`WriteRunBranch`. The loop knows the internal steps of branch setup.

**Target:** The git interface exposes `BranchForTask(ctx, taskID, title,
meta)` — one call. Internally handles stack head detection, reset, rebase,
checkout-or-rename. Returns the branch name. The loop calls it once and
writes the branch to state.

`TaskMeta` is a plain data struct the orchestrator populates from the task
backend before calling:

```go
type TaskMeta struct {
    Branch      string // stored branch name from prior iteration
    ExternalRef string // PR URL from prior iteration
}
```

**AC:**
1. `BranchForTask` exists on the `git.Repo` interface
2. `loop_git.go` does not contain `setStackHead`, `checkoutExistingBranch`,
   `ResetToDefaultBranch`, `PrepareForNextTask`, or `EnsureUpToDate`
3. `loop.go:Run()` calls `g.BranchForTask` (one call) for branch setup
4. `branchParams` struct is deleted from `loop_git.go`
5. `setStackHead` and `checkoutExistingBranch` move to `go/internal/git/`
   as unexported functions
6. All existing integration tests pass unchanged

**Greppable assertion:** `grep -r 'setStackHead\|checkoutExistingBranch\|PrepareForNextTask\|ResetToDefaultBranch' go/internal/loop/` returns zero matches.

---

### Bead 2: git.ResumeTask replaces resumeViaPR

**Current state:** `loop_git.go:resumeViaPR` is 84 lines receiving 12 fields
(`resumeViaPRParams`). It queries task metadata, calls multiple git query
functions, delegates to `resolveByPRState` (60 lines, 13 fields), which
delegates to `finalizePR`. The loop knows every step of PR state resolution.

**Target:** `g.ResumeTask(ctx, meta, opts)` — one call. Returns whether the
task was handled (merged, open PR, needs agent). The orchestrator passes
`TaskMeta` as data and acts on the result.

**AC:**
1. `ResumeTask` exists on the `git.Repo` interface
2. `resumeViaPR`, `resolveByPRState`, `findExistingPRForTask` are deleted
   from `loop_git.go`
3. `loop.go:Run()` calls `g.ResumeTask` (one call) for the resume check
4. `resumeViaPRParams` and `resolveByPRStateParams` structs are deleted
5. The git package does not import `tasks` or `attempts` packages
6. All existing integration tests pass unchanged

**Greppable assertion:** `grep -r 'resumeViaPR\|resolveByPRState\|findExistingPRForTask' go/internal/loop/` returns zero matches.

---

### Bead 3: git.Ship handles the full post-signal pipeline

**Current state:** `git.Ship` exists but only does push+PR. The loop
choreographs: push → create PR → poll reviewers → merge → post-merge update.
This lives in `finalizePR` (125 lines, 17 fields).

**Target:** `g.Ship(ctx, opts)` handles push, PR create/update, reviewer
polling, and merge attempt. When a cross-module concern arises (CI failure,
unresolved conflict, review feedback), Ship returns a typed result and the
orchestrator decides what to do — call the verifier, spawn a fix agent,
retry, or give up.

```go
type ShipOpts struct {
    TaskID    string
    TaskTitle string
    Body      string
    AutoMerge bool
    Reviewers []Reviewer
}

type ShipResult struct {
    PRNumber int
    PRURL    string
    Merged   bool
    // Non-nil when Ship could not complete — orchestrator decides next step.
    CIFailure  *CIFailureError
    Conflict   *UnresolvedConflictError
    ReviewFix  *ReviewFixNeeded
}
```

The orchestrator owns the retry loop:

```go
for {
    result, err := g.Ship(ctx, shipOpts)
    if err != nil { return err }
    if result.Merged { break }

    if result.CIFailure != nil {
        fixed := v.TryFixCI(ctx, result.CIFailure)
        if !fixed { break }
        continue
    }
    if result.Conflict != nil {
        resolved := v.TryFixConflict(ctx, result.Conflict)
        if !resolved { break }
        continue
    }
    break
}
```

No callbacks. The git module returns data, the orchestrator acts on it.

**AC:**
1. `Ship` on the `git.Repo` interface accepts `ShipOpts` (data only, no
   callbacks) and returns `ShipResult`
2. `finalizePR` is deleted from `loop_iteration.go`
3. `finalizePRParams` struct is deleted
4. The loop does not call `PostMergeUpdateMain`, `SetLocalTestsPassed`,
   `SetKnownPRNumber`, `GetPRState`, `GetPRBase`, `PollReview`, or
   `MergeWithRetry` directly
5. No `func` type fields in `ShipOpts` or any params struct
6. All existing integration tests pass unchanged

**Greppable assertion:** `grep -r 'finalizePR\|PostMergeUpdateMain\|SetLocalTestsPassed\|SetKnownPRNumber\|PollReview' go/internal/loop/` returns zero matches (except test helpers).

---

## Phase 2: collapse the post-signal pipeline

### Bead 4: completeTask replaces handlePostSignal + runAndComplete tail

**Status:** Partially landed (ralph-6a80, PR #510). `handlePostSignal` and
`runAndCompleteParams` were deleted, but `completeTaskParams` replaced
module references with 22 `func` fields — same dependency threading via
callbacks instead of interfaces. Needs re-work.

**Current state:** `completeTaskParams` has 22 func fields wrapping module
methods (`shipFn`, `finalizePRFn`, `closeTaskFn`, `getStateFn`, etc.).
Each is a closure over a module on the `Loop` struct. This is the same
anti-pattern — the god object became a god callback bag.

**Target:** `completeTask` receives only data: the task ID, branch name,
summary, PR number, and any results the orchestrator has already obtained
from modules. The orchestrator calls modules directly (verify, ship, close
bead, record completion) and passes results between them. `completeTask`
does not need to call back into modules — the orchestrator sequences the
calls.

**AC:**
1. `completeTaskParams` contains zero `func` type fields
2. `completeTaskParams` contains zero interface fields
3. `completeTaskParams` contains only data: primitives, data structs
   (no methods), and `*logging.Logger`
4. The orchestrator in `Run()` calls modules directly and passes results
   as data to `completeTask`
5. `TestOrchestratorParamsNoModules` passes (universal no-func-types check)
6. All existing integration tests pass unchanged

**Greppable assertion:** `grep -r 'handlePostSignal\|pushSignalPR\|runAndCompleteParams' go/internal/loop/` returns zero matches (already satisfied).

---

## Phase 3: replace Manager with focused Git interface

### Bead 5: define git.Repo interface and implementation

**Current state:** `git.Manager` has 97 methods and 20 fields. `GitOps` is
a 52-method interface. Tests must implement all 52 methods via `StubGitOps`.

**Target:** `git.Repo` interface with ~10 intent-level methods (BranchForTask,
ResumeTask, Ship, FlushUnpushedWork, HeadRev, HasDiff, WorkDir,
RemoveWorktree, plus any remaining essentials). A concrete `gitImpl` struct
holds internal state and implements the interface.

`GitHub` becomes unexported (`gitHub`) or stays exported but only used
inside the git package. No external caller references it.

**AC:**
1. `git.Repo` interface exists with ≤12 methods
2. `git.Manager` struct does not exist
3. `git.GitOps` interface does not exist
4. `github.GitHub` (or `git.GitHub`) is not referenced outside `go/internal/git/`
5. Loop unit tests stub `git.Repo` (~10 methods)
6. Integration tests pass a GitHub stub at construction: `git.New(dir,
   ralphDir, stubGH)` — no test-specific code in production
7. `merge.go` calls `g := git.New(dir, ralphDir); g.Ship(ctx, opts)` — no
   20-field construction
8. All existing tests pass

**Greppable assertions:**
- `grep -rn 'git\.Manager\|git\.GitOps' go/` returns zero matches
- `grep -rn 'git\.GitHub\|git\.StubGitHub' go/internal/loop/` returns zero matches
- `grep -rn 'git\.GitHub\|git\.StubGitHub' go/cmd/` returns zero matches

---

## Non-goals

- Prompt building (`loop_prompt.go`) — already stateless, takes individual args
- Verifier internals — already behind its own struct with focused config
- Task backend interface — already a clean interface
- Agent module — already encapsulated

## How to prevent regression

The architecture must be enforced mechanically. Documentation alone doesn't
work — agents satisfy the letter of guidance while drifting from the intent.

### Test: GitHub not referenced outside git package

```go
func TestGitHubIsInternal(t *testing.T) {
    // Scan all .go files outside go/internal/git/
    // Fail if any reference git.GitHub, git.StubGitHub, or git.NewStubGitHub
}
```

### Test: no modules in orchestrator params structs

```go
func TestOrchestratorParamsNoModules(t *testing.T) {
    // Parse all type *Params/*Opts struct fields in loop_*.go (non-test)
    // For each field, check its type:
    //
    //   Allowed:
    //     - Primitives: string, int, bool, time.Duration, etc.
    //     - Data structs: structs with no methods (git.TaskMeta, git.ShipResult)
    //     - *logging.Logger (cross-cutting, ubiquitous)
    //     - context.Context
    //
    //   Forbidden:
    //     - Interfaces with methods (tasks.Backend, git.GitOps, git.Ops)
    //     - Pointers to structs with methods (*state.Store, *attempts.Tracker,
    //       *Verifier, *ratelimit.Limiter, *analyzer.Analyzer, *git.Repo)
    //       — except *logging.Logger
    //     - Function types: func(...) ... — callbacks are module references
    //       in disguise. If a downstream function needs a module's result,
    //       the caller obtains it and passes the result as data.
    //
    // This applies to ALL packages in go/internal/ — not just the
    // orchestrator. Any package can grow into the same anti-pattern.
    //
    // Top-level structs (Loop, Verifier, git.Repo) may hold module
    // references as fields — that's dependency injection at construction.
    // But params/opts structs passed to functions must not carry them.
}
```

Function callbacks (`func(...)`) are **forbidden** in params/opts structs.
They are module references in disguise — wrapping `v.TryFixCI` in
`func(*CIFailureError) CIFixResult` still threads the verifier module
through the params struct. The orchestrator calls modules directly and
passes results as data.

### AGENTS.md addition

> The orchestrator (loop package) holds module references on the `Loop`
> struct and calls them directly. Params/opts structs passed to functions
> must carry only data: primitives, data structs (no methods), and
> `*logging.Logger`. No interfaces, no struct-with-methods pointers, and
> no function types. `TestOrchestratorParamsNoModules` enforces this.
>
> If a downstream function needs a module's result, the orchestrator calls
> the module first and passes the result as data. If a module operation
> requires a decision from another module, the called module returns data
> describing the situation and the orchestrator makes the decision.
>
> GitHub is git's internal persistence layer. Do not reference `git.GitHub`,
> `git.StubGitHub`, or any GitHub type outside `go/internal/git/`.
> `TestGitHubIsInternal` enforces this.
