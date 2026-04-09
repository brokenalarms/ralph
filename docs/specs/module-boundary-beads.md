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

**Target:** `g.Ship(ctx, opts)` — one call for "get this work into a PR and
optionally merge." Internally handles push, PR create/update, reviewer
polling, merge with retry, post-merge main sync.

The orchestrator provides callbacks in `ShipOpts` for cross-module concerns
(spawning fix agents):

```go
type ShipOpts struct {
    TaskID       string
    TaskTitle    string
    Body         string
    AutoMerge    bool
    Reviewers    []Reviewer
    OnCIFailure  func(*CIFailureError) CIFixResult
    OnConflict   func(*UnresolvedConflictError) bool
    OnReviewFix  func(string, *AutoReview, int) bool
}
```

**AC:**
1. `Ship` on the `git.Repo` interface accepts `ShipOpts` with reviewer
   config, merge options, and fix callbacks
2. `finalizePR` is deleted from `loop_iteration.go`
3. `finalizePRParams` struct is deleted
4. The loop does not call `PostMergeUpdateMain`, `SetLocalTestsPassed`,
   `SetKnownPRNumber`, `GetPRState`, `GetPRBase`, `PollReview`, or
   `MergeWithRetry` directly
5. All existing integration tests pass unchanged

**Greppable assertion:** `grep -r 'finalizePR\|PostMergeUpdateMain\|SetLocalTestsPassed\|SetKnownPRNumber\|PollReview' go/internal/loop/` returns zero matches (except test helpers).

---

## Phase 2: collapse the post-signal pipeline

### Bead 4: completeTask replaces handlePostSignal + runAndComplete tail

**Current state:** After the agent signals, `runAndComplete` calls
`handlePostSignal` which calls `pushSignalPR` and `finalizePR` (after
bead 3 replaces finalizePR with `g.Ship`). `handlePostSignal` has 16
dependency fields. `runAndComplete` has 35.

**Target:** `completeTask(ctx, completeOpts) loopAction` is the single
post-completion function. After bead 3, the Ship call is one line. What
remains is: verify → ship → close bead → record completion → notify.
That's 5 concerns, each one call.

**AC:**
1. `completeTask` exists in the loop package with ≤10 fields in its params
2. `handlePostSignal` and `pushSignalPR` are deleted
3. `handlePostSignalOpts` and `runAndCompleteParams` structs are deleted
4. `loop.go:Run()` calls: run agent → `completeTask`. Two phases, not one
   35-field mega-delegation
5. No params struct in `go/internal/loop/` has more than 10 fields
6. All existing integration tests pass unchanged

**Greppable assertion:** `grep -r 'handlePostSignal\|pushSignalPR\|runAndCompleteParams' go/internal/loop/` returns zero matches.

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

### Test: git.Repo not threaded through loop params structs

```go
func TestGitNotThreaded(t *testing.T) {
    // Parse all type *Params struct in loop_*.go (non-test)
    // Fail if any field type is git.Repo or git.GitOps
    // The Loop struct itself may hold git.Repo — that's the one reference
    // point. But it must not be copied into params structs for downstream
    // functions. The orchestrator calls g.Method() directly.
}
```

### Test: no params struct exceeds 10 fields

```go
func TestNoGodParams(t *testing.T) {
    // Parse loop_*.go (non-test), find type *Params struct
    // Fail if any has >10 fields
}
```

### AGENTS.md addition

> The `git.Repo` interface has intent-level methods ("ship this work"), not
> implementation-level methods ("squash commits"). Add implementation
> detail as internal methods on the concrete type, or as options on an
> existing interface method (e.g. a field on `ShipOpts`).
>
> The orchestrator holds one `git.Repo` reference and calls it directly.
> Do not thread `git.Repo` through params structs to downstream functions.
> If a downstream function needs a git result, the orchestrator calls the
> git method and passes the result as data. `TestGitNotThreaded` enforces
> this.
>
> GitHub is git's internal persistence layer. Do not reference `git.GitHub`,
> `git.StubGitHub`, or any GitHub type outside `go/internal/git/`.
> `TestGitHubIsInternal` enforces this.
