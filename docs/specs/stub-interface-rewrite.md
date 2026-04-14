# Stub interface rewrite — handoff

> **Status**: Phases A–B merged. Phase C (loop test migration) is the next and largest piece of work. This document is the binding handoff for the agent picking up from here. Read it in full before touching code.

## Test pyramid — the architectural rule

Three test layers, each with a clear rule about what's real and what's stubbed:

| Layer | git module | git CLI | GitHub API | Agent | Task backend | Purpose |
|---|---|---|---|---|---|---|
| **git module unit** | real (SUT) | real (via `initBareRepo`) | stub (`stubGitHub`) | N/A | N/A | Test git module's own logic against real git |
| **loop module unit** | stub (`stubRepo`) | (transitive) | (transitive) | stub | stub | Test loop's decision paths in isolation |
| **integration** | real | real | stub (`stubGitHub`) | stub | stub | End-to-end flow; ordered-timeline assertions |

Module-boundary stubbing IS legitimate for unit tests — `stubRepo` is the git module's peer-level fake, used by loop unit tests to isolate loop behavior. It is not the same smell as stubbing an external system; it is the test pyramid.

Integration tests catch what unit-with-stubs miss: real `*git.Repo` against `initBareRepo`, asserting the full sequence of observable effects.

---

## What "integration test" means here

A test is an integration test iff its assertions *depend on real git state transitions*. If the assertion is "the loop exited after 5 iterations" or "the blocked task was skipped," running real git changes nothing — those are loop unit tests.

Integration-worthy assertions look like:
- "After iteration N, `origin/main` points to the commit the loop just squashed"
- "After the evolve, the user's commits on the feature branch are still present"
- "After two-task sequence, the second task's branch was based on the first task's final commit"
- "The merge conflict was detected by running a real rebase against a real base branch that had diverged"

These require a real bare repo, real git operations, and ordered-timeline assertions (*when* each observable state change happens).

---

## Current state (after PR #548)

### Landed (on main)

**internal/git package — fully migrated to the new stub API:**

- `stubGitHub` (unexported) — true in-memory fake of the `gitHub` interface. Starting world configured via `StubGitHubConfig{PRs: []StubPR{...}, Checks: map[int][]CICheckResult{...}, ...}`. Fixed behavior: `MergePR` flips state, `CreatePR` appends to world, `ReopenPR` reverses close. Per-method `*Err` fields for fault injection. No sequenced slices, no callback fields, no hybrids.
- `StubPR` — data describing a PR in the fake's world. `Conflicted bool` / `Blocked bool` are world-state causes (the fake's `MergePR` derives its return value from these).
- `newStubGitHub(cfg StubGitHubConfig) gitHub` — unexported constructor, package-private to `internal/git`.
- `newRepoForTest(cfg Config, gh gitHub, opts ...repoTestOpt) *Repo` — package-private helper in `test_helpers_test.go`. Constructs a real `*Repo` with test-supplied dependencies. Defaults `runner` → `newStubRunner()`, `state` → `newMemState()`. Options: `withRunner`, `withState`, `withWorktreeBranch`, `withBranchRenamed`, `withPrevBranch`, `withKnownPRNumber`.
- All 11 internal/git test files migrated. Zero `&Repo{}` / `&StubGitHub{}` / `NewStubGitHub()` / `capturingGitHub` / `stubManager` / post-construction field mutations remain in test code.
- Deleted: `capturingGitHub`, `stubManager`, `errRunner`/`errRunnerImpl`, several legacy stub-infra tests, transition-style tests that relied on sequenced responses.

**git module's public surface:**

- `stubRepo` (unexported) — true in-memory fake of the `Ops` interface. ~50 methods. Built via `git.NewStub(cfg StubRepoConfig) Ops`. PR-related methods delegate to an inner `stubGitHub` built from `cfg.GitHub`. State-mutation methods (`CommitAll`, `RenameBranchTo`, `SetBranchRenamed`, etc.) modify internal state so subsequent reads reflect SUT-driven changes.
- `NewStub(cfg StubRepoConfig) Ops` — public constructor, exported for loop tests.

### Still present (legacy)

**These remain until Phase C completes. Do not delete them yet — loop tests depend on them.**

- `StubGitHub` exported struct with ~60 mutable fields (`testing.go`)
- `NewStubGitHub()` no-arg constructor with default-world behavior
- `StubRepo` exported struct with mutable fields (`testing.go`)
- `NewStubRepo()` no-arg constructor
- `testing_test.go` — tests the legacy `StubGitHub` infrastructure
- `stubRunner` (`test_helpers_test.go`) — programs git-subprocess responses. Out of scope for this rewrite; smell flagged for a separate handoff.

### Coverage gaps flagged during Phase A (file separately if worth closing)

- **PR title as subject formatting** — `git_merge.go:761` formats the commit-subject string passed to `MergePR`. Test was deleted in PR #547 because the assertion read `gh.LastMergeOpts.Subject` (stub internal). The formatter is a pure string operation; the clean fix is to extract a named function and unit-test it directly. No current test exercises this line.
- **`CreatePRViaAPI` fallback path** — `gh pr create` fails, REST API succeeds. Legacy test asserted on separate error flags; the new `stubGitHub`'s `CreatePRViaAPI` delegates to `CreatePR` and shares error state, so the "one fails, the other succeeds" split can't be modeled without further fake refactoring.

---

## Phase C — the next work (loop test migration)

### Scope (measured, not estimated)

Across `internal/loop/*_test.go`:

- **232 construction sites** using `&git.StubRepo{}` or `git.NewStubRepo()` across 21 files
- **231 post-construction field mutations** (`gm.X = ...`) — the same anti-pattern Phase A eliminated from internal/git
- **31 callback-based overrides** (`gm.ShipFunc = func(...)`, `gm.MergeRetryFunc = func(...)`, etc.) — the forbidden pattern the new `stubRepo` does not support at all
- **37 `TestIntegration_*` tests** in `loop_integration_test.go` (single largest concentration: 60 construction sites, 129 field mutations, 8 callbacks in that one file)

### Non-mechanical

Mechanical substitution is not enough. The new `stubRepo` has:
- No exported mutable fields (the 231 field mutations must move into `StubRepoConfig` at construction, or be removed as anti-pattern assertions)
- No callback fields (the 31 `ShipFunc`/`MergeRetryFunc` overrides must be reframed into static-world tests)

**Every callback-based test is transition-style**: "first call returns A, second call returns B, agent runs in between to mutate state." Per the spec's reframing rule, these split into two static-world tests:
- World where outcome = A → SUT takes branch 1
- World where outcome = B → SUT takes branch 2

The transition itself is GitHub's behavior, not the SUT's.

### Integration-test promotion

Of the 37 `TestIntegration_*` tests, **most (~30)** are loop unit tests misnamed — their assertions don't depend on real git. They migrate to `git.NewStub(cfg)` and rename `TestIntegration_*` → `TestLoop_*`.

**A subset (~5-8)** genuinely depend on real git state transitions and should promote to real integration tests. Candidates identified during spec research:
- Happy path end-to-end
- TwoTasksCompleteSequentially (stack head derivation from real branches)
- MergeConflictThenRetrySucceeds (real conflict required)
- StackedPRSkipsMergeButCloses (real base branch ≠ default)
- EvolveRebasePreservesUserCommits (real rebase against real diverged branch)
- PriorIterationCommit_SignalOnRetry_ShipsAndCloses (real commits crossing iteration boundary)
- LifecycleOrdering_BranchRenameAndReviewers (ordering of real git ops)
- CIFailureTriggersFixAgent (end-to-end fix agent flow)

These move to a new file `loop_integration_real_test.go` and use real `*git.Repo` via a new factory (Phase D).

### Expected test count delta

During Phase A migrations, ~21 tests were deleted across internal/git (transition-style tests, call-history assertions, stub-infra tests). For Phase C with 31 callbacks, 231 mutations, and 37 classification candidates, the delta will likely be **30–50 tests deleted or reframed-into-splits**. Each deletion needs a one-line rationale in the commit message.

### Forbidden during Phase C

Under no circumstances:
- Re-introduce mutable fields on `stubRepo` or `StubRepoConfig`
- Add callback fields (`ShipFunc`, `MergeRetryFunc`, etc.) to `stubRepo`
- Add sequenced-response slices to `StubRepoConfig`
- Leave post-construction field mutations in test code
- Reframe by adding "inspection accessors" that let tests read stub-internal state

If a test seems to require one of these, **delete it** with a rationale. Coverage is usually absorbed by other tests; if not, write a separate static-world test.

### Recommended execution order

1. **Inventory phase**: per-file, produce a short table of (site count, callback count, mutation count, complexity).
2. **Start with leaf files** (few sites, no callbacks): `loop_resume_test.go`, `loop_test.go`, `loop_task_polling_test.go`, `loop_task_selection_test.go`, `loop_prompt_test.go`, `loop_verifybuild_test.go`, `loop_finalize_test.go`. Pure mechanical swaps; validates the pattern.
3. **Next tier**: `loop_init_test.go`, `loop_refactor_test.go`, `loop_signal_test.go`, `loop_push_test.go`, `loop_display_test.go`, `loop_merge_test.go`, `loop_verification_test.go`. Mixed — some mechanical, some anti-pattern reframing.
4. **Heavy files**: `loop_ship_ci_test.go`, `loop_verify_test.go`, `loop_posttask_test.go`, `loop_branch_test.go`, `loop_completion_test.go`, `loop_lifecycle_test.go`. Dense with callbacks and mutations.
5. **Classification pass**: `loop_integration_test.go` (37 tests). Triage into migrate-as-unit vs promote-to-integration.
6. **Integration tests**: build `git.NewForTest(cfg, StubGitHubConfig) Ops` factory (Phase D). Create `loop_integration_real_test.go`. Port the 5-8 promoted tests to real git.
7. **Legacy deletion**: delete `StubGitHub` + `StubRepo` + `NewStubGitHub()` no-arg + `NewStubRepo()` + `testing_test.go`. Uncomment arch test entries if any.
8. **Arch enforcement** (Phase F — see below).

### PR sizing

Single "migrate all loop tests" PR is unreviewable. Per-file PRs work but multiply review cycles (20+ PRs). Recommended: **batched per domain** — 4–6 PRs, each covering a coherent group of files. Suggested groupings align with execution steps 2–6 above.

Every PR message documents:
- Tests migrated (count + one-line pattern per type)
- Tests reframed from call-history → state-change (named)
- Tests deleted with one-line rationale each
- Test count before/after

---

## Phase D — factories (smaller)

After Phase C produces an ordered list of integration candidates, build:

```go
// git/testing.go — NEW, exported.
// Real *Repo with real execRunner, real state, real logger, but stub gitHub
// built from ghCfg. Integration tests use this.
func NewForTest(cfg Config, ghCfg StubGitHubConfig) Ops
```

This is the integration-test seam. Real git operations run; GitHub API calls are stubbed. Tests configure the world through `ghCfg` and assert on real git state after the SUT runs.

---

## Phase E — `Repo` → unexported `repo`

Once no external caller references `*git.Repo`:
- Rename the struct: `Repo` → `repo`.
- `git.New(cfg Config)` return type stays `Ops` (status quo).
- Remaining `*git.Repo` references in loop/cmd packages (if any) become `git.Ops`.

This is a rename commit, mechanical.

---

## Phase F — arch enforcement tests

Add to `internal/git/config_arch_test.go` (or a new `stubs_arch_test.go`):

1. **`TestNoExportedFieldsOnStubs`** — walks `testing.go` with `go/ast`, fails on any exported field on `stubGitHub` or `stubRepo` or any `func`-typed field on `StubGitHubConfig`/`StubRepoConfig`.
2. **`TestNoSequencedResponseSlices`** — walks `StubGitHubConfig`/`StubRepoConfig` fields, fails on any slice-of-slices or slice-of-result-types.
3. **`TestStubConstructorsReturnInterfaces`** — `NewStub`, `New`, `NewForTest` return interface types (not concrete).
4. **`TestRepoIsUnexported`** — no exported `Repo` identifier.
5. **`TestNoRepoStructLiterals`** — walks all `.go` files (excluding `testing.go` / `test_helpers_test.go`), fails on `&repo{` or `&stubRepo{` construction.
6. **`TestNoLoopStubRepoMutation`** — walks `internal/loop/*_test.go`, fails on `gm.\w+\s*=\s*[^=]` patterns against stub types.

These lock the invariants; regression becomes a CI failure.

---

## Acceptance criteria

1. `grep -rn '^type Stub\|^type stub' go/internal/git/testing.go` lists exactly `stubGitHub`, `stubRepo` (unexported), `StubGitHubConfig`, `StubRepoConfig`, `StubPR` (exported data). No other type names.
2. `grep -rn '^type Repo ' go/internal/git` returns zero matches.
3. `grep -rn '\*git\.Repo\|git\.Repo\b' go/internal/loop go/cmd` returns zero matches.
4. `grep -rn '\(gh\|gm\|stub\)\.\w\+ *=' go/internal/{git,loop}` returns zero matches against stub fields.
5. `grep -rn 'Func *func' go/internal/git/testing.go` returns zero matches.
6. `grep -rn 'StubGitHub{\|StubRepo{\|&StubGitHub\|&StubRepo\|&Repo{\|&repo{\|&stubRepo{' go/internal go/cmd` returns zero matches outside `testing.go` / `test_helpers_test.go`.
7. `grep -rnE 'Responses \[\]\[\]|Responses \[\](MergeResult|int|string|CICheckResult|PRDetail)' go/internal/git/testing.go` returns zero matches.
8. `go test ./internal/git/... ./internal/loop/...` passes. Count may drop from 30–50 tests documented per commit; net increase possible from static-world splits.
9. `go vet ./...` clean.
10. All Phase F arch tests present and passing.
11. `pollableGitHub`, `capturingGitHub`, `errRunnerImpl`, `errRunner`, `ciTriggerGit` absent.
12. `loop_integration_real_test.go` exists with at least one test using real `*git.Repo` against `initBareRepo` with stubbed gitHub only, asserting a timeline of observable git state changes.
13. `loop_integration_test.go` either renamed or its tests renamed (`TestIntegration_*` → `TestLoop_*`) to reflect they are loop unit tests with scenarios.

---

## Out of scope (flagged for future)

### `stubRunner` layer

`stubRunner` in `test_helpers_test.go` programs git-subprocess responses via `.On("fetch", "", nil)` and similar, with call-history inspection via `.CalledWith("push", ...)`. This is a lower-layer version of the stub anti-pattern, but closing it requires migrating all git-module tests that currently use it to `initBareRepo`-only. That's a large separate piece of work. This rewrite does not address it.

### Kill `stubRepo` entirely

The user flagged (during PR #548 review) concerns about `stubRepo`'s complexity — 411 lines of parallel git-module behavior that must stay in sync with real `*Repo`. An alternative architecture is: loop tests use real `*git.Repo` everywhere, no `stubRepo` at all. Trade-offs:
- Loss of 411 lines of test infrastructure
- Test suite ~6–20x slower (rough estimate: 7s → 45–150s for loop)
- Real-git coverage at every loop test, not just integration tests

The current path (Phase C as planned) keeps `stubRepo`. Reconsidering this is a scope reset — if chosen, Phase C becomes "migrate loop tests to real `*git.Repo` via `NewForTest`" with no stubRepo, and `stubRepo` gets deleted rather than built out to cover all loop test cases.

---

## Handoff notes for the next agent

### If proceeding with Phase C as planned

1. Read `/Users/daniel/Developer/ralph/go/internal/git/testing.go` to see the shape of `stubRepo`, `StubRepoConfig`, and the existing Phase-A-migrated patterns in the file.
2. Read one already-migrated internal/git test file (`ci_test.go` is a good reference) to see how the patterns land.
3. Read **one** loop test file at a time, study the test shapes (callbacks, mutations, assertions), then migrate.
4. Per test, decide: pure mechanical, reframe (state-change), split (two static tests), or delete. Document in commit message.
5. Never add callback fields. Never add mutable stub fields. Never add sequenced slices. Never add post-construction mutation surfaces.
6. If you feel stuck — a test requires a pattern the new fake doesn't support — the answer is usually to split or delete the test, not extend the fake.
7. Migrate files in the order suggested above (leaf files first). Each commit runs `go build`, `go vet`, and `go test` before being committed.

### Partial PR from Phase C preview

Branch `stub-migration-phase-c` (not yet pushed to PR) has one commit migrating `loop_resume_test.go` (the simplest file, 1 site). That commit demonstrates the basic pattern:

```go
// Before:
gm := &git.StubRepo{
    ProjectDir:     dir,
    WorkDir:        dir,
    WorktreeBranch: "ralph/...",
    RemoteURLValue: "...",
}

// After:
gm := git.NewStub(git.StubRepoConfig{
    ProjectDir:     dir,
    WorkDir:        dir,
    WorktreeBranch: "ralph/...",
    RemoteURL:      "...",  // renamed from RemoteURLValue
})
```

A call-history assertion (`if gm.ResumeCalls == 0`) was deleted because the downstream assertion (`if !agentCalled`) covers the same behavior through observable effects.

Use that commit as the starting template.
