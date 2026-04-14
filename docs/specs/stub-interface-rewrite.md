# Stub interface rewrite — handoff

> **Goal**: `StubGitHub` and `StubRepo` must be true in-memory fakes — each one implements exactly the production interface and exposes no public surface beyond that interface. The subject under test interacts with the stub through interface methods only, identical to how it would interact with the production type. `NewStubX(cfg)` is a thin factory that hands back an interface value with its internal world pre-loaded from `cfg`; the factory is not a configuration seam in its own right, and there is no other way to get an instance.

Read the **Testing → Stubs and test doubles** section of `AGENTS.md` before touching code. The rules there are binding; this handoff is the concrete migration plan for applying them to the existing `internal/git` scaffolding.

---

## Design principle

The fake's behavior is **fixed and centralized**. Every test runs against the same behavior model. What varies per test is the initial **state of the world** — which PRs exist, what their checks are, what reviewers are configured — passed in as data through `StubGitHubConfig`.

Tests do not program the fake's responses per call. There are no sequenced-response slices, no callback fields, no inline override helpers. If a test seems to need "pending three times, then passing," it is testing a property of GitHub rather than a property of the SUT — split it into two static-world tests (pending → keeps polling or times out; passing → stops and returns success) and the need disappears.

The fake models GitHub's state transitions the way real GitHub does: `MergePR` flips a PR to merged, `CreatePR` adds a PR to the world, `ReopenPR` flips closed → open. After the SUT runs, the fake's state reflects what the SUT did — queryable through the interface methods the SUT itself uses. No stub-internal fields are ever read or written by tests.

---

## Target shape (what to build)

### 1. `StubGitHub` implements `gitHub` and nothing else

The production interface lives at `go/internal/git/github.go:131` (`type gitHub interface`). `ghCLI` is the production implementation. `stubGitHub` is the test implementation. Both expose the same 22 methods to callers — period.

```go
// go/internal/git/testing.go (after rewrite)

// StubGitHubConfig declares the initial state of the in-memory fake's world.
// Every field is plain data. None influences the fake's behavior as code —
// the behavior is fixed in the method implementations.
type StubGitHubConfig struct {
    Available bool

    // The world. PRs that exist when the test starts; the fake mutates this
    // world in response to interface calls (MergePR flips State, CreatePR
    // appends, etc.) so subsequent reads reflect what the SUT did.
    PRs    []StubPR
    Checks map[int][]CICheckResult // CI checks keyed by PR number

    // Static return values for pure-read methods.
    RunLog           string
    Reviewers        []Reviewer
    RequiredChecks   []string
    JobStepCount     int
    PollReviewResult *AutoReview
    FetchThreadIDs   map[int]string

    // Fault injection — one error field per method that can fail.
    // A test needing "CreatePR returns an error" sets CreatePRErr.
    // Single value per field; no sequencing.
    CreatePRErr           error
    CreatePRViaAPIErr     error
    EditPRErr             error
    ReopenPRErr           error
    ListChecksErr         error
    FindOpenPRErr         error
    FindPRErr             error
    GetPRErr              error
    ListAllPRsErr         error
    SearchPRErr           error
    PRDiffErr             error
    GetJobStepCountErr    error
    DetectReviewersErr    error
    PollReviewErr         error
    RequiredChecksErr     error
    ReplyToReviewErr      error
    FetchThreadIDsErr     error
    ResolveThreadErr      error
    ListOpenPRBranchesErr error
    PRDiffOutput          string
}

// StubPR describes a PR that exists in the fake's world.
type StubPR struct {
    Number  int
    Title   string
    URL     string  // derived as https://github.com/owner/repo/pull/<Number> when empty
    Branch  string  // head ref — FindOpenPR/FindPR match on this
    Base    string  // defaults to "main" when empty
    HeadSHA string  // derived as "stub-sha-<Number>" when empty
    State   PRState // defaults to PRStateOpen when empty
}

// stubGitHub is the in-memory fake. Unexported. Tests receive it only
// through the gitHub interface.
type stubGitHub struct {
    cfg StubGitHubConfig
    prs map[int]*StubPR // mutable copy of cfg.PRs, keyed by number
    nextPRNumber int
}

// NewStubGitHub returns a stubGitHub initialized with cfg's world.
// The return type is the interface, so callers cannot see internal state.
func NewStubGitHub(cfg StubGitHubConfig) gitHub {
    s := &stubGitHub{cfg: cfg, prs: make(map[int]*StubPR)}
    maxNum := 0
    for i := range cfg.PRs {
        pr := cfg.PRs[i] // copy
        s.prs[pr.Number] = &pr
        if pr.Number > maxNum {
            maxNum = pr.Number
        }
    }
    s.nextPRNumber = maxNum + 1
    return s
}
```

Interface methods read and write the world. Examples:

```go
// GetPR returns the PR's current state from the world.
func (s *stubGitHub) GetPR(nwo string, prNumber int) (*PRDetail, error) {
    if s.cfg.GetPRErr != nil {
        return nil, s.cfg.GetPRErr
    }
    pr, ok := s.prs[prNumber]
    if !ok {
        return nil, nil
    }
    return &PRDetail{
        State:   pr.State,
        BaseRef: pr.Base,
        HeadRef: pr.Branch,
        HeadSHA: pr.HeadSHA,
    }, nil
}

// MergePR flips the PR's state to merged in the world and returns a result
// derived from what it found.
func (s *stubGitHub) MergePR(prNumber int, _ string, _ MergeOpts) MergeResult {
    pr, ok := s.prs[prNumber]
    if !ok {
        return MergeResult{Merged: false, Message: "PR not found"}
    }
    if pr.State != PRStateOpen {
        return MergeResult{Merged: false, Message: "PR not open"}
    }
    pr.State = PRStateMerged
    return MergeResult{Merged: true}
}

// CreatePR appends a new PR to the world.
func (s *stubGitHub) CreatePR(opts CreatePROpts) (int, error) {
    if s.cfg.CreatePRErr != nil {
        return 0, s.cfg.CreatePRErr
    }
    num := s.nextPRNumber
    s.nextPRNumber++
    s.prs[num] = &StubPR{Number: num, Branch: opts.Head, Base: opts.Base, State: PRStateOpen}
    return num, nil
}

// ListChecks returns the static checks for the given PR.
func (s *stubGitHub) ListChecks(prNumber int, _ string) ([]CICheckResult, error) {
    return s.cfg.Checks[prNumber], s.cfg.ListChecksErr
}
```

### 2. `StubRepo` implements `Ops` and nothing else

Same pattern applied to `Ops` (22+ methods on `go/internal/git/gitops.go`). `StubRepoConfig` carries the pre-arranged state. `stubRepo` is unexported; `NewStubRepo(cfg) Ops` returns the interface.

The stub's internal GitHub dependency is built from `cfg.GitHub StubGitHubConfig` — one config in, one fake out, no nested mutation.

### 3. Every call site is rewritten

There is one construction shape. Every test file builds its stub like this:

```go
gh := NewStubGitHub(StubGitHubConfig{
    Available: true,
    PRs: []StubPR{
        {Number: 42, Branch: "feature", Base: "main", State: PRStateOpen},
    },
    Checks: map[int][]CICheckResult{
        42: {{Name: "ci", Bucket: "pass"}},
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

1. **No exported fields on any stub type.** Every field on `stubGitHub` and `stubRepo` is lowercase. Tests never read or write the fake's internals directly. Observable state is reached through the same interface methods production uses.

2. **No callback-valued fields of any shape.** Configuration fields of type `func(...)` are banned. This includes the variants the previous attempt was about to introduce (`ListChecksFunc`, `CreatePRFunc`, `ShipFunc`, `HeadRevFunc`, `MergeRetryFunc`, `DiffFilesBetweenFunc`, `FlushUnpushedWorkFunc`, `OnRenameBranch`, `OnDetectActiveReviewers`, `OnMerge`, and any new `*Func` field).

3. **No sequenced-response slices.** Fields like `ListChecksResponses [][]CICheckResult` or `MergePRResponses []MergeResult` are banned. These are callbacks in disguise — tests programming per-call behavior into the stub. A method that varies call-over-call in production (e.g. CI that transitions over time) is modeled as state transitions driven by OTHER interface methods (e.g. `MergePR` flips state). A test that seems to need "pending three times then passing" is testing GitHub's behavior, not the SUT's — split it into two static-world tests.

4. **No partial-stub hybrids.** No type embeds a stub and overrides one of its methods. Every stub is a single type that implements the full interface. The previous scaffolds `pollableGitHub`, `capturingGitHub`, `errRunnerImpl`, and `ciTriggerGit` are deleted.

5. **No inline "tiny helper" override types in test files.** If a test thinks it needs a helper that mutates stub state between calls, it is reintroducing the hybrid pattern. Reframe the test as two static-world tests.

6. **Constructors return the interface type, not the concrete pointer.** `NewStubGitHub(cfg) gitHub`, `NewStubRepo(cfg) Ops`. There is no `*ForInspection` variant — tests that want to assert on "what happened" assert through the same interface methods production uses (the fake's state after the SUT ran), or on the SUT's own return values.

7. **`NewStubX()` with no arguments is removed.** Every test must state its starting world. The zero-value config is `StubGitHubConfig{}` which yields an unavailable GitHub with no PRs.

8. **The stub does not delegate to a sub-stub.** Today `StubRepo.FindOpenPRForBranch` falls through to `s.GH.FindOpenPR` when `OpenPR == 0`. That produces two implicit behavior layers per call. After the rewrite, `stubRepo` either serves the response from its own world or returns a zero value. Tests that need GitHub-layer behavior configure `cfg.GitHub` on the `StubRepoConfig`; the stub plumbing constructs an inner `stubGitHub` with that config during `NewStubRepo`.

9. **No test file instantiates a stub through a struct literal.** All construction goes through `NewStubGitHub` / `NewStubRepo`. Struct literals are forbidden because they exposed the mutation surface this rewrite is closing.

---

## How to rewrite tests that seem to need sequenced responses

This is the judgment call that comes up most. Every existing test that uses `ChecksFunc: func(call int) []CICheckResult { ... }` or similar sequenced logic needs to be reframed. The pattern:

- "CI pending N times then passing" → **split into two tests**:
  - Test A: world has CI = passing → SUT's poll loop returns success immediately
  - Test B: world has CI = pending → SUT's poll loop times out (or hits max retries, or whatever its configured bound is)

Both branches of the SUT's behavior are covered. The transition itself is GitHub's concern, not the SUT's.

- "MergePR fails once, then succeeds" → same split. Test the retry logic with a static failure world (does it retry?) and the success path separately (does it stop when it works?).

If a test's assertion is "the poll loop called ListChecks exactly 3 times," that assertion is the anti-pattern. The test is proving a property of the stub's script, not the SUT. Delete it — replace it with an assertion on observable behavior (the poll loop returned success, the SUT's final state is X).

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

All `internal/loop/*_test.go` files that construct `StubRepo` or `NewStubRepo()` — migrate every site. Use `grep -rn 'gm\.GH\.\|gm\.Ship[A-Z]\|gm\.Merge[A-Z]\|gm\.HeadRev[A-Z]\|gm\.Push[A-Z]\|gm\.\w*Func\s*=\|StubRepo{' go/internal/loop/` as the starting call-site list; expect more once underscores and renamed locals are included.

One non-test file changes: `go/internal/git/testing.go` — structural rewrite. Nothing in `go/cmd/ralph/` needs to change (cmd tests use real git, not the stub).

---

## Migration order

Do not attempt this as a single commit.

1. **Commit 1 — land the new stub API.** Add `StubGitHubConfig`, `StubPR`, `stubGitHub`, `NewStubGitHubCfg(cfg) gitHub`, and the full set of interface methods alongside the existing `StubGitHub` type. Name the new constructor `NewStubGitHubCfg` temporarily to avoid colliding with the legacy zero-arg `NewStubGitHub()`; the final commit renames it to `NewStubGitHub` after the legacy is removed. Arch guard: `var _ gitHub = (*stubGitHub)(nil)` at the top of testing.go. Tests still pass because nothing uses the new API.

2. **Commit 2 — migrate `internal/git` tests to the new API.** File by file (or grouped commits per coherent batch). `go test ./internal/git/...` must pass after each file is migrated. When a test used sequenced responses or a hybrid override, split it into static-world tests per the reframing rule. Delete `pollableGitHub`, `capturingGitHub`, `errRunnerImpl` when the last caller is gone. The final commit in this stage removes the old `StubGitHub`, `NewStubGitHub()` (no-arg), every exported field, `testing_test.go` (which tested the old infrastructure), and renames `NewStubGitHubCfg` → `NewStubGitHub`.

3. **Commit 3 — do the same for `StubRepo`.** Add `StubRepoConfig`, `stubRepo`, `NewStubRepoCfg(cfg) Ops`. Nothing uses it yet.

4. **Commits 4–N — migrate `internal/loop` tests.** One commit per coherent group of tests is fine; a single "loop test migration" commit is fine if every test still passes. Do not break the build between commits. Delete the old `StubRepo`, its exported fields, and rename `NewStubRepoCfg` → `NewStubRepo` when the last caller is gone.

5. **Commit N+1 — arch enforcement.** Add `TestNoExportedFieldsOnStubs` to `go/internal/git/config_arch_test.go` that walks `testing.go` with `go/ast` and fails on any exported field or any `func` type on a stub struct. Add a `TestStubConstructorsReturnInterfaces` that checks `NewStubGitHub` and `NewStubRepo` have interface return types. Add a `TestNoSequencedResponseSlices` that fails if any field on `StubGitHubConfig` or `StubRepoConfig` is a slice-of-slices or a slice of result types (sequenced responses). These tests lock the invariants so regression is a CI failure, not a code review catch.

---

## Acceptance criteria

1. `grep -rn '^type Stub\|^type stub' go/internal/git/testing.go` lists exactly `stubGitHub` and `stubRepo` as unexported types, plus `StubGitHubConfig`, `StubRepoConfig`, and `StubPR` as exported config structs. No other type names.

2. `grep -rn '\(gh\|gm\|stub\)\.\w\+ *=' go/internal/{git,loop}` returns zero matches against stub fields. (Writing to fields on *real* types like `Repo` or to local test variables is fine; writing to stub fields is the target.)

3. `grep -rn 'Func *func' go/internal/git/testing.go` returns zero matches.

4. `grep -rn 'StubGitHub{\|StubRepo{\|&StubGitHub\|&StubRepo' go/internal` returns zero matches. Every stub is constructed via `NewStubGitHub(cfg)` or `NewStubRepo(cfg)`.

5. `grep -rnE 'Responses \[\]\[\]|Responses \[\](MergeResult|int|string|CICheckResult|PRDetail)' go/internal/git/testing.go` returns zero matches. No sequenced-response slices anywhere.

6. `go test ./internal/git/... ./internal/loop/...` passes. Test count may change where transition-style tests were split into two static-world tests — this is expected and desirable. The migration commit that performs a split documents the rationale in its commit message.

7. `go vet ./...` clean.

8. `TestNoExportedFieldsOnStubs`, `TestStubConstructorsReturnInterfaces`, and `TestNoSequencedResponseSlices` are present and pass.

9. `pollableGitHub`, `capturingGitHub`, `errRunnerImpl`, `errRunner`, `ciTriggerGit` are all absent from the codebase.

10. No new type embeds `stubGitHub` or `stubRepo`. `grep -rn '\*stubGitHub\|\*stubRepo' go/internal --include='*_test.go'` returns only declarations in `testing.go` itself.

---

## Verification plan

- Run `go test ./internal/git/...` after each migrated file. Expect green. If a test goes red, the migration translated its intent incorrectly — fix it in the same commit, never skip.
- Run `go test ./internal/loop/...` after each migrated file in Commits 4–N.
- Final: `go test ./...` full suite, `go vet ./...`, then push.

---

## Followup (not this session)

The loop's "integration" tests (`go/internal/loop/loop_integration_test.go`) construct `NewStubRepo()` and thereby stub the entire git module. Real-git integration coverage lives only in the `internal/git` package's own tests. After this rewrite lands, file a separate handoff to migrate loop integration tests to use real `*git.Repo` against `initBareRepo`, stubbing only the `gitHub` interface. That work depends on this rewrite — it needs the new `stubGitHub` construction-only pattern to be the only shape in the codebase.
