# Stub interface rewrite — handoff

> **Goal**: the git module exposes exactly one public type — the `Ops` interface. Two implementations live inside the package: `repo` (real, backed by the git CLI and GitHub API) and `stubRepo` (in-memory fake for tests in other packages). Both are unexported. Both are constructed through factory functions that return `Ops`. No concrete git implementation type ever escapes the `internal/git` package.

Read the **Testing → Stubs and test doubles** section of `AGENTS.md` before touching code. The rules there are binding; this handoff is the concrete migration plan.

---

## Design principles

**The fake's behavior is fixed and centralized.** Every test runs against the same behavior model. What varies per test is the initial **state of the world** — which PRs exist, what their checks are, what reviewers are configured — passed in as data through a config struct.

**Tests never program the fake's responses per call.** No sequenced-response slices. No callback fields. No inline override helpers. No embed-and-override hybrids. If a test seems to need "pending three times, then passing," it is testing a property of GitHub rather than the SUT — split it into two static-world tests (pending → keeps polling or times out; passing → stops and returns success) and the need disappears.

**The fake models GitHub's state transitions the way real GitHub does.** `MergePR` flips a PR to merged, `CreatePR` adds a PR to the world, `ReopenPR` flips closed → open. After the SUT runs, the fake's state reflects what the SUT did — queryable through the interface methods the SUT itself uses. Stub-internal fields are never read or written by tests.

**No concrete git implementation type is exported.** `Ops` is the only type the rest of the codebase sees. `git.New(cfg) Ops` returns the real implementation; `git.NewStub(cfg) Ops` returns the fake. Callers hold the interface, not the struct.

---

## Target architecture

### Types

```
git.Ops    — public interface (~60 methods; the single exported abstraction)
repo       — real implementation (unexported struct; currently the exported "Repo")
stubRepo   — stub implementation (unexported struct; currently the exported "StubRepo")
stubGitHub — stub of the gitHub interface (unexported; currently the exported "StubGitHub")
```

### Factories

```go
// Production — builds a real repo with real git CLI + real GitHub.
func New(cfg Config) Ops

// Test — builds a stubRepo with an in-memory world. Used by loop tests and
// any other package that needs an Ops value without real git.
func NewStub(cfg StubRepoConfig) Ops
```

### Package-private test seam

For tests *inside* `internal/git` that need to exercise real git behavior against a stubbed GitHub (the only scenario where a real `repo` must be constructed with an injected `gitHub`):

```go
// internal/git/test_helpers_test.go (or similar _test.go file)
func newRepoForTest(cfg Config, gh gitHub, runner Runner, state stateStore) *repo
```

Lives in a `_test.go` file, so it is unreachable from production code and from any other package's tests. It's the only path for constructing a `*repo` by injecting internal dependencies. No `&repo{}` struct literals anywhere.

---

## Target shape (what to build)

### 1. `stubGitHub` implements `gitHub` and nothing else

The production interface lives at `go/internal/git/github.go:131` (`type gitHub interface`). `ghCLI` is the production implementation. `stubGitHub` is the test implementation. Both expose the same 22 methods to callers — period.

```go
// StubGitHubConfig declares the initial state of the fake's world and any
// static return values. All fields are plain data. None programs per-call
// behavior. For fault injection, set the *Err field for the method that
// should fail — a single value per field, no sequencing.
type StubGitHubConfig struct {
    Available bool

    PRs    []StubPR                  // the PRs that exist in the world
    Checks map[int][]CICheckResult   // CI checks keyed by PR number

    RunLog           string
    Reviewers        []Reviewer
    RequiredChecks   []string
    JobStepCount     int
    PollReviewResult *AutoReview
    FetchThreadIDs   map[int]string
    PRDiffOutput     string
    SearchPRResult   int

    // Fault injection — one error field per method.
    CreatePRErr           error
    EditPRErr             error
    ReopenPRErr           error
    // ... (one per method that can fail)
}

// StubPR describes a PR that exists in the fake's world. Defaults applied at
// construction: URL → https://github.com/owner/repo/pull/<N>, HeadSHA →
// stub-sha-<N>, Base → "main", State → PRStateOpen.
type StubPR struct {
    Number  int
    Title   string
    URL     string
    Branch  string
    Base    string
    HeadSHA string
    State   PRState
}

type stubGitHub struct {
    cfg          StubGitHubConfig
    prs          map[int]*StubPR
    nextPRNumber int
}

// Compile-time guard. The return type is the interface so callers never see
// the concrete type.
var _ gitHub = (*stubGitHub)(nil)

func NewStubGitHubCfg(cfg StubGitHubConfig) gitHub {
    // ... initialize world from cfg.PRs, apply defaults, return interface
}
```

Interface methods read and write the world:

- `GetPR(n)` → look up PR n in the world, return PRDetail
- `MergePR(n)` → flip PR n's state to merged, return success; or return failure if not found/not open
- `CreatePR(opts)` → allocate next number, append to world, return number
- `ReopenPR(n)` → flip PR n's state to open
- `EditPR(n, title)` → update PR n's title in the world
- `ListChecks(n)` → return `cfg.Checks[n]`
- `FindOpenPR(branch)` → scan world for Open PR matching branch
- `ListOpenPRBranches()` → scan world, return branches of Open PRs
- static-return methods (`GetRunLog`, `DetectActiveReviewers`, `GetRequiredChecks`, `GetJobStepCount`, `PollReview`, `FetchReviewThreadIDs`) return their `cfg` fields

### 2. `stubRepo` implements `Ops` and nothing else

Same pattern applied to `Ops` (22+ methods on `go/internal/git/gitops.go`). `StubRepoConfig` carries the pre-arranged state. `stubRepo` is unexported. `NewStub(cfg StubRepoConfig) Ops` returns the interface.

The stub's internal GitHub dependency is built from `cfg.GitHub StubGitHubConfig` — one config in, one fake out, no nested mutation. Tests configure the GitHub world through the outer config field; there's no separate GH handle to reach into.

### 3. Every call site is rewritten

There is one construction shape per type. Every test file builds its stubs like this:

```go
// Full-fake Ops (used by loop tests):
ops := git.NewStub(git.StubRepoConfig{
    HeadRev: "abc123",
    GitHub: git.StubGitHubConfig{
        Available: true,
        PRs: []git.StubPR{{Number: 42, Branch: "feature", State: git.PRStateOpen}},
    },
})

// Real repo with stubbed GitHub (used by internal/git tests only):
gh := NewStubGitHubCfg(StubGitHubConfig{
    Available: true,
    Checks: map[int][]CICheckResult{1: {{Name: "ci", Bucket: "pass"}}},
})
r := newRepoForTest(Config{Logger: &testLog{}, BaseBranch: "main"}, gh, nil, nil)
```

Not:
```go
gh := NewStubGitHub()
gh.Checks = ...              // FORBIDDEN — field mutation
r := &Repo{github: gh, ...}  // FORBIDDEN — struct literal for real impl
```

---

## What is explicitly forbidden

These patterns were present in the codebase before this rewrite and must not appear after it. Each rule is an invariant, not a guideline.

1. **No exported fields on any stub type.** Every field on `stubGitHub` and `stubRepo` is lowercase. Tests never read or write the fake's internals directly. Observable state is reached through the same interface methods production uses.

2. **No callback-valued fields of any shape.** Configuration fields of type `func(...)` are banned. This includes the variants the previous attempt was about to introduce (`ListChecksFunc`, `CreatePRFunc`, `ShipFunc`, `HeadRevFunc`, `MergeRetryFunc`, `DiffFilesBetweenFunc`, `FlushUnpushedWorkFunc`, `OnRenameBranch`, `OnDetectActiveReviewers`, `OnMerge`, and any new `*Func` field).

3. **No sequenced-response slices.** Fields like `ListChecksResponses [][]CICheckResult` or `MergePRResponses []MergeResult` are banned. These are callbacks in disguise — tests programming per-call behavior into the stub. A method that varies call-over-call in production (e.g. CI that transitions over time) is modeled as state transitions driven by OTHER interface methods (e.g. `MergePR` flips state). A test that seems to need "pending three times then passing" is testing GitHub's behavior, not the SUT's — split it into two static-world tests.

4. **No partial-stub hybrids.** No type embeds a stub and overrides one of its methods. Every stub is a single type that implements the full interface. The previous scaffolds `pollableGitHub`, `capturingGitHub`, `errRunnerImpl`, and `ciTriggerGit` are deleted.

5. **No inline "tiny helper" override types in test files.** If a test thinks it needs a helper that mutates stub state between calls, it is reintroducing the hybrid pattern. Reframe the test as two static-world tests.

6. **No exported concrete git implementation type.** After the rewrite, `Repo` is renamed to unexported `repo`. `StubRepo` is renamed to unexported `stubRepo`. The only exported type from the git module is the `Ops` interface and the `StubRepoConfig` / `StubGitHubConfig` / `StubPR` data structs.

7. **No `&repo{` or `&stubRepo{` struct literals in any file.** All construction goes through the factory functions. For internal/git tests that need to inject a stub `gitHub` into a real `repo`, the package-private `newRepoForTest` helper in a `_test.go` file is the only seam.

8. **No `git.Ops` parameter in any data-only config struct.** Config remains pure data. `Config.Logger` is already typed as `Log` (an interface), which is the one allowed exception because logger is ubiquitous infrastructure. No new interface fields on Config.

9. **Factories return the interface type, not the concrete pointer.** `NewStubGitHubCfg(cfg) gitHub`, `NewStub(cfg StubRepoConfig) Ops`, `New(cfg Config) Ops`. There is no `*ForInspection` variant — tests that want to assert on "what happened" assert through the same interface methods production uses (the fake's state after the SUT ran), or on the SUT's own return values.

10. **No-arg constructors are removed.** Every test must state its starting world. `NewStubGitHub()` (zero-arg, with defaults) is deleted. The zero-value config `StubGitHubConfig{}` yields an unavailable GitHub with no PRs — an explicit, auditable starting point.

11. **The stub does not delegate to a sub-stub through a public field.** Today `StubRepo.FindOpenPRForBranch` falls through to `s.GH.FindOpenPR`. After the rewrite, `stubRepo` holds its inner `stubGitHub` in an unexported field, constructed from `cfg.GitHub StubGitHubConfig`.

---

## Out of scope — `stubRunner` has the same smell

`stubRunner` is the test double for the `Runner` interface (git subprocess executor). It has the same shape as the old `StubGitHub`: programmed responses via `.On("fetch", "", nil)` and `.OnSequence(...)`, call-history inspection via `.CalledWith("push", ...)`. A principled fix would require a true git fake, which is impractical (git's surface area is enormous).

The right move for tests that today use `stubRunner`:

- Where feasible, migrate to `initBareRepo(t)` — real git in a temp dir, zero stubbing at the runner layer
- For tests that genuinely need to inject specific responses, keep `stubRunner` but understand that it is a known pragmatic exception, not a pattern to extend

This rewrite does not address `stubRunner`. It is flagged as a follow-up; if it becomes painful, file a separate handoff.

---

## How to rewrite tests that seem to need sequenced responses

This is the judgment call that comes up most. Every existing test that uses `ChecksFunc: func(call int) []CICheckResult { ... }` or similar sequenced logic needs to be reframed.

- "CI pending N times then passing" → **split into two tests**:
  - Test A: world has CI = passing → SUT's poll loop returns success immediately
  - Test B: world has CI = pending → SUT's poll loop times out (or hits max retries)

Both branches of the SUT's behavior are covered. The transition itself is GitHub's concern.

- "MergePR fails once, then succeeds" → same split. Test the retry loop with a static failure world (does it retry within bounds?) and the success path separately (does it stop when it works?).

If a test's assertion is "the poll loop called ListChecks exactly 3 times," that assertion is the anti-pattern. The test is proving a property of the stub's script, not the SUT. Delete it — replace it with an assertion on observable behavior (the poll loop returned success, the SUT's final state is X).

---

## File inventory

Touched directly:
- `go/internal/git/testing.go` — rewrite `StubGitHub` and `StubRepo` per above; rename `StubRepo` → `stubRepo` (unexported) and change `NewStubRepo` → `NewStub` returning `Ops`.
- `go/internal/git/git.go` — rename `Repo` struct → `repo` (unexported). `New(cfg Config)` return type stays `Ops` (status quo).
- `go/internal/git/test_helpers_test.go` — delete `capturingGitHub`, `errRunnerImpl`, `errRunner`; add `newRepoForTest`; rewrite `stubManager` to delegate to it or delete it.
- `go/internal/git/ci_test.go` — delete `pollableGitHub`; migrate 5 construction sites; reframe transition-style tests as static-world pairs.
- `go/internal/git/github_test.go` — migrate `capturingGitHub` use.
- `go/internal/git/git_merge_pipeline_test.go` — migrate 3 `capturingGitHub` sites.
- `go/internal/git/git_merge_test.go` — migrate 7 `capturingGitHub` sites and all `gh.ChecksFunc = ...` / `gh.Checks = ...` mutations.
- `go/internal/git/git_branch_test.go`, `merge_stack_test.go`, `resume_test.go`, `runner_test.go`, `testing_test.go` — replace field-mutation configuration; replace `&Repo{...}` struct literals with `newRepoForTest`.
- `go/internal/loop/*_test.go` — migrate every `NewStubRepo()` call to `git.NewStub(cfg)`. Tests that mutate stub fields to configure behavior must move that configuration into the cfg.
- `go/cmd/ralph/main.go` and anywhere else that imports `git.Repo` — change to `git.Ops` (the interface). cmd tests use real git; no stub migration needed there.

One non-test production file changes structurally: `go/internal/git/git.go` — `Repo` renamed to `repo`.

---

## Migration order

Do not attempt this as a single commit. Every commit keeps `go test ./...` green.

1. **Commit 1 — land `stubGitHub` alongside legacy.** Add `StubGitHubConfig`, `StubPR`, `stubGitHub`, `NewStubGitHubCfg(cfg) gitHub`, and the full set of interface methods. Arch guard: `var _ gitHub = (*stubGitHub)(nil)`. Tests still pass because nothing uses the new API yet. **(DONE as of commit 0e47ccf.)**

2. **Commit 1b — land `newRepoForTest` test helper.** In a `_test.go` file inside `internal/git`, add the package-private constructor. No callers yet; build stays green.

3. **Commits 2a–2f — migrate `internal/git` tests.** File by file (grouped where small). For each file:
   - Replace `NewStubGitHub()` + field mutation with `NewStubGitHubCfg(cfg)`.
   - Replace `&Repo{...}` struct literals with `newRepoForTest(cfg, gh, ...)`.
   - Split transition-style tests into static-world variants per the reframing rule.
   - Delete `pollableGitHub`, `capturingGitHub`, `errRunnerImpl` when their last caller is gone.
   - The final commit in this stage deletes the old `StubGitHub`, `NewStubGitHub()` no-arg constructor, and `testing_test.go` (which tested the legacy infrastructure).
   - Also in this final commit: rename `NewStubGitHubCfg` → `NewStubGitHub` (the legacy no-arg version is gone, so the spec-mandated name is free).

4. **Commit 3a — rename `Repo` → `repo` (unexported).** Rename in `git.go` and everywhere `*Repo` or `Repo` appears inside `internal/git`. The `Ops` interface return type for `New()` is unchanged. Outside packages only see `Ops`; they were already using `Ops` where they depended on the interface. Any remaining `*git.Repo` references in loop or cmd get changed to `git.Ops`.

5. **Commit 3b — land `stubRepo` fake alongside legacy `StubRepo`.** Add `StubRepoConfig`, `stubRepo`, `NewStub(cfg StubRepoConfig) Ops`. Uses the same pattern as `stubGitHub`: starting-world data + static returns + per-method error fault injection.

6. **Commits 4–N — migrate `internal/loop` tests.** Replace `NewStubRepo()` + field mutation with `git.NewStub(cfg)`. Loop tests never construct `*git.repo` directly — they hold `git.Ops`.

7. **Commit N+1 — delete legacy.** Delete `StubRepo` exported struct, `NewStubRepo()` no-arg constructor, every exported field. Rename `NewStub` constructor if further naming cleanup makes sense.

8. **Commit N+2 — arch enforcement.** Add tests in `config_arch_test.go` (or a new `stubs_arch_test.go`):
   - `TestNoExportedFieldsOnStubs` — walks `testing.go` with `go/ast`, fails on any exported field or any `func` type on a stub struct
   - `TestStubConstructorsReturnInterfaces` — checks `NewStubGitHub`, `NewStub`, `New` all have interface return types
   - `TestNoSequencedResponseSlices` — walks `StubGitHubConfig` and `StubRepoConfig` fields, fails on any slice-of-slices or slice-of-result-types
   - `TestNoRepoStructLiterals` — walks all `.go` files (including tests), fails on any `&repo{` or `&stubRepo{` construction outside `testing.go` and `test_helpers_test.go`
   - `TestRepoIsUnexported` — fails if `Repo` reappears as an exported identifier

---

## Acceptance criteria

1. `grep -rn '^type Stub\|^type stub' go/internal/git/testing.go` lists exactly `stubGitHub`, `stubRepo` (unexported types), and `StubGitHubConfig`, `StubRepoConfig`, `StubPR` (exported config data).

2. `grep -rn '^type Repo ' go/internal/git` returns zero matches. The exported `Repo` name is gone.

3. `grep -rn '\*git\.Repo\|git\.Repo\b' go/internal/loop go/cmd` returns zero matches. The loop and cmd packages depend only on `git.Ops`.

4. `grep -rn '\(gh\|gm\|stub\)\.\w\+ *=' go/internal/{git,loop}` returns zero matches against stub fields.

5. `grep -rn 'Func *func' go/internal/git/testing.go` returns zero matches.

6. `grep -rn 'StubGitHub{\|StubRepo{\|&StubGitHub\|&StubRepo\|&Repo{\|&repo{\|&stubRepo{' go/internal go/cmd` returns zero matches outside `testing.go` and `test_helpers_test.go`.

7. `grep -rnE 'Responses \[\]\[\]|Responses \[\](MergeResult|int|string|CICheckResult|PRDetail)' go/internal/git/testing.go` returns zero matches.

8. `go test ./internal/git/... ./internal/loop/...` passes. Test count may change where transition-style tests were split into two static-world tests — documented in the commit message.

9. `go vet ./...` clean.

10. Arch tests from Commit N+2 all present and passing.

11. `pollableGitHub`, `capturingGitHub`, `errRunnerImpl`, `errRunner`, `ciTriggerGit` are all absent from the codebase.

12. No type embeds `stubGitHub` or `stubRepo`. `grep -rn '\*stubGitHub\|\*stubRepo' go/internal --include='*_test.go'` returns only declarations in `testing.go` and `test_helpers_test.go`.

---

## Verification plan

- Run `go test ./internal/git/...` after each migrated file. Expect green.
- Run `go test ./internal/loop/...` after each migrated file in Commits 4–N.
- Final: `go test ./...` full suite, `go vet ./...`, then push.

---

## Followup (not this session)

The loop's "integration" tests (`go/internal/loop/loop_integration_test.go`) construct `NewStubRepo()` today — after this rewrite, they'll use `git.NewStub(cfg)`. A better end state (separate handoff) would be: loop integration tests construct a real `*repo` against `initBareRepo`, stubbing only the `gitHub` interface. That gains real-git coverage at the integration layer. Depends on this rewrite landing first, since it needs `newRepoForTest` to be exported for cross-package use (or a new factory like `git.NewForIntegrationTest(cfg, gh) Ops` that wraps `newRepoForTest`).

`stubRunner` is a similar smell one layer down (responses programmed per-key, call-history inspection). Migration to `initBareRepo(t)` where feasible is the right answer; no full rewrite planned.
