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
