# Loop ready-sort: default to bd's `hybrid`, delete the Go re-sort

Status: **Implemented.** Delivered via
**ralph-45w2** (Stage-2 passthrough + delete Go re-sort — merged, PR #745) and
**ralph-8n36** (split `getNextIssue` into Stage-1 resume / Stage-2 — merged).
Related prompt change: **ralph-fumr** (priority-assignment rubric + no
hold-vs-release). This doc is now a design record, not a to-do.
Author: ralph-task session 2026-06-30/07-01.

## Problem

Priority ordering starves lower-priority beads: a steady stream of P1s means
older P2s never run and rot (go redundant / diverge from the current spec).

## Fix (two small changes)

bd's `bd ready --sort hybrid` already solves this — no bespoke ordering, no
metadata, no CLI surface. `getNextIssue()` in `go/internal/tasks/bd.go` splits
into two stages:

1. **Split out resume (Stage 1).** Keep the existing WIP-preemption check as its
   own step: if `state.current_task_id` points at a valid in-progress bead
   (still open/in_progress, assigned to loop, executable type, deps satisfied —
   the current `resumeTask()` checks), resume it UNLESS a strictly-higher-
   priority not-in-progress ready bead exists. Stays **priority-based** — "older"
   is never a reason to abandon started work. Independent of the sort policy.

2. **Pass the sort through (Stage 2).** For the non-resume path, call
   `bd ready --json --sort hybrid --assignee=ralph-loop <exclude-types>` and take
   **row 0**. Delete `bestIssue()` and `issueTypeRank` — bd does the ordering.

`hybrid` is a hidden default, not a CLI flag. If an override is ever wanted it is
a plain config key, not new CLI surface.

## Why hybrid (evidence, not assumption)

bd source `internal/storage/sqlbuild/ready.go`, cutoff = `now - 48h`:

```
ORDER BY (created_at >= now-48h ? 0 : 1) ASC,
         (created_at >= now-48h ? priority : 999) ASC,
         created_at ASC, id ASC
```

Two tiers: beads **younger than 48h** are ordered by priority and served first;
beads **older than 48h** ignore priority and drain **strictly oldest-first**. So
priority is honored only for a bead's first 48h; after that it's pure
FIFO-by-age — nothing old gets starved by newer higher-priority work.

Measured 2026-07-01 in a throwaway db (P0@0d, P1@30d, P2@90d):

| `--sort`  | Order                      |
|-----------|----------------------------|
| priority  | P0(0d) -> P1(30d) -> P2(90d) |
| hybrid    | P0(0d) -> P2(90d) -> P1(30d) |
| oldest    | P2(90d) -> P1(30d) -> P0(0d) |

Also: `--sort priority` is already type-aware (`bug < task < feature`), so
deleting the Go re-sort changes no priority-tiebreak behavior. Dependencies are
handled by `bd ready`'s live blocker-aware gating — the sort only ever orders the
already-workable frontier, so late-added deps and chains are respected for free.

## Beads (all created owned, wired #1->#2, and released)

1. **ralph-45w2** — ralph loop: pass `--sort hybrid` to bd ready and take row 0;
   delete bestIssue + issueTypeRank (Stage-2 passthrough). **Closed — merged in
   PR #745.**
2. **ralph-8n36** — ralph loop: split `getNextIssue` into explicit Stage-1 resume
   and Stage-2 passthrough (dep #1). Stage-1 preemption stays priority-based and
   does not resurrect `bestIssue`. **Closed — merged.**

Related (separate concern, not part of the ready-sort change):
- **ralph-fumr** — ralph task: forbid offering hold-vs-release + add a P0–P4
  priority-assignment rubric to `task-manager.md` (defines the "lower priority =
  do this after" lever this doc's `hybrid` reasoning relies on). **Closed — merged.**

## Rejected

- Bespoke "release-to-loop order" key — every capture mechanism was either
  non-programmatic (task-manager stamps metadata) or non-reproducible (loop
  lazy-stamps poll-time). `oldest`/`created_at` is the reproducible stand-in if
  strict order is ever wanted; `hybrid` is the better default for un-starving.
- A CLI `--order-mode`/`--ready-sort` flag — unnecessary surface; hybrid is a
  hidden default.

Version note: intra-priority tiebreak is `created_at DESC` on bd 1.0.3, flips to
ASC/FIFO per beads PR #4095; session upgraded to 1.0.5. Affects neither `hybrid`
nor `oldest`.
