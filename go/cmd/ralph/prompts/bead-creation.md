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

Use `bd create --deps` for dependency chains — not notes. When creating
related beads, always check if one depends on or blocks another and link
them with `bd dep add <issue> <depends-on>`. Missing dependencies cause
tasks to be worked out of order.

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

Before any `bd update`, run two checks:

1. `bd show <id>` — verify the bead is not closed. `bd update` on a closed bead silently succeeds — there is no error to catch.
2. `bd state <id> phase` — check whether the loop is actively working on it. A bead may still show `open` in `bd show` while `phase=implementing` — the phase field is the authoritative in-flight indicator.

Act based on what you find:

- **Closed** (`bd show` status = closed) → never update or reopen closed beads.
  Create a new bead and reference the original. This applies whether the fix
  was wrong, incomplete, or follow-on work is needed.
- **phase=implementing** (`bd state <id> phase` = implementing) → a loop agent
  is actively working on this bead right now. Do not update it unless the user
  explicitly confirms the change should go into the active iteration. If there
  is any doubt, create a follow-up bead with a dependency on this one instead.
- **status=in_progress** (`bd show` status = in_progress, phase ≠ implementing)
  → the bead is claimed but no agent is mid-iteration. Do not update it without
  explicit user confirmation. If unsure, create a follow-up bead with a
  dependency instead.
- **Open** → modify freely.

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
