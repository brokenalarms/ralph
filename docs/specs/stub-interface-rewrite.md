# Stub interface rewrite — handoff

> **Status**: partial. PR #544 landed the `stubGitHub` true-in-memory fake and started migration. Remaining work is listed below.

## Architecture — the test pyramid

Three test layers, each with a clear rule about what's real and what's stubbed:

| Layer | git module | git CLI | GitHub API | Agent | Task backend | Purpose |
|---|---|---|---|---|---|---|
| **git module unit** | real (SUT) | real (via `initBareRepo`) | stub | N/A | N/A | Test git module's own logic against real git |
| **loop module unit** | stub (`stubRepo`) | (transitive) | (transitive) | stub | stub | Test loop's decision paths in isolation |
| **integration** | real | real (via `initBareRepo`) | stub | stub | stub | End-to-end flow; ordered-timeline assertions |

**Only external systems are stubbed at the integration layer** — GitHub API, LLM agent, bead CLI. Everything else (git module, git binary, verifier, state) runs real. This is the layer that catches the seams unit tests miss.

**Module-boundary stubbing IS legitimate for unit tests** — the `stubRepo` is the git module's peer-level fake, used by loop unit tests to isolate loop behavior from git behavior. This isn't the same smell as stubbing an external system; it's the test pyramid.

---

## What "integration test" means here

A test is an integration test iff its assertions *depend on real git state transitions*. If the assertion is "the loop exited after 5 iterations" or "the blocked task was skipped," running real git changes nothing — those are loop unit tests.

Integration-worthy assertions look like:
- "After iteration N, `origin/main` points to the commit the loop just squashed"
- "After the evolve, the user's commits on the feature branch are still present"
- "After the two-task sequence, the second task's branch was based on the first task's final commit"
- "The merge conflict was detected by running a real rebase against a real base branch that had diverged"

These require a real bare repo, real git operations, and ordered-timeline assertions (*when* did each observable state change).

---

## Current state (post-PR-#544)

Landed:
- `stubGitHub` — unexported, true in-memory fake of the `gitHub` interface. Starting world configured via `StubGitHubConfig{PRs: []StubPR{...}, Checks: map[int][]CICheckResult{...}, ...}`. Fixed behavior: `MergePR` flips state, `CreatePR` appends, `ReopenPR` reverses close. Per-method `*Err` fields for fault injection. No sequenced slices, no callback fields, no hybrids.
- `StubPR` — data describing a PR in the fake's world, including `Conflicted bool` / `Blocked bool` as world-state causes that the fake's `MergePR` derives outcomes from.
- `newRepoForTest(cfg Config, gh gitHub, opts ...repoTestOpt) *Repo` — package-private helper in `test_helpers_test.go`. Constructs a real `*Repo` with test-supplied dependencies. Defaults `runner` → `newStubRunner()`, `state` → `newMemState()`. Options: `withRunner`, `withState`, `withWorktreeBranch`, `withBranchRenamed`. Internal/git tests only.
- `ci_test.go` + `git_branch_test.go` migrated to the new API. Zero `&Repo{}` struct literals. `pollableGitHub` hybrid deleted. 3 transition-style tests reframed as static-world pairs; 3 tests asserting on stub internals deleted.

Legacy still present (scheduled for removal):
- `StubGitHub` — exported, ~60 mutable fields. Still used by 9 test files in internal/git.
- `NewStubGitHub()` — zero-arg constructor.
- `StubRepo` — exported, same-shape legacy stub of `Ops`. Used by all 37 tests in `loop_integration_test.go`.
- `NewStubRepo()` — zero-arg constructor.
- `NewStubGitHubCfg(cfg) gitHub` — temporary name (collision avoidance). Rename to unexported `newStubGitHub` in final commit.
- `stubManager(dir, runner, gh)` — old test helper; deprecated in favor of `newRepoForTest`.
- `capturingGitHub`, `errRunnerImpl`, `errRunner` in `test_helpers_test.go` — legacy hybrid types.

---

## Remaining work

### Phase A — finish internal/git migration

Migrate the 9 remaining test files to `NewStubGitHubCfg` + `newRepoForTest`. File-by-file, one commit per file (or small grouped commits):

- `resume_test.go` (5 sites)
- `runner_test.go` (5 `stubManager` + 1 `errRunner` site — delete `errRunner`/`errRunnerImpl` when last caller is gone)
- `merge_stack_test.go` (6 sites)
- `github_test.go` (6 sites + 1 `capturingGitHub`)
- `git_merge_pipeline_test.go` (20 sites + 3 `capturingGitHub`)
- `git_merge_test.go` (25 sites + 7 `capturingGitHub`) — final migration; delete `capturingGitHub`, `StubGitHub`, `NewStubGitHub()` no-arg, `stubManager`, `testing_test.go` in this commit
- Rename `NewStubGitHubCfg` → unexported `newStubGitHub` (no remaining external callers)

Per-file: replace `NewStubGitHub()` + field mutation with `NewStubGitHubCfg(StubGitHubConfig{...})`. Replace `&Repo{...}` literals with `newRepoForTest(Config{...}, gh, opts...)`. Split transition-style tests into static-world pairs; delete tests that assert on stub internals. Every deletion documented in the commit message with rationale.

### Phase B — `stubRepo` (unit-test fake of `Ops`)

Apply the stubGitHub pattern to `Ops`. Same rules:

```go
// internal/git/testing.go (additions)

type StubRepoConfig struct {
    // World state — the repo the fake pretends to manage.
    ProjectDir   string
    WorkDir      string
    BaseBranch   string
    HeadSHA      string
    WorktreeBranch string
    RemoteURL    string
    KnownPRNumber int

    // Inner GitHub fake — constructed by NewStub from this config.
    GitHub StubGitHubConfig

    // Results that depend on world state and are hard to derive
    // (tests configure these directly rather than modeling branch
    // divergence explicitly).
    Ship ShipResult  // returned by Ship when called
    MergeRetrySucceeds bool  // outcome of MergeWithRetry
    // ... etc. — data, not callbacks.

    // Per-method fault injection.
    PushErr          error
    ShipErr          error
    MergeRetryErr    error
    // ... per method that can fail
}

type stubRepo struct {
    cfg StubRepoConfig
    gh  gitHub  // built from cfg.GitHub during NewStub
    // internal state that mutates as the SUT drives ops
    currentBranch string
    currentHead   string
}

var _ Ops = (*stubRepo)(nil)

// NewStub returns a stubRepo as the Ops interface. External packages use this.
func NewStub(cfg StubRepoConfig) Ops {
    return &stubRepo{
        cfg:           cfg,
        gh:            newStubGitHub(cfg.GitHub),
        currentBranch: cfg.WorktreeBranch,
        currentHead:   cfg.HeadSHA,
    }
}
```

Behavior model: fixed and shared. `HeadRev()` returns `currentHead` (mutated by state-changing operations like `CommitAll`). `RemoteURL()` returns `cfg.RemoteURL`. `Ship()` consumes the `cfg.Ship` result (the test's declared outcome) and updates internal state accordingly.

Forbidden in stubRepo:
- Exported fields
- Callback fields (`ShipFunc`, `MergeRetryFunc`, etc.)
- Sequenced-response slices
- Zero-arg constructor
- Direct access to `.gh` field from outside (tests configure inner github via `cfg.GitHub`)

### Phase C — classify and migrate loop tests

The 37 tests in `loop_integration_test.go` are misnamed. Classify each by what it asserts:

- **Loop unit tests** (~30): assertions about loop decisions (iterations, skips, phase transitions, resume paths, stop-file handling, etc.). Migrate to `stubRepo` via `git.NewStub(cfg)`. Rename from `TestIntegration_*` to `TestLoop_*` (or similar) to reflect that git-level assertions aren't happening.
- **Integration candidates** (~5-8): assertions that depend on real git state:
  - Happy-path end-to-end (branch created → commits → push → merge → base advances)
  - TwoTasksCompleteSequentially (stack head derivation from real branches)
  - MergeConflictThenRetrySucceeds (real conflict created; real retry path)
  - StackedPRSkipsMergeButCloses (real base branch != default)
  - EvolveRebasePreservesUserCommits (real rebase against real diverged branch)
  - PriorIterationCommit_SignalOnRetry_ShipsAndCloses (real commits surviving iteration boundary)
  - LifecycleOrdering_BranchRenameAndReviewers (ordering of real git operations)
  - CIFailureTriggersFixAgent (end-to-end fix agent flow)

For the integration candidates: relocate to a new file `loop_integration_real_test.go` and rewrite to use real git. Each test:
1. Boots a real bare repo via `initBareRepo(t)` as origin
2. Boots a working repo pointed at it
3. Constructs a real `*git.Repo` (via an exported test factory; see below)
4. Wires a real Runner, real state, real verifier; stubs only the agent (Claude), task backend, and gitHub
5. Runs the loop iteration(s)
6. **Asserts a timeline of observable effects in order** — e.g.:
   - Before iteration: `ls .git/refs/heads/` shows no feature branch
   - After branch setup: feature branch exists at base-branch SHA
   - After agent: working tree has expected file changes; commit made
   - After ship: bare repo's branch ref advanced to match working repo
   - After merge: bare repo's base branch advanced; feature branch ref deleted
   - After close: bead close recorded with PR#

### Phase D — factories

Add a git-package factory that wraps a real `*Repo` but takes a stub `gitHub`:

```go
// internal/git/testing.go — NEW, exported
func NewForIntegrationTest(cfg Config, ghCfg StubGitHubConfig) Ops {
    // Builds a real *repo (aka *Repo, pre-rename) with the real execRunner
    // and a real state store, BUT with newStubGitHub(ghCfg) as the gitHub.
    // Loop integration tests use this.
}
```

Loop unit tests use `git.NewStub(cfg)` (full fake). Loop integration tests use `git.NewForIntegrationTest(cfg, ghCfg)` (real repo, stub GitHub).

### Phase E — rename `Repo` → unexported `repo`

Once no external caller references `*git.Repo`:
- Rename the struct: `Repo` → `repo`.
- `git.New(cfg Config)` return type stays `Ops` (status quo).
- Any remaining `*git.Repo` references in loop or cmd packages change to `git.Ops`.
- Internal files still reference `*repo` directly where needed.

### Phase F — delete legacy and add arch enforcement

Delete `StubRepo`, `NewStubRepo()`, every exported field, `testing_test.go`.

Add arch tests in `internal/git/config_arch_test.go` (or a new `stubs_arch_test.go`):
- `TestNoExportedFieldsOnStubs` — walks `testing.go` with `go/ast`, fails on any exported field or any `func` type on `stubGitHub` or `stubRepo`.
- `TestNoSequencedResponseSlices` — walks `StubGitHubConfig`/`StubRepoConfig`, fails on slice-of-slices or slice-of-result-types.
- `TestStubConstructorsReturnInterfaces` — `NewStub`, `New`, `NewForIntegrationTest` return interface types.
- `TestRepoIsUnexported` — no exported `Repo` identifier.
- `TestNoRepoStructLiterals` — walks all `.go` files, fails on `&repo{` or `&stubRepo{` outside `testing.go`/`test_helpers_test.go`.
- `TestNoLoopStubRepoMutation` — walks loop `_test.go` files, fails on field-access-assignment patterns against stubs.

---

## Acceptance criteria

1. `grep -rn '^type Stub\|^type stub' go/internal/git/testing.go` lists exactly `stubGitHub`, `stubRepo` (unexported), `StubGitHubConfig`, `StubRepoConfig`, `StubPR` (exported data).
2. `grep -rn '^type Repo ' go/internal/git` returns zero matches.
3. `grep -rn '\*git\.Repo\|git\.Repo\b' go/internal/loop go/cmd` returns zero matches.
4. `grep -rn '\(gh\|gm\|stub\)\.\w\+ *=' go/internal/{git,loop}` returns zero matches against stub fields.
5. `grep -rn 'Func *func' go/internal/git/testing.go` returns zero matches.
6. `grep -rn 'StubGitHub{\|StubRepo{\|&StubGitHub\|&StubRepo\|&Repo{\|&repo{\|&stubRepo{' go/internal go/cmd` returns zero matches outside `testing.go` / `test_helpers_test.go`.
7. `grep -rnE 'Responses \[\]\[\]|Responses \[\](MergeResult|int|string|CICheckResult|PRDetail)' go/internal/git/testing.go` returns zero matches.
8. `go test ./internal/git/... ./internal/loop/...` passes.
9. `go vet ./...` clean.
10. Arch tests from Phase F all present and passing.
11. `pollableGitHub`, `capturingGitHub`, `errRunnerImpl`, `errRunner`, `ciTriggerGit` absent.
12. `loop_integration_real_test.go` exists with at least the happy-path end-to-end test. Its assertions check real git state after each phase of the loop iteration.
13. `loop_integration_test.go` renamed (or its tests renamed `TestIntegration_*` → `TestLoop_*`) to reflect that these are loop unit tests with scenarios.

---

## Out of scope — `stubRunner` has the same smell one layer down

`stubRunner` programs git-subprocess responses via `.On("fetch", "", nil)` and similar, with call-history inspection via `.CalledWith("push", ...)`. This is a lower-layer version of the stub anti-pattern, but fixing it requires a true git CLI fake (impractical) or migrating all git-module tests to `initBareRepo` (large scope).

This rewrite does not address `stubRunner`. It is flagged for a separate handoff; if its cost becomes painful, migrate internal/git tests to `initBareRepo`-only.

---

## How to rewrite tests that seem to need sequenced responses

Every existing test that uses `ChecksFunc: func(call int) []CICheckResult { ... }` or similar sequenced logic needs to be reframed:

- "CI pending N times then passing" → **split into two static-world tests**:
  - Test A: world has CI = passing → SUT's poll loop returns success immediately
  - Test B: world has CI = pending → SUT's poll loop times out
  Both branches of the SUT's behavior are covered. The transition itself is GitHub's concern.

- "MergePR fails once, then succeeds" → same split. Test the retry logic with a static failure world (does it retry within bounds?) and the success path separately (does it stop when it works?).

If a test's assertion is "the poll loop called ListChecks exactly 3 times," that assertion is the anti-pattern. Delete it — replace with an assertion on observable behavior (the poll loop returned success, the SUT's final state is X).
