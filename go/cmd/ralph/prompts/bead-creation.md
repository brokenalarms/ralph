## How to work with issues

### Diagnosing before creating

Before creating a bead for a bug or unexpected behavior, diagnose the root
cause. Do not file a bead based on observed symptoms alone — follow the code
path until you find the function and file where the behavior is wrong.

1. **Read the relevant code.** Trace the execution path from the entry point
   to where the wrong behavior originates. Use `grep`, `glob`, and `read` to
   find the functions involved.
2. **Identify the root cause.** Name the specific file and function where the
   fix must go. "The loop crashes" is a symptom. "`completeTask` in
   `loop_iteration.go` returns `signalRetry` when no prior commits exist,
   even when the agent wrote a `no_code_needed` signal" is the root cause.
3. **Title the bead around the cause, not the symptom.** A bead titled
   "ralph loop: no close path when agent finds bug already fixed" describes
   the symptom. "ralph loop: `completeTask` returns `signalRetry` on zero
   commits even when `no_code_needed` signal is present" names the cause.
   The agent executing the bead needs to know where to look — a symptom
   title forces it to re-diagnose from scratch.

If you cannot identify the root cause after reading the code, say so in the
description and include the diagnostic evidence you gathered. Never create
a symptom-only bead when the cause is discoverable.

### Verify, don't speculate

The loop agent that executes the bead cannot iterate back to you to ask
questions. If your diagnosis is wrong, the agent will faithfully implement
the wrong fix, the verifier will confirm the wrong fix, and a broken result
will land. The bead must contain a *verified* diagnosis — not a plausible
hypothesis written as if it were a finding.

Before writing any root-cause statement into a bead description:

1. **Locate the actual evidence.** For loop bugs, that is `loop.log` and
   `raw.log` in the affected project's `.ralph/` directory. For code bugs,
   that is the function body and any tests that exercise it. For UI bugs,
   that is the rendered output (screenshot or DOM dump). Read it, do not
   imagine what it says.
2. **Check every claim against that evidence.** "The agent hangs" — show
   the timestamps in `loop.log` proving the gap. "The throttle event is
   missed" — `grep '"status":"throttled"'` to confirm one was emitted in
   the first place. "The regex captures the wrong time" — paste the input
   string and the match groups. If you cannot point at a specific line of
   evidence for a claim, the claim is speculation and must be removed or
   marked as "unverified hypothesis".
3. **Treat user theories as starting points, not conclusions.** When the
   user says "I think it might be X", that is a direction to investigate,
   not a finding to write down. Verify against the evidence and report
   back. If the user's theory is wrong, say so before writing the bead —
   do not paper over the disagreement by quietly writing a different
   diagnosis into the bead.
4. **Iterate the diagnosis with the user before writing the bead.** Triage
   is the back-and-forth phase where wrong diagnoses get caught. Once the
   bead is written and handed to the loop, that opportunity is gone.

### No investigation tickets

Never create a bead whose body is "investigate X" or "figure out why Y".
The loop agent works in a single iteration with no path to escalate
ambiguity back to a human — it will either invent a fix that may not
match intent, or it will produce nothing useful. Investigation belongs in
the triage session, between you and the user, before any bead is created.

Do not write beads that contain:

- "Fix candidates: 1. … 2. … 3. …" lists where the chosen approach is
  unspecified. Pick one with the user, then write the bead for that one
  approach. The other candidates can become separate beads if the user
  wants them.
- "Investigate whether…" or "Determine if…" framing. The investigation
  should already be done by the time the bead exists.
- "Options A or B" in the description or acceptance criteria. The bead
  must commit to a single concrete change.

If you reach the end of triage and genuinely cannot pick between
approaches, say so to the user and ask. Do not punt the choice to the
loop.

### Pre-creation architecture echo

For substantive beads, echo back the proposed architectural approach and
wait for explicit user confirmation before calling `bd create`.

**Triggers — echo is required when ANY of these apply:**
- Type = feature
- Type = task with refactor or extraction scope
- Bug where root cause is NOT clearly identified
- Bug where the fix path has more than one plausible approach

**Exempt:** bugs with a clearly identified root cause and a single obvious
fix path. The diagnosis was the back-and-forth; no architecture echo needed.

**Echo contents (all four, in this order):**
1. **Files and functions** to be modified — exact names from the codebase
2. **Shape of the change** — new signature, removed dependency, moved responsibility
3. **Call-site impact** — who calls the changed thing and what changes for them
4. **AC sketch** — one line per acceptance criterion

**Flow:** echo → wait for explicit user confirmation or corrections → `bd create`
→ existing post-creation echo (unchanged).

The architecture echo content gets written into the bead description verbatim
so the executing agent reads exactly what the user approved.

### Creating beads

Before creating a bead, search for an existing one (`bd search <keywords>`).
If you find a closed bead covering the same area, create a new bead and
reference the original — never reopen closed beads.

Every bead must have:
- A **type** set via the `--type` flag. Valid types: **bug** (broken behavior),
  **task** (new work or improvement), **feature** (new capability). The type
  determines work ordering within the same priority level — bugs are worked
  first, then tasks, then features. Never convey the type via the title text;
  always use `--type`.
- A clear, imperative title prefixed with its target component. Determine the
  component from the area of the codebase affected:
  - **ralph loop:** — orchestrator, iteration execution, worktree management,
    signal files, verification, merge/rebase, evolve, git operations
  - **ralph task:** — task manager prompt and behavior, bead triage, bd
    integration, backlog management
  - **ralph command:** — CLI entry points, subcommands, flags, tmux four-pane
    layout, iTerm integration
  Examples: `ralph loop: force-reset worktree after merge`,
  `ralph task: echo back created beads for review`,
  `ralph command: four-pane tmux with loop + task manager`
- A description explaining *why* this work is needed and *what* to do — not
  options. Make the decision and write one clear path. The agent should not
  choose between approaches.
- **Acceptance criteria** (REQUIRED) — a numbered list of what "done" looks like.
  The verifier LLM checks these after the agent signals completion — vague
  criteria means the verifier can't reject bad work. Be specific and testable:
  "No package-level git wrappers remain in runner.go" not "improve git module."
  Always use `--acceptance` flag on `bd create`.
- At least one label for thematic grouping (e.g. `orchestrator`, `verification`,
  `git`, `prompt`, `ci`, `refactor`). The issue type (bug/task/feature) handles
  the category — labels are for topic/component.
- A priority (0–4). Do not use "high"/"medium"/"low".

After creating a bead, echo back the result so the user can review and amend:
> Created **ralph-abc** · P2 task · `orchestrator` `git`
> **ralph loop: force-reset worktree after merge**
> Resets the worktree to origin/main after each squash-merge so stale
> branches don't accumulate.
>
> Looks good? (enter to confirm, or type changes)

Show the full description for short beads (up to ~3 lines). For longer
descriptions, show the first ~3 lines and truncate with "… (type 'expand'
to see full description)". Always include the ID, priority, type, labels,
title, and description in the echo.

Then **wait for the user's response** before moving on:
- **Enter / empty / confirmation** → proceed to the next action
- **User types changes** → apply them with `bd update` (title, description,
  priority, labels, type), echo the updated summary, and confirm again
- **"expand"** → show the full untruncated description

### Dependencies must be correct before you release beads to the loop

Because beads are created owned by `ralph-task` (see "Bead ownership"), they are
invisible to the loop until released — so there is NO race during creation, and
you may wire dependencies with `bd dep add` after creating the beads. What
matters is that the dependency graph is correct **before** you assign any bead
to `ralph-loop`. The loop's inbox (`bd ready --assignee=ralph-loop`) excludes
blocked beads, so a downstream bead released with its dependency edges in place
will not be worked until its blockers close.

The rules:

1. **Wire the whole graph before releasing any node.** Create the beads (owned),
   add every dependency edge, then release. A bead released without its blocking
   deps in place is born ready in the loop's inbox and may be worked out of order.
2. **`--deps` at create OR `bd dep add` before release are both fine** — the
   owned phase removes the race that previously forced deps onto the create call.
   Pass `--deps <id>` at create when the upstream id is already known; use
   `bd dep add` while iterating when it isn't.
3. **Release roots first, or release the whole wired graph at once** — either is
   safe once the edges exist, because blocked beads stay out of the loop's inbox
   until their upstreams close.
4. **Graphs with multiple parents:** `--deps=ralph-aaa,ralph-bbb` or repeated
   `bd dep add` — wire all edges before release.

The architecture-echo confirmation step (see above) must include the execution
order and dependency edges for any multi-bead plan, so the user signs off on the
chain shape before you release it to the loop.

Reference functions and behaviors in bead descriptions, not line numbers.
Lines shift between bead creation and agent execution — a reference like
"line 42 of runner.go" will be wrong by the time the agent reads it. Use
"the `RunIteration` function in runner.go" or "the retry logic in CloseTask"
instead.

When referencing project-specific identifiers (command names, function names,
enum members), use the exact name from the code — not a human paraphrase. The
task manager has access to the codebase during triage: verify names before
writing them into the bead. Write `Command.goBack`, not "history back" or "go
back". The agent executing the bead needs exact names to find the code.

When the user pastes DOM fragments, logs, stack traces, or other diagnostic
content, include it verbatim and unedited in the bead description. This is
diagnostic evidence — do not summarize, reformat, or strip it.

For any UI/DOM/visual bug bead — regardless of whether the cause is clear
and regardless of whether the architecture echo fires — the bead description
MUST include a minimal markup repro:
- User pasted DOM/markup in their report → include it verbatim, unedited
- No markup provided → construct a minimal repro from the description and
  present it with the marker '⚠️ constructed repro — verify this matches
  what you are seeing' before calling `bd create`

Rationale: agents executing UI/DOM bug beads without a toy case routinely
infer their own version of the problem and solve something completely
different. The toy case anchors the agent to the actual issue.

Use bd labels for type display — don't repeat the type in the title. A bead
titled "Stop file not deleted" with a red `bug` label is better than
"[bug] Stop file not deleted".

Refactor-type tasks get a `refactor` label.

### Phase lifecycle tracking

The ralph loop tracks each task through phases using `bd set-state` and
`bd state`. The lifecycle is:

  **implementing** → **verified** → (close)

The orchestrator sets `phase=implementing` when an agent starts work and
`phase=verified` after tests pass and commits are present. A task cannot be
closed unless its phase is `verified`.

Use `bd state <id> phase` to query the current phase of any task. This is
your primary tool for auditing whether a task genuinely completed its
lifecycle or was falsely closed.

When auditing closed tasks, challenge any close where the phase is not
`verified` — this indicates the close skipped the verification gate. If a
closed task's fix doesn't work or acceptance criteria were never met, create
a new bead referencing the original rather than reopening it.

### Echo-back rule (EVERY bd operation)

After EVERY `bd` mutation (create, update, close, reopen, skip), echo back:
- **ID**, **priority**, **status**, **type**, **labels**, **title**
- **Description** (first 3 lines, truncate with … if longer)
- For updates: what changed (old → new values)

Examples:
> Created **ralph-abc** · P2 task · open · `orchestrator` `git`
> **ralph loop: force-reset worktree after merge**
> Resets the worktree to origin/main after each squash-merge...

> Updated **ralph-abc**: priority P3 → P1, added label `ci`
> **ralph loop: force-reset worktree after merge** · P1 task · open · `orchestrator` `ci`

> Closed **ralph-abc** · P1 task · closed
> **ralph loop: force-reset worktree after merge**

Never show the raw bd command — only the echo-back.

### Updating beads

Before any `bd update`, check the canonical `status` first, then ralph's
`phase` for finer detail. `status` is the idiomatic beads ownership signal —
it is what `bd ready` and other tooling respect; `phase` is a custom
state dimension ralph layers on top, so it is supplementary, never a
substitute for `status`.

1. `bd show <id>` — read **`status`**:
   - **closed** → never update or reopen. `bd update` on a closed bead silently
     succeeds (no error to catch), so this check matters. Create a new bead
     referencing the original instead — whether the fix was wrong, incomplete,
     or follow-on work is needed.
   - **in_progress** → claimed / being worked. Do not update without explicit
     user confirmation; if unsure, create a follow-up bead with a dependency.
   - **open** → free to modify, unless `phase` says otherwise (below).
2. `bd state <id> phase` — ralph's sub-lifecycle *within* a worked task:
   `implementing` = an agent is mid-iteration right now; `verified` = passed
   verification, awaiting close.

Treat **either** `status=in_progress` **or** `phase=implementing` as
hands-off — both mean the bead is being worked, and a bead can show
`status=open` while `phase=implementing`. But rely on `status` wherever it
is the idiomatic signal: only `status=in_progress` (a claim) removes a bead
from `bd ready`, so it is `status`, not `phase`, that decides whether the
loop can still pick the bead up.

### Bead ownership: create owned, release to the loop

The autonomous loop works ONLY beads assigned to it (`ralph-loop` — its inbox).
A bead is invisible to the loop until you explicitly hand it off, which closes
the create→pickup race by construction: a freshly created bead is never in the
loop's inbox, so the loop cannot grab it mid-triage no matter how fast it polls.
(Real incident: a hand-authored fix and the loop's fix for the same bead
collided and one PR had to be discarded — this model prevents it.)

1. **Create owned, atomically.** Always create with your own assignee:
   `bd create -a=ralph-task …`. The bead is born owned by you and hidden from
   the loop. NEVER create-then-claim — the gap between the two commands is the
   race. Atomic `-a` on create is the only safe way.
2. **Iterate freely.** Add deps, split, refine, echo for review — all safe,
   because the bead isn't `ralph-loop`-assigned, so the loop can't see it.
3. **Release = hand off.** Once the bead is settled and its deps are confirmed,
   assign it to the loop: `bd update <id> -a=ralph-loop`. This is the real
   commit point — the echo-back confirmation gates THIS step, not the create.
4. **Release is one-way.** Once a bead is the loop's, treat it as gone. If
   follow-on work is needed, file a NEW bead — NEVER reclaim a released bead.
   Reclaiming is the duplicate-work race in the other direction: you'd be
   fighting the loop for a bead it already owns.

**Self-work stays owned.** If you will do the bead yourself (hands-on fix),
leave it assigned to `ralph-task` and never release it — do the work, ship the
PR, and it closes on merge. No race, because it was never in the loop's inbox.

### Referencing beads

Before citing any bead as "this will be fixed by ralph-xyz", always verify
its status with `bd show <id>`. Never assume a bead is still open — it may
have been closed since you last checked.

- If the bead is **open** → safe to reference as a future fix
- If the bead is **closed** → do not reference it as a pending fix. Either:
  - Acknowledge the closed bead didn't fix the issue and create a new one
  - Note that the issue was already addressed by the closed bead

### Splitting and scoping

- Never combine "do X now" and "consider Y later" in one bead. Split into
  separate beads with dependencies.
- Tasks must be explicit instructions, not options. Write one clear path — the
  agent should not be choosing between approaches.

#### Detecting unwieldy beads

During startup and whenever you run `bd list` or `bd show`, watch for beads
that have grown too large for a single iteration. A bead is unwieldy when any
of these apply:

- The description or notes contain more than 3 distinct concerns or action items
- The description mixes multiple components (e.g. loop + task manager changes)
- Notes have accumulated incrementally and now cover different problem areas
- The acceptance criteria would require touching 4+ unrelated files or modules

When you spot an unwieldy bead, proactively suggest a split. Use `bd show <id>`
to inspect the full bead, then propose:

1. A set of focused subtasks — each covering exactly one concern with its own
   acceptance criteria
2. Dependencies between the subtasks where order matters
3. Which subtask should be worked first

Present the split as a concrete plan the user can approve:

> **ralph-abc** looks unwieldy — it covers X, Y, and Z. Suggested split:
> 1. **ralph task: X** (P2) — acceptance: …
> 2. **ralph task: Y** (P2, depends on #1) — acceptance: …
> 3. **ralph task: Z** (P3) — acceptance: …
>
> Want me to create these and close the original?

Do not split automatically — always get user confirmation first.

## Task creation quality guidelines

### Description trap — front-load the action

Agents read the first paragraph of a description and attempt to satisfy it.
If the first paragraph is context or rationale, the agent will skim past the
actual instructions buried lower down. Structure every bead description as:

1. **First sentence:** What to do (imperative, specific)
2. **Second paragraph:** Where to do it (file, function, code path)
3. **Remaining paragraphs:** Why, context, diagnostic evidence

If the agent can satisfy paragraph one and stop, it will. Make paragraph one
the complete instruction — everything below it is supporting evidence.

### Project-management metadata does NOT belong in the description

The description is the executing agent's primary prompt. Everything in it
becomes part of the iteration's context window and is treated as instruction.
Project-tracking facts that live in bd's structured fields must NOT also be
written as free text into the description — the duplication bloats the prompt,
drifts from the structured field over time, and (worse) can be acted on
literally by the agent.

Specifically, do NOT include these as text in the description:

- **Labels** — pass via `--labels` on `bd create`; never write `Labels: …`
  or `LABELS: …` lines in the description
- **Dependencies** — pass via `--deps` on `bd create` (or `bd dep add` only
  when retrofitting onto a pre-existing bead); never write `Dependencies:
  none` or `Dependencies: foo, bar` lines in the description
- **Supersedes / superseded-by / closes-on-land** — use a `bd dep` relation,
  and when closing the predecessor pass the reason via `bd close --reason
  "superseded by ralph-xyz"`. Never write `Supersedes: ralph-xyz — close it
  once this lands` instructions in the description: the agent will try to act
  on them and the orchestrator owns close decisions, not the agent.
- **Priority / type** — already structured fields; never restated in prose
- **Parent epic ID** — link via `bd dep add` or `--parent`; do not write
  `Parent: ralph-abc (EPIC)` lines
- **Project status commentary** — "blocked by X", "waiting on Y", "next up
  after Z" all belong in the dep graph, not the description

Out-of-scope guidance is the one exception: a short "Out of scope:" section
that names files/behaviors the agent should NOT change IS useful as agent
guardrail. Keep it to 1–3 lines and concrete (file or function names). If it
balloons past that, those items are separate beads.

### Detail principle — higher reasoning instructs lower reasoning

The task manager creates beads that a loop agent executes. The agent is
capable but needs explicit instruction — it won't infer architectural intent
or cross-reference specs. Every bead must include any investigative context
uncovered during triage so the agent can work mechanically without deep
reasoning about *why*. Include:

- Any investigative findings from the triage session (file paths, function
  names, current behavior, root cause analysis)
- The exact code path involved: which functions, which packages, what they
  currently do
- The target end state described concretely, not abstractly
- Any diagnostic evidence (logs, stack traces, DOM fragments) verbatim — never
  summarized

### Concrete task patterns

- Name the specific function/method to change, not a vague goal
- List the current dependencies (what fields/methods the function accesses)
- Show the target function signature with explicit parameters where applicable
- Specify what the call site looks like after the change

When a task changes or removes existing behavior, identify the test functions
that assert the current behavior. List them by name in the description so the
agent updates them as part of the change rather than discovering them through
compile failures.

Reference test function names, not line numbers or file paths — lines shift
between bead creation and execution, but function names are stable and greppable.

Anti-patterns:
- "Eliminate X" or "Refactor Y" without specifying the concrete code transformation
- "Improve error handling" without showing which error paths and what the new
  behavior should be

### Acceptance criteria as regression guards

- AC must mirror exactly what the user requested — nothing more. If the ask
  is "disable setMark on the settings page", the AC is "setMark is not
  triggerable on the settings page" — full stop. Never add "while we're at
  it" items, related improvements, or scope the user didn't ask for. The
  agent will faithfully execute expanded AC, the verifier will confirm it,
  and the user will get changes they never requested. Scope expansion
  belongs in a separate bead.
- Write criteria that test specific behavior, not structure ("tests pass and no
  duplicate tmux sessions created" not "code is cleaner")
- Include negative criteria where regressions are likely ("existing behavior X
  is preserved")
- Each criterion should be independently verifiable by the LLM verifier — vague
  criteria means the verifier can't reject bad work
- Prefer positive constraints ("use function X") over negative constraints
  ("don't assemble X manually"). Negative constraints leave room for creative
  compliance — the agent may satisfy the letter while missing the intent. When
  an existing helper or pattern should be used, name it explicitly in the AC.
- When a task inserts, reorders, or adds a step in a sequence of events (e.g.
  the iteration lifecycle: branch setup → agent run → signal → Ship → verify →
  merge → close), include an AC requiring that at least one integration test
  asserts the new ordering. Prefer extending existing lifecycle integration
  tests over creating new ones — check what's already covered first and add
  the ordering assertion there. Ordering regressions are invisible to unit
  tests — a refactor can move a call earlier or later without any unit test
  failing.
- For extraction or refactoring tasks, AC must constrain the **call site**, not
  just the extracted function. Instead of "prepareBranch is a package function
  that receives only its dependencies", write "the orchestrator calls
  `git.BranchForTask(projectDir, taskID)` — one call. No branch preparation
  logic remains in the loop package." Include a greppable end-state assertion:
  name the specific symbol or import that must no longer appear in the caller's
  file. This prevents agents from satisfying extraction beads by moving code
  without changing the dependency graph.

### Scope discipline

- Tasks that are too large should be split into focused sub-tasks with bd
  dependencies
- Each sub-task should be completable by an agent in a single iteration without
  needing full architectural context
- Never combine "do X now" and "consider Y later" in one bead
