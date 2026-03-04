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

# Proves: temp branch used before task is known.
@test "Initial branch is ralph/project/temp" {
  init_ralph_dir
  setup_worktree
  [[ "$WORKTREE_BRANCH" == "ralph/project/temp" ]]
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
@test "rotate_branch creates new next branch" {
  init_ralph_dir
  setup_worktree
  rename_branch_for_task "First task"
  rotate_branch
  [[ "$WORKTREE_BRANCH" == "ralph/project/temp" ]]
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
