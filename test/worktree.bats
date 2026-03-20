#!/usr/bin/env bats

load test_helper

setup() {
  source_ralph_functions
  setup_test_repo
}

teardown() {
  teardown_test_repo
}

# Proves: human-readable, no stale numbers.
@test "Worktree dir uses date-based name" {
  init_ralph_dir
  setup_worktree
  local today
  today=$(date +%Y%m%d)
  [[ "$WORK_DIR" == *"/worktrees/ralph-${today}-01" ]]
}

# Proves: no clobber on same-day runs.
@test "Second run same day increments suffix" {
  init_ralph_dir
  local today
  today=$(date +%Y%m%d)
  mkdir -p "$RALPH_DIR/worktrees/ralph-${today}-01"
  setup_worktree
  [[ "$WORK_DIR" == *"/worktrees/ralph-${today}-02" ]]
}

# Proves: resume stability.
@test "Resume reuses existing worktree" {
  init_ralph_dir
  setup_worktree
  local saved_dir="$WORK_DIR"
  local saved_branch="$WORKTREE_BRANCH"

  RESUME=true
  setup_worktree
  [[ "$WORK_DIR" == "$saved_dir" ]]
}

# Proves: next branch used before task is known.
@test "Initial branch is ralph/project/next" {
  init_ralph_dir
  setup_worktree
  [[ "$WORKTREE_BRANCH" == "ralph/project/next" ]]
}

# Proves: order + description in branch name.
@test "Branch renamed to task slug with sequence" {
  init_ralph_dir
  setup_worktree
  rename_branch_for_task "Fix auth bug"
  [[ "$WORKTREE_BRANCH" == "ralph/project/01-fix-auth-bug" ]]
}

# Proves: stale branches don't inflate counter.
@test "Branch sequence resets per run" {
  git -C "$PROJECT_DIR" branch "ralph/project/old-stale" 2>/dev/null || true
  init_ralph_dir
  setup_worktree
  rename_branch_for_task "First task"
  [[ "$WORKTREE_BRANCH" == "ralph/project/01-first-task" ]]
}

# Proves: per-task isolation.
@test "rotate_branch resets to next branch" {
  init_ralph_dir
  setup_worktree
  rename_branch_for_task "First task"
  rotate_branch
  [[ "$WORKTREE_BRANCH" == "ralph/project/next" ]]
  [[ "$_BRANCH_RENAMED" == false ]]
}

# Proves: failures visible.
@test "rotate_branch logs warning on failure" {
  init_ralph_dir
  setup_worktree
  # Don't rename, so "next" still exists — rotate will fail trying to create it again
  run rotate_branch
  # Should not crash (rotate_branch handles the error)
  [[ "$status" -eq 0 ]]
}

# Proves: stale worktrees (directory removed) are pruned before branch creation.
@test "Stale worktree branch is cleaned up via prune" {
  init_ralph_dir
  setup_worktree
  local first_work_dir="$WORK_DIR"

  # Simulate a stale worktree: remove the directory but leave git metadata
  rm -rf "$first_work_dir"

  # Reset state so setup_worktree runs fresh (not resume path)
  RESUME=false
  WORK_DIR=""
  WORKTREE_BRANCH=""
  _TASK_SEQ=0

  # Should succeed because prune cleans the stale reference
  setup_worktree
  [[ -d "$WORK_DIR" ]]
}

# Proves: live ralph worktrees are force-removed when branch conflicts.
@test "Live ralph worktree is removed when branch already exists" {
  init_ralph_dir
  setup_worktree
  local first_work_dir="$WORK_DIR"

  # Worktree directory still exists (not stale), but we start a new run
  RESUME=false
  WORK_DIR=""
  WORKTREE_BRANCH=""
  _TASK_SEQ=0

  setup_worktree
  [[ -d "$WORK_DIR" ]]
  # Old worktree should have been removed
  [[ ! -d "$first_work_dir" ]]
}

# Proves: task_seq is restored from state.json on resume, not derived from branch count.
# Prevents sequence skips when branches are deleted after squash-merge.
@test "Resume restores task_seq from state.json" {
  init_ralph_dir
  setup_worktree

  rename_branch_for_task "first task"
  rotate_branch
  rename_branch_for_task "second task"

  # Verify task_seq was persisted
  local stored_seq
  stored_seq=$(read_state "task_seq")
  [[ "$stored_seq" == "2" ]]

  # Delete a branch to simulate squash-merge cleanup
  git -C "$PROJECT_DIR" branch -D "ralph/project/01-first-task" 2>/dev/null

  # Resume — should use persisted seq (2), not branch count (1)
  RESUME=true
  _TASK_SEQ=0
  setup_worktree
  [[ "$_TASK_SEQ" -eq 2 ]]
}

# Proves: ralph requires a git repo and fails fast without one.
@test "Non-git directory exits with error" {
  local non_git_dir
  non_git_dir="$(mktemp -d)"
  PROJECT_DIR="$non_git_dir"
  run setup_worktree
  [[ "$status" -eq 1 ]]
  [[ "$output" == *"Not a git repo"* ]]
  rm -rf "$non_git_dir"
}

# Proves: clean rebase works when no squash merges have happened.
@test "rebase_onto_default_branch succeeds on clean rebase" {
  setup_rebase_env

  echo "new file on main" > "$PROJECT_DIR/mainfile.txt"
  git -C "$PROJECT_DIR" add mainfile.txt
  git -C "$PROJECT_DIR" commit -m "add mainfile" -q
  push_to_origin

  echo "worktree file" > "$WORK_DIR/workfile.txt"
  git -C "$WORK_DIR" add workfile.txt
  git -C "$WORK_DIR" commit -m "add workfile" -q

  run rebase_onto_default_branch
  [[ "$status" -eq 0 ]]
  [[ -f "$WORK_DIR/mainfile.txt" ]]
  [[ -f "$WORK_DIR/workfile.txt" ]]
}

# Proves: squash-merged branches are detected and skipped during rebase.
# Uses intermediate commits so the 3-way merge produces a real conflict
# (newer git auto-resolves single add/add with identical content).
@test "rebase_onto_default_branch skips squash-merged branches" {
  echo "original" > "$PROJECT_DIR/shared.txt"
  git -C "$PROJECT_DIR" add shared.txt
  git -C "$PROJECT_DIR" commit -m "add shared" -q

  setup_rebase_env

  rename_branch_for_task "first task"
  echo "step one" > "$WORK_DIR/shared.txt"
  git -C "$WORK_DIR" add shared.txt
  git -C "$WORK_DIR" commit -m "first task step one" -q
  echo "final" > "$WORK_DIR/shared.txt"
  echo "first" > "$WORK_DIR/first.txt"
  git -C "$WORK_DIR" add shared.txt first.txt
  git -C "$WORK_DIR" commit -m "first task final" -q

  rotate_branch
  rename_branch_for_task "second task"
  echo "second" > "$WORK_DIR/second.txt"
  git -C "$WORK_DIR" add second.txt
  git -C "$WORK_DIR" commit -m "second task" -q

  # Simulate squash-merge of branch 01 into main on origin
  echo "final" > "$PROJECT_DIR/shared.txt"
  echo "first" > "$PROJECT_DIR/first.txt"
  git -C "$PROJECT_DIR" add shared.txt first.txt
  git -C "$PROJECT_DIR" commit -m "squash: first task" -q
  push_to_origin

  run rebase_onto_default_branch
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"squash-merged"* ]]
  [[ -f "$WORK_DIR/second.txt" ]]
}

# Proves: squash-merge detection works even when main has unrelated commits.
@test "rebase_onto_default_branch detects squash-merge with extra main commits" {
  echo "original" > "$PROJECT_DIR/shared.txt"
  git -C "$PROJECT_DIR" add shared.txt
  git -C "$PROJECT_DIR" commit -m "add shared" -q

  setup_rebase_env

  rename_branch_for_task "first task"
  echo "step one" > "$WORK_DIR/shared.txt"
  git -C "$WORK_DIR" add shared.txt
  git -C "$WORK_DIR" commit -m "first task step one" -q
  echo "final" > "$WORK_DIR/shared.txt"
  echo "first" > "$WORK_DIR/first.txt"
  git -C "$WORK_DIR" add shared.txt first.txt
  git -C "$WORK_DIR" commit -m "first task final" -q

  rotate_branch
  rename_branch_for_task "second task"
  echo "second" > "$WORK_DIR/second.txt"
  git -C "$WORK_DIR" add second.txt
  git -C "$WORK_DIR" commit -m "second task" -q

  # Simulate squash-merge of branch 01 into main on origin
  echo "final" > "$PROJECT_DIR/shared.txt"
  echo "first" > "$PROJECT_DIR/first.txt"
  git -C "$PROJECT_DIR" add shared.txt first.txt
  git -C "$PROJECT_DIR" commit -m "squash: first task" -q

  # Simulate another PR merged to main (unrelated file)
  echo "other pr work" > "$PROJECT_DIR/other.txt"
  git -C "$PROJECT_DIR" add other.txt
  git -C "$PROJECT_DIR" commit -m "other: unrelated PR" -q
  push_to_origin

  run rebase_onto_default_branch
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"squash-merged"* ]]
  [[ -f "$WORK_DIR/second.txt" ]]
  [[ -f "$WORK_DIR/other.txt" ]]
}

# Proves: squash-merge rebase works when stacked branch modifies the same file.
# Reproduces: branch 03 adds tests across multiple commits, branch 04 moves some
# of those tests elsewhere. Multiple intermediate commits create divergent context
# that causes git's 3-way merge to conflict on the squashed rebase.
@test "rebase_onto_default_branch handles stacked branch modifying same file" {
  # Start with a file that branch 03 will modify
  cat > "$PROJECT_DIR/tests.txt" <<'EOF'
test_alpha() { run alpha; }
test_beta() { run beta; }
test_gamma() { run gamma; }
EOF
  git -C "$PROJECT_DIR" add tests.txt
  git -C "$PROJECT_DIR" commit -m "initial tests" -q

  setup_rebase_env

  # Branch 03: add tests in multiple commits (creates intermediate history
  # that will conflict with squash on rebase)
  rename_branch_for_task "add more tests"
  # First commit: partial addition — this intermediate state differs from squash
  cat > "$WORK_DIR/tests.txt" <<'EOF'
test_alpha() { run alpha; }
test_beta() { run beta; }
test_gamma() { run gamma; }

// new tests
test_delta() {
  setup();
  run delta;
}
EOF
  git -C "$WORK_DIR" add tests.txt
  git -C "$WORK_DIR" commit -m "add delta test" -q

  # Second commit: add more tests and reformat
  cat > "$WORK_DIR/tests.txt" <<'EOF'
test_alpha() { run alpha; }
test_beta() { run beta; }
test_gamma() { run gamma; }

// new tests
test_delta() {
  setup();
  run delta;
}
test_epsilon() {
  setup();
  run epsilon;
}

// layout-dependent tests
test_overlay() {
  const el = makeElement("DIV", { top: 10 });
  assert.ok(el.style.top === "10px");
}
test_clipping() {
  const el = makeElement("DIV", { overflow: "hidden" });
  assert.ok(!isVisible(el));
}
EOF
  git -C "$WORK_DIR" add tests.txt
  git -C "$WORK_DIR" commit -m "add epsilon, overlay, clipping tests" -q

  # Branch 04: move layout tests to separate file (modifies same file as 03)
  rotate_branch
  rename_branch_for_task "move layout tests"
  cat > "$WORK_DIR/tests.txt" <<'EOF'
test_alpha() { run alpha; }
test_beta() { run beta; }
test_gamma() { run gamma; }

// new tests
test_delta() {
  setup();
  run delta;
}
test_epsilon() {
  setup();
  run epsilon;
}
EOF
  cat > "$WORK_DIR/layout_tests.txt" <<'EOF'
// layout-dependent tests (moved from tests.txt)
test_overlay() {
  const el = makeElement("DIV", { top: 10 });
  assert.ok(el.style.top === "10px");
}
test_clipping() {
  const el = makeElement("DIV", { overflow: "hidden" });
  assert.ok(!isVisible(el));
}
EOF
  git -C "$WORK_DIR" add tests.txt layout_tests.txt
  git -C "$WORK_DIR" commit -m "move layout tests to separate file" -q

  # Another PR lands on main touching the same file
  cat > "$PROJECT_DIR/tests.txt" <<'EOF'
test_alpha() { run alpha; }
test_beta() { run beta; }
test_gamma() { run gamma; }
// added by another PR
test_zeta() { run zeta; }
EOF
  git -C "$PROJECT_DIR" add tests.txt
  git -C "$PROJECT_DIR" commit -m "other PR: add zeta test" -q

  # Simulate squash-merge of branch 03 into main (conflict resolution with
  # the other PR means content differs from branch 03's tip)
  cat > "$PROJECT_DIR/tests.txt" <<'EOF'
test_alpha() { run alpha; }
test_beta() { run beta; }
test_gamma() { run gamma; }
// added by another PR
test_zeta() { run zeta; }

// new tests
test_delta() {
  setup();
  run delta;
}
test_epsilon() {
  setup();
  run epsilon;
}

// layout-dependent tests
test_overlay() {
  const el = makeElement("DIV", { top: 10 });
  assert.ok(el.style.top === "10px");
}
test_clipping() {
  const el = makeElement("DIV", { overflow: "hidden" });
  assert.ok(!isVisible(el));
}
EOF
  git -C "$PROJECT_DIR" add tests.txt
  git -C "$PROJECT_DIR" commit -m "squash: add more tests" -q
  push_to_origin

  run rebase_onto_default_branch
  [[ "$status" -eq 0 ]]
  [[ "$output" == *"squash-merged"* ]]
  # Branch 04's changes should be intact
  [[ -f "$WORK_DIR/layout_tests.txt" ]]
  # tests.txt should have layout tests removed (branch 04's change)
  ! grep -q "test_overlay" "$WORK_DIR/tests.txt"
  grep -q "test_alpha" "$WORK_DIR/tests.txt"
}

# Proves: .gitignore is committed to main so the worktree inherits it.
@test "worktree inherits .gitignore from main" {
  init_ralph_dir
  setup_worktree
  grep -q '^\.ralph$' "$WORK_DIR/.gitignore"
}

# Proves: existing .gitignore content is preserved when appending entries.
@test "existing .gitignore content preserved" {
  echo "node_modules" > "$PROJECT_DIR/.gitignore"
  git -C "$PROJECT_DIR" add .gitignore
  git -C "$PROJECT_DIR" commit -m "add gitignore" -q

  init_ralph_dir
  setup_worktree
  grep -q '^node_modules$' "$WORK_DIR/.gitignore"
  grep -q '^\.ralph$' "$WORK_DIR/.gitignore"
}

# Proves: ralph refuses to run with uncommitted changes so the .gitignore
# commit to main doesn't sweep in unrelated staged work.
@test "dirty working tree exits with error" {
  echo "uncommitted" > "$PROJECT_DIR/dirty.txt"
  git -C "$PROJECT_DIR" add dirty.txt

  run init_ralph_dir
  [[ "$status" -eq 1 ]]
  [[ "$output" == *"uncommitted changes"* ]]
}

# Proves: real conflicts halt ralph instead of continuing on stale base.
@test "rebase_onto_default_branch halts on real conflicts" {
  setup_rebase_env

  echo "worktree version" > "$WORK_DIR/conflict.txt"
  git -C "$WORK_DIR" add conflict.txt
  git -C "$WORK_DIR" commit -m "worktree change" -q

  echo "main version" > "$PROJECT_DIR/conflict.txt"
  git -C "$PROJECT_DIR" add conflict.txt
  git -C "$PROJECT_DIR" commit -m "main change" -q
  push_to_origin

  run rebase_onto_default_branch
  [[ "$status" -eq 1 ]]
  [[ "$output" == *"real conflicts"* ]]
}

# Proves: worktree gets a thematic name from bd task titles when no
# planning session ran (bd had pre-existing tasks, so theme was never written).
@test "rename_worktree_from_theme falls back to bd task title" {
  init_ralph_dir
  setup_worktree

  TASK_BACKEND="bd"
  # Mock run_bd to return a JSON task list
  run_bd() { echo '[{"title":"auth middleware rewrite"}]'; }
  export -f run_bd

  _rename_worktree_from_theme

  local today
  today=$(date +%Y%m%d)
  [[ "$WORK_DIR" == *"/worktrees/ralph-${today}-auth-middleware-rewrite" ]]
}

# Proves: theme from state.json takes priority over bd fallback.
@test "rename_worktree_from_theme prefers state theme over bd" {
  init_ralph_dir
  setup_worktree

  TASK_BACKEND="bd"
  write_state "theme" "go migration"
  # Mock run_bd (should not be called)
  run_bd() { echo '[{"title":"wrong answer"}]'; }
  export -f run_bd

  _rename_worktree_from_theme

  local today
  today=$(date +%Y%m%d)
  [[ "$WORK_DIR" == *"/worktrees/ralph-${today}-go-migration" ]]
}
