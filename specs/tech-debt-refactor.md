# Tech Debt Refactoring Strategy

## Problem
AI-assisted development accelerates feature delivery but compounds tech debt invisibly. Research shows AI-generated code produces 4x code duplication, +30% static analysis warnings, and +41% increased complexity. Without a forcing function, codebases degrade iteration by iteration — each Claude session adds functional code that works but quietly erodes structure.

Ralph orchestrates autonomous iterations that ship features fast. But it has no mechanism to make Claude *feel* the weight of accumulated debt or to periodically pause feature work and pay it down. The result: a ratchet that only turns one direction.

## Philosophy
Drawn from the Boy Scout Rule ("leave it cleaner than you found it") and the principle that refactoring should be a continuous rhythm, not a crisis response:

1. **Debt is felt, not tracked** — Rather than maintaining a separate debt ledger, the planning prompt forces Claude to *look* at the codebase critically before planning work. If debt is visible at planning time, it gets scheduled.
2. **Refactor tasks are first-class** — They appear in the plan alongside feature tasks, with the same atomic-commit and test requirements. No separate "cleanup sprint."
3. **Periodic cadence** — Every Nth iteration, the execution prompt reminds Claude to inspect the code it just touched and clean up collateral mess. Small, frequent refactors prevent debt from compounding.
4. **Scope discipline** — Refactor tasks are scoped to code that was recently changed or that the current task touches. No speculative whole-codebase rewrites.

## Design

### 1. Planning-phase debt scan
During planning (`planning.md`), after reading the repo and before writing tasks, Claude performs a structured debt assessment of the areas it plans to touch:

- **Duplication**: repeated patterns, copy-paste code across files
- **Naming**: unclear variable/function names, inconsistent conventions
- **Complexity**: functions longer than ~50 lines, deep nesting, boolean flag params
- **Dead code**: unused functions, unreachable branches, commented-out code
- **Coupling**: modules that reach too far into each other's internals

If the assessment surfaces actionable items, they become plan tasks — interleaved with feature work so debt gets paid incrementally, not deferred to "later."

### 2. Execution-phase refactor check
A new prompt section in the execution template (`shared.md`) instructs Claude to apply the Boy Scout Rule after completing its primary task:

> Before committing, review the files you touched. If you see duplication, unclear names, dead code, or unnecessary complexity that you introduced or that was already there, clean it up as part of this commit. Keep refactoring scoped to the files you modified — don't go on a codebase-wide crusade.

This is lightweight — no extra iteration, no separate task. It piggybacks on work already in progress.

### 3. Periodic dedicated refactor iteration
Every N iterations (configurable, default 5), ralph injects a refactor-only task into the execution loop. This task uses a dedicated prompt (`prompts/refactor.md`) that instructs Claude to:

1. Run static analysis if available (shellcheck, eslint, etc.)
2. Review recent git history for patterns of growing complexity
3. Identify the highest-leverage cleanup in the files changed over the last N iterations
4. Execute the refactor with full test coverage
5. Commit with a clear "refactor:" prefix

The refactor iteration is injected *between* regular tasks — it doesn't consume a task slot from the plan.

### 4. Configuration
New CLI flag:
- `--refactor-every <N>`: Inject a refactor iteration every N iterations (default: 5, 0 to disable)

State tracking:
- `state.json` gains `iterations_since_refactor` counter, reset after each refactor iteration

## Implementation

### File: `prompts/refactor.md`
Dedicated prompt for refactor iterations. Template variables: `{{WORK_DIR}}`, `{{RECENT_FILES}}` (files changed in last N iterations, populated by ralph.sh).

Content instructs Claude to:
- Read AGENTS.md/CLAUDE.md for project conventions
- Review `{{RECENT_FILES}}` for debt signals (duplication, complexity, naming, dead code)
- Pick the single highest-impact refactor
- Execute it with tests
- Commit with `refactor:` prefix

### File: `prompts/shared.md`
Add a "Boy Scout Rule" section to the existing standards:

```markdown
### Boy Scout Rule
Before committing, review the files you touched in this task. Clean up:
- Duplication you introduced or found
- Unclear names in code you modified
- Dead code (unused functions, unreachable branches, commented-out code)
- Unnecessary complexity (extract a function, flatten nesting)
Keep cleanup scoped to files you changed. Don't refactor code you didn't touch.
```

### File: `prompts/planning.md`
Add a debt assessment step between reading the repo and writing tasks:

```markdown
## Debt assessment
Before writing tasks, scan the areas you plan to touch for tech debt:
- Duplicated patterns or copy-paste code
- Functions over ~50 lines or deeply nested logic
- Unclear or inconsistent naming
- Dead code, commented-out blocks, unused imports
- Tight coupling between modules

If you find actionable debt in code the planned work will touch, add refactor tasks to the plan. Interleave them with feature tasks — don't batch all refactors at the end.
```

### File: `ralph.sh`
In `run_execution()`, after the iteration counter increment:

```bash
# Check if a refactor iteration is due
local since_refactor
since_refactor=$(read_state "iterations_since_refactor")
since_refactor=${since_refactor:-0}

if (( REFACTOR_EVERY > 0 && since_refactor >= REFACTOR_EVERY )); then
  log_phase "--- Refactor iteration ---"
  local recent_files
  recent_files=$(git -C "$WORK_DIR" diff --name-only "HEAD~${REFACTOR_EVERY}" HEAD 2>/dev/null || echo "")

  local refactor_prompt
  refactor_prompt=$(<"$PROMPTS_DIR/refactor.md")
  refactor_prompt="${refactor_prompt//\{\{WORK_DIR\}\}/$WORK_DIR}"
  refactor_prompt="${refactor_prompt//\{\{RECENT_FILES\}\}/$recent_files}"

  run_claude "$refactor_prompt"
  write_state "iterations_since_refactor" "0"
  continue  # don't count this as a task iteration
fi

write_state "iterations_since_refactor" "$((since_refactor + 1))"
```

Add CLI flag parsing for `--refactor-every`.

## Acceptance criteria
- Planning phase includes debt assessment — plans contain interleaved refactor tasks when debt is found
- Shared execution prompt includes Boy Scout Rule — files touched during a task get cleaned up before commit
- Every N iterations (default 5), a dedicated refactor iteration runs targeting recently-changed files
- `--refactor-every 0` disables periodic refactor iterations
- `--refactor-every N` configures the cadence
- Refactor iterations don't consume plan task slots
- Refactor commits use `refactor:` prefix
- State tracks `iterations_since_refactor` and resets after each refactor
- All existing tests continue to pass
