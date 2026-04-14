# Fix Loop Extraction

The loop package has three near-identical fix-and-retry flows that were left
unextracted when the rest of `orchestrator-refactor.md` landed. Each spawns a
fix agent, checks whether the agent made commits, and pushes if it did. The
pre/post hooks differ (CI log fetching, conflict marker detection, review
comment filtering) but the scaffold is the same.

## Current state

Three methods on `*Loop` in `go/internal/loop/loop_verify.go`:

- `tryFixCI(ctx, ciErr, nextTask, workDir, rawLogPath) git.CIFixResult`
- `tryFixConflict(ctx, taskID, nextTask, workDir, rawLogPath) bool`
- `tryFixReviewComments(ctx, reviewerName, review, prNumber, nextTask, workDir, rawLogPath) bool`

Each one:

1. Reads `HeadRev()` before running
2. Does fix-type-specific prep (CI log dump, diff files, reviewer filtering)
3. Calls `l.verifier.Spawn*FixAgent(...)` 
4. Reads `HeadRev()` after
5. If `headBefore == headAfter` → returns no-commits result
6. If commits exist → `ForcePush()` and returns applied result

The three return types differ by necessity (`CIFixResult` enum for CI,
`bool` for conflict/review) because the CI caller in `doShip` distinguishes
between "fix applied" and "infrastructure failure — skip retry loop".

## Target

A single `fixLoop(ctx, opts)` helper that takes:

- A `FixValidator` callback to spawn the agent and return a pass/fail signal
- Pre-run data capture (`prepare func() preparedFix`)
- Post-run classification returning a typed result

The three existing `tryFix*` methods become thin wrappers that supply their
fix-specific prep and classification, with the shared HeadRev-before/after
commit-detection and ForcePush living in the helper.

## Why this is worth doing

- The three functions accumulated subtle drift: `tryFixCI` snapshots task
  files for out-of-scope modification detection; the other two don't. A
  shared scaffold makes it obvious when one variant is doing work the
  others should also do.
- Each method is called from exactly one site, so the thin-wrapper risk is
  low — one call site, one wrapper, one place to look.

## Non-goals

- Changing the fix agent prompt templates or validator logic
- Merging the three return types into one — `CIFixResult` stays a tri-state
  because the CI caller needs the "no commits = infrastructure failure"
  signal that conflict/review fixes don't produce

## Acceptance criteria

1. A `fixLoop` helper (function or method) exists in `internal/loop/` that
   owns the shared `HeadRev`-before/after and `ForcePush` steps.
2. `tryFixCI`, `tryFixConflict`, `tryFixReviewComments` call `fixLoop` for
   the shared scaffold rather than inlining it.
3. `grep -c "HeadRev()" internal/loop/loop_verify.go` returns at most 2
   (one for reading in the helper, one for the post-call comparison —
   ideally just 2, not 6 as today).
4. `grep -c "ForcePush" internal/loop/loop_verify.go` returns exactly 1.
5. All existing tests in `loop_verify_test.go` and `loop_ship_ci_test.go`
   continue to pass.
6. `go test ./internal/loop/...` passes.

## File inventory

- `go/internal/loop/loop_verify.go` — extract `fixLoop`, rewrite the three
  `tryFix*` methods as wrappers
- `go/internal/loop/loop_verify_test.go` — may need adjustment if test
  stubs depended on the separate call structure
