# Stub interface rewrite — handoff

> **Goal**: `StubGitHub` and `StubRepo` must be proper in-memory fakes — each one implements exactly the production interface and exposes no public surface beyond that interface. The subject under test interacts with the stub through interface methods only, identical to how it would interact with the production type. `NewStubX(cfg)` is a thin factory that hands back an interface value with its internal state pre-loaded from `cfg`; the factory is not a configuration seam in its own right, and there is no other way to get an instance.

Read the **Testing → Stubs and test doubles** section of `AGENTS.md` before touching code. The rules there are binding; this handoff is the concrete migration plan for applying them to the existing `internal/git` scaffolding.

---

## Target shape (what to build)

### 1. `StubGitHub` implements `gitHub` and nothing else

The production interface lives at `go/internal/git/github.go:131` (`type gitHub interface`). `ghCLI` is the production implementation. `stubGitHub` is the test implementation. Both expose the same 22 methods to callers — period.

```go
// go/internal/git/testing.go (after rewrite)

// StubGitHubConfig declares the canned responses a stub will play back.
// Unset fields yield zero-value responses; the stub never falls through to
// real HTTP or CLI calls.
type StubGitHubConfig struct {
    Available bool

    // Sequenced responses: one entry consumed per call. Out-of-range calls
    // reuse the last entry. Errors slice is indexed identically; an error
    // short-circuits the corresponding value.
    ListChecksResponses [][]CICheckResult
    ListChecksErrors    []error

    CreatePRResponses   []int
    CreatePRErrors      []error

    MergePRResponses    []MergeResult

    // Single-value responses (no test needs multi-call variation here today).
    FindOpenPRNumber   int
    FindOpenPRErr      error
    FindPRNumber       int
    FindPRTitle        string
    FindPRURL          string
    FindPRErr          error
    SearchPRNumber     int
    PRDiffOutput       string

    GetPRResponses     []*PRDetail
    GetPRErrors        []error

    ListOpenPRBranches []string
    ReopenPRErr        error
    CreatePRViaAPINum  int
    CreatePRViaAPIErr  error
    JobStepCount       int
    ListAllPRs         []PRInfo
    ListAllPRsErr      error
    ActiveReviewers    []Reviewer
    DetectReviewersErr error
    PollReviewResult   *AutoReview
    PollReviewErr      error
    RequiredChecks     []string
    RequiredChecksErr  error
    ReplyToReviewErr   error
    FetchThreadIDs     map[int]string
    FetchThreadIDsErr  error
    ResolveThreadErr   error
    EditPRErr          error
    RunLog             string
    HeadSHA            string
}

// stubGitHub is unexported. Tests receive it through the gitHub interface.
type stubGitHub struct {
    cfg StubGitHubConfig

    // Private call indices for sequenced responses.
    listChecksIdx int
    createPRIdx   int
    mergePRIdx    int
    getPRIdx      int

    // Private call records. Expose via read-only accessors if a test
    // needs to assert a call happened; never via direct field access.
    mergeCallCount int
    lastMergeOpts  MergeOpts
    editPRTitle    string
}

// NewStubGitHub returns a stubGitHub configured from cfg.
// The return type is the interface, so callers cannot see internal state.
func NewStubGitHub(cfg StubGitHubConfig) gitHub {
    return &stubGitHub{cfg: cfg}
}

// NewStubGitHubForInspection is the only constructor that returns the
// concrete type. Use this exclusively when a test must assert on recorded
// calls (merge count, last merge opts, etc.). The returned value still
// satisfies gitHub and is passed to the subject under test through the
// interface; the concrete pointer is kept only for inspection.
func NewStubGitHubForInspection(cfg StubGitHubConfig) *stubGitHub {
    return &stubGitHub{cfg: cfg}
}

// Inspection accessors (read-only).
func (s *stubGitHub) MergeCallCount() int         { return s.mergeCallCount }
func (s *stubGitHub) LastMergeOpts() MergeOpts    { return s.lastMergeOpts }
func (s *stubGitHub) LastEditPRTitle() string     { return s.editPRTitle }
```

Interface methods read from `cfg` and advance the indices. Example:

```go
func (s *stubGitHub) ListChecks(pr int, repo string) ([]CICheckResult, error) {
    i := s.listChecksIdx
    if i >= len(s.cfg.ListChecksResponses) {
        i = len(s.cfg.ListChecksResponses) - 1
        if i < 0 {
            return nil, nil
        }
    }
    s.listChecksIdx++
    var err error
    if i < len(s.cfg.ListChecksErrors) {
        err = s.cfg.ListChecksErrors[i]
    }
    if err != nil {
        return nil, err
    }
    return s.cfg.ListChecksResponses[i], nil
}
```

### 2. `StubRepo` implements `Ops` and nothing else

Same pattern applied to `Ops` (22+ methods on `go/internal/git/gitops.go`). `StubRepoConfig` carries the pre-arranged state and sequenced responses. `stubRepo` is unexported; `NewStubRepo(cfg) Ops` returns the interface. A parallel `NewStubRepoForInspection` returns the concrete pointer for call-tracking assertions.

The stub's internal GitHub dependency is built from `cfg.GitHub StubGitHubConfig` — one config in, one fake out, no nested mutation.

### 3. Every call site is rewritten

There is one construction shape. Every test file builds its stub like this:

```go
gh := NewStubGitHub(StubGitHubConfig{
    Available: true,
    ListChecksResponses: [][]CICheckResult{
        {{Name: "test", Bucket: "pending"}},
        {{Name: "test", Bucket: "pending"}},
        {{Name: "test", Bucket: "pass"}},
    },
})
mgr := &Repo{github: gh, logger: &testLog{}}
```

Not:
```go
gh := NewStubGitHub()
gh.Checks = ...      // FORBIDDEN
gh.ChecksFunc = ...  // FORBIDDEN
```

---

## What is explicitly forbidden

These patterns were present in the codebase before this rewrite and must not appear after it. Each rule is an invariant, not a guideline.

1. **No exported fields on any stub type.** Every field on `stubGitHub` and `stubRepo` is lowercase. If a test needs to read recorded state (e.g. "how many times was MergePR called"), expose a read-only accessor method. Tests never assign to stub fields.

2. **No callback-valued fields of any shape.** Configuration fields of type `func(...)` are banned. This includes the variants the previous attempt was about to introduce (`ListChecksFunc`, `CreatePRFunc`, `ShipFunc`, `HeadRevFunc`, `MergeRetryFunc`, `DiffFilesBetweenFunc`, `FlushUnpushedWorkFunc`, `OnRenameBranch`, `OnDetectActiveReviewers`, `OnMerge`, and any new `*Func` field). Multi-call behavior is expressed as sequenced response slices on the config, indexed by a private call counter inside the stub.

3. **No partial-stub hybrids.** No type embeds a stub and overrides one of its methods. Every stub is a single type that implements the full interface. The previous scaffolds `pollableGitHub`, `capturingGitHub`, `errRunnerImpl`, and `ciTriggerGit` are deleted.

4. **Constructors return the interface type, not the concrete pointer.** `NewStubGitHub(cfg) gitHub`, `NewStubRepo(cfg) Ops`. The concrete pointer is only returned by the `*ForInspection` variant, and only when a test genuinely needs to read recorded state — which it reads via accessor methods, not field access.

5. **`NewStubX()` with no arguments is removed.** Every test must state its stub config. A test that truly needs defaults passes `StubGitHubConfig{Available: true}` (or similar) — the zero-value config is `StubGitHubConfig{}` which yields an unavailable GitHub with empty responses.

6. **The stub does not delegate to a sub-stub.** Today `StubRepo.FindOpenPRForBranch` falls through to `s.GH.FindOpenPR` when `OpenPR == 0`. That produces two implicit behavior layers per call. After the rewrite, `stubRepo` either serves the response from its own config or returns a zero value. Tests that need GitHub-layer behavior configure `cfg.GitHub` on the `StubRepoConfig`; the stub plumbing constructs an inner `stubGitHub` with that config during `NewStubRepo`.

7. **No test file instantiates a stub through a struct literal.** All construction goes through `NewStubGitHub` / `NewStubRepo`. Struct literals are forbidden because they exposed the mutation surface this rewrite is closing.

8. **Go static dispatch traps do not apply here** — because rule 3 bans the embed-and-override pattern that would expose them. If a future change tempts you to embed a stub and override one method, stop and extend the config instead.

---

## File inventory

Touched directly:
- `go/internal/git/testing.go` — rewrite `StubGitHub` and `StubRepo` per above.
- `go/internal/git/test_helpers_test.go` — delete `capturingGitHub`, `errRunnerImpl`, `errRunner`.
- `go/internal/git/ci_test.go` — delete `pollableGitHub`; migrate 5 construction sites.
- `go/internal/git/github_test.go` — migrate capturingGitHub use at line 171.
- `go/internal/git/git_merge_pipeline_test.go` — migrate 3 capturingGitHub sites.
- `go/internal/git/git_merge_test.go` — migrate 7 capturingGitHub sites and all `gh.ChecksFunc = ...` / `gh.Checks = ...` mutations.
- `go/internal/git/git_branch_test.go`, `merge_stack_test.go`, `resume_test.go`, `runner_test.go`, `testing_test.go` — replace field-mutation configuration with `NewStubGitHub(cfg)`.

All 13 `internal/loop/*_test.go` files that construct `StubRepo` or `NewStubRepo()` — migrate every site. Roughly 100+ lines will change across the loop package. Use `grep -rn 'gm\.GH\.\|gm\.Ship[A-Z]\|gm\.Merge[A-Z]\|gm\.HeadRev[A-Z]\|gm\.Push[A-Z]\|gm\.\w*Func\s*=\|StubRepo{' go/internal/loop/` as the starting call-site list; expect more once underscores and renamed locals are included.

One non-test file changes: `go/internal/git/testing.go` — structural rewrite. Nothing in `go/cmd/ralph/` needs to change (cmd tests use real git, not the stub).

---

## Migration order

Do not attempt this as a single commit.

1. **Commit 1 — land the new stub API.** Add `StubGitHubConfig`, `stubGitHub`, `NewStubGitHub(cfg)`, `NewStubGitHubForInspection(cfg)`, and the inspection accessors alongside the existing `StubGitHub` type. Do not delete the old type yet. Arch test: `var _ gitHub = (*stubGitHub)(nil)` at the top of `testing.go`. Tests still pass because nothing uses the new API.

2. **Commit 2 — migrate `internal/git` tests to the new API.** File by file. `go test ./internal/git/...` must pass after each file is migrated. Delete `pollableGitHub`, `capturingGitHub`, `errRunnerImpl` when the last caller is gone. Remove the old `StubGitHub`, `NewStubGitHub()` (no-arg), and every exported field when no caller remains.

3. **Commit 3 — do the same for `StubRepo`.** Add `StubRepoConfig`, `stubRepo`, `NewStubRepo(cfg) Ops`, `NewStubRepoForInspection(cfg)`. Nothing uses them yet.

4. **Commits 4–N — migrate `internal/loop` tests.** One file per commit is fine; one commit per coherent group of tests is fine; a single "loop test migration" commit is fine if every test still passes. Do not break the build between commits. Delete the old `StubRepo`, its exported fields, and the old constructor when the last caller is gone.

5. **Commit N+1 — arch enforcement.** Add `TestNoExportedFieldsOnStubs` to `go/internal/git/arch_test.go` (or create it) that walks `testing.go` with `go/ast` and fails on any exported field or any `func` type on a stub struct. Add a `TestStubConstructorsReturnInterfaces` that checks `NewStubGitHub` and `NewStubRepo` have interface return types. These tests lock the invariants so regression is a CI failure, not a code review catch.

---

## Acceptance criteria

1. `grep -rn '^type Stub\|^type stub' go/internal/git/testing.go` lists exactly `stubGitHub` and `stubRepo` as unexported types, plus `StubGitHubConfig` and `StubRepoConfig` as exported config structs. No other type names.

2. `grep -rn '\(gh\|gm\|stub\)\.\w\+ *=' go/internal/{git,loop}` returns zero matches against stub fields. (Writing to fields on *real* types like `Repo` or to local test variables is fine; writing to stub fields is the target.)

3. `grep -rn 'Func *func' go/internal/git/testing.go` returns zero matches.

4. `grep -rn 'StubGitHub{\|StubRepo{\|&StubGitHub\|&StubRepo' go/internal` returns zero matches. Every stub is constructed via `NewStubX(cfg)` or `NewStubXForInspection(cfg)`.

5. `go test ./internal/git/... ./internal/loop/...` passes with the same test count as before the rewrite (no tests silently deleted).

6. `go vet ./...` clean.

7. `TestNoExportedFieldsOnStubs` and `TestStubConstructorsReturnInterfaces` are present and pass.

8. `pollableGitHub`, `capturingGitHub`, `errRunnerImpl`, `errRunner`, `ciTriggerGit` are all absent from the codebase. (The last is already deleted as of commit 5667ee3.)

9. No new type embeds `stubGitHub` or `stubRepo`. `grep -rn '\*stubGitHub\|\*stubRepo' go/internal --include='*_test.go'` returns only declarations in `testing.go` itself.

---

## Verification plan

- Run `go test ./internal/git/...` after each migrated file in Commit 2. Expect green. If a test goes red, the migration translated its intent incorrectly — fix it in the same commit, never skip.
- Run `go test ./internal/loop/...` after each migrated file in Commits 4–N.
- Final: `go test ./...` full suite, `go vet ./...`, then push.

---

## What to do if a test seems to require a callback

You will encounter at least one test whose current shape uses `ChecksFunc: func(call int) []CICheckResult { ... }` with branching logic like "if call < 3 return pending else return pass." Do not recreate this as a callback. Express it as a sequence:

```go
// Before (forbidden after rewrite):
gh.ChecksFunc = func(call int) []CICheckResult {
    if call < 3 {
        return []CICheckResult{{Name: "test", Bucket: "pending"}}
    }
    return []CICheckResult{{Name: "test", Bucket: "pass"}}
}

// After (required):
gh := NewStubGitHub(StubGitHubConfig{
    Available: true,
    ListChecksResponses: [][]CICheckResult{
        {{Name: "test", Bucket: "pending"}},
        {{Name: "test", Bucket: "pending"}},
        {{Name: "test", Bucket: "pending"}},
        {{Name: "test", Bucket: "pass"}}, // last entry repeats on subsequent calls
    },
})
```

The sequence form is always equivalent or strictly clearer. If a test's branching logic is genuinely too complex to express as a static sequence, that is a signal the test is doing too much — split it into two tests, each with a simpler sequence.

---

## Followup (not this session)

The loop's "integration" tests (`go/internal/loop/loop_integration_test.go`) construct `NewStubRepo()` and thereby stub the entire git module. Real-git integration coverage lives only in the `internal/git` package's own tests. After this rewrite lands, file a separate handoff to migrate loop integration tests to use real `*git.Repo` against `initBareRepo`, stubbing only the `gitHub` interface. That work depends on this rewrite — it needs the new `stubGitHub` construction-only pattern to be the only shape in the codebase.
