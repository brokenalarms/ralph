# Tech Debt Refactoring Strategy

## Problem
AI-assisted development accelerates feature delivery but compounds tech debt invisibly. Research shows AI-generated code produces 4x code duplication, +30% static analysis warnings, and +41% increased complexity. Without a forcing function, codebases degrade iteration by iteration — each Claude session adds functional code that works but quietly erodes structure.

Ralph orchestrates autonomous iterations that ship features fast. But it has no mechanism to make Claude *feel* the weight of accumulated debt or to periodically pause feature work and pay it down. The result: a ratchet that only turns one direction.

## Philosophy
Inspired by [The Holy Order of Clean Code](https://church.btas.dev/) (btachinardi/church) — a Claude Code plugin that deploys specialized purist agents to enforce code quality — and the Boy Scout Rule ("leave it cleaner than you found it").

The key insight from the Church's size-purist: files are organisms that want to grow. They feed on lazy additions. But the refactoring response must be **proportionate and human-centered**:

1. **Refactoring serves human readability, not arbitrary rules** — The goal is code that humans can read and navigate. A 500-line file with one cohesive responsibility is fine. A 300-line file with three distinct responsibilities needs splitting.
2. **Don't refactor for the sake of activity** — If a refactor iteration finds nothing worth cleaning, it should signal completion without making changes. Churn pollutes git history and wastes iterations.
3. **Balance extraction with indirection cost** — One-line utility functions used once create navigation overhead without value. Extract shared code when it represents a genuine single source of truth (e.g., a format string used everywhere). Three similar lines are better than a premature abstraction.
4. **Dead code is always worth removing** — Unused functions, commented-out blocks, unreachable branches. Git remembers. Delete them. (Church's dead-code-purist: "Commented code is not 'maybe useful later' — it's clutter.")
5. **500 lines is the split signal, not a hard cap** — Per the Church's size-purist thresholds, 500+ lines is the "critical" mark for any file type. When a file crosses this, look for distinct responsibilities. If they exist, split. If the file is cohesive, leave it.
6. **Periodic cadence** — Every Nth iteration, Ralph injects a refactor-only iteration. Small, frequent cleanups prevent debt from compounding to crisis levels.
7. **Scope discipline** — Refactor tasks are scoped to recently changed code. No speculative whole-codebase rewrites.

## Design

### 1. Planning-phase debt scan
During planning (`planning.md`), after reading the repo and before writing tasks, Claude scans the areas it plans to touch for readability issues: bloated files (500+ lines with distinct responsibilities), dead code, unclear naming, deep nesting (4+ levels), and genuine duplication. Refactor tasks get interleaved with feature tasks — but only when the cleanup genuinely improves human readability, not because a checklist said to.

### 2. Execution-phase Boy Scout Rule
A section in `shared.md` reminds Claude to glance at files it touched before committing. If dead code, unclear names, or growing file sizes are present, clean them up. But if the code reads fine, leave it alone. Don't extract helpers used once. Don't create abstractions for one-time operations.

### 3. Periodic dedicated refactor iteration
Every N iterations (configurable, default 5), ralph injects a refactor-only task into the execution loop. This task uses `prompts/refactor.md` which instructs Claude to:

1. Read project conventions (AGENTS.md/CLAUDE.md)
2. Review recently changed files for debt signals
3. **If nothing meaningful stands out, signal completion without changes**
4. If genuine debt exists, pick the single highest-impact cleanup
5. Execute with tests, commit with `refactor:` prefix

The refactor iteration is injected *between* regular tasks — it doesn't consume a plan task slot.

### 4. Configuration
New CLI flag:
- `--refactor-every <N>`: Inject a refactor iteration every N iterations (default: 5, 0 to disable)

State tracking:
- `state.json` gains `iterations_since_refactor` counter, reset after each refactor iteration

## Implementation

### File: `prompts/refactor.md`
Dedicated prompt for refactor iterations. Template variables: `{{WORK_DIR}}`, `{{RECENT_FILES}}`.

Critically, the prompt explicitly states: "If nothing meaningful stands out, signal completion without making changes. Refactoring for the sake of activity is worse than no refactoring."

### File: `prompts/shared.md`
Boy Scout Rule section — a lightweight reminder, not a mandate. Emphasizes dead code removal and 500-line file awareness. Explicitly discourages extracting one-line helpers and premature abstractions.

### File: `prompts/planning.md`
Debt assessment step between reading the repo and writing tasks. Focuses on human readability signals. Explicitly states: "don't add refactor tasks just because you can."

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
  refactor_prompt=$(build_refactor_prompt "$recent_files")

  run_claude "$refactor_prompt" "" "raw"
  write_state "iterations_since_refactor" "0"
  continue  # don't count this as a task iteration
fi

write_state "iterations_since_refactor" "$((since_refactor + 1))"
```

Add CLI flag parsing for `--refactor-every`. Add `build_refactor_prompt()` function. Modify `run_claude()` to accept raw pre-built prompts.

## Acceptance criteria
- Planning phase includes debt assessment — plans contain interleaved refactor tasks when genuinely valuable debt is found
- Shared execution prompt includes Boy Scout Rule as a reminder, not a mandate
- Every N iterations (default 5), a dedicated refactor iteration runs targeting recently-changed files
- Refactor iterations that find no meaningful debt complete without changes (no forced busywork)
- `--refactor-every 0` disables periodic refactor iterations
- `--refactor-every N` configures the cadence
- Refactor iterations don't consume plan task slots
- Refactor commits use `refactor:` prefix
- State tracks `iterations_since_refactor` and resets after each refactor
- All existing tests continue to pass
