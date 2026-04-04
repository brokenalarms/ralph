# GitHub Module Migration — Hand-off Document

This document describes the current state of the GitHub integration in the git
package and the remaining work to reach the target architecture.

## Context

Ralph shells out to `gh` (GitHub CLI) for all PR operations. The original
implementation used `gh pr` subcommands which return JSON via `--json`/`--jq`.
A systemic bug was discovered: when jq processes an empty array,
`.[0].number` returns the literal string `"null"`, which passes all `!= ""`
guards and flows through the system as a valid PR number. This caused 9 tabi
beads to be falsely closed with "Verified — null open, merge pending".

The fix has two layers:
1. **Immediate**: Migrate `gh pr` calls to `gh api` with proper jq (`// empty`)
2. **Systemic**: Type PR numbers as `int` at system entry (bead ralph-ckpy)

## Current State (as of PR #443)

### Already migrated to `gh api`

| Function | Notes |
|---|---|
| `FindOpenPR` | Uses `gh api repos/{nwo}/pulls` with `// empty`. Has `"null"` guard. |
| `FindPR` | Uses `gh api repos/{nwo}/pulls` with `state=all`. |
| `CreatePRViaAPI` | Uses `gh api --method POST`. Returns PR number as string (needs typing). |
| `MergePR` | Uses `gh api -X PUT .../merge`. Falls back to `gh pr merge --admin`. |
| `deleteBranch` | Uses `gh api --method DELETE`. |
| `GetJobStepCount` | Uses `gh api` for workflow runs. |
| `CheckEnforceAdmins` | Uses `gh api`. |
| `PostEnforceAdmins` | Uses `gh api`. |

### Still using `gh pr` / `gh run` CLI

| Function | Current command | Migration path |
|---|---|---|
| `ListOpenPRBranches` | `gh pr list --json headRefName` | `gh api repos/{nwo}/pulls?state=open --jq '.[].head.ref'` |
| `EditPR` | `gh pr edit {num} --title` | `gh api -X PATCH repos/{nwo}/pulls/{num}` |
| `ListChecks` | `gh pr checks {num} --json name,state,bucket` | `gh api repos/{nwo}/commits/{sha}/check-runs` (needs head SHA) |
| `GetRunLog` | `gh pr checks` + `gh run view --log-failed` | Keep CLI — log streaming has no clean API equivalent |
| `SearchPR` | `gh pr list --search` | `gh api search/issues?q=...` |
| `PRDiff` | `gh pr diff {num}` | `gh api repos/{nwo}/pulls/{num}` with Accept: diff header |
| `ReopenPR` | `gh pr reopen {num}` | `gh api -X PATCH repos/{nwo}/pulls/{num} -f state=open` |
| `GetPRState` | `gh pr view {num} --json state` | `gh api repos/{nwo}/pulls/{num} --jq .state` |
| `GetPRBase` | `gh pr view {num} --json baseRefName` | `gh api repos/{nwo}/pulls/{num} --jq .base.ref` |
| `GetPRHead` | `gh pr view {num} --json headRefName` | `gh api repos/{nwo}/pulls/{num} --jq .head.ref` |
| `GetPRHeadSHA` | `gh pr view {num} --json headRefOid` | `gh api repos/{nwo}/pulls/{num} --jq .head.sha` |
| `ListAllPRs` | `gh pr list --state all --json ...` | `gh api repos/{nwo}/pulls?state=all&per_page=100` (paginate) |

### Consolidation opportunity

`GetPRState`, `GetPRBase`, `GetPRHead`, `GetPRHeadSHA` each make a separate
API call for one field from the same endpoint. These should be consolidated
into a single `GetPR(nwo, number) (*PRDetail, error)` that fetches the full
PR object once. Callers that need multiple fields make one call instead of
three.

## PR Number Typing (bead ralph-ckpy)

The systemic fix for the "null" string bug. Currently all PR numbers flow
through the system as `string`. The fix:

1. Add `ParsePRNumber(raw string) (int, error)` — rejects empty, "null",
   non-numeric, zero, negative
2. Change `GitHub` interface methods to return `int` for PR numbers
3. Change `ShipResult.PRNumber` and `finalizePRParams.prNumber` to `int`
4. All callers use `== 0` instead of `== ""` for "no PR" checks

This is a prerequisite for the API migration — as functions are migrated,
they should return typed PR numbers.

## GitHub Interface

The `GitHub` interface in github.go (line 51) is the correct abstraction
boundary per architecture.md. Production uses `ghCLI`; tests inject stubs.
This is clean and should be preserved.

The interface currently has ~20 methods. After consolidating the
`GetPR{State,Base,Head,HeadSHA}` family into `GetPR`, it drops to ~17.
This is acceptable for the domain surface area.

## GitOps / Manager Relationship

Separate from the GitHub migration, there is a broader architectural concern:
`GitOps` in gitops.go is an 80-method interface that mirrors `Manager` 1:1.
Every method is a trivial forwarder. This is not a real abstraction — it's a
god object behind an interface.

The target architecture (per architecture.md) is:
- `Manager` is the git module's orchestrator — it composes focused helpers
- The loop should depend on focused interfaces (`GitHub`, `Worktree`,
  `BranchOps`) not a monolithic `GitOps`
- `Manager` can satisfy multiple focused interfaces

This decomposition is separate from the GitHub API migration and should be
planned as its own spec. Do not attempt to refactor GitOps while migrating
GitHub calls — these are independent concerns.

## Migration Order

1. **PR number typing** (ralph-ckpy) — prerequisite, prevents "null" class bugs
2. **Consolidate GetPR family** — reduce 4 calls to 1, simplifies interface
3. **Migrate remaining `gh pr` calls** — one function at a time, each its own
   bead if needed
4. **GetRunLog stays on CLI** — log streaming doesn't have a clean API path

Each migration should update both `ghCLI` and the test stubs. The `GitHub`
interface is the contract — update it first, then fix implementations.

## Key Files

- `go/internal/git/github.go` — GitHub interface + ghCLI implementation
- `go/internal/git/gitops.go` — GitOps interface (Manager mirror, separate concern)
- `go/internal/git/git_merge.go` — Ship, Push, merge retry (consumes PR numbers)
- `go/internal/loop/loop_iteration.go` — handlePostSignal (consumes ShipResult)
- `docs/specs/architecture.md` — target architecture spec
