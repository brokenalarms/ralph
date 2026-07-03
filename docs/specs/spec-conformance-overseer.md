# Spec-Conformance Overseer — Draft Proposal

Status: **Draft — for user review.** This is a proposal, not a decision record.
It makes concrete recommendations at every decision point per ralph-rtg6; the
user should redirect anything they'd rather see done differently before it is
beaded and released.

## Problem

Ralph built itself with no conformance gate. Every merge runs through tests
and an LLM correctness/AC verifier (`internal/verify`), but nothing ever asks
the broader question: *does this merged work still match the spec it was
implementing, and does it still fit the target architecture?* A task can pass
its own acceptance criteria and still drift from the feature spec's intent, or
quietly violate an architecture.md rule (e.g. a new `exec.Command("git", ...)`
outside `internal/git` that the depguard/`arch_boundary_test.go` gate happens
not to cover). Nothing re-reads recent merges against spec intent, so drift
compounds silently until an unrelated future task collides with it.

Scope is broader than `docs/specs/architecture.md`: any batch of merged tasks
must be judged against **whatever spec(s) those tasks were actually
implementing** — the feature spec under `docs/specs/` they came from, in
addition to architecture.md, which every task is implicitly judged against
regardless of which feature spec (if any) it belongs to.

## Reuse: the closure-audit is the same shape

`go/cmd/ralph/prompts/task-manager.md` already has a **Recent-closure audit**
(`## Recent-closure audit`, startup check at "Recent-closure audit check
(non-blocking)"): on `ralph task` startup, it reads a timestamp marker
(`{{RALPH_DIR}}/last-audit.timestamp`), finds `ralph-loop`-closed beads newer
than that marker, and — only if the user says `yes` — re-reads each merge
commit/PR diff and judges it criterion-by-criterion against the bead's own
acceptance criteria, surfacing mismatches. `no`/`skip` dismiss semantics and
the marker-write step are already fully specified.

This is the exact shape a spec-conformance audit needs: re-read recent merges,
judge against criteria, surface findings. The only thing that changes is the
criteria — acceptance criteria (what the bead promised) becomes spec intent
(what the spec the bead's work belongs to actually says), and the corpus is
no longer just one bead's diff but the *cumulative* effect of a batch. The
overseer is **not** new machinery: it is a second, parallel audit dimension
inside the same startup-check → ask → run-on-yes → write-marker pattern,
added to `task-manager.md` alongside the existing one rather than replacing
or entangling with it. They get independent markers so answering `no`/`skip`
to one never silently dismisses the other.

## Design

### 1. Trigger cadence — recommendation: batch-of-N, not per-commit or wall-clock

Per-commit is too noisy — the loop ships many small commits per bead, and
per-commit spec judgment would fire the ask constantly during a healthy
draining run. A wall-clock interval (every N minutes/hours) doesn't fit
ralph's actual rhythm: the loop runs in bursts gated by `calls_per_hour` and
`max_iterations`, then sits idle between sessions, so a calendar-based timer
would fire at arbitrary, contextless moments.

**Recommendation: batch by count of `ralph-loop`-closed beads since the last
spec audit**, mirroring the existing closure-audit's marker mechanism but
counting beads instead of gating on presence-since-timestamp alone:

- New marker file `{{RALPH_DIR}}/last-spec-audit.timestamp` (same shape as
  `last-audit.timestamp` — a Unix timestamp, missing = epoch 0), kept
  independent from the AC-closure marker.
- New config knob `spec_audit_batch_size` (int, default **5**), following the
  existing `<subject>_threshold`/`<subject>_limit` naming family in
  `go/internal/config/config.go` (`CascadeSkipLimit`, `TestSaturationThreshold`,
  `StagnationThreshold`). Read the same way `MaxCompactionParks` etc. are —
  a plain `Config` field with a `FlagDef` entry, no new CLI surface beyond
  what the existing threshold knobs already get.
- At `ralph task` startup, alongside the existing closure-audit check: count
  `ralph-loop`-closed beads with close time after the marker. If the count is
  `>= spec_audit_batch_size`, surface the ask (same non-blocking style as the
  closure-audit: append the question, never run automatically). If it is
  below threshold, stay silent — exactly like the existing check's "if none,
  remain silent" rule.
- **On-demand override**: the user can always type an explicit request (e.g.
  "audit spec conformance") during a `ralph task` session to run it early,
  regardless of batch size — the batching gates the unprompted ask, not the
  capability itself.

### 2. Mapping a batch of commits to the spec(s) it implemented

Today there is no way to answer "which `docs/specs/*.md` file was this bead
implementing?" — `bd.go` has no `spec:` label convention (confirmed: no
existing label pattern beyond `skipped`). This has to be introduced, not
inferred after the fact from a diff.

**Recommendation:** extend bead creation, not audit time. When a bead is
created as part of implementing a feature spec under `docs/specs/`, tag it
with a label `spec:<slug>` where `<slug>` is the spec's filename without
`.md` (e.g. a bead implementing `docs/specs/stacked-prs.md` gets
`--labels spec:stacked-prs`). This is a one-line addition to the "Creating
beads" guidance already in `task-manager.md` — the task manager already knows
which spec it's beading out, since it either wrote the spec in the same
session or is decomposing one the user pointed at.

At audit time, group the closed batch by their `spec:` label (via `bd show
<id> --json`, same call the closure-audit already makes to fetch acceptance
criteria):

- Beads with a `spec:<slug>` label → judge their merged diff against
  `docs/specs/<slug>.md` (or `docs/specs/done/<slug>.md` if it has since been
  retired there) **and** `docs/specs/architecture.md`.
- Beads with no `spec:` label (most bug fixes, small tasks, refactors without
  a driving feature spec) → judge against `docs/specs/architecture.md` only.

This is what makes the overseer's scope broader than architecture.md per AC
#3: architecture.md is always in the judged set, but it is not the only
document — any feature spec a bead's label points at joins it.

### 3. Drift-reporting mechanism — recommendation: new beads, never a hard block

The bead text is explicit: "surface findings as beads." This differs from the
existing AC closure-audit, which only *presents* findings in chat — the
spec-conformance audit goes one step further and files them as tracked work,
because spec drift is exactly the kind of finding that needs to survive past
the end of the chat session (unlike an AC mismatch, which the user typically
acts on immediately in the same conversation).

- For each confirmed drift finding: `bd create -a=ralph-task --type=bug
  --labels=conformance-drift,spec:<slug-or-architecture>` referencing the
  offending bead ID, its merge commit/PR, and the specific spec section or
  architecture.md rule violated. Created **owned** by `ralph-task`, echoed to
  the user, and released through the same single post-create review gate as
  every other bead ("Bead ownership" / "The one release gate" in
  `task-manager.md`) — no special-casing.
- Never a block. The overseer must not gate the loop, CI, or a merge — it
  runs after the fact, asynchronously, and its only output is beads a human
  reviews before they reach the loop. This matches the task manager's stated
  session goal ("maximize unattended loop runtime") and the existing "No
  manual-verify gating" rule: an automatic hard stop on drift would directly
  contradict both.
- Write the marker (`last-spec-audit.timestamp`) only after the audit
  completes or the user explicitly says `skip`, mirroring the existing
  dismiss semantics (`yes` → run + write marker; `no` → skip this session,
  re-ask next time; `skip` → write marker without running).

### 4. Primary placement — recommendation: `ralph task` periodic audit

Three placements were in scope per the bead. Evaluated against what already
exists in the repo:

- **A `ralph-task` periodic audit** (recommended). This is a direct extension
  of the existing closure-audit, which already lives here, already has the
  ask/dismiss/marker machinery, and already runs with full interactive LLM
  judgment and complete repo context (it can open any file under
  `docs/specs/`, trace call sites, read current code state — exactly what
  spec-conformance judgment needs, per the closure-audit's own "not
  mechanical reconciliation" principle).
- **A loop post-merge gate** (rejected as primary). The loop's only per-merge
  hook today is `execRunPostTask` (`go/internal/loop/loop_iteration.go`),
  which calls `verify.RunPostTask` — a lightweight **shell script** hook
  (runs a configured command or an npm/Makefile target, with a few env vars),
  not an LLM call. Wiring a full spec-conformance LLM judgment into this path
  would add real latency and cost to every single merge — the hottest path
  in the whole system — and directly fights the "maximize unattended loop
  runtime" goal that governs the task manager's own release discipline. It
  also has none of the ask/skip/batch semantics an audit needs; it fires
  unconditionally on every task.
- **A CI check** (rejected as primary). `.github/workflows/` today has only
  `test.yml` (Go build/lint/test) and `bump-version.yml` — no existing
  LLM-based check to piggyback on. Standing one up means new CI credentials,
  a new workflow file, and a harness for "judge this diff against an
  arbitrary spec file" with none of the existing interactive-session context
  the task manager already has for free. This is strictly more new surface
  than extending the task-manager prompt.

Recommendation: **`ralph task` periodic audit**, extending `task-manager.md`'s
existing closure-audit section with a second, sibling audit. Nothing new is
built for triggering, dismissing, or storage — only the judgment criteria and
the spec-to-bead mapping (label) are new.

## What the new `task-manager.md` section looks like (concretely)

A new `## Spec-conformance audit` section, placed immediately after `##
Recent-closure audit`, structured identically:

1. Startup check (non-blocking ask), gated on `spec_audit_batch_size` instead
   of "any unaudited closure," using `last-spec-audit.timestamp`.
2. Scope: `ralph-loop`-closed beads only (self-work is exempted, same
   rationale as the existing audit — self-work is already known-correct by
   the person who did it).
3. Procedure: for each bead in the batch, resolve its spec set (§2 above),
   open the current state of the affected code (not just the diff — same
   "diff is the starting point, not the whole job" principle as the existing
   audit), and judge conformance to each spec in the set.
4. Findings become beads (§3 above), never inline-only presentation.
5. Dismiss semantics identical to the existing audit (`yes`/`no`/`skip`).

## Follow-up implementation beads

1. **ralph task: add `spec_audit_batch_size` config knob** — add the field to
   `go/internal/config/config.go` + `flags.go` (default 5), following the
   existing threshold/limit knob pattern.
2. **ralph task: `spec:<slug>` bead label convention at creation time** —
   update the "Creating beads" guidance in `task-manager.md` (and
   `bead-creation.md` if it duplicates the labeling rules) so beads created
   while decomposing a `docs/specs/*.md` file get `--labels spec:<slug>`.
   Depends on nothing; can land first.
3. **ralph task: `last-spec-audit.timestamp` marker + batch-size startup
   check** — add the non-blocking startup check (mirroring the existing
   closure-audit check) that counts `ralph-loop`-closed beads since the
   marker and asks when `>= spec_audit_batch_size`. Depends on #1.
4. **ralph task: `## Spec-conformance audit` procedure section in
   task-manager.md** — write the full audit procedure (spec-set resolution,
   conformance judgment, finding-to-bead creation, dismiss semantics) as
   specified above. Depends on #2 and #3. Its acceptance criteria must
   require moving this spec file to `docs/specs/done/` and confirming no
   source file still references `docs/specs/spec-conformance-overseer.md`,
   once the section is implemented and the marker/label mechanics are live.
